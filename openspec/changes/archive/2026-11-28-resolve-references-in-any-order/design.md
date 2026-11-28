# Design

## Context

`internal/load` has two ways to make a target. A rule call whose arguments hold no function of `ctx` is evaluated as
the BUILD file runs: `makeTemplate` reads its values, and `resolveAs` resolves each reference on the spot, running
another package's BUILD file through `workspace.load` while the first is stopped part way (`workspace.loading`,
`buildFile.thread`, `notDeclaredYet`). A call with such a function keeps `fn`, `args` and `kwargs` on its declaration
(`internal/load/buildfile.go`, the declarer) and is made in each configuration by `workspace.template`
(`internal/load/config.go`), after checking only what can be checked at the call: the arguments' shape, the name,
`hasOutput`, a static `base` and `tool_caches`, and which steps are listed. Configuration finds cycles through
`configurer.enter` and `cycleError`. `copyToolCaches` copies a deferred target's `tool_caches` so that the list can't
change after its check (#24); issue #25 is the same problem for `steps`.

## Goals / Non-Goals

**Goals:**

- One path: every target's rule is evaluated by `workspace.template` as it is configured.
- No BUILD file ever runs while another is stopped part way.
- Keep the text and position of every error that survives.

**Non-Goals:**

- A pass that evaluates every target, or checks unused ones.
- Changing the graph, keys, executors, or how functions of `ctx` behave.
- Deferring `tool_cache()`, which declares no target and names none.

## Decisions

### Declaration records, configuration evaluates

The declarer takes the deferred path for every kind but `tool_cache`: it unpacks the arguments against the rule's
parameters, which checks keywords, required arguments and the name, takes `params`, freezes the arguments and stores
`fn`, `args`, `kwargs` and `make`. It no longer resolves a static `base` or `tool_caches`. `hasOutput` keeps coming
from the raw arguments, and `kind` from the rule, so `imageTarget`, `filesTarget` and `resolveAs`'s kind checks still
know them without evaluating. `d.static` becomes a cache, filled by the first evaluation of a rule whose arguments hold
no function of `ctx`, since nothing else in a template depends on the configuration: `arg()`s are applied in
`workspace.edge`.

`newAdd`, `newDep`, `mount()` and the index and publication rules stop resolving: `add()` and `dep()` record their
`targetRef` with the label parsed, and `resolveAs` runs only within `make`, in configuring mode, where every file it
names is complete or is loaded completely by `lookup`. With nothing resolved as a file runs, `workspace.loading`,
`buildFile.thread`, `buildFile.declaring`, `notDeclaredYet` and the "not an earlier" branches of `resolveAs` go, and
`load` no longer rolls back the packages stored by a failed one.

*Alternative*: keep evaluating rules as files run and defer only the reference checks to the end of each file.
Rejected: it needs its own pending-reference list, pointers back into templates and a separate cycle search, and still
evaluates and reports targets that no command uses.

### Errors keep their positions

`template` reports a mistake at the declaring call (`d.pos`), at an `EvalError`'s innermost frame, or at a `posError`'s
position. `addStep` gains the position of its `add()` call, the innermost frame outside `<builtin>` as
`describeEvalError` chooses, and `makeLayer`/`makeTask` wrap a failure to resolve its source in a `posError` there, so
it is reported where it is today. `template` adds "(configuring ...)" only for a rule with a function of `ctx`, whose
template does depend on the configuration; the others read as they do today.

### Freeze the arguments

The declarer calls `Freeze` on every argument it keeps. Starlark freezes transitively, so a list or dict passed to a
rule, and every one it holds, is fixed; a later `append`, `pop` or item assignment fails at that line with Starlark's
own "frozen" error, which `describeEvalError` reports at the BUILD file's position. The unused-`run()` check reads the
frozen `steps`, which are what the target is evaluated from, so the two agree (#25). `copyToolCaches` goes.

*Alternative*: copy the arguments at the call. Rejected: a change after the call would be silently ignored by the
target, where freezing reports it at the line that makes it. Existing BUILD files that change a passed list need a
small edit; they were already wrong for deferred targets.

### Cycles are configuration's

`configurer.enter` already reports a cycle as targets are configured, in any configuration, with `placeCycle` naming
the reference that closes it. With every reference resolved there, it reports static cycles too, including a target
that names itself.

`vamos update` is the exception: it configures every target, and leaves one it can't configure unsure, keeping the
pins, so a cycle there would pass. It also looks for cycles among the references that the targets it checks give as
they are, which every configuration has, and fails on one before configuring anything, naming the closing reference as
`placeCycle` does; a cycle that a function of `ctx` makes in some configurations stays unsure.

### Pulls and publications evaluate what they use

`everyPull` already configures every target; the pulls it collects by `kind` are then evaluated for their images by
`Pulls`, which also evaluates the pulls named to it. `LoadPublication` evaluates the publication through `template`, as
any other rule.

## Risks / Trade-offs

- [A broken target that no command builds goes unnoticed] → It fails the first command that builds it, at its
  position; CI that builds or checks the workspace's targets finds it. This is already how packages behave.
- [Error order changes: a call-shape error below a value error is now reported first] → Each error keeps its position.
- [Evaluating per configuration costs more] → Only rules with a function of `ctx` are evaluated more than once, as now.
- [Freezing breaks a BUILD file that changes a list after passing it] → The error names the line; accepted as the
  issue's intended behavior.
- [A label repeated through a target configured already is missed, as before this change] → `configurer.image`
  returns a target configured already, such as `loop`, without walking it, so `enter` doesn't see that it reaches
  another configuration of a label being configured, such as `app` in debug mode below `app` in release mode: `seed`
  then `app` builds where `app` then `seed` reports `app -> loop -> app`. The configured graph has no cycle either way,
  so what builds is sound; only the label rule is applied unevenly. `vamos update` likewise keeps the pins no pull uses
  when the target that would close the cycle is declared first, and may drop one when another is. Checking every reuse
  was tried and dropped: summaries of what each target reaches cost quadratic memory on shallow shared graphs, and
  walking again costs quadratic time once a label has two configurations. A follow-up can bound that work.

## Migration Plan

No data or flag changes. Users may drop `lambda ctx:` wrappers that only delayed a reference, and must stop changing a
list or dict after passing it to a rule.
