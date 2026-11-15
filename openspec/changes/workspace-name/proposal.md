# Proposal

## Why

Nothing a coordinator receives says which workspace a build came from: labels such as `//:app` are relative to
their workspace, so two workspaces' targets are indistinguishable on a shared coordinator.
Telemetry that follows a target across builds, and any future reference from one workspace to another,
needs a stable, declared identity for the workspace; this change adds one, and nothing yet depends on it.

## What Changes

- The `WORKSPACE` file becomes a Starlark file that vamos evaluates, rather than a marker whose contents are ignored.
  An empty `WORKSPACE`, and a workspace with none, remain valid.
- `WORKSPACE` may call `workspace(name = "...")` once, giving the workspace a symbolic name.
  The name is optional, and follows Bazel's rules for module names: lowercase letters, digits, `.`, `-` and `_`,
  starting with a letter and ending with a letter or digit, so that a reverse-DNS name such as
  `com.acme.storefront` is valid and the name can later appear in labels.
  Mistakes are reported as the file loads, naming the file, line and column, as they are for BUILD files.
- The name travels with every request a client makes to an executor (graph, push, publish and export) as metadata,
  as `Emulate` and provenance do. It is never part of an action's key, and vamos never puts it into a step,
  so that identical work in differently named workspaces still shares cached results.
- vamos derives no name from anything else: not the repository, its remote, nor the directory.
  A workspace without a name is simply unnamed.
- A name is trusted as declared. Every clone of a workspace, on every developer's machine and in every pipeline,
  sends the same name to a coordinator, so a coordinator can't tell two workspaces that chose one name from one
  workspace built in many places, and doesn't try. Choosing a name no other workspace uses is the workspace's job,
  which a reverse-DNS name does.

## Capabilities

### New Capabilities

- `workspace-identity`: how a workspace declares its name in `WORKSPACE`, the rules the name follows,
  how it reaches the executor, and the guarantee that it never affects what is built or cached.

### Modified Capabilities

None.

## Impact

- `internal/load`: evaluate `WORKSPACE` as Starlark where today it is only found (`buildfile.go`), and expose the
  name; nested workspaces each evaluate their own.
- `internal/api`: a workspace name field on `GraphRequest`, `PushRequest`, `PublishRequest` and `ExportRequest`;
  `internal/remote` carries it; `apitest.Contract` covers it.
- `internal/cli`: passes the loaded name on every request.
- `internal/sched`: no change to `Action` or keys; a test should hold that the name never reaches a key.
- Documentation: the workspace section of `docs/user/reference/configuration.md`; `internal/load/doc.go`.
- Later changes depend on this one: `persistent-traces` keys target history by workspace name, and
  `telemetry-export` labels spans with it.
