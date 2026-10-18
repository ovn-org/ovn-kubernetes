---
name: create-okep-issue
description: Open a GitHub enhancement tracking issue for a new OVN-Kubernetes feature. Use when the user wants to propose a new feature, open an enhancement issue, or before writing an OKEP.
---

# Create OKEP Issue

Interview the user to define the problem space, then open a GitHub enhancement tracking issue.
The issue number becomes the OKEP number.

## Steps

1. **Interview** — ask one question at a time until you can clearly answer both:
   - *What would you like to be added?* — the feature description
   - *Why is this needed?* — the rationale

   Look up anything discoverable in the codebase rather than asking. Provide a recommended
   answer for each question and wait for the user to confirm or correct it.

   Done when: the user confirms both answers are accurate.

2. **Open the issue**:
   ```bash
   gh issue create \
     --title "<title>" \
     --label "kind/feature" \
     --body "## What would you like to be added?

   <description>

   ## Why is this needed?

   <rationale>"
   ```

3. **Report the issue number** — hand it to the user and note that it is the OKEP number
   for when they run `write-okep`.
