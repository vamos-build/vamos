# Design

## Context

`openWorkspace` in `internal/load/buildfile.go` finds the root by statting `WORKSPACE` upward from `-C` and never reads
it. Both `Load`/`LoadPublication` and `Pulls` (for `vamos update`) go through `open`, which calls `openWorkspace` and
then `ReadLock`. BUILD and `.star` files run through `starlark.ExecFileOptions` with `w.predeclared()`, and their
mistakes come out as `file:line:col` via `describeExecError`/`describeEvalError`.

The CLI builds `api.GraphRequest` in `runLoaded` (`internal/cli/cli.go`), and `PushRequest`, `ExportRequest` and
`PublishRequest` after the graph job, in `buildImage`, `deliver` and `publishFromExecutor`. `internal/remote` sends
`api.Request` as JSON, and decoding does not reject unknown fields, so an older coordinator ignores a field it doesn't
know. `Emulate` is the precedent for request metadata that is not part of any key. The proposal and the
`workspace-identity` spec cover the motivation and required behavior.

## Goals / Non-Goals

**Goals:**

- Evaluate `WORKSPACE` once, when the workspace opens, so every command that opens it, `vamos update` included, reports
  a mistake in it before any BUILD file loads.
- Get the name from load to every `api` request the CLI builds, without changing `graph.Graph` or any key.

**Non-Goals:**

- Using the name on the executor side. Coordinators and workers receive it and do nothing with it until
  `persistent-traces` and `telemetry-export`.
- Names in labels (`@name//pkg:target`), references between workspaces, or any other `WORKSPACE` builtin.
- An executor-side check that a name follows the rules. The client checks it, and the name is trusted as declared.
- A length limit. Bazel's module names have none, and a reverse-DNS name is short.

## Decisions

### Evaluate WORKSPACE in `openWorkspace`, with its own builtins

`openWorkspace` reads the root's `WORKSPACE`, if there is one, and runs it on a thread whose only predeclared names
are `workspace` plus the Starlark universe. The thread has no `Load` function, so `load()` fails, and the BUILD file's
builtins are simply undefined, which Starlark reports with their name and position. The thread honours the load's
context via `cancelWith`, as BUILD threads do. Errors go through `describeExecError`, so they read like BUILD mistakes.
The name is kept on `workspace.name`.

*Alternative:* add `workspace` to `predeclared()` and refuse it outside `WORKSPACE`. Rejected: `WORKSPACE` would then
see every BUILD builtin, and each one would need a check that it isn't being called from `WORKSPACE`.

*Alternative:* parse `workspace(name = "...")` with a regular expression. Rejected: the proposal makes `WORKSPACE`
Starlark so that it can grow, and a regular expression couldn't report positions the same way.

Only the root's `WORKSPACE` is read. Nested workspaces are already skipped by `packageDirs` and never become packages,
so they are never evaluated.

### `workspace()` is a builtin with one keyword-only `name`

The builtin unpacks `name` with `starlark.UnpackArgs` and accepts no positional argument. It records the call's
position, and a second call fails, citing the first name. The name is checked against
`^[a-z]([a-z0-9._-]*[a-z0-9])?$`, and a failure names the rule it breaks: empty, first character, a character not
allowed, or last character. The check is an unexported function, `checkWorkspaceName`, which `load`'s own tests call.
`api` can't call it: `run` imports `api` and must not import `load` or Starlark, so a check an executor ever needs
belongs in `api` or a package beneath it, with `load` calling that.

### The name leaves load through `Request.Opened`

`load.Request` gains `Opened func(name string)`, called once the workspace has opened and `WORKSPACE` has evaluated,
before any BUILD file loads. This follows the existing callbacks on `Request` (`Pin`, `Store`, `DestPlatform`). The
CLI's `inv.request` sets it to store the name on the invocation. Every place that builds an `api` request then reads
it from there.

*Alternative:* put the name on `graph.Graph`. Rejected: the graph is a stored blob that executors read, and a field
there would change its digest between differently named workspaces, which the spec forbids.

*Alternative:* change `Load` to return a result struct. Rejected: it would touch every caller, `runLoaded`'s
`loadGraph` function type included, for one string.

### `api` gets `Workspace string` on the four request types

`GraphRequest`, `PushRequest`, `PublishRequest` and `ExportRequest` each gain
`Workspace string json:"workspace,omitempty"`, documented as metadata like `Emulate`: no part of any key, and empty
for an unnamed workspace. The `Clone` methods copy it by value already, since it is a string. `internal/remote` needs no
code change beyond what JSON carries. `apitest.Contract` submits graph, push and publish jobs with a name and checks
that they behave exactly as they would without one. That includes the same action keys, so a named job after an
unnamed one is `cached`. That shows executors ignore the name, but not that it arrives, since an executor that never
received it would behave the same. The round-trips in `api_test.go` show that JSON carries it, and the features below
show that the CLI sends it.

The coordinator and `run.Local` accept the field, and nothing reads it yet. Where to keep it for later use is a
decision for `persistent-traces`.

### Features observe the name at the scenario's coordinator

The scenario's coordinator in `steps_remote_test.go` already wraps the handler to count jobs. It also decodes each
`POST /v1/jobs` body's `workspace` fields, so that a new step, `ThenTheCoordinatorWasToldTheWorkspaceIs(name)`, can
check them, with `""` meaning unnamed. Because executors ignore the name, these are the only checks that each of the
four requests carries it: build, push, publish and save on the coordinator, the last of which sends an export, cover
`runLoaded`, `buildImage`'s push, `publishFromExecutor` and `deliver`. The cache guarantee is a scenario with
`ThenTheTargetsWere(reused)`, and the step guarantee is a probe with `ThenTheStepsObserved`.

### A glob that selects `WORKSPACE` builds on it

vamos excludes no file from a glob by name, so a glob such as `**` in the root package selects `WORKSPACE`, and the
name with it. That stays so: the guarantee is that vamos never adds the name to a key, the graph or a step itself, not
that the name can't reach one through a file the workspace chose to build on. Excluding `WORKSPACE` from globs would
make one file special for a guarantee no one needs, and would change the keys of targets whose globs select it today.

## Risks / Trade-offs

- [Names collide between unrelated workspaces] → The spec accepts this. Documentation recommends reverse-DNS names.
- [A future executor wants to trust the name for access control] → Out of scope. The design states that the name is
  declared, not authenticated.
- [A `WORKSPACE` that isn't valid Starlark, accepted today since its contents are ignored, starts failing every
  command] → A Bazel `WORKSPACE`, full of `load()` and repository rules, is one, and vamos finds one in an ancestor
  directory too. Mitigated: every `WORKSPACE` that vamos reads today is one we control, and each is empty or a comment.

## Migration Plan

Nothing to migrate. Every existing `WORKSPACE` is ours and is empty or a comment, which evaluates to an unnamed
workspace, so existing workspaces stay unnamed. A new client talking to an older coordinator sends a field that
the coordinator ignores. An older client talking to a new coordinator sends none, which reads as unnamed. Rolling back
removes the field, and a `WORKSPACE` that calls `workspace()` is then ignored again by an older vamos.
