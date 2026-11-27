# Tasks

## 1. Declaration records

- [ ] 1.1 Make the declarer in `internal/load/buildfile.go` take the deferred path for every kind but `tool_cache`:
  unpack against the rule's parameters, take `params`, compute `hasOutput` and the listed steps from the raw
  arguments, `Freeze` every argument, store `fn`, `args`, `kwargs` and `make`, and stop calling `makeBase` and
  `resolveMounts`; remove `copyToolCaches`. Add the scenarios "A missing required argument of an unused target", "A
  duplicate name" and "An unused target's values are not read" to `internal/features/buildfile_test.go`, and the frozen
  argument scenarios "Appending to a list after passing it as steps", "Removing from a list after passing it as steps"
  and "A list shared by two targets" to the same file, the first two reproducing issue #25. Verify with
  `go test -short ./internal/load/... ./internal/features/ -run Buildfile`.
- [ ] 1.2 Stop `newAdd`, `newDep` in `config.go`, `mount()` and the declarer's checks from resolving references as a
  file runs: parse the label and keep the `targetRef`; give `addStep` the position of its `add()` call, the innermost
  frame outside `<builtin>`. Remove `workspace.loading`, `buildFile.thread`, `buildFile.declaring`, `notDeclaredYet`,
  the "not an earlier" branches of `resolveAs` and `load`'s rollback of packages stored by a failed load, and update
  the doc comments of `load`, `lookup`, `resolve` and the declarer. Verify with `go build ./...` and `golangci-lint`.

## 2. Evaluation on configuration

- [ ] 2.1 Make `workspace.template` evaluate every declaration: cache the template in `d.static` for a rule with no
  function of `ctx`, add "(configuring ...)" only for one with such a function, and report a failure to resolve an
  `add()` source as a `posError` at the `add()` call from `makeLayer`/`makeTask`. Rewrite
  `TestTargets_LocalDependenciesAreDeclaredFirst` in `internal/features/targets_test.go` as the rule "A reference names
  a target declared anywhere", with "A base declared below in the same file", "An add() source declared below in the
  same file", "A dep() with arguments of a target declared below", "An add() of a dep() passing on a parameter, to a
  target declared below" and "A reference of the wrong kind declared below", and the misspelling scenarios with their
  `has no target ...; did you mean ...?` messages; update the label scenarios in `workspaces_test.go` that expect
  "is not an earlier"; add "A mistake in a target used in two configurations" and
  "A reference made in a .star function". Verify with `go test -short ./internal/features/ -run 'Targets|Workspaces'`.
- [ ] 2.2 Resolve `tool_caches` and check mount conflicts in `make`, with `tool_cache()` itself still checked at the
  call. Add "A tool cache declared below" to `internal/features/tool_caches_test.go`. Verify with
  `go test -short ./internal/load/... ./internal/features/ -run ToolCaches`, whose scenarios must pass unchanged.
- [ ] 2.3 Make `Pulls` in `pulls.go` evaluate the pulls it pins, and `LoadPublication` evaluate the publication through
  `template`. Verify with `go test -short ./internal/load/... ./internal/features/ -run 'Update|Publication'`.

## 3. References across packages and cycles

- [ ] 3.1 Rewrite `TestWorkspaces_PackagesMayNameEachOthersTargets` in `internal/features/workspaces_test.go` so its doc
  comment and scenarios specify that packages may name targets declared anywhere in each other: turn "A target declared
  below the point where its file loaded the other package fails" into "A package names a target that the package
  naming it declares below", which succeeds; add "The issue's workspace builds from every entry point", building
  `//:app` and `//lib:tools`; and extend `TestWorkspaces_OnlyNeededTargetsLoad` with "A mistake in a target is reported
  when it is built". Verify with `go test -short ./internal/features/ -run Workspaces`.
- [ ] 3.2 Move the workspace cycle examples to "targets use each other in a cycle" at the closing reference, as
  "Two packages whose targets name each other" and "Three packages in a cycle", and add "A target that is its own
  base" to `TestTargets_CyclesAreErrors`, keeping its scenarios of cycles through functions of `ctx` unchanged. Verify
  with `go test -short ./internal/features/`.

## 4. Documentation

- [ ] 4.1 Rewrite "Cross-package dependencies" in `docs/user/reference/configuration.md`: a BUILD file declares its
  targets, each is evaluated when a command needs it, a reference may name any target declared anywhere, a mistake in
  a target is reported when it is built, and a rule's arguments are frozen; drop the advice to declare targets above,
  and check `docs/user/` for other mentions of declaration order. Update `internal/load/doc.go`'s invariants and
  lifecycle: declaration and evaluation as separate stages, and no partially loaded package. Verify with
  `scripts/check`'s markdownlint and `go doc ./internal/load`.

## 5. Integration

- [ ] 5.1 Run `scripts/check`, then `scripts/check full`, and fix what they report. Run the root `BUILD`'s checks as
  `.agents/kb/self-build.md` describes, and fix any BUILD file in the repository or its examples that changes a list
  after passing it to a rule.
