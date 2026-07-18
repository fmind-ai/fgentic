# Fgentic launch announcement checklist

Status: prepared for maintainer execution. Publication, account access, replies under the maintainer's identity, and reception tracking are human-owned. Do not post automatically.

Channel routes and rules were verified on 2026-07-18. Re-read every live rule immediately before posting; community policy overrides this handoff.

## Launch truth gate

Do not start the 48-hour announcement window until every item passes on one exact revision.

- [ ] The repository is public and the chosen revision or release is immutable and recorded.
- [ ] CI and CodeQL are green for the exact launch revision; there is no unresolved security advisory or release-blocking known defect.
- [ ] The README status, prerequisites, resource estimates, model boundaries, and teardown commands match the chosen revision.
- [ ] A runtime owner has run the quick start from a clean environment on that revision and recorded the observed duration and host profile. Do not reuse old runtime evidence as a current launch claim.
- [ ] A 30-second clip has been captured from that revision with no credentials, tokens, private room content, internal hostnames, or notification overlays visible.
- [ ] The clip labels the selected profile. If it uses the deterministic default, it says “transport fixture — not a language model.” If it uses vLLM or a provider, it names the boundary without exposing credentials or presenting one response as a quality benchmark.
- [ ] The clip shows one `!agents` or mention/`!ask` flow and the resulting reply. Any architecture overlay matches the path in [the draft](blog-post.md).
- [ ] A human has revised the article into their own voice, confirmed every current-state claim, selected the canonical publication URL, and removed the draft status line.
- [ ] The article links to the exact launch revision or release for reproducibility, plus the current roadmap, security reporting path, and 15-minute evaluation instructions.
- [ ] The maintainer has reserved time to answer technical questions for the first two hours and twice daily for the following two days.

Pause the launch if any gate fails. Fix the source or state first; do not explain around a broken quick start, red check, leaked secret, or inaccurate claim in announcement copy.

## Claim boundaries for every channel

Keep these statements consistent even when the copy is shortened:

- Fgentic is experimental, pre-1.0, and independently governed.
- The default evaluation profile proves integration with a deterministic fixture, not model intelligence or production readiness.
- A real model has been proved on the local reference path, but no production remote or public agent is enabled by default.
- The federation lab proves separate Matrix collaboration and direct A2A delegation planes; it has no Matrix appservice and does not prove one cross-org mention-to-reply flow.
- Matrix provides organization-level identity; a Signed AgentCard plus transport authorization identifies an exported A2A agent. Neither boundary makes room content trusted.
- Reservations are token-admission ceilings, not consumption. Do not turn `tokensConsumed: null` into a usage claim.
- The repo-owned k3d profiles do not enforce NetworkPolicy. Production isolation needs a known-enforcing target and conformance evidence.
- Fgentic composes Matrix, A2A, MCP, agentgateway, and kagent. Their upstream foundation homes do not certify, host, or endorse Fgentic.

Never describe the project as production-ready, CNCF/AAIF/Matrix Foundation hosted, officially compatible, fully A2A conformant, encrypted by default, or the first implementation without new independent evidence.

## Recommended 48-hour sequence

| Window        | Action                                                                                                               | Human evidence to record                            |
| ------------- | -------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------- |
| Before launch | Recheck the TWIM cutoff and prepare the entry; do not send it so early that it falls outside the evidence window.    | Current cutoff and intended edition                 |
| Hour 0        | Publish the canonical article and 30-second clip, then verify every public link from a signed-out browser.           | Article URL, clip URL, publication time, revision   |
| Hour 0–1      | Post LinkedIn from the maintainer account.                                                                           | Post URL and time                                   |
| Hour 1–3      | Submit Show HN only while the maintainer can stay present and the repository is directly runnable.                   | HN item URL and first-comment time                  |
| Hour 3–12     | Cover both subreddits through their permitted weekly threads or a documented rule-based deferral.                    | Thread comments, deferrals, and live rule evidence  |
| Hour 6–24     | Submit TWIM for the current or next edition and share targeted questions in kagent, agentgateway, and AAIF channels. | TWIM acknowledgement and community message evidence |
| Hour 24       | Answer substantive questions, correct factual errors at the source, and summarize recurring technical objections.    | Corrections and issue links                         |
| Hour 48       | Record every covered, deferred, or rejected channel on issue #69 and convert actionable feedback into scoped issues. | Final reception comment                             |

