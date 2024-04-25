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
  * The code of conduct committee for this project is the same as the maintainers list for this project. The current maintainers can be found in [MAINTAINERS.md](./MAINTAINERS.md). If you face any issues please reach out to one of the maintainers on our slack channel (workspace: https://ovn-org.slack.com/, channel: #ovn-kubernetes)

### Trust

Be trustworthy. During a review, your actions both build and help maintain the trust that the community has placed in this project. Below are examples of ways that we build trust:

* **Transparency** - If a pull request won't be merged or shouldn't be merged, clearly say why and tag a maintainer to close it. If a pull request won't be reviewed for a while, let the author know so they can set expectations and understand why it's blocked.
* **Integrity** - Put the project's best interests ahead of personal relationships or company affiliations when deciding if a change should be merged.
* **Stability** - Only LGTM when then change won't negatively impact project stability. It can be tempting to LGTM a pull request that doesn't meet our quality standards, for example when the review has been delayed, or because we are trying to deliver new features quickly, but regressions can significantly hurt trust in our project.

## Process

* Reviewers are automatically assigned via the load-balancing algorithm using contributors from the ovn-org/ovn-kubernetes-members team.
* Reviewers may opt out of reviewing of any PR or the reviewing process altogether by contacting committers or setting their github profile status as "busy" and removing themselves from any currently assigned PR.
* Reviewers should wait for automated checks to pass before reviewing
* At least 1 approved review is required from a maintainer before a pull request can be merged
* All CI checks must pass
* If a PR is stuck for some reason it is down to the reviewer to determine the best course of action:
  * PRs may be closed if they are no longer relevant
  * A maintainer may choose to carry a PR forward on their own, but they should ALWAYS include the original author's commits
  * A maintainer may choose to open additional PRs to help lay a foundation on which the stuck PR can be unstuck. They may either rebase the stuck PR themselves or leave this to the author
* Maintainers should not merge their pull requests without a review
* Once a reviewer has approved the PR, the reviewer should add a committer for final PR approval and merge.
* In times of need, i.e. to fix pressing security issues or fix critical panic issues, the Maintainers may, at their discretion, merge PRs without review. They must add a comment to the PR explaining why they did so.


## Checklist

Below are a set of common questions that apply to all pull requests:

- [ ] Is this PR targeting the correct branch?
- [ ] Does the commit message provide an adequate description of the change?
- [ ] Does the affected code have corresponding unit, end-to-end and feature integration tests?
- [ ] Are the changes documented, not just with inline documentation, but also with conceptual documentation such as an overview of a new feature, or task-based documentation like a tutorial? Consider if this change should be announced on your project blog.
- [ ] Does this introduce breaking changes that would require an announcement or bumping the major version?


## Reading List

Reviewers are encouraged to read the following articles for help with common reviewer tasks:

* [The Art of Closing: How to closing an unfinished or rejected pull request](https://blog.jessfraz.com/post/the-art-of-closing/)
* [Kindness and Code Reviews: Improving the Way We Give Feedback](https://product.voxmedia.com/2018/8/21/17549400/kindness-and-code-reviews-improving-the-way-we-give-feedback)
* [Code Review Guidelines for Humans: Examples of good and back feedback](https://phauer.com/2018/code-review-guidelines/#code-reviews-guidelines-for-the-reviewer)
