# GPUStack Operator Agent Instructions

GPUStack Operator is a Kubernetes operator for managing GPU resources in a cluster. It provides features like GPU discovery, allocation, and monitoring to optimize GPU usage for workloads. 

GPUStack Operator relies on following famous Kubernetes projects:
- Node Feature Discovery: A Kubernetes add-on for detecting hardware features and system configuration.
- Kueue: A Kubernetes-native job queueing system. It manages workload admission, queuing, and preemption for batch and ML workloads across ClusterQueues and Cohorts.

## Guidelines

Behavioral guidelines to reduce common coding mistakes, derived from [Andrej Karpathy's observations](https://x.com/karpathy/status/2015883857489522876) on LLM coding pitfalls.

Tradeoff: These guidelines bias toward caution over speed. For trivial tasks, use judgment.

### 1. Think Before Coding

**Don't assume. Don't hide confusion. Surface tradeoffs.**

Before implementing:
- State your assumptions explicitly. If uncertain, ask.
- If multiple interpretations exist, present them - don't pick silently.
- If a simpler approach exists, say so. Push back when warranted.
- If something is unclear, stop. Name what's confusing. Ask.

### 2. Simplicity First

**Minimum code that solves the problem. Nothing speculative.**

- No features beyond what was asked.
- No abstractions for single-use code.
- No "flexibility" or "configurability" that wasn't requested.
- No error handling for impossible scenarios.
- If you write 200 lines and it could be 50, rewrite it.

Ask yourself: "Would a senior engineer say this is overcomplicated?" If yes, simplify.

### 3. Surgical Changes

**Touch only what you must. Clean up only your own mess.**

When editing existing code:
- Don't "improve" adjacent code, comments, or formatting.
- Don't refactor things that aren't broken.
- Match existing style, even if you'd do it differently.
- If you notice unrelated dead code, mention it - don't delete it.

When your changes create orphans:
- Remove imports/variables/functions that YOUR changes made unused.
- Don't remove pre-existing dead code unless asked.

The test: Every changed line should trace directly to the user's request.

### 4. Goal-Driven Execution

**Define success criteria. Loop until verified.**

Transform tasks into verifiable goals:
- "Add validation" → "Write tests for invalid inputs, then make them pass"
- "Fix the bug" → "Write a test that reproduces it, then make it pass"
- "Refactor X" → "Ensure tests pass before and after"

For multi-step tasks, state a brief plan:
```
1. [Step] → verify: [check]
2. [Step] → verify: [check]
3. [Step] → verify: [check]
```

Strong success criteria let you loop independently. Weak criteria ("make it work") require constant clarification.

## Skills

Operational runbook index for common GPUStack Operator debugging tasks are in [.claude/skills](.claude/skills/README.md). Consult these proactively when a user asks about GPU discovery, allocation, monitoring, or workload scheduling — even if the question is informal or incomplete.

When a skill is triggered, read the corresponding SKILL.md file directly and follow its steps. These are plain Markdown guides, not plugins in the Claude Code skill system.

@.claude/skills/README.md
