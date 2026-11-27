# Proposal

## Why

A reference to a target must come after that target's declaration, within a BUILD file and across packages, and a
cross-package reference works or fails depending on which file started loading first, which depends on the target the
command names ([issue #1](https://github.com/vamos-build/vamos/issues/1)).
So an error can appear in a file that is not wrong on its own, a graph that builds from one target fails from another,
and users wrap references in `lambda ctx:` only to make them resolve late.

That workaround has a flaw of its own: a target given by a function of `ctx` reads its argument lists after the file
has run, while the check that every `run()` step is in some target's `steps` reads them as the call made them, so the
two disagree when a list changes after the call
([issue #25](https://github.com/vamos-build/vamos/issues/25)).

## What Changes

- **Running a BUILD file declares its targets and evaluates none of them.** Each rule call records its target's name,
  kind, position and `params`, and checks only the call's shape: its keywords, required arguments, a unique string
  name and `params`. Its arguments are kept as given, frozen.
- **A rule is evaluated when its target is configured**: when the command names it, or a target being configured
  references it, in the configuration that reference gives through `dep()`, a platform or `arg()`. Evaluation reads
  the values, resolves the references and loads the packages they name. Today's path for functions of `ctx` becomes
  the only path; a rule without one is evaluated once, whatever the configuration. `tool_cache()` declares no target
  and names none, so it is still evaluated as it is called.
- **A reference can name any target of the workspace**, declared above or below it, in its own file or another
  package's: a `base`, an `add()` source, a `dep()`, an `index()` manifest, a publication's checks and images, and a
  `mount()` in `tool_caches`.
- **A rule's arguments are frozen at the call.** A BUILD file that changes a list or dict after passing it to a rule
  fails at the change, with Starlark's error for a frozen value. The unused-`run()` check and the target then read the
  same lists, which fixes issue #25.
- Every reference error still names the file, line and column of the call making the reference: the rule's call for a
  `base`, `dep()`, manifest, check or mount, and the `add()` call for its source, at the innermost BUILD or `.star`
  frame. Cycles are reported by configuration, as cycles through functions of `ctx` are now.
- **BREAKING**:
  - Mistakes in a rule's values or references, such as a misspelled base, a bad image or a cycle, are reported only
    when a command configures the target, as packages already are only when something needs them.
  - The errors "is not an earlier pull() or layer()", "is not an earlier glob() or task()" and "isn't declared yet:
    its BUILD file is stopped at ..." go; a missing target in the file itself reads as one in another package does.
    Errors can appear in a different order.
  - A BUILD file that changes an argument after the call fails.
- The user reference's "Cross-package dependencies" section and `internal/load`'s package doc describe the new rules.

Out of scope: a pass that evaluates every target of the workspace. `vamos update` configures every target as it does
now, which reports nothing new.

## Capabilities

### New Capabilities

- `target-references`: how a BUILD file declares targets and when their rules are evaluated, what a reference may name
  regardless of declaration order and package, where its mistakes are reported, and how a rule's arguments are fixed.

### Modified Capabilities

None. `tool-caches` refuses a bad cache path and mount conflicts "when loading": a `tool_cache()` call is still
checked as the BUILD file runs, and a target's mounts as it is evaluated, which every command building that target
does before anything runs, so its requirements and scenarios hold unchanged.

## Impact

- `internal/load`:
  - `buildfile.go`: the declarer, `newAdd`, `makeBase`, `resolve`/`resolveAs`, `load`, `lookup`; `notDeclaredYet`,
    `workspace.loading`, `buildFile.thread` and `buildFile.declaring` go.
  - `config.go`: `newDep` and `workspace.template`.
  - `toolcache.go`: `resolveMounts` and `copyToolCaches`, which freezing replaces.
  - `pulls.go`, for the pulls `vamos update` pins.
  - `doc.go`'s invariants.
- `internal/features`: the declaration-order, workspace, cycle and only-needed-targets scenarios change; new scenarios
  for both issues.
- `docs/user/reference/configuration.md`, "Cross-package dependencies".
- No change to the graph, executors, cache keys or the CLI's flags.
