#!/usr/bin/env python3
"""Deterministic offline tests for the embedded model-cache loaders."""

from __future__ import annotations

import multiprocessing
import os
import re
import sys
import tempfile
import types
import unittest
from collections.abc import Callable
from pathlib import Path
from typing import Protocol
from unittest import mock

ROOT = Path(__file__).resolve().parent
LOADER_MANIFESTS = (
    ROOT / "vllm" / "model-cache.yaml",
    ROOT / "embeddings" / "model-cache.yaml",
)
INIT_MANIFESTS = (
    ROOT / "vllm" / "helmrelease.yaml",
    ROOT / "embeddings" / "embeddings-helmrelease.yaml",
    ROOT / "embeddings" / "reranker-helmrelease.yaml",
)


class _Event(Protocol):
    def set(self) -> None: ...

    def wait(self, timeout: float | None = None) -> bool: ...


def _literal_blocks(path: Path, needle: str) -> list[str]:
    lines = path.read_text(encoding="utf-8").splitlines()
    blocks: list[str] = []
    for index, line in enumerate(lines):
        if line.strip() != "- |":
            continue
        indentation = len(line) - len(line.lstrip())
        content_indentation = indentation + 2
        content: list[str] = []
        for candidate in lines[index + 1 :]:
            if candidate and len(candidate) - len(candidate.lstrip()) < content_indentation:
                break
            content.append(candidate[content_indentation:] if candidate else "")
        block = "\n".join(content)
        if needle in block:
            blocks.append(block)
    return blocks


def _loader_scripts() -> tuple[str, ...]:
    return tuple(script for manifest in LOADER_MANIFESTS for script in _literal_blocks(manifest, "snapshot_download"))


def _run_loader(
    script: str,
    root: Path,
    revision: str,
    download: Callable[..., None],
    *,
    before_unlock: Callable[[], None] | None = None,
) -> None:
    testable = script.replace(
        'root = Path("/models")',
        'root = Path(os.environ["MODEL_TEST_ROOT"])',
    )
    if testable == script:
        raise AssertionError("loader no longer declares the canonical /models root")
    if before_unlock is not None:
        original = "os.close(lock_descriptor)\nlock_descriptor = -1"
        if testable.count(original) != 1:
            raise AssertionError("loader no longer has one exact transaction unlock boundary")
        testable = testable.replace(original, f"before_unlock()\n{original}", 1)
    module = types.ModuleType("huggingface_hub")
    module.snapshot_download = download
    with (
        mock.patch.dict(
            os.environ,
            {"MODEL_REVISION": revision, "MODEL_TEST_ROOT": str(root)},
            clear=False,
        ),
        mock.patch.dict(sys.modules, {"huggingface_hub": module}),
    ):
        namespace: dict[str, object] = {
            "__name__": "__main__",
            "before_unlock": before_unlock,
        }
        try:
            exec(compile(testable, "<model-loader>", "exec"), namespace)
        finally:
            descriptor = namespace.get("lock_descriptor")
            if type(descriptor) is int and descriptor >= 0:
                os.close(descriptor)


def _loader_child(
    script: str,
    root: Path,
    revision: str,
    started: _Event,
    download_started: _Event,
    transaction_complete: _Event,
    release: _Event,
) -> None:
    started.set()

    def download(*, local_dir: Path, **_kwargs: object) -> None:
        download_started.set()
        (local_dir / "weights.bin").write_text(revision, encoding="utf-8")

    def wait_before_unlock() -> None:
        transaction_complete.set()
        if not release.wait(10):
            raise TimeoutError("test did not release the loader transaction")

    _run_loader(
        script,
        root,
        revision,
        download,
        before_unlock=wait_before_unlock,
    )


def _successful_download(
    test: unittest.TestCase,
    expected_repo_id: str,
    calls: list[tuple[str, str]],
) -> Callable[..., None]:
    def download(
        *,
        repo_id: str,
        revision: str,
        local_dir: Path,
        max_workers: int,
    ) -> None:
        test.assertEqual(repo_id, expected_repo_id)
        test.assertEqual(max_workers, 4)
        test.assertTrue(local_dir.is_dir())
        calls.append((repo_id, revision))
        (local_dir / "weights.bin").write_text(revision, encoding="utf-8")

    return download


