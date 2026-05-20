# Governance

> **Status: Draft — under review and discussion.** This document is a proposal and has not yet been ratified by the Substrate maintainers. Feedback welcome via PR review or on the `ate-dev@groups.google.com` mailing list.

Substrate is an Apache-2.0 open-source project. This document describes how decisions get made and how contributors can take on more responsibility over time.

For day-to-day collaboration norms (PR workflow, communication, AI-tool disclosure), see [COLLABORATING.md](COLLABORATING.md). For build and contribution instructions, see [CONTRIBUTING.md](CONTRIBUTING.md). For community conduct, see [code-of-conduct.md](code-of-conduct.md).

## Roles

**Contributors** — anyone who opens issues, PRs, or joins discussions. Must follow the Code of Conduct and sign the [Google Contributor License Agreement](https://cla.developers.google.com/about) before their first contribution is merged (see [CONTRIBUTING.md](CONTRIBUTING.md)).

**Reviewers** — contributors with sustained, high-quality work in a specific area. They review PRs, triage issues, and mentor newcomers. Nominated by a Reviewer or Maintainer; confirmed by majority of Maintainers.

**Maintainers** — Reviewers with broad responsibility for project health. They approve and merge PRs, make architectural decisions, manage releases, and represent the project externally. Nominated by an existing Maintainer; confirmed by a 2/3 supermajority of Maintainers.

The current list of Maintainers and per-area Reviewers lives in the [`CODEOWNERS`](.github/CODEOWNERS) file.

## Decisions

- **Code changes.** Every PR needs at least one Maintainer approval and green CI before merge. Authors do not merge their own PRs.
- **Design changes.** File a GitHub issue or discussion describing the proposal and tag relevant Maintainers. Allow at least one week for feedback. Significant changes require majority Maintainer support.
- **Disputes.** Try to resolve on the PR or issue first. If that fails, any participant can ask the Maintainers to decide; Maintainers resolve by majority vote.
- **Code-of-Conduct issues.** Reported and handled per [code-of-conduct.md](code-of-conduct.md).

## Activity

Roles require ongoing participation. Reviewers and Maintainers inactive for six months may have their status reviewed, with allowances for known absences (e.g., sabbatical, parental leave).

## Changing this document

Open a PR. Allow at least one week for discussion. Requires a 2/3 supermajority of Maintainers to merge.
