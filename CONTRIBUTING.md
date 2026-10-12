# Contributing Guide

* [New Contributor Guide](#contributing-guide)
  * [Ways to Contribute](#ways-to-contribute)
  * [Find an Issue](#find-an-issue)
  * [Ask for Help](#ask-for-help)
  * [Pull Request Lifecycle](#pull-request-lifecycle)
  * [Development Environment Setup](#development-environment-setup)
  * [Sign Your Commits](#sign-your-commits)
  * [Pull Request Checklist](#pull-request-checklist)
  * [AI Guidelines](#ai-guidelines)

Welcome! We are glad that you want to contribute to our project! 💖

All contributors are expected to follow our [Code of Conduct](./CODE_OF_CONDUCT.md).

As you get started, you are in the best position to give us feedback on areas of
our project that we need help with including:

* Problems found during setting up a new developer environment
* Gaps in our Quickstart Guide or documentation
* Bugs in our automation scripts

If anything doesn't make sense, or doesn't work when you run it, please open a
bug report and let us know!

## Ways to Contribute

We welcome many different types of contributions including:

* New features
* Builds, CI/CD
* Bug fixes
* Documentation
* Issue Triage
* Answering questions on Slack/Mailing List
* Web design
* Communications / Social Media / Blog Posts
* Release management

Not everything happens through a GitHub pull request. Please come to our
[meetings](./MEETINGS.md) or [contact us](https://cloud-native.slack.com/archives/C08452HR8V6) and let's discuss how we can work
together. 

### Come to Meetings

Absolutely everyone is welcome to come to any of our meetings. You never need an
invite to join us. In fact, we want you to join us, even if you don’t have
anything you feel like you want to contribute. Just being there is enough!

You can find out more about our meetings [here](./MEETINGS.md). You don’t have to turn on
your video. The first time you come, introducing yourself is more than enough.
Over time, we hope that you feel comfortable voicing your opinions, giving
feedback on others’ ideas, and even sharing your own ideas, and experiences.

## Find an Issue

We have good first issues for new contributors and help wanted issues suitable
for any contributor. [good first issue](https://github.com/ovn-kubernetes/ovn-kubernetes/labels/good%20first%20issue) has extra information to
help you make your first contribution. [help wanted](https://github.com/ovn-kubernetes/ovn-kubernetes/labels/help%20wanted) are issues
suitable for someone who isn't a core maintainer and is good to move onto after
your first pull request.

Sometimes there won’t be any issues with these labels. That’s ok! There is
likely still something for you to work on. If you want to contribute but you
don’t know where to start or can't find a suitable issue, you can reach out to us on [Slack](https://cloud-native.slack.com/archives/C08452HR8V6) and we will be happy to help.

Once you see an issue that you'd like to work on, please post a comment saying
that you want to work on it. Something like "I want to work on this" is fine.

## Ask for Help

The best way to reach us with a question when contributing is to ask on:

* The original github issue
* The developer mailing list (mailing-list: https://groups.google.com/g/ovn-kubernetes)
* Our Slack channel (workspace: https://cloud-native.slack.com/, channel: #ovn-kubernetes)

## Pull Request Lifecycle

1. When you open a PR a reviewer will automatically be assigned. This need not be a maintainer.
2. Make sure that your PR is passing CI - if you need help with failing checks please feel free to ask!
3. Once the assigned reviewer approves (+1), a maintainer (see `MAINTAINERS.md` for the current list) will review and merge your PR. You may be asked to make additional changes by maintainer post reviewer's approval.

In some cases, other changes may conflict with your PR. If this happens, you will get notified by a comment in the issue that your PR requires a rebase, and the `needs-rebase` label will be applied. Once a rebase has been performed, this label will be automatically removed.

## Development Environment Setup

You can easily setup a developer environment by following the instructions [here](https://ovn-kubernetes.io/master/installation/launching-ovn-kubernetes-on-kind/).

## Sign Your Commits

### DCO
Licensing is important to open source projects. It provides some assurances that
the software will continue to be available based under the terms that the
author(s) desired. We require that contributors sign off on commits submitted to
our project's repositories. The [Developer Certificate of Origin
(DCO)](https://probot.github.io/apps/dco/) is a way to certify that you wrote and
have the right to contribute the code you are submitting to the project.

You sign-off by adding the following to your commit messages. Your sign-off must
match the git user and email associated with the commit.

    This is my commit message

    Signed-off-by: Your Name <your.name@example.com>

Git has a `-s` command line option to do this automatically:

    git commit -s -m 'This is my commit message'

If you forgot to do this and have not yet pushed your changes to the remote
repository, you can amend your commit with the sign-off by running 

    git commit --amend -s 

## Logical Grouping of Commits

It is a recommended best practice to keep your changes as logically grouped as
possible within individual commits. If while you're developing you prefer doing
a number of commits that are "checkpoints" and don't represent a single logical
change, please squash those together before asking for a review.

When addressing review comments, please perform an interactive rebase and edit commits directly rather than adding new commits with messages like "Fix review comments".
After force-pushing the changes, GitHub shows a "force-pushed" entry with a comparison link, that allows reviewers to check the incremental diff.
Avoid mixing edited commits and a rebase within the same force-push, as that would make the incremental diff harder to read for reviewers. Push the rebased branch separately.

To help reviewers, you can also add a comment including the comparison link, to make sure it persists (GitHub sometimes consolidates several diffs into a single one).

## Commit message guidelines

A good commit message should describe what changed and why.

1. The first line should:
  
* contain a short description of the change (preferably 50 characters or less,
    and no more than 72 characters)
* be entirely in lowercase with the exception of proper nouns, acronyms, and
    the words that refer to code, like areas/function/variable names
* be prefixed with the name of the sub component being changed

  Examples:

  * networkpolicy: validate ipBlock strictly
  * egressip: fix frequently rebalancing IPs
  * services: fix etp=local + session-affnity integration

2. Keep the second line blank.
3. Wrap all other lines at 72 columns (except for long URLs).
4. If your patch fixes an open issue, you can add a reference to it at the end
   of the log. Use the `Fixes: #` prefix and the issue number. For other
   references use `Refs: #`. `Refs` may include multiple issues, separated by a
   comma.

   Examples:

   * `Fixes: #1337`
   * `Refs: #1234`

Sample complete commit message:

```txt
subcomponent: explain the commit in one line

Body of commit message is a few lines of text, explaining things
in more detail, possibly giving some background about the issue
being fixed, etc.

The body of the commit message can be several paragraphs, and
please do proper word-wrap and keep columns shorter than about
72 characters or so. That way, `git log` will show things
nicely even when it is indented.

Fixes: #1337
Refs: #453, #154
```

## Pull Request Checklist

When you submit your pull request, or you push new commits to it, our automated
systems will run some checks on your new code. We require that your pull request
passes these checks, but we also have more criteria than just that before we can
accept and merge it. We recommend that you check the following things locally
before you submit your code:

* Verify that Go code has been formatted and linted

```console
cd ovn-kubernetes/go-controller/
make lint
```

* If you are introducing new CRDs verify that Yaml files have been formatted (see
  [codegen generator](https://ovn-kubernetes.io/master/developer-guide/developer/#generating-crd-yamls-using-codegen))
* Verify that unit tests are passing locally

```console
cd ovn-kubernetes/go-controller/
make test
```

* All modular changes must be accompanied by new unit tests if they don't exist already.

* All functional changes and new features must be accompanied by extensive end-to-end test coverage

* Documentation updates are required when user-facing behavior changes

## AI Guidelines

You may use AI tools when preparing your contribution (issue, pull request etc...).
As the author, you are responsible for understanding every change you submit.

* Review and test AI-generated changes yourself before submitting. Do not rely
  on reviewers to perform the first review.
* Be prepared to explain any change in your PR. If you cannot explain why a
  change was made, the PR may be closed.
* Respond to review comments in your own words. Reviewers want to engage
  directly with you, not with generated responses.
* If you used AI tools when preparing your PR, disclose this (see
  [Disclosure](#disclosure) below).
* All contributions must follow this guide and use commit messages that align
  with our [commit message guidelines](#commit-message-guidelines). Large
  AI-generated PRs and AI-generated commit messages are discouraged.

### Disclosure

Disclose AI tool use when it has materially contributed to what you submit.
You must still adhere to our [DCO](#dco) and sign off commits.

**How to disclose:** Use the "Special notes for your reviewer" section in the
PR, and optionally add a trailer to the commit. Acceptable commit trailer
formats include:

* `Assisted-by: Tool Name and/or Model Name <contact@example.com>`
* `Co-authored-by: Tool Name and/or Model Name <contact@example.com>`

Many AI tools add `Co-authored-by` automatically; that is acceptable.

**Requires disclosure:** AI wrote or suggested code, tests, documentation, or
commit messages that you used; AI suggested an approach or refactor that
shaped the implementation.

**Does not require disclosure:** General Q&A or learning, IDE autocomplete,
using AI to explain existing code, asking AI to review your code, or
spell-check and minor fixes. When in doubt, disclose.
