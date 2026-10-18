---
name: code-review
description: Review the most recent commit using independent reviewers for correctness, architecture, simplicity, adversarial cases, and testing.
---
Perform a comprehensive engineering review of the most recent commit (HEAD) using parallel subagents.

Spawn **five independent review agents**, each with a distinct focus:

1. **Correctness reviewer:** Find logic errors, behavioral regressions, edge cases, incorrect assumptions, and error-handling defects. Trace relevant execution paths.

2. **Architecture reviewer:** Critique design decisions, abstraction boundaries, coupling, API design, and consistency with the surrounding codebase. Consider better alternative designs.

3. **Simplicity reviewer:** Find unnecessary complexity, redundant code, avoidable abstractions, confusing control flow, and opportunities to simplify the implementation.

4. **Adversarial reviewer:** Try to break the implementation. Investigate unusual inputs, failure scenarios, state transitions, concurrency hazards, and resource management problems. Construct concrete counterexamples where possible.

5. **Testing reviewer:** Evaluate existing tests, identify missing behavioral coverage, weak assertions, untested failure paths, and specific tests likely to uncover defects.

### Instructions for all agents
- Independently examine the complete diff and relevant surrounding code.
- Inspect callers, callees, and existing tests as necessary.
- Do not limit the review to changed lines.
- Be skeptical, thorough, and specific.
- Identify worthwhile improvements, not just bugs.
- Do not modify production code.
- Report findings with file/line references, severity, reasoning, and concrete recommendations.
- Do not manufacture findings to satisfy a quota.

### Orchestration
Run all five agents in parallel, up to the number of subagents this environment allows at once. If fewer than five slots are free, run the rest in a later wave as slots free up. Keep all five perspectives; do not merge or drop one to fit.

Wait for every agent to finish before synthesizing results.

As the coordinating agent:
1. Collect all findings.
2. Deduplicate overlapping findings without losing important details.
3. Independently verify the most significant findings against the code.
4. Distinguish confirmed defects from design suggestions and speculative concerns.
5. Rank findings by impact and confidence.
6. Identify disagreements between reviewers.
7. Report areas where review coverage was incomplete.

Produce one consolidated review with:
- Critical and high-priority defects
- Other correctness concerns
- Design and architectural improvements
- Simplification opportunities
- Testing gaps
- Up to five highest-value recommended changes (fewer, or none, if the review found fewer worth making)

Include a brief summary of what each subagent investigated and found.

Do not implement fixes.
