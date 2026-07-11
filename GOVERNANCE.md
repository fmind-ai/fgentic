# Governance

Fgentic aims for vendor-neutral, open governance compatible with a future foundation home (CNCF Sandbox first, AAIF when adoption warrants — see SPEC §1). Until the community grows, governance is deliberately simple and honest about its current shape.

## Current model: maintainer-led

1. The project is led by its maintainers, listed in [MAINTAINERS.md](MAINTAINERS.md). While there is a single maintainer, that maintainer decides; decisions of record land in [docs/adr/](docs/adr/) or SPEC.md, never in private channels.
1. All decisions, roadmaps, and discussions are public: GitHub issues, milestones, ADRs, and Discussions. There is no private decision path except security reports ([SECURITY.md](SECURITY.md)).
1. Settled design decisions (SPEC §4 D1–D16, ADRs) are revisited by proposing a new ADR with evidence — not relitigated per-PR.

## Contributor ladder

1. **Contributor** — anyone with a merged PR (DCO-signed, Apache-2.0).
1. **Reviewer** — sustained, quality contributions in an area (`area/*` label scope); may be asked to review PRs in that area. Nominated by a maintainer, recorded in MAINTAINERS.md.
1. **Maintainer** — merge rights and release rights; sustained ownership of one or more areas, demonstrated judgment on the project's principles (open standards, sovereignty, honesty about limits). Nominated by an existing maintainer, public lazy consensus (1 week) among maintainers, recorded in MAINTAINERS.md.

Inactive maintainers (12 months without activity) move to emeritus status after a private heads-up.

## Changes to governance

This document changes by PR with maintainer lazy consensus (1 week). When the project reaches three maintainers from at least two organizations, this model will be revisited toward a steering structure that matches foundation expectations.
