---
name: TODO
about: Record work a pull request knowingly left undone, usually a verification with no host to run it on
title: 'todo: '
labels: todo
type: Task

---
<!-- Keep the title at or under 80 characters, prefix included: one clause naming what was left
     undone, not why it could not run. Detail belongs in the body.

Please only use this template for work that an in-flight or merged PR deliberately deferred.
Something that is simply broken is a Bug Report; something not built yet is an Enhancement. -->

**What was left undone**:

**Where it came from** (PR, commit, or spec section):

**How this was known** (tick one — this is a different question from the line above, which asks
where the work is; this one asks who noticed it was missing):
- [ ] **The change knew** — the pull request deferred this deliberately and said so at the time
- [ ] **Worked out afterwards** — somebody read the change later and derived that it had been left

<!-- Both are legitimate records and neither is ranked above the other. They differ in what a later
     reader may trust.

     "The change knew" carries the author's own account of what was deferred AND why. "Worked out
     afterwards" carries a later reader's reconstruction, which can be exactly right about the gap
     and wrong about the reason — and the reason is the part the next person acts on, because it is
     what tells them where to start.

     So if this was worked out afterwards, keep the two apart in the body: write what you READ, then
     write what you INFERRED from it, in separate sentences. A reconstruction presented as the
     author's intent is the one failure this distinction exists to prevent. -->

**What it needs in order to run** (hardware, cluster shape, driver version, credentials, tooling):

**How to verify it** (the exact case or command, and the figure that would prove it):

**What is unproven until then**:
<!-- Name the claim that currently rests on unit tests alone, so a reader knows precisely what to
     distrust. "Untested" invites distrusting the whole change; naming the claim does not. -->