class ModelCacheContractTest(unittest.TestCase):
    def test_loader_transactions_are_serialized_per_target(self) -> None:
        scripts = _loader_scripts()
        self.assertEqual(len(scripts), 3)
        context = multiprocessing.get_context("fork")
        for script in scripts:
            target_name = re.search(r'target = root / "([^"]+)"', script)
            if target_name is None:
                raise AssertionError("loader omits its target")

            with self.subTest(target=target_name.group(1)), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                first_started = context.Event()
                first_download = context.Event()
                first_complete = context.Event()
                first_release = context.Event()
                first = context.Process(
                    target=_loader_child,
                    args=(
                        script,
                        root,
                        "a" * 40,
                        first_started,
                        first_download,
                        first_complete,
                        first_release,
                    ),
                )
                second_started = context.Event()
                second_download = context.Event()
                second_complete = context.Event()
                second_release = context.Event()
                second = context.Process(
                    target=_loader_child,
                    args=(
                        script,
                        root,
                        "b" * 40,
                        second_started,
                        second_download,
                        second_complete,
                        second_release,
                    ),
                )
                try:
                    first.start()
                    self.assertTrue(first_started.wait(5))
                    self.assertTrue(first_download.wait(5))
                    self.assertTrue(first_complete.wait(5))

                    second.start()
                    self.assertTrue(second_started.wait(5))
                    self.assertFalse(second_download.wait(0.5))
                    self.assertTrue(first.is_alive())
                    self.assertTrue(second.is_alive())

                    first.terminate()
                    first.join(5)
                    self.assertFalse(first.is_alive())
                    self.assertNotEqual(first.exitcode, 0)

                    self.assertTrue(second_download.wait(5))
                    self.assertTrue(second_complete.wait(5))
                    second_release.set()
                    second.join(5)
                    self.assertFalse(second.is_alive())
                    self.assertEqual(second.exitcode, 0)
                finally:
                    for process in (first, second):
                        if process.is_alive():
                            process.terminate()
                        process.join(5)

    def test_different_model_targets_are_independently_lockable(self) -> None:
        first_script, second_script, *_ = _loader_scripts()
        context = multiprocessing.get_context("fork")
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            first_started = context.Event()
            first_download = context.Event()
            first_complete = context.Event()
            first_release = context.Event()
            first = context.Process(
                target=_loader_child,
                args=(
                    first_script,
                    root,
                    "a" * 40,
                    first_started,
                    first_download,
                    first_complete,
                    first_release,
                ),
            )
            second_started = context.Event()
            second_download = context.Event()
            second_complete = context.Event()
            second_release = context.Event()
            second = context.Process(
                target=_loader_child,
                args=(
                    second_script,
                    root,
                    "b" * 40,
                    second_started,
                    second_download,
                    second_complete,
                    second_release,
                ),
            )
            try:
                first.start()
                self.assertTrue(first_complete.wait(5))
                self.assertTrue(first.is_alive())

                second.start()
                self.assertTrue(second_started.wait(5))
                self.assertTrue(second_download.wait(5))
                self.assertTrue(second_complete.wait(5))
                self.assertTrue(first.is_alive())

                second_release.set()
                second.join(5)
                self.assertEqual(second.exitcode, 0)
                first_release.set()
                first.join(5)
                self.assertEqual(first.exitcode, 0)
            finally:
                for process in (first, second):
                    if process.is_alive():
                        process.terminate()
                    process.join(5)

    def test_all_loaders_publish_one_exact_revision_snapshot(self) -> None:
        scripts = _loader_scripts()
        self.assertEqual(len(scripts), 3)
        for script in scripts:
            target_name = re.search(r'target = root / "([^"]+)"', script)
            repo_id = re.search(r'repo_id="([^"]+)"', script)
            if target_name is None or repo_id is None:
                raise AssertionError("loader omits its target or Hugging Face repository")

            with self.subTest(target=target_name.group(1)), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                target = root / target_name.group(1)
                old_revision = "a" * 40
                current_revision = "b" * 40
                next_revision = "c" * 40

                target.mkdir()
                (target / ".ready").write_text(f"{old_revision}\n", encoding="utf-8")
                (target / "weights.bin").write_text(old_revision, encoding="utf-8")
                (target / "stale-from-old-revision").write_text("stale", encoding="utf-8")

                # Simulate termination after the legacy directory was moved away but before
                # the replacement serving symlink was published.
                legacy = root / f".{target.name}.legacy"
                interrupted_snapshot = root / f".{target.name}-{current_revision}-interrupted.snapshot"
                target.rename(legacy)
                interrupted_snapshot.mkdir()
                (root / f".{target.name}.next").symlink_to(
                    interrupted_snapshot.name,
                    target_is_directory=True,
                )

                def failed_legacy_retry(*, local_dir: Path, **_kwargs: object) -> None:
                    (local_dir / "partial.bin").write_text("partial", encoding="utf-8")
                    raise RuntimeError("synthetic legacy retry failure")

                with self.assertRaisesRegex(RuntimeError, "synthetic legacy retry"):
                    _run_loader(script, root, current_revision, failed_legacy_retry)

                self.assertTrue(target.is_dir())
                self.assertFalse(target.is_symlink())
                self.assertEqual((target / "weights.bin").read_text(encoding="utf-8"), old_revision)
                self.assertEqual((target / ".ready").read_text(encoding="utf-8").strip(), old_revision)
                self.assertFalse(legacy.exists())
                self.assertFalse((root / f".{target.name}.next").exists())

                calls: list[tuple[str, str]] = []
                expected_repo_id = repo_id.group(1)
                download = _successful_download(self, expected_repo_id, calls)
                _run_loader(script, root, current_revision, download)

                self.assertTrue(target.is_symlink())
                self.assertFalse(target.readlink().is_absolute())
                self.assertEqual((target / "weights.bin").read_text(encoding="utf-8"), current_revision)
                self.assertFalse((target / "stale-from-old-revision").exists())
                self.assertEqual(
                    (target / ".ready").read_text(encoding="utf-8").strip(),
                    f"snapshot-v2:{current_revision}",
                )
                self.assertEqual(calls, [(expected_repo_id, current_revision)])
                self.assertEqual(len(tuple(root.glob(f".{target.name}-*.snapshot"))), 1)
                self.assertFalse((root / f".{target.name}.legacy").exists())

                def unexpected_download(**_kwargs: object) -> None:
                    raise AssertionError("same ready revision attempted another download")

                _run_loader(script, root, current_revision, unexpected_download)

                published_before_failure = target.readlink()

                def failed_download(*, local_dir: Path, **_kwargs: object) -> None:
                    (local_dir / "partial.bin").write_text("partial", encoding="utf-8")
                    raise RuntimeError("synthetic interrupted download")

                with self.assertRaisesRegex(RuntimeError, "synthetic interrupted"):
                    _run_loader(script, root, next_revision, failed_download)

                self.assertEqual(target.readlink(), published_before_failure)
                self.assertEqual((target / "weights.bin").read_text(encoding="utf-8"), current_revision)
                self.assertEqual(
                    (target / ".ready").read_text(encoding="utf-8").strip(),
                    f"snapshot-v2:{current_revision}",
                )
                self.assertFalse(tuple(root.glob(f".{target.name}-{next_revision}-*.snapshot")))

                _run_loader(script, root, next_revision, download)
                self.assertEqual((target / "weights.bin").read_text(encoding="utf-8"), next_revision)
                self.assertEqual(
                    (target / ".ready").read_text(encoding="utf-8").strip(),
                    f"snapshot-v2:{next_revision}",
                )
                self.assertEqual(len(tuple(root.glob(f".{target.name}-*.snapshot"))), 1)

    def test_loader_implementations_share_one_publication_contract(self) -> None:
        scripts = _loader_scripts()
        normalized = {
            re.sub(
                r'repo_id="[^"]+"',
                'repo_id="<repo>"',
                re.sub(r'target = root / "[^"]+"', 'target = root / "<target>"', script),
            )
            for script in scripts
        }
        self.assertEqual(len(scripts), 3)
        self.assertEqual(len(normalized), 1)

    def test_serving_init_containers_require_versioned_markers(self) -> None:
        scripts = tuple(
            script
            for manifest in INIT_MANIFESTS
            for script in _literal_blocks(manifest, "model download did not complete")
        )
        self.assertEqual(len(scripts), 3)
        for script in scripts:
            self.assertIn('expected_marker = f"snapshot-v2:{expected_revision}"', script)
            self.assertIn(".strip() == expected_marker", script)
            self.assertNotIn(".strip() == expected_revision", script)


if __name__ == "__main__":
    unittest.main()
