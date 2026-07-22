"""Dependency-free validation of a serialized audit record against its closed JSON Schema.

A small interpreter for the exact JSON Schema subset the content-bounded audit schemas use
(`type` string/integer, `minLength`, `enum`, `pattern`, `required`, `additionalProperties: false`).
It enforces the enum and pattern values, so a test can prove ADR 0018 gate 2 — the output contains the
exact allowlisted keys AND enum values — for a real record instance, not just field-name equality.
"""

from __future__ import annotations

import json
import re
from pathlib import Path


def load_schema(path: Path) -> dict[str, object]:
    parsed = json.loads(path.read_text(encoding="utf-8"))
    assert isinstance(parsed, dict)
    return parsed


def schema_property_names(schema: dict[str, object]) -> set[str]:
    properties = schema["properties"]
    assert isinstance(properties, dict)
    return {str(name) for name in properties}


def schema_required(schema: dict[str, object]) -> set[str]:
    required = schema["required"]
    assert isinstance(required, list)
    return {str(name) for name in required}


def validate_instance(instance: dict[str, object], schema: dict[str, object]) -> None:
    """Raise ``AssertionError`` if ``instance`` violates the closed ``schema``."""
    raw_properties = schema["properties"]
    assert isinstance(raw_properties, dict)
    properties = {str(name): spec for name, spec in raw_properties.items()}
    assert schema["additionalProperties"] is False
    unexpected = set(instance) - set(properties)
    assert unexpected == set(), f"instance carries keys outside the closed schema: {unexpected}"
    assert schema_required(schema) <= set(instance), "instance is missing a required field"
    for key, value in instance.items():
        spec = properties[key]
        assert isinstance(spec, dict)
        expected_type = spec.get("type")
        if expected_type == "integer":
            assert isinstance(value, int), key
            assert not isinstance(value, bool), key
        elif expected_type == "string":
            assert isinstance(value, str), key
            min_length = spec.get("minLength")
            if isinstance(min_length, int):
                assert len(value) >= min_length, key
            enum = spec.get("enum")
            if isinstance(enum, list):
                assert value in enum, key
            pattern = spec.get("pattern")
            if isinstance(pattern, str):
                assert re.search(pattern, value) is not None, key
        else:  # pragma: no cover - guards against an unhandled schema type slipping in
            raise AssertionError(f"unhandled schema type for {key!r}: {expected_type!r}")
