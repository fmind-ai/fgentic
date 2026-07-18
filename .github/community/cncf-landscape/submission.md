# CNCF Landscape entry preparation

Status: prepared but ineligible for filing; do not open an upstream pull request before every gate below passes.

Verified on 2026-07-18 against the official [CNCF Landscape entry guidelines](https://github.com/cncf/landscape/blob/master/README.md#new-entries), [landscape data](https://github.com/cncf/landscape/blob/master/landscape.yml), and [validation workflow](https://github.com/cncf/landscape/blob/master/.github/workflows/validate.yml).

## Eligibility

| Criterion                   | Verified state                                                                                                              | Filing gate                                                                                                       |
| --------------------------- | --------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------- |
| Public cloud-native project | The repository is public, Apache-2.0 licensed, and Kubernetes-native.                                                       | Recheck repository visibility and license immediately before filing.                                              |
| At least 300 GitHub stars   | **Blocked: 1/300** on 2026-07-18.                                                                                           | Wait until the live repository count is at least 300.                                                             |
| Existing category           | Proposed: `AI Agent / Agent Framework`, currently the closest existing home for an agent collaboration platform.            | Ask CNCF reviewers to confirm the category; do not request a new category.                                        |
| One best-fit box            | The fragment proposes only one entry.                                                                                       | Do not add a second path or duplicate product entry.                                                              |
| SVG logo with English name  | `fgentic.svg` is a transparent, path-only Outfit wordmark with the English project name.                                    | Recheck the rendered asset and copy it to upstream `hosted_logos/fgentic.svg`.                                    |
| Controlling organization    | The public controller is the [`fmind-ai`](https://github.com/fmind-ai) GitHub organization. No Crunchbase URL was verified. | The maintainer must verify or create the controlling-organization record if CNCF requests one; never guess a URL. |

## Prepared upstream patch

1. Copy `fgentic.svg` to `hosted_logos/fgentic.svg` in a current fork of `cncf/landscape`.
1. Insert `landscape-fragment.yml` alphabetically after `Crew AI` in `AI Agent / Agent Framework`.
1. Re-read the upstream guidelines and confirm the live star count is at least 300.
1. Confirm the project description, repository URL, proposed category, logo rendering, and controlling organization are still accurate.
1. Run the upstream `cncf/landscape2-validate-action@v2` data validation or its current documented equivalent.
1. **Human:** open the upstream pull request under the maintainer's identity and address CNCF review.
1. Record the upstream pull request, merged entry, or reviewer feedback on [#66](https://github.com/fmind-ai/fgentic/issues/66).

The wordmark uses the canonical Fmind visual system's Outfit 600 geometry, navy `#0F172A`, and indigo `#646CFF`. Its glyphs are converted to SVG paths, so the upstream asset has no private font or filesystem dependency, keeps a transparent background, and remains legible on the Landscape's white canvas.
