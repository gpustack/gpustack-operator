---
name: Bug Report
about: Report a bug encountered while using GPUStack Operator
title: 'bug: '
labels: kind/bug
type: Bug

---

<!-- Keep the title at or under 80 characters, prefix included: one clause naming the symptom, not
     the diagnosis. Detail belongs in the body — a title that needs a comma is usually two issues.

Please use this template while reporting a bug and provide as much info as possible. Not doing so may result in your bug not being addressed in a timely manner. Thanks!

If the matter is security related, do not file it here — disclose it privately to
security@gpustack.ai, or open a private advisory at
https://github.com/gpustack/gpustack-operator/security/advisories/new.
-->


**What happened**:

**What you expected to happen**:

**How to reproduce it (as minimally and precisely as possible)**:

**How this was found** (tick one):
- [ ] **Hit it** — running the operator, doing something I meant to do
- [ ] **Reported** — somebody else hit it and told us
- [ ] **Worked out** — read the code, a specification or a manifest and derived that this must happen

<!-- The three are NOT ranked, and "worked out" is not a lesser answer: plenty of real defects are
     found by reading, and a reproduction recipe can be written for a state nobody has ever been in.
     It is asked because it changes what the fix has to clear, never how seriously the report is read.

     "Hit it" and "reported" carry their own evidence that the state is reachable. "Worked out" does
     not yet, so the body should also answer:

       - Who configures the thing that triggers this, and what were they trying to do? A state only
         this project assembles is a state to document, not one to add a rule for.
       - Does the fix ask a user to tell us something they have already told us? A field that
         restates a known fact adds a copy that can disagree with the original; it adds no
         information.
       - Would the check that catches this only ever fire on inputs we wrote ourselves?

     A defect that answers none of the three may still be real and still be worth a warning, a status
     condition or a documented caveat. What it should not become is schema: a rule is much harder to
     take back than a paragraph. -->

**Anything else we need to know?**:

**Environment**:
- Kubernetes version (use `kubectl version`):
- GPUStack version:
- GPUStack Operator version:
- Cloud provider or hardware configuration:
- OS (e.g: `cat /etc/os-release`):
- Kernel (e.g. `uname -a`):
- Install tools:
- Accelerator preflight (for anything involving an accelerator, run `gpustack-operator device-manager preflight --dry-run` on the affected node and paste the output — see https://github.com/gpustack/gpustack-operator/blob/main/docs/operation/preflight.md for how to invoke it):
- Others:

<!-- Every command the binary offers, with its flags and a runnable invocation:
https://github.com/gpustack/gpustack-operator/blob/main/docs/reference/commands.md
-->
