# Tasks

## 1. Evaluate WORKSPACE and declare the name (`internal/load`)

- [x] 1.1 Add the name rule as the unexported `checkWorkspaceName`, which names the rule a name breaks, with a table
  test of the spec's valid and invalid names; verify with `go test ./internal/load -run CheckWorkspaceName`.
- [x] 1.2 Evaluate the root's `WORKSPACE` in `openWorkspace` on its own thread. Predeclare only `workspace`, set no
  `Load`, use `cancelWith`, and report errors through `describeExecError`. Store the name on `workspace.name`. Verify
  with `go test ./internal/load`.
- [x] 1.3 Implement `workspace(name=...)`: keyword only, a string, at most once (a second call cites the first name),
  checked by `checkWorkspaceName`. Verify through 1.5's scenarios.
- [x] 1.4 Add `Request.Opened func(name string)`, called after `WORKSPACE` evaluates and before any BUILD file loads,
  and document `WORKSPACE` evaluation and `Opened` in `internal/load/doc.go`. Verify with `go test ./internal/load`
  and `go doc ./internal/load`.
- [x] 1.5 Add `workspaces_test.go` rules for "WORKSPACE is a Starlark file that vamos evaluates", "WORKSPACE declares
  the name with workspace()" and "A workspace name follows the rules of a module name". Cover the mistakes with
  `ThenItFailsWith` (file, line and column), `ThenNoTargetRan` and `ThenNothingIsSaved`, starting from a background that
  needs no buildah. Add the `vamos update` scenario to `update_test.go`. Verify with
  `go test -short ./internal/features -run 'TestWorkspaces_|TestUpdate_'`.
- [x] 1.6 Update the Workspaces section of `docs/user/reference/configuration.md`: `WORKSPACE` is Starlark,
  `workspace(name=...)`, the name rules, the reverse-DNS recommendation, that vamos never adds the name to a key or a
  step, and that a glob selecting `WORKSPACE` builds on its contents as on any file's. Make
  the feature rules' doc comments quote it. Verify with the documentation link and format checks in
  `.agents/kb/documentation.md`.

## 2. Carry the name in executor requests (`internal/api`, `internal/remote`, `internal/cli`)

- [x] 2.1 Add `Workspace string json:"workspace,omitempty"` to `GraphRequest`, `PushRequest`, `PublishRequest` and
  `ExportRequest`, documented as metadata like `Emulate` that is no part of any key. Confirm the `Clone` methods and
  `api_test.go` round-trips keep it. Verify with `go test ./internal/api`.
- [x] 2.2 Extend `apitest.Contract`: graph, push and publish jobs given a name behave as without one, and a named graph
  job after an unnamed one of the same graph reports `cached`. This shows executors ignore the name, not that it
  arrives: 2.4 checks that. Verify with `go test ./internal/run ./internal/remote ./internal/coord`.
- [x] 2.3 In `internal/cli`, set `Opened` in `inv.request` to record the name on the invocation, and set `Workspace`
  in `runLoaded`, `deliver`, `buildImage`'s push and `publishFromExecutor`. Verify with `go test ./internal/cli`.
- [x] 2.4 In `steps_remote_test.go`, record the `workspace` field of each `POST /v1/jobs` body that the scenario's
  coordinator receives, and add `ThenTheCoordinatorWasToldTheWorkspaceIs`. Add scenarios for "vamos derives no
  workspace name" and "The name travels with every request to an executor": build, push, publish, save (which sends
  the export) and two clones, so that each of 2.3's four requests is checked as received. Verify with
  `go test ./internal/features -run 'TestWorkspaces_|TestCoordinator_'`.
- [x] 2.5 Add scenarios for "The name never affects what is built or cached": a renamed workspace's `app` is
  `reused`, a probe step observes no variable holding the name, and a renamed workspace's `app` whose glob selects
  `WORKSPACE` `ran`. Verify with
  `go test ./internal/features -run TestWorkspaces_`.

## 3. Integration

- [x] 3.1 Run `scripts/check` and then `scripts/check full`, and confirm both pass.
- [x] 3.2 Run the build smoke checks in `.agents/kb/smoke-checks.md` against a workspace whose `WORKSPACE` names it,
  and against the example projects, whose empty or missing `WORKSPACE` files must still build.
- [x] 3.3 Run `openspec validate workspace-name --strict` and confirm it passes.

## Workflow follow-up

- Archive the change with `/opsx:archive` once it is reviewed and merged, creating `openspec/specs/workspace-identity`.
