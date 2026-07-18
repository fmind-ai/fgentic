---
type: Runbook
title: Demo Recording
description: Capture and publish the 30-second Matrix mention-to-agent-reply proof without leaking credentials or overstating the tested profile.
---

# Demo recording

This runbook prepares the proof-of-life media for the README. No placeholder or synthetic clip should be published: the recording must come from the exact revision and profile named on screen, pass the checks below, and receive the issue-required human review.

## Evidence contract

The default evaluation and production-shaped profiles prove different things. Keep their captures separate.

| Capture                 | Required profile                      | What it may show                                                                                           | Required disclosure                                                                                 |
| ----------------------- | ------------------------------------- | ---------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------- |
| README preview          | `demo` through `mise run demo:up`     | Sanitized ready state, Alice login, `#lobby`, one real Matrix mention, and the resulting agent-ghost reply | `Default evaluation profile: deterministic transport fixture — not a language model.`               |
| Observability companion | An installed `local` or `gcp` profile | Representative mention traffic followed by non-empty panels in `Fgentic — LLM Token & Cost Guard`          | `Production-shaped profile: aggregate model tokens — not user, room, team, or invoice attribution.` |

The `demo` overlay deliberately omits observability, so it cannot produce a Grafana cost-panel shot. Do not hide a profile change with an invisible edit. If one video contains both captures, insert a full-frame topology-change card before Grafana and keep the profile label visible for the whole segment.

## Prepare the private capture

Only the designated runtime owner may perform these steps because the repo-owned demo uses shared Docker, k3d, image, and port state.

1. Choose a green runtime source commit and require `test -z "$(git status --porcelain)"` before either `demo:up` run. The demo snapshots tracked and untracked working-tree changes, so a dirty checkout cannot be identified by `HEAD`. Record the clean commit's full SHA, UTC capture time, profile, model boundary, host OS, and whether the segment is live or recorded. The later media-only PR head will differ because it adds the captured assets; never claim that PR head itself was exercised.
1. Close unrelated browser tabs and applications. Disable notifications, password-manager prompts, shell history overlays, and desktop widgets. Use a fresh browser profile at 1280×720 or another 16:9 resolution.
1. Run `mise run demo:up` privately first. Retain the generated Alice password only for this disposable cluster; never put it in a recording, issue, PR, clipboard history, or repository file.
1. Trust the generated local CA, open the printed Element URL, sign in as the printed Alice user, and open the seeded `#lobby:fgentic.localhost` room. Confirm that the mapped ghosts are present before recording.
1. Keep every raw capture under the gitignored `.agents/tmp/demo-recording/` directory until the secret review and human approval are complete.

## Record the terminal handoff