“Covered” means a compliant submission, an explicit moderator/editor deferral, or a documented rule-based deferral. It never means evading a community rule to satisfy the clock.

## This Week in Matrix

Official route: join [`#thisweekin:matrix.org`](https://matrix.to/#/#thisweekin:matrix.org), mention the `TWIM` user, and submit one Markdown message. The helper bot `@this-week-in:matrix.org` must acknowledge it. The regular cutoff is around 16:00 Paris time on Friday, though editors may announce a different cutoff. Use `###` for the project heading and keep the entry to a few paragraphs rather than a changelog.

Copy-ready entry:

```markdown
### Fgentic — sovereign Matrix rooms for A2A agents

[Fgentic](https://github.com/fmind-ai/fgentic) is a new experimental Apache-2.0 platform where humans and AI agents share self-hosted Matrix rooms. A small Go appservice maps an exact Matrix agent identity to an A2A target through agentgateway, then posts the result back to the room. kagent is the reference Kubernetes runtime; the bridge's CI also completes a Matrix appservice round trip against a standalone A2A server.

The evaluation profile on the exact launch revision is runnable on a laptop and defaults to a credential-free deterministic model fixture, so it proves the Matrix -> bridge -> agentgateway -> kagent path without prompt egress or token cost. A separate federation lab exercises a room-v12 Matrix exchange between two admitted homeservers, a denied control, and one tightly scoped org-to-org A2A route. It is pre-1.0: no production remote is enabled, the k3d profiles do not enforce NetworkPolicy, and the federation lab does not yet join the two planes into one cross-org mention flow.

Read the launch article: <canonical article URL>

Try it: https://github.com/fmind-ai/fgentic#evaluate-in-15-minutes

Security and design limits: https://github.com/fmind-ai/fgentic/blob/main/docs/security.md
```

Replace the article field, send the entry as one message, and confirm the bot acknowledgement. If Friday's cutoff has passed, submit for the next edition rather than asking editors to disguise a late entry.

## Hacker News

Show HN is for work readers can try, not for the launch blog itself. Link directly to the repository, keep the project runnable without signup, begin the title with `Show HN`, and be present for questions. Do not ask anyone to upvote or seed comments.

Suggested title:

```text
Show HN: Fgentic – sovereign agents in Matrix rooms over A2A
```

Suggested first comment:

```text
Hi HN — I built Fgentic to test whether human/agent collaboration can be self-hosted without coupling chat, agent identity, model routing, and cross-org delegation to one vendor tenant.

The path on the exact launch revision is Matrix -> a small Go appservice -> A2A through agentgateway -> a kagent Agent -> a Matrix reply. The laptop demo defaults to a deterministic in-cluster response, so it is free and proves wiring rather than model quality; vLLM and configured provider profiles use the same collaboration path.

The project is experimental and the limitations are material: no production remote agent is enabled, the repo k3d profiles do not enforce NetworkPolicy, same-org agent rooms are plaintext by policy, and the federation lab proves its Matrix and A2A planes separately rather than one cross-org mention flow.

I would value feedback on the protocol boundaries, the 15-minute install, and whether the security limits are visible early enough. I will be here to answer implementation questions.
```

If signed-out users cannot clone, run, and reach the documented evaluation path, use a normal HN link submission for the technical article or wait; do not label reading material as Show HN.

## r/selfhosted

As verified on 2026-07-18, r/selfhosted requires promoted apps to be self-hostable, released, production-ready, documented, and useful to self-hosters. Projects younger than three months belong only in the current **New Project Megathread**. The repository was created on 2026-07-10 and describes itself as experimental pre-1.0, so a standalone promotional post is not currently eligible.

Re-read the live rules and current megathread on launch day. If the megathread permits this experimental project, follow its current field template exactly; otherwise record a rule-based deferral on #69 and return when the eligibility gates change. The `AI Involvement` answer is identity-bound: the human maintainer must write a complete factual disclosure before posting.

```markdown
**Project Name:** Fgentic

**Repo/Website:** https://github.com/fmind-ai/fgentic

**Description:** I maintain Fgentic, an experimental Apache-2.0 platform for running humans and A2A agents in self-hosted Matrix rooms on Kubernetes. The laptop evaluation includes documentation, a deterministic credential-free default, and a complete teardown path. It is pre-1.0 rather than production-ready: the default proves integration rather than model quality, and the k3d profile renders but does not enforce NetworkPolicy. Self-hosters retain control of the Matrix homeserver and conversation history and can choose a self-hosted or external model boundary.

**Deployment:** Follow the documented 15-minute Docker/k3d evaluation at https://github.com/fmind-ai/fgentic#evaluate-in-15-minutes. It requires Docker, Git, mise, about 8 GiB of Docker memory, four CPUs, and 10 GiB of free disk.

**AI Involvement:** <human-authored factual disclosure required before posting>
```

Do not conceal the maintainer relationship, omit the pre-1.0 status, or repost after removal without moderator guidance.

## r/kubernetes

As verified on 2026-07-18, every new tool or framework announcement belongs as a comment in the weekly **Show off your new tools and projects** thread. Re-read the live rules and locate the current thread on launch day. Do not create a standalone post or submit the launch article as a link. The comment's technical value must stand on its own: disclose the maintainer relationship, explain why the work is Kubernetes-specific, and remain available for discussion.

Suggested weekly-thread comment:

```text
Maintainer disclosure: I build Fgentic, an experimental Apache-2.0 project that composes Matrix, a Go A2A bridge, agentgateway, and kagent through Flux.

The Kubernetes-specific problem was not deploying another chat bot. It was preserving explicit workload and protocol boundaries: the bridge maps a Matrix identity to one Agent, agentgateway owns model/A2A/MCP routing and credentials, kagent remains replaceable behind A2A, and Flux reconciles the composition. The integration gate proves the bridge against a standalone A2A server with no kagent resources installed.

The default laptop demo is deterministic and credential-free. It proves the transport, not model intelligence or production isolation. The repo k3d profiles deliberately do not enforce NetworkPolicy; target deployments must prove isolation with a known-enforcing engine. The project is pre-1.0, and no production remote agent is enabled by default.

Architecture and quick start: https://github.com/fmind-ai/fgentic#readme
Security boundaries: https://github.com/fmind-ai/fgentic/blob/main/docs/security.md

I would value feedback from platform engineers on the namespace/policy boundaries, the A2A runtime seam, and what evidence you would require before evaluating this on a real cluster.
```

Record both the weekly thread URL and the comment permalink. If no current thread is available or moderators reject the comment, record that rule-based deferral instead of opening a standalone post.

## LinkedIn

Publish from the maintainer's account after personal revision. Keep the first-person motivation and limitation paragraph; do not turn the post into a foundation-logo inventory.

```text
I have been working on a simple question: can humans and AI agents collaborate in the same chat room without anchoring identity, conversation history, models, and agent routing in one proprietary tenant?

Today I am sharing Fgentic, an experimental Apache-2.0 answer built from open boundaries: Matrix for collaboration, A2A for delegation, MCP for governed tools, agentgateway for the data plane, kagent as the reference Kubernetes runtime, and a small Go bridge between the room and the agent.

The 15-minute laptop profile is public and defaults to a deterministic in-cluster response. That makes the integration testable without a model credential, prompt egress, or token charge — and it means the default demo proves wiring, not model quality.

The limitations matter: Fgentic is pre-1.0, no production remote agent is enabled by default, the laptop k3d profiles do not enforce NetworkPolicy, and the federation lab currently proves Matrix collaboration and direct A2A delegation as separate planes.

What I want next is precise feedback from platform teams and Matrix/A2A/MCP/kagent/agentgateway practitioners: where does the install fail, which trust boundary is unclear, and what evidence would you need for a real cross-organization evaluation?

Article: <canonical article URL>
Repository: https://github.com/fmind-ai/fgentic
Roadmap: https://github.com/fmind-ai/fgentic/issues/316
```

## kagent community

Use the current Discord route from the official [kagent community page](https://kagent.dev/community). Coordinate with the already prepared [kagent outreach handoff](../kagent/outreach.md) so the launch does not duplicate or pre-empt the specific issue #67 relationship request.

```text
Hi kagent community — maintainer disclosure: I build Fgentic, an experimental Apache-2.0 Matrix-to-A2A collaboration layer that uses unmodified kagent Agents as its reference Kubernetes runtime.

I have published the evaluation path and would value technical feedback on one boundary: does the current exact Matrix-ghost -> namespace/name mapping, A2A dispatch through agentgateway, and standalone-A2A integration test make runtime ownership clear enough?

The default demo is deterministic and proves transport, not model quality or production readiness. Overview: https://github.com/fmind-ai/fgentic/blob/main/.github/community/kagent/one-pager.md
```

## agentgateway community

Use the current Discord badge or community-meeting link from the official [agentgateway repository](https://github.com/agentgateway/agentgateway). Ask about the data-plane integration; do not ask for generic amplification or imply AAIF endorsement.

```text
Hi agentgateway community — maintainer disclosure: I build Fgentic, an experimental Matrix-to-A2A collaboration platform that routes its local A2A, MCP, and model boundaries through agentgateway.

The evaluation path is public. I would value review of the integration assumptions: the bridge holds no model credential, local A2A routes through agentgateway to kagent, MCP is internal and allowlisted, and the federation lab exports one exact JWT-protected A2A route with reservation limits. The deterministic default proves wiring rather than model quality.

Architecture: https://github.com/fmind-ai/fgentic/blob/main/docs/architecture.md
Security boundaries: https://github.com/fmind-ai/fgentic/blob/main/docs/security.md
```

## AAIF community

Use the current Discord link from the official [AAIF site](https://aaif.io/). Fgentic is not an AAIF-hosted project. Frame the post as an independent composition using AAIF-hosted MCP and agentgateway, with A2A correctly described as a separate Linux Foundation project.

```text
Hi AAIF community — maintainer disclosure: I build Fgentic, an independently governed Apache-2.0 platform for human/agent collaboration in self-hosted Matrix rooms.

Fgentic composes MCP and agentgateway with Matrix, A2A, kagent, and a small Go bridge. I am sharing the experimental evaluation path to get protocol-boundary feedback, not to claim AAIF hosting or endorsement. In particular: is the separation between A2A delegation, internal allowlisted MCP tools, and the agentgateway credential/policy boundary explained clearly enough for an operator to audit?

Open-stack governance map: https://github.com/fmind-ai/fgentic/blob/main/docs/open-stack.md
Repository: https://github.com/fmind-ai/fgentic
```

## Reception record

Add one comment to [issue #69](https://github.com/fmind-ai/fgentic/issues/69) after 48 hours. Record facts and actionable feedback, not vanity totals or inferred sentiment.

```markdown
Launch revision/release: <immutable SHA or tag>

Canonical article and clip: <URLs>

Window: <UTC start> to <UTC end>

Channels:

| Channel      | Submitted UTC | Public URL or rule-based deferral | Outcome |
| ------------ | ------------- | --------------------------------- | ------- |
| TWIM         |               |                                   |         |
| Hacker News  |               |                                   |         |
| r/selfhosted |               |                                   |         |
| r/kubernetes |               |                                   |         |
| LinkedIn     |               |                                   |         |
| kagent       |               |                                   |         |
| agentgateway |               |                                   |         |
| AAIF         |               |                                   |         |

Recurring technical feedback:

- <claim or boundary challenged, with source URL>

Corrections made:

- <source change, announcement edit, or none>

Follow-up issues:

- <scoped issue URL, owner, and why it belongs in v1>
```

Open follow-up issues only for reproduced defects, missing evidence, or concrete documentation gaps. Do not convert general praise, feature brainstorming, or channel metrics into roadmap scope without the normal v1 triage.

## Current official references

- [This Week in Matrix submission guide](https://matrix.org/twim-guide/)
- [Show HN guidelines](https://news.ycombinator.com/showhn.html)
- [r/selfhosted live rules](https://www.reddit.com/r/selfhosted/about/rules)
- [r/kubernetes live rules](https://www.reddit.com/r/kubernetes/about/rules)
- [kagent community](https://kagent.dev/community)
- [agentgateway repository and community links](https://github.com/agentgateway/agentgateway)
- [Agentic AI Foundation](https://aaif.io/)
