# ovn-kubernetes Project Governance

The ovn-kubernetes project is dedicated to creating a robust Kubernetes Networking platform built from the ground up by leveraging Open vSwitch (OVS) as the data plane, and Open Virtual Network (OVN) as the SDN Controller. The project focuses strictly on enhancing networking for the Kubernetes platform and includes a wide variety of features that are critical to enterprise and telco users.

This governance explains how the project is run.

- [Values](#values)
- [Maintainers](#maintainers)
  - [Becoming a Maintainer](#becoming-a-maintainer)
  - [Removing a Maintainer](#removing-a-maintainer)
- [Members](#members)
  - [Becoming a Member](#becoming-a-member)
  - [Removing a Member](#removing-a-member)
- [Area Maintainers](#area-maintainers)
  - [Becoming an Area Maintainer](#becoming-an-area-maintainer)
  - [Removing an Area Maintainer](#removing-an-area-maintainer)
- [GitHub Teams and Repository Access](#github-teams-and-repository-access)
- [Meetings](#meetings)
- [Code of Conduct](#code-of-conduct)
- [Security Response Team](#security-response-team)
- [Voting](#voting)
- [Modifying this Charter](#modifying-this-charter)

## Values

The ovn-kubernetes and its leadership embrace the following values:

- Openness: Communication and decision-making happens in the open and is discoverable for future
  reference. As much as possible, all discussions and work take place in public
  forums and open repositories.

- Fairness: All stakeholders have the opportunity to provide feedback and submit
  contributions, which will be considered on their merits.

- Community over Product or Company: Sustaining and growing our community takes
  priority over shipping code or sponsors' organizational goals.  Each
  contributor participates in the project as an individual.

- Inclusivity: We innovate through different perspectives and skill sets, which
  can only be accomplished in a welcoming and respectful environment.

- Participation: Responsibilities within the project are earned through
  participation, and there is a clear path up the contributor ladder into leadership
  positions.

## Maintainers

ovn-kubernetes Maintainers have write access to the [project GitHub repository](https://github.com/ovn-kubernetes/ovn-kubernetes).
They can merge patches from others, and their own patches after independent review.
For pressing security or critical panic fixes, the [Reviewing Guide](./REVIEWING.md#process)
allows them to merge without review if they explain why in a PR comment. The current maintainers
can be found in [MAINTAINERS.md](./MAINTAINERS.md).  Maintainers collectively manage the project's
resources and contributors.

This privilege is granted with some expectation of responsibility: maintainers
are people who care about the ovn-kubernetes project and want to help it grow and
improve. A maintainer is not just someone who can make changes, but someone who
has demonstrated their ability to collaborate with the team, get the most
knowledgeable people to review code and docs, contribute high-quality code, and
follow through to fix issues (in code or tests).

A maintainer is a contributor to the project's success and a citizen helping
the project succeed.

The collective team of all Maintainers is known as the Maintainer Council, which
is the governing body for the project.

Maintainers have purview over all areas and Area Maintainers. They create and
dissolve areas, appoint and remove Area Maintainers, approve changes to an
area's file scope in `CODEOWNERS`, and may override or merge any PR regardless
of area boundaries. Area Maintainers operate under the authority delegated by
the Maintainers and are expected to escalate cross-area concerns or contentious
decisions to them.

### Becoming a Maintainer

To become a Maintainer you need to demonstrate the following:

- be a current [Member](#members),
- commitment to the project:
  - participate in discussions, contributions, code and documentation reviews
    for 10 months or more,
  - perform reviews for 10 non-trivial pull requests as part of the
    `ovn-kubernetes/ovn-kubernetes-reviewers` auto-assignment reviewer pool (see
    [Joining the auto-assignment reviewer pool](./REVIEWING.md#joining-the-auto-assignment-reviewer-pool)),
  -  contribute 15 non-trivial pull requests and have them merged,
- ability to write quality code and/or documentation,
- ability to collaborate with the team,
- understanding of how the team works (policies, processes for testing and code review, etc),
- understanding of the project's code base and coding and documentation style.

A new Maintainer must be proposed by an existing maintainer by sending a message to the
[developer mailing list](https://groups.google.com/g/ovn-kubernetes). A simple majority vote of existing Maintainers
approves the application.  Maintainers nominations will be evaluated without prejudice
to employer or demographics.

Maintainers who are selected will be granted the necessary GitHub rights.

### Removing a Maintainer

Maintainers may resign at any time if they feel that they will not be able to
continue fulfilling their project duties.

Maintainers may also be removed after being inactive, failure to fulfill their 
Maintainer responsibilities, violating the Code of Conduct, or other reasons.
Inactivity is defined as a period of very low or no activity in the project 
for a year or more, with no definite schedule to return to full Maintainer 
activity.

A Maintainer may be removed at any time by a 2/3 vote of the remaining maintainers.

Depending on the reason for removal, a Maintainer may be converted to Emeritus
status.  Emeritus Maintainers will still be consulted on some project matters,
and can be rapidly returned to Maintainer status if their availability changes.

## Members

Members are active contributors who have shown a commitment to the project. They
have privileges to review and approve pull requests and are part of the
`ovn-kubernetes/ovn-kubernetes-members` GitHub team, which grants write access.
Automatic PR review assignment is handled through the separate
`ovn-kubernetes/ovn-kubernetes-reviewers` team. Because Members have write access, an
approving review from a Member counts toward the branch protection rule that
requires at least one approval before a pull request can be merged. Members are
not Maintainers, but they are expected to contribute to the project and
collaborate with the team.

### Becoming a Member

To become a Member, you need to demonstrate the following:
- commitment to the project:
  - participate in discussions, contributions, code and documentation reviews
    for 3 months or more,
  - perform reviews for 5 non-trivial pull requests,
  - contribute 10 non-trivial pull requests and have them merged,
- ability to write quality code and/or documentation,
- ability to collaborate with the team (e.g., participate in project meetings,
  join discussion in the CNCF slack channel, etc.),
- understanding of how the team works (policies, processes for testing and
  code review, etc),
- understanding of the project's code base and coding and documentation style.

A new Member must be proposed by an existing maintainer by sending a message to
the developer mailing list. The application is approved with two affirmative
votes from current maintainers.

### Removing a Member

Members may resign at any time.

Members may also be removed after being inactive for a period of 6 months or
more, for failure to fulfill their responsibilities, or for violating the Code
of Conduct. A Member may be removed at any time by a simple majority vote of the
maintainers.

Members who are consistently unresponsive to assigned PR reviews may be
contacted by Maintainers to discuss their availability and commitment. If the
pattern of non-responsiveness continues, the Member may be removed.

## Area Maintainers

Area Maintainers are trusted contributors who own a specific area of the
codebase (e.g. KubeVirt, Egress IP, Services). They have the authority to review,
approve, and merge pull requests that **exclusively** touch files within their
area, as defined in `CODEOWNERS`. Area Maintainers are not full Maintainers —
they cannot merge PRs that touch files outside their designated area.

Area Maintainers are automatically requested as reviewers by GitHub when a PR
modifies files matching their `CODEOWNERS` patterns. They can merge qualifying
PRs by commenting `/area-maintainer-approved` on the PR, which triggers the
merge bot (`.github/workflows/area-merge.yml`) to verify file scope and CI
status before merging.

### Area Maintainer Responsibilities

- **Area health and ownership:** Take ownership of the overall health of the
  area. Maintain high code quality standards, ensure technical debt is managed,
  foster innovation without rushing changes that compromise stability, and
  promptly bring up stuck PRs for review at community meetings.
- **PR approval:** Review and approve pull requests within their area.
  Ensure contributions meet quality standards and are well-tested. Area
  Maintainers must not approve or merge their own pull requests — another
  area maintainer, reviewer, or repo maintainer must review and approve them.
- **Design proposals:** Own, review, and drive OKEPs (OVN-Kubernetes Enhancement
  Proposals) related to their area.
- **Documentation:** Ensure documentation for the area is accurate and
  up to date.
- **CI health:** Monitor CI for their area and address failures and flakes
  promptly without needing to be pinged.
- **Upstream engagement:** Attend OVN-Kubernetes upstream meetings and represent
  the area's interests, especially for cross-area collaborations.
- **Community support:** Help other contributors working in the area with
  reviews, guidance, and mentoring.
- **Communication:** Keep the Maintainers informed of important changes,
  design decisions, and roadmap items happening in the area.
- **Scope management:** If the area's file list in `CODEOWNERS` needs
  expansion or contraction, file a request to the Maintainers, who have the
  final say on the area's scope.

### Becoming an Area Maintainer

Area Maintainers are typically individuals who are already fulfilling the
responsibilities listed above — the role formalizes what they are already doing.
To become an Area Maintainer you need to demonstrate the following:

- commitment to the specific area:
  - participate in discussions, contributions, code and documentation reviews
    related to the area for 3 months or more,
  - perform reviews for 5 non-trivial pull requests in the area,
  - contribute 10 non-trivial pull requests to the area and have them merged,
- demonstrated ownership of area health: maintaining code quality, addressing
  CI failures, keeping documentation current, and driving improvements without
  compromising stability,
- deep understanding of the area's code, design, and interactions with the
  rest of the project,
- ability to write quality code and/or documentation,
- ability to collaborate with the team.

A new Area Maintainer must be proposed by an existing Maintainer by sending a
message to the [developer mailing list](https://groups.google.com/g/ovn-kubernetes).
The appointment requires approval from a simple majority of the Maintainers.
Once approved, the new Area Maintainer's GitHub username is added to the
relevant entries in `CODEOWNERS`.

### Removing an Area Maintainer

Area Maintainers may resign at any time.

Area Maintainers may also be removed after being inactive in their area for a
period of 6 months or more, for failure to fulfill their responsibilities, or
for violating the Code of Conduct. An Area Maintainer may be removed at any
time by a simple majority vote of the Maintainers.

## GitHub Teams and Repository Access

Access to the [project GitHub repository](https://github.com/ovn-kubernetes/ovn-kubernetes)
is granted through the following GitHub teams:

- **`ovn-kubernetes/ovn-kubernetes-admin`** — has **admin** access to the repository.
- **`ovn-kubernetes/ovn-kubernetes-members`** — has **write** access to the repository, so an
  approving review from a member counts toward the required approval.
- **`ovn-kubernetes/ovn-kubernetes-reviewers`** — backs automatic PR review assignment: GitHub's
  load-balanced code-review assignment routes review requests to this team (see
  [`CODEOWNERS`](./CODEOWNERS) and the [Reviewing Guide](./REVIEWING.md)).

## Meetings

Time zones permitting, Maintainers are expected to participate in the public
developer meeting, details of which can be found
[here](./MEETINGS.md).  

Maintainers will also have closed meetings in order to discuss security reports
or Code of Conduct violations.  Such meetings should be scheduled by any
Maintainer on receipt of a security issue or CoC report.  All current Maintainers
must be invited to such closed meetings, except for any Maintainer who is
accused of a CoC violation.

## Code of Conduct

[Code of Conduct](./CODE_OF_CONDUCT.md)
violations by community members will be discussed and resolved
on the private Slack Maintainer channel.

## Security Response Team

The Maintainers will appoint a Security Response Team to handle security reports.
This committee may simply consist of the Maintainer Council themselves.  If this
responsibility is delegated, the Maintainers will appoint a team of at least two 
contributors to handle it.  The Maintainers will review who is assigned to this
at least once a year.

The Security Response Team is responsible for handling all reports of security
holes and breaches according to the [security policy](./SECURITY.md).

## Voting

While most business in ovn-kubernetes is conducted by "[lazy consensus](https://community.apache.org/committers/lazyConsensus.html)", 
periodically the Maintainers may need to vote on specific actions or changes.
A vote can be taken on [the developer mailing list](https://groups.google.com/g/ovn-kubernetes) or
the private Maintainer Slack Channel for security or conduct matters.  
Votes may also be taken at [the developer meeting](./MEETINGS.md).  Any Maintainer may
demand a vote be taken.

Most votes require a simple majority of all Maintainers to succeed, except where
otherwise noted.  Two-thirds majority votes mean at least two-thirds of all 
existing maintainers.

## Modifying this Charter

Changes to this Governance and its supporting documents may be approved by 
a 2/3 vote of the Maintainers.
