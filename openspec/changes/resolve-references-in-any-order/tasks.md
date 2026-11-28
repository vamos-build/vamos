# Tasks

## 1. Declaration records

- [x] 1.1 Make the declarer in `internal/load/buildfile.go` take the deferred path for every kind but `tool_cache`:
  unpack against the rule's parameters, take `params`, compute `hasOutput` and the listed steps from the raw
  arguments, `Freeze` every argument, store `fn`, `args`, `kwargs` and `make`, and stop calling `makeBase` and
  `resolveMounts`; remove `copyToolCaches`. Add the scenarios "A missing required argument of an unused target", "A
  duplicate name" and "An unused target's values are not read" to `internal/features/buildfile_test.go`, and the frozen
  argument scenarios "Appending to a list after passing it as steps", "Removing from a list after passing it as steps"
  and "A list shared by two targets" to the same file, the first two reproducing issue #25. Verify with
  `go test -short ./internal/load/... ./internal/features/ -run Buildfile`.
- [x] 1.2 Stop `newAdd`, `newDep` in `config.go`, `mount()` and the declarer's checks from resolving references as a
  file runs: parse the label and keep the `targetRef`; give `addStep` the position of its `add()` call, the innermost
  frame outside `<builtin>`. Remove `workspace.loading`, `buildFile.thread`, `buildFile.declaring`, `notDeclaredYet`,
  the "not an earlier" branches of `resolveAs` and `load`'s rollback of packages stored by a failed load, and update
  the doc comments of `load`, `lookup`, `resolve` and the declarer. Verify with `go build ./...` and `golangci-lint`.

## 2. Evaluation on configuration

- [x] 2.1 Make `workspace.template` evaluate every declaration: cache the template in `d.static` for a rule with no
  function of `ctx`, add "(configuring ...)" only for one with such a function, and report a failure to resolve an
  `add()` source as a `posError` at the `add()` call from `makeLayer`/`makeTask`. Rewrite
  `TestTargets_LocalDependenciesAreDeclaredFirst` in `internal/features/targets_test.go` as the rule "A reference names
  a target declared anywhere", with "A base declared below in the same file", "An add() source declared below in the
  same file", "A dep() with arguments of a target declared below", "An add() of a dep() passing on a parameter, to a
  target declared below" and "A reference of the wrong kind declared below", and the misspelling scenarios with their
  `has no target ...; did you mean ...?` messages; update the label scenarios in `workspaces_test.go` that expect
  "is not an earlier"; add "A mistake in a target used in two configurations" and
  "A reference made in a .star function". Verify with `go test -short ./internal/features/ -run 'Targets|Workspaces'`.
- [x] 2.2 Resolve `tool_caches` and check mount conflicts in `make`, with `tool_cache()` itself still checked at the
  call. Add "A tool cache declared below" to `internal/features/tool_caches_test.go`. Verify with
  `go test -short ./internal/load/... ./internal/features/ -run ToolCaches`, whose scenarios must pass unchanged.
- [x] 2.3 Make `Pulls` in `pulls.go` evaluate the pulls it pins, and `LoadPublication` evaluate the publication through
  `template`. Verify with `go test -short ./internal/load/... ./internal/features/ -run 'Update|Publication'`.

## 3. References across packages and cycles

- [x] 3.1 Rewrite `TestWorkspaces_PackagesMayNameEachOthersTargets` in `internal/features/workspaces_test.go` so its doc
  comment and scenarios specify that packages may name targets declared anywhere in each other:
  turn "A target declared below the point where its file loaded the other package fails"
  into "A package names a target that the package naming it declares below", which succeeds;
  add "A workspace whose packages name each other's later targets builds from every entry point",
  saving `//:app` to see the files its `add()` names and building `//lib:tools` and `//app:image`;
  and extend `TestWorkspaces_OnlyNeededTargetsLoad` with "A mistake in a target is reported when it is built".
  Verify with `go test ./internal/features/ -run Workspaces`, without `-short`, where buildah builds.
- [x] 3.2 Move the workspace cycle examples to "targets use each other in a cycle" at the closing reference, as
  "Targets of two packages that name each other in a cycle fail"
  and "Targets of three packages that use each other in a cycle fail",
  and add "A target that is its own base" to `TestTargets_CyclesAreErrors`,
  keeping its scenarios of cycles through functions of `ctx` unchanged.
  A cycle is reported at the declaration of the target whose reference closes it, naming the reference,
  such as `add: source "b"`, as those scenarios have it:
  say so in the spec's cycle requirement, which now says the reference's own position.
  Verify with `go test ./internal/features/ -run 'Workspaces|Targets'`, without `-short`, where buildah builds.

## 4. Documentation

- [x] 4.1 Rewrite "Cross-package dependencies" in `docs/user/reference/configuration.md`: a BUILD file declares its
  targets, each is evaluated when a command needs it, a reference may name any target declared anywhere, a mistake in
  a target is reported when it is built, and a rule's arguments, and `dep()`'s as it is called, are frozen; drop the
  advice to declare targets above, there and in `docs/user/reference/build/README.md`, and check `docs/user/` for
  other mentions of declaration order. Update
  `internal/load/doc.go`'s invariants and lifecycle: declaration and evaluation as separate stages, and no partially
  loaded package. Verify with `scripts/check`'s markdownlint and `go doc ./internal/load`.
- [x] 4.2 Update `docs/user/reference/cli/update.md` for how a full update finds pulls now: it checks the rules of the
  targets the walk finds and of what they use as given, failing on a mistake there or on targets that use each other so
  in a cycle; follows the labels given to `add()`
  and `dep()` as BUILD and `.star` files run; and keeps the pins no pull found uses, with a "kept ..., since ..." line,
  when a target can't be configured, a target beyond those has a mistake, or a package can't be loaded.

## 5. Integration

- [ ] 5.1 Run `scripts/check`, then `scripts/check full`, and fix what they report. Run the root `BUILD`'s checks as
  `.agents/kb/self-build.md` describes, and fix any BUILD file in the repository or its examples that changes a list
  after passing it to a rule.
