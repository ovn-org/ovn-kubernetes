---
name: write-okep
description: Draft an OVN-Kubernetes Enhancement Proposal (OKEP). Use when the user wants to write, draft, or create an OKEP or design doc for OVN-Kubernetes. If no GitHub issue exists yet, run create-okep-issue first.
---

# Write an OKEP

## Steps

1. **Get the issue number** — if the user doesn't have one, run `create-okep-issue` first.
   The issue number is the OKEP number.

2. **Read the GitHub issue** for initial context:
   ```bash
   gh issue view <issue> --comments
   ```
   Extract the feature description, rationale, and any design discussion from comments.

3. **Interview** — work through the design one question at a time. For each question:
   provide your recommended answer, then wait for confirmation before moving on.
   Look up anything discoverable in the codebase rather than asking.
   The decisions are the user's — put each one and wait.

   Done when: no open design questions remain.

4. **Gap-check** — read `docs/okeps/okep-4368-template.md` and test the design against
   every section (testing, backwards compatibility, version skew, alternatives, etc.).
   For any section not yet covered, ask one targeted question.

   Done when: every template section has a clear answer.

5. **Scaffold** `docs/okeps/okep-<issue>-<slug>.md` from the template at
   `docs/okeps/okep-4368-template.md`. Every section must be complete — no TBDs.

6. **Register** in `mkdocs.yml` under `Enhancement Proposals:`:
   ```yaml
   - Feature Title: okeps/okep-<issue>-<slug>.md
   ```

7. **Review** — check every item before declaring done:
   - [ ] Issue link present in header
   - [ ] File named `okep-<issue>-<slug>.md`
   - [ ] All template sections complete — no TBDs or empty placeholders
   - [ ] `mkdocs.yml` entry added