Use [asciinema](https://docs.asciinema.org/manual/cli/quick-start/) without stdin capture. Running `demo:up` a second time reuses the owned cluster and repeats its reconciliation and acceptance; the idle-time limit and output filter keep only the final handoff in the cast. Start the cast:

```bash
umask 077
mkdir -p .agents/tmp/demo-recording
asciinema rec --idle-time-limit 1 .agents/tmp/demo-recording/demo-ready.cast
```

Inside the recorded shell, redact the generated password before it reaches the terminal:

```bash
set -euo pipefail
test -z "$(git status --porcelain)"
DEMO_SOURCE_SHA="$(git rev-parse HEAD)"
DEMO_CAPTURE_UTC="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
mise run demo:up 2>&1 \
  | sed -n '/^Fgentic evaluation is ready\.$/,$p' \
  | sed -E 's/^(Password:).*/\1 [redacted]/'
printf 'Source:   %s\nCaptured: %s\n' "${DEMO_SOURCE_SHA}" "${DEMO_CAPTURE_UTC}"
exit
```

`pipefail` preserves a non-zero `demo:up` result even though its progress output is suppressed, and `errexit` stops the cast before it prints evidence for a failed run; discard any failed cast. Replay the whole successful cast locally with `asciinema play .agents/tmp/demo-recording/demo-ready.cast`. Inspect every frame; the only permitted ready-state fields are URL, user, redacted password, room, provider/model, source SHA, and capture time. The cast must not contain tokens, kubeconfig paths, Docker details, unrelated shell output, or the unredacted password. Do not upload the raw cast to asciinema.org.

## Record the 30-second Element loop

Record the browser locally to `.agents/tmp/demo-recording/mention-reply.webm`, without microphone audio. Use a real Matrix mention selected by Element so the event carries `m.mentions`; do not substitute an edited chat mock-up.

Keep a visible footer throughout the browser clip: `demo · source <12-character SHA> · <UTC capture time>`. The full source SHA and the same timestamp belong in the issue evidence. This identifies the runtime commit; it is not the SHA of the later media-only PR.

| Time    | Shot                                                                                                                                                                                  |
| ------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 0–4 s   | Sanitized terminal ready state: `fgentic.localhost`, Alice, `#lobby`, and `demo` provider; password remains redacted.                                                                 |
| 4–9 s   | Element login completes and the seeded lobby opens. Password glyphs may be visible; the password itself, browser autofill UI, and recovery prompts may not.                           |
| 9–15 s  | Alice selects `@agent-docs-qa:fgentic.localhost` with Element's mention picker and sends `Confirm that the evaluation path works.`                                                    |
| 15–26 s | Keep the originating event and the agent ghost in frame until the deterministic reply appears: `Fgentic's deterministic evaluation model is working through agentgateway and kagent.` |
| 26–30 s | Hold the reply with the profile disclosure visible. Do not imply answer quality, tool use, model intelligence, NetworkPolicy enforcement, or production readiness.                    |

If the response exceeds the timing budget, keep the wait authentic and trim only dead time. Do not pre-place the reply, change timestamps, or splice a seeded probe to look like the newly recorded mention.

After the browser clip passes frame-by-frame review, create the README GIF from that WebM with [FFmpeg's palette filters](https://ffmpeg.org/ffmpeg-filters.html#palettegen-1):

```bash
ffmpeg -i .agents/tmp/demo-recording/mention-reply.webm \
  -vf 'fps=12,scale=960:-1:flags=lanczos,split[s0][s1];[s0]palettegen=max_colors=128[p];[s1][p]paletteuse=dither=bayer' \
  -loop 0 .agents/tmp/demo-recording/mention-reply.gif
```

Replay the GIF at normal speed and compare its first and final frames with the WebM. The profile, source, timestamp, mention, reply, and disclosure must remain legible after downscaling.

## Record Grafana separately

Use an already installed, accepted `local` or `gcp` candidate profile; do not create that runtime from the RED lane. After representative model traffic, open `Fgentic — LLM Token & Cost Guard` at `https://grafana.<server_name>` and require non-empty data for the relevant provider/model interval. Show the token-rate and 15-minute-burn panels with the separate-profile disclosure visible. Follow [Reading the cost dashboards](cost-dashboards.md): the panels are aggregate operational signals, not per-request, person, room, partner, team, or currency evidence.

Do not record the Grafana login, generated administrator password, query inspector, datasource credentials, unrelated dashboards, or other tenant data. A static dashboard render or an empty panel is not runtime evidence.

## Approve and publish

The human reviewer must watch every frame at normal speed and inspect the cast as text before any asset enters Git. Record the approved SHA, profile, UTC time, reviewer, and findings on issue #25. Reject the capture if it contains a secret, notification, private room content, misleading edit, unreadable mobile frame, or unsupported claim.

After approval, publish only these stable paths:

```text
docs/assets/demo/demo-ready.cast
docs/assets/demo/mention-reply.webm
docs/assets/demo/mention-reply.gif
```

Keep the GIF small enough for a fast README preview and link it to the sharper WebM. Insert the following block after the README status notice and before the opening product sentence; do not add it before the real assets exist:

```markdown
[![Fgentic 30-second Matrix mention-to-agent-reply demo](docs/assets/demo/mention-reply.gif)](docs/assets/demo/mention-reply.webm)

> Default evaluation profile: deterministic transport fixture — not a language model. [Terminal cast](docs/assets/demo/demo-ready.cast) · [Run it locally](#evaluate-in-15-minutes)
```

Before merging the media PR:

1. Open the README from a signed-out browser and verify that the preview and both links work without installing anything.
1. Confirm the on-screen profile, runtime source SHA, and capture time match the issue evidence. The media PR must be based on that source commit, and `git diff --name-only <runtime-source-sha>...HEAD` must contain only `README.md` plus the three approved `docs/assets/demo/` files; any runtime-source change invalidates the capture.
1. Run `mise run check:docs`, then allow the installed commit and push hooks to run the canonical `check` and `test` gates.
1. Keep publication human-owned. A green media PR does not authorize announcing, posting, or presenting it under the maintainer's identity.
