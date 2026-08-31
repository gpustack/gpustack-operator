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
