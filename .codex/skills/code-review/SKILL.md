---
name: code-review
description: Review a commit range, a single commit, or a described set of commits (HEAD when none is given) using independent reviewers for correctness, architecture, simplicity, adversarial cases, and testing.
---
Perform a comprehensive engineering review of the change named in the request, using parallel subagents.

### The change under review
The request names the change as a commit range such as `abc123..def456` (the commits after `abc123`, up to and including `def456`), a single commit, or a description such as "the last three commits" or "this branch since main". If it names nothing, review the most recent commit, `HEAD^..HEAD`.

Resolve the request to a concrete range of full SHAs with git before spawning anyone. `<from>` must be an ancestor of `<to>`: a description relative to another branch, such as "this branch since main", starts at `git merge-base <branch> <to>`, so commits that branch gained since the fork are not mistaken for reversions. If the description is ambiguous, if a given `<from>` is not an ancestor of `<to>`, or if the range holds no commits, say so and ask rather than guessing.

Review the combined diff of the range (`git diff <from> <to>`) as one change. The commit messages in it (`git log <from>..<to>`) tell you the intent, but they are not evidence that the code is correct. Only committed code is under review, as it stood at `<to>`. Read surrounding code at `<to>` too: if `<to>` is not `HEAD`, or the working tree has uncommitted changes, use `git show <to>:<path>` or a separate checkout of `<to>` rather than the working tree, because later commits can add or fix code the range relies on. If the working tree has uncommitted changes, ignore them, but mention them in the report.

The request may add context, for example that the range fixes findings from an earlier review. Use that context to direct attention, and still judge the code on its own merits.

Spawn **five independent review agents**, each with a distinct focus:

1. **Correctness reviewer:** Find logic errors, behavioral regressions, edge cases, incorrect assumptions, and error-handling defects. Trace relevant execution paths.

2. **Architecture reviewer:** Critique design decisions, abstraction boundaries, coupling, API design, and consistency with the surrounding codebase. Consider better alternative designs.

3. **Simplicity reviewer:** Find unnecessary complexity, redundant code, avoidable abstractions, confusing control flow, and opportunities to simplify the implementation.

4. **Adversarial reviewer:** Try to break the implementation. Investigate unusual inputs, failure scenarios, state transitions, concurrency hazards, and resource management problems. Construct concrete counterexamples where possible.

5. **Testing reviewer:** Evaluate existing tests, identify missing behavioral coverage, weak assertions, untested failure paths, and specific tests likely to uncover defects.

### Instructions for all agents
- Independently examine the complete diff of the resolved range and relevant surrounding code as of `<to>`. Give each agent the range's SHAs and any context from the request.
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

Produce one consolidated review headed `Reviewed <from>..<to>` with the resolved SHAs, followed by:
- Critical and high-priority defects
- Other correctness concerns
- Design and architectural improvements
- Simplification opportunities
- Testing gaps
- Up to five highest-value recommended changes (fewer, or none, if the review found fewer worth making)

Include a brief summary of what each subagent investigated and found.

Do not implement fixes.
