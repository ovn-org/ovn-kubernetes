# OKEP-4368: OKEP Template

* Issue: [#4368](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/4368)
(Every OKEP must have an associated enhancement tracking issue that is
added to the ovn-kubernetes repo, so please open one if you don't have one
yet. Then use the Issue number xxxx as the unique number for the OKEP-xxxx
number as well as use the same file naming convention as used in this template.
So the OKEP file name must be `okep-xxxx-title.md` - The goal is that that
github issue will have disucssion details about this feature - using meeting
notes; slack threads for discussions is not desired as preserving history is
hard)

## Problem Statement

(1-2 sentence summary of the problem we are trying to solve here)

## Goals

(Bullet list of Primary goals of this proposal.)

## Non-Goals

(Bullet list of What is explicitly out of scope for this proposal.)

## Why OVN-Kubernetes?

(Required. Explain why this feature belongs in OVN-Kubernetes rather than in an
upstream project (OVN, OVS, FRR), the Kubernetes API, or a companion project.
See [Project Scope](../governance/SCOPE.md) for the full rubric.)

Answer the following:

* How does this proposal affect the pod/VM data path?
* Does it require OVN-specific capabilities, or could any CNI implement it?
* If it overlaps with an upstream project (OVN, OVS, FRR, K8s API), what is the
  upstream contribution plan and the shim removal timeline?

## Introduction

(Can link to external doc -- but we should bias towards copying
the content into the OKEP as online documents are easier to lose
-- e.g. owner messes up the permissions, accidental deletion)
Give a good detailed introduction to the problem including the
ecosystem information

## User-Stories/Use-Cases

(What new user-stories/use-cases does this OKEP introduce?)

A user story should typically have a summary structured this way:

1. **As a** [user concerned by the story]
2. **I want** [goal of the story]
3. **so that** [reason for the story]

The “so that” part is optional if more details are provided in the description.
A story can also be supplemented with examples, diagrams, or additional notes.

e.g

Story 1: Deny traffic at a cluster level

As a cluster admin, I want to apply non-overridable deny rules to certain pod(s)
and(or) Namespace(s) that isolate the selected resources from all other cluster
internal traffic.

For Example: The admin wishes to protect a sensitive namespace by applying an
AdminNetworkPolicy which denies ingress from all other in-cluster resources
for all ports and protocols.

## Proposed Solution

What is the proposed solution to solve the problem statement?

### API Details

(... details, can point to PR PoC with changes but this section has to be
explained in depth including details about each API field and validation
details)

* add details if ovnkube API is changing

### Implementation Plan

(... rough sketch of how the feature will be implemented. for example
which controllers in ovnkube will be touched. are we changing any of
the contracts between controllers or introducing new ones. basically,
if there's a chance we make a decision here that is not easily reversible
call it out here).

### Testing Details

* Unit Testing details
* E2E Testing details
* API Testing details
* Scale Testing details
* Cross Feature Testing details - coverage for interaction with other features

### Documentation Details

* New proposed additions to ovn-kubernetes.io for end users
to get started with this feature
* when you open an OKEP PR; you must also edit
https://github.com/ovn-kubernetes/ovn-kubernetes/blob/13c333afc21e89aec3cfcaa89260f72383497707/mkdocs.yml#L135
to include the path to your new OKEP (i.e Feature Title: okeps/<filename.md>)

## Risks, Known Limitations and Mitigations


## Feature Matrix Coverage

(Required. For each dimension below, state whether this proposal supports it,
explicitly defers it (with a tracking issue), or is not applicable. Silent gaps
are grounds for rejection.)

| Dimension | Status | Notes |
|-----------|--------|-------|
| Local gateway (lgw) | | |
| Shared gateway (sgw) | | |
| IPv4 | | |
| IPv6 | | |
| Dual-stack | | |
| OVN Interconnect (multi-zone) | | |
| Default cluster network | | |
| User-Defined Networks (UDN/CUDN) | | |
| DPU / SmartNIC offload | | |
| KubeVirt VMs | | |

## OVN-Kubernetes Version Skew

which version is this feature planned to be introduced in?
check repo milestones/releases to get this information for
when the next release is planned for

## Backwards Compatibility

(Describe any backwards compatibility considerations for this feature.
This should include:)

- Any changes to the API, datapath, or other components that may be backwards incompatible.
- E2E tests: When introducing new functionality, prefer adding new E2E tests rather than
  modifying existing ones. Existing E2E tests should continue to validate that the current
  API remains functional. If you must modify existing E2E tests, ensure the changes do not
  break validation of the current API behavior.

## Graduation Criteria

(When is this feature considered stable/GA? What signals indicate it is ready
to be enabled by default? Include: test coverage, known-limitation resolution,
community adoption signals.)

## Alternatives

(List other design alternatives and why we did not go in that
direction)

## References

(Add any additional document links. Again, we should try to avoid
too much content not in version control to avoid broken links)
