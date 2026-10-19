# Reviewing Guide

This document covers who may review pull requests for this project, and provides guidance on how to perform code reviews that meet our community standards and code of conduct. All reviewers must read this document and agree to follow the project review guidelines. Reviewers who do not follow these guidelines may have their privileges revoked.

## The Reviewer Role

We welcome all contributors to wear their reviewer hats! The reviewer role is distinct from the approver/maintainer role. Anyone is welcome to be a reviewer and take on the reviewer role in our community. Reviewers can LGTM a pull request but they cannot merge it. A maintainer/approver handles the final approval and merging of the pull request. The current maintainers can be found in [MAINTAINERS.md](./MAINTAINERS.md).

## Values

All reviewers must abide by the [Code of Conduct](CODE_OF_CONDUCT.md) and are also protected by it. A reviewer should not tolerate poor behavior and is encouraged to report any behavior that violates the Code of Conduct. All of our values listed above are distilled from our Code of Conduct.

Below are concrete examples of how it applies to code review specifically:

### Inclusion

Be welcoming and inclusive. You should proactively ensure that the author is successful. While any particular pull request may not ultimately be merged, overall we want people to have a great experience and be willing to contribute again. Answer the questions they didn't know to ask or offer concrete help when they appear stuck.

### Sustainability

Avoid burnout by enforcing healthy boundaries. Here are some examples of how a reviewer is encouraged to act to take care of themselves:

* Authors should meet baseline expectations when submitting a pull request, such as writing tests and relevant documentation.
* If your availability changes, you can step down from a pull request and have someone else assigned.
* If interactions with an author are not following code of conduct, raise it up with your Code of Conduct committee or point of contact. It's not your job to coax people into behaving.
  * The code of conduct committee for this project is the same as the maintainers list for this project. The current maintainers can be found in [MAINTAINERS.md](./MAINTAINERS.md). If you face any issues please reach out to one of the maintainers on our slack channel (workspace: https://cloud-native.slack.com/, channel: #ovn-kubernetes)

### Trust

Be trustworthy. During a review, your actions both build and help maintain the trust that the community has placed in this project. Below are examples of ways that we build trust:

* **Transparency** - If a pull request won't be merged or shouldn't be merged, clearly say why and tag a maintainer to close it. If a pull request won't be reviewed for a while, let the author know so they can set expectations and understand why it's blocked.
* **Integrity** - Put the project's best interests ahead of personal relationships or company affiliations when deciding if a change should be merged.
* **Stability** - Only LGTM when then change won't negatively impact project stability. It can be tempting to LGTM a pull request that doesn't meet our quality standards, for example when the review has been delayed, or because we are trying to deliver new features quickly, but regressions can significantly hurt trust in our project.

## Process

* **Reviewers for area-owned files** are automatically assigned when a PR touches files listed in `CODEOWNERS`. GitHub requests reviews from the owners listed on the matching pattern lines (area maintainers and reviewers alike). Merge authority is determined separately by the merge bot based on the `Area Maintainer:` header in `CODEOWNERS`.
* Area owners listed in `CODEOWNERS` must be part of the `ovn-kubernetes/ovn-kubernetes-members` team, which grants repository write access. Without write access, GitHub ignores them as code owners — it will not assign them for review and their approving reviews will not count toward the required approval.
* If no area-specific pattern matches, reviewers are assigned via the load-balancing algorithm using contributors from the ovn-kubernetes/ovn-kubernetes-reviewers team (configured in `CODEOWNERS`). This load balancing — which reviewers are eligible, the routing algorithm, and how busy reviewers are handled — is configured by the Maintainers in the `ovn-kubernetes-reviewers` team's GitHub settings (the team's "Code review assignment" configuration).
* Area maintainers are appointed by the repo Maintainers (see [Area Maintainers](./GOVERNANCE.md#area-maintainers) in the governance docs).
* **Area maintainer merge:** Area maintainers can merge PRs that **only** touch files within their area by commenting `/area-maintainer-approved` on the PR. The merge bot (`.github/workflows/area-merge.yml`) verifies that all changed files are within the commenter's area in `CODEOWNERS` and that all CI checks pass before merging. If CI is still running, the bot waits and merges automatically when checks go green. Area maintainers cannot use `/area-maintainer-approved` on their own PRs — the bot will reject the attempt; another area maintainer or repo maintainer must approve and merge instead. PRs touching files outside the area maintainer's scope require a committer's approval.
* Reviewers can temporarily pause assignment for a couple of weeks (e.g. PTO or a busy stretch) by setting their GitHub profile status to "busy", which excludes them from load-balanced assignment, and removing themselves from any currently assigned PR. For longer unavailability, give the Maintainers a heads up. To opt out of the reviewing process altogether, contact the Maintainers to be removed from both the `ovn-kubernetes/ovn-kubernetes-reviewers` team and its mirror `ovn-kubernetes/ovn-kubernetes-reviewers-copy`, which `/assign-reviewer` draws from. The two teams are not synchronized automatically; Maintainers must apply every membership change to both, otherwise a reviewer who left the pool keeps receiving re-assignment requests, or a new reviewer never receives them.
* **Reviewers are responsible for reviewing their assigned PRs in a reasonable time.** Track your assigned PRs on the [review assignment dashboard](https://github.com/orgs/ovn-kubernetes/projects/1/views/5).
* **Keep the review moving — the goal is a reviewed PR, not that one specific person reviews it.** An assigned reviewer's responsibility is to make sure the PR actually gets reviewed, not necessarily to do the whole review alone. If you are the assignee and need help, lack the expertise for part of the change, or are unavailable, comment `/assign-reviewer` on the PR to pull in another reviewer. The Request Another Reviewer workflow (`.github/workflows/reassign-reviewer.yml`) requests the `ovn-kubernetes/ovn-kubernetes-reviewers-copy` team, a mirror of the `ovn-kubernetes-reviewers` membership, so GitHub's code review assignment routes the PR to an available member (mark yourself "busy" to be skipped). Don't let a PR stall waiting on a single person.
* Reviewers should wait for automated checks to pass before reviewing
* At least 1 approved review is required from a maintainer before a pull request can be merged. **Exception:** PRs that exclusively touch files within a single area (as defined in `CODEOWNERS`) may be merged by the designated area maintainer via `/area-maintainer-approved` without a full maintainer approval.
* All CI checks must pass
* If a PR is stuck for some reason it is down to the reviewer to determine the best course of action:
  * PRs may be closed if they are no longer relevant
  * A maintainer may choose to carry a PR forward on their own, but they should ALWAYS include the original author's commits
  * A maintainer may choose to open additional PRs to help lay a foundation on which the stuck PR can be unstuck. They may either rebase the stuck PR themselves or leave this to the author
* Maintainers should not merge their pull requests without a review
* Once a reviewer has approved the PR, the reviewer should add a committer for final PR approval and merge.
* In times of need, i.e. to fix pressing security issues or fix critical panic issues, the Maintainers may, at their discretion, merge PRs without review. They must add a comment to the PR explaining why they did so.

## Joining the auto-assignment reviewer pool

Anyone is welcome to review PRs (see [The Reviewer Role](#the-reviewer-role)). This section is specifically about joining the `ovn-kubernetes/ovn-kubernetes-reviewers` team — the pool that GitHub's load-balanced code-review assignment routes PRs to when no `CODEOWNERS` area pattern matches (see [Process](#process)).

To be added to the auto-assignment pool:

1. **Become a Member first.** Automatic assignment is only useful when the assignee's approval can count toward merge, and only Members have the write access that makes an approving review count. Follow [Becoming a Member](./GOVERNANCE.md#becoming-a-member) (proposed by a Maintainer on the developer mailing list, approved by two maintainer votes).
2. **Ask to be added to the reviewers team.** Raise it on the [developer mailing list](https://groups.google.com/g/ovn-kubernetes), in a [community meeting](./MEETINGS.md), or in the `#ovn-kubernetes` channel on the CNCF Slack (workspace: https://cloud-native.slack.com/), tagging the Maintainers. A Maintainer then adds you to the `ovn-kubernetes/ovn-kubernetes-reviewers` GitHub team and to its mirror `ovn-kubernetes/ovn-kubernetes-reviewers-copy` (see [Process](#process) for why two teams exist), after which GitHub starts routing load-balanced review requests and `/assign-reviewer` re-assignments to you.

## Checklist

Below are a set of common questions that apply to all pull requests:

- [ ] Is this PR targeting the correct branch?
- [ ] Does the commit message provide an adequate description of the change?
- [ ] Does the affected code have corresponding unit, end-to-end and feature integration tests?
- [ ] Are the changes documented, not just with inline documentation, but also with conceptual documentation such as an overview of a new feature, or task-based documentation like a tutorial? Consider if this change should be announced on your project blog.
- [ ] Does this introduce breaking changes that would require an announcement or bumping the major version?


## Reading List

Reviewers are encouraged to read the following articles for help with common reviewer tasks:

* [The Art of Closing](https://blog.jessfraz.com/post/the-art-of-closing/)
* [Kindness and Code Reviews: Improving the Way We Give Feedback](https://product.voxmedia.com/2018/8/21/17549400/kindness-and-code-reviews-improving-the-way-we-give-feedback)
* [Code Review Guidelines for Humans](https://phauer.com/2018/code-review-guidelines/#code-reviews-guidelines-for-the-reviewer)
