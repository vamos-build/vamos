# Tasks

## 1. BUILD language and graph

- [x] 1.1 Add the `tool_cache` builtin to `load` with a required `path` and optional `owner` and `group`; refuse any
  other argument, a missing `path`, a path that is not absolute or not as `path.Clean` leaves it, is `/`, contains `$`,
  is at or beneath `/dev`, `/proc` or `/sys`, or equals, lies beneath or contains `/etc/hosts`, `/etc/hostname`,
  `/etc/resolv.conf`, `/run/.containerenv`, `/run/secrets` or `/etc/crypto-policies`, a non-numeric `owner` or `group`,
  and one of `owner` and `group` without the other, naming the call; parse `owner` and `group` as decimal digits only,
  from 0 to 4294967294, to integers, with the scenario "An owner out of range", with `load` unit tests refusing
  `"4294967295"`, `"-1"`, `"+1000"`, `""` and a 30-digit string, and accepting `"0"`, `"4294967294"` and `"01000"` as
  1000. Verify with the scenarios "A tool_cache rule needs a path", "A tool_cache rule names no variable", "An owner
  given by name", "An owner without a group" and "A cache path not in clean form" in a new
  `internal/features/tool_caches_test.go`, from a background that needs no buildah, whose rules call `parallelRule(t)`
  so that each scenario's caches are in a cache directory of its own and scenarios that reuse a workspace name and label
  never see each other's, and with `load` unit tests refusing a rule's path at or beneath `/dev`, `/proc` or `/sys`, and
  `/etc`, `/run`, `/etc/hosts` and `/run/.containerenv/cache`, and accepting `/device/cache` and `/run/cache`, with the
  scenario "A cache path that holds a file every step is given".
- [x] 1.2 Add the `mount(label, path = ...)` builtin, and `tool_caches`, a list of `mount()` values, to `task` and
  `layer`: resolve each label to a `tool_cache` rule and each mount to its effective path, store the mounts in
  `graph.Layer` and `graph.Task` sorted by path with canonical labels, and refuse a label that names any other kind of
  target and a value in `tool_caches` that is not a `mount()`; refuse a `tool_cache` label given to any command that
  takes a target (`build`, which builds a publication's images instead of refusing it, needs its own check, as for a
  glob), or named by a `base`, `dep()`, `add()` or publication check. Verify with "A label that is no cache", "Building
  a tool cache", "Depending on a tool cache", a `cli` test that every command that takes a target, `build`, `save` and
  its `write` alias, `export`, `push`, `load`, `graph` and `publish`, which takes a publication, refuses one with that
  message, and `load` unit tests of label canonicalization across packages and of effective paths with and without an
  override.
- [x] 1.2b Make `vamos update` skip `tool_cache` declarations when it scans a workspace for pulls, and refuse a
  `tool_cache` label named to it saying that a tool cache is mounted with `mount()`. Verify with "Updating a tool cache"
  and "A workspace-wide update passes over tool caches", which keeps a pin in use and removes one no longer used, so
  that the uncertain-discovery path, which keeps every pin, can't pass it.
- [x] 1.3 Refuse conflicting mounts when loading: a mount path that is not absolute or not as `path.Clean` leaves it, is
  `/`, contains `$`, is at or beneath `/dev`, `/proc` or `/sys`, or equals, lies beneath or contains `/etc/hosts`,
  `/etc/hostname`, `/etc/resolv.conf`, `/run/.containerenv`, `/run/secrets` or `/etc/crypto-policies`; two mounts at one
  effective path; one cache mounted twice; a mounted path nested in another, or equal to, containing or beneath a
  secret's file; an `add()` destination or a `workdir()` with no variable equal to or beneath a mounted path once
  cleaned, or a task's `source` with no `..` component there once cleaned; and a target with caches and no `run()` step.
  Verify with "A cache path beneath /dev", "Two mounts at one path", "One cache mounted twice", "A task's output inside
  a cache", "Nested caches", "A workdir() in a cache" and "A layer with caches and no run() step", plus a Go table test
  in `load` covering mounts in both directions, a mount path equal to, containing or beneath a secret's file, and
  `source`, `add()` and `workdir()` refused beneath a mounted path, including when spelled other than `path.Clean`
  leaves it, accepted above one, `add()` and `workdir()` refused when a `..` component puts them beneath once cleaned
  and left to the runner when they hold a variable, and a `source` with a `..` component left to the runner, so that
  `/work/../build` with a cache at `/build` loads.
- [x] 1.4 Refuse a target with `tool_caches` in an unnamed workspace as it is configured, naming its declaration.
  Verify with "Mounting a cache in an unnamed workspace", "An unmounted cache in an unnamed workspace" and `workspace-identity`'s
  "Only a named workspace builds a target that mounts a cache".
- [x] 1.5 Document the `tool_cache` rule in a new `docs/user/reference/build/tool-cache.md` (including the user's
  assertion, "may be given empty", the unnamed-workspace refusal, the path rules and that a target with caches needs a
  `run()` step; that a cache's parents exist from the first `run()` step and that a tool that deletes and remakes a
  directory holding a cache, as `npm ci` does `node_modules`, fails; that a tool whose cache misses what it doesn't
  track, as Go's misses the C libraries cgo uses, or that trusts files' modification times, as `go test`'s results do,
  can break the assertion; that a step that changes a cache's owner or mode must leave it usable by the rule's owner,
  with `vamos prune` to repair it, `--no-tool-caches` to get the attempts a build starts past it, and
  `--no-tool-caches --rebuild` to rerun a named target cold; that a target can't mount a cache above a path its
  base mounted one at; that caches need chroot isolation; and that on a FIPS host, an image whose
  `/usr/share/crypto-policies/back-ends/FIPS` links to a cache path's location makes buildah add its FIPS binds once the
  mount point exists), add `tool_caches` to `task.md` and `layer.md`, link it from the BUILD reference's index, and
  verify each example in it loads with `vamos build` in a scratch workspace.

## 2. Keys and requests

- [x] 2.1 Add `ToolCaches` (path, label, owner and group) to `sched.Action` for layers and tasks, filled from the graph,
  advance `ActionVersion` to 12 and `graph.Version` to 5 with their comments. Verify with `sched` action tests that an
  action without caches encodes as before apart from the version, that changing a mount's effective path, label, owner
  or group changes the key, a `cli` test that a client refuses an executor offering only graph version 4, a test that a
  graph's mounts survive its JSON round trip, `graph.Validate` tests refusing malformed mounts (a relative or `/` path,
  a duplicate path, one label at two paths, nesting, an empty label, an owner out of range, a path that contains
  `/etc/hosts`, caches on a node with no `run()` step), with the literal path checks, which call `graph.CheckPath`
  and so refuse a NUL byte, in one function of `graph` that
  `load` calls too, and the scenarios
  "Overriding a cache's path with its own path keeps the hit" and "Owners written differently are one owner".
- [x] 2.2 Add `NoToolCaches` to `api.GraphRequest`, and `Workspace` and `NoToolCaches` to `sched.Request` as metadata
  outside the key, filled by the scheduler from `sched.Options.Workspace`, which already keys target history, and a new
  `Options.NoToolCaches`; have the local executor, where it sets `Workspace` in the scheduler's options, fail a job,
  when it reads the graph and before anything runs, whose graph mounts caches with a `Workspace` that breaks the
  workspace name rules, or is empty without `NoToolCaches`; add a coordinator-mode option to `run`'s local executor,
  which `vamos coordinator --local` sets, under which it skips that check and gives every cache empty; and move the name
  rules from `load` to `graph` so that `load`, `sched` and `run` share them. Verify with a `sched` test that two jobs
  from differently named workspaces get one key for the same target, with the `apitest` JSON contract round-tripping
  both fields and the worker's and subprocess's request payloads carrying them, with `run` tests of an invalid name, or
  an empty one without `NoToolCaches`, failing the job before anything runs, of an empty name with `NoToolCaches`
  succeeding, and of the coordinator-mode option giving every cache empty, for two attempts of different keys under one
  name, the second seeing nothing the first wrote, and for an omitted or malformed name; with a `cli` test that `vamos
  coordinator --local` sets it, and with the scenarios "A local coordinator keeps no cache between builds" and "An API
  request without a workspace name"; with `run` tests of the runner refusing an empty or malformed name when it works
  out a persistent cache's directory, while a worker's or a `--no-tool-caches` attempt with no name succeeds, and with a
  `run()` step that reads `/proc/self/mountinfo` and finds no workspace name in it. Update the configuration reference's
  account of the workspace name to say that it chooses a workspace's tool caches, and that only a named workspace can
  mount them.
- [x] 2.3 Add the `--no-tool-caches` build flag in `cli`, passed into the graph request, and document it in the CLI flag
  reference beside `--rebuild`, noting that combined with it the named targets rerun cold while their unchanged
  dependencies are still reused. Verify with a `cli` flag test, `vamos build --help`, and "A clean rebuild reuses a
  dependency built warm".
- [x] 2.4 Count a target's `ToolCaches` among its own inputs in `sched`'s history attribution (`diffActions`), so that a
  mount that moved, another cache or another owner and group is `own-inputs`, and add mounted caches to the `own-inputs`
  row of the miss-cause table in [cache.md](../../../docs/user/reference/cache.md). Verify with
  `cache-miss-attribution`'s "A moved cache mount is own-inputs" in `internal/features/misses_test.go`, and `sched`
  history tests that a changed path, label, and owner and group are each `own-inputs`, and that the mounts, counted as
  one difference, choose the nearest of two entries.

## 3. Mounting on the local executor

- [ ] 3.1 Resolve each mount, in `run`, to `<CacheDir>/tool-caches/<sha256 of workspace>/<sha256 of
  label>/<owner>-<group>-<mapping>/data`, creating on first use the label's directory and then the owner's, each under a
  `.tmp-` name renamed into place, and treating `ENOTEMPTY` and `EEXIST` from that rename as a lost race and any other
  error as a failure, with `<owner>-<group>` the rule's numeric IDs and `<mapping>` a digest of the builder's UID and
  GID mappings, or `host` without any, and `data/` chmodded to 0755 after it is made; when `NoToolCaches` is set, use
  instead a directory in a `tool-caches/` directory of the attempt's buildah container's directory
  (`Store.ContainerDirectory`), made once the builder exists, chmodded to 0755, with the same owner and group, mapped as
  the persistent cache's are, shared by the attempt's steps, deleted by the runner when the attempt ends, before the
  container is removed, and otherwise removed with the container. Verify with `run` unit tests of two attempts creating
  one cache at once, of a second owner added beside a label that has one, of an attempt that succeeds, fails or is
  cancelled removing its temporary caches with its container, of `run.RemoveAttempt` removing them for a killed attempt,
  and of the layout, ownership mapped through the builder's ID mappings, a directory per owner and group, a builder
  whose ID mappings changed finding a new, empty directory, including one that keeps the cache root's host IDs and
  remaps another, two owners that two stores' mappings put on one host ID keeping separate directories, a directory made
  under umask 077 having mode 0755, and a real non-root buildah `run()` step, with a builder whose ID mappings differ
  from the store's, writing to a persistent and to a temporary cache, seeing the rule's owner and group on it, and
  leaving the image's mount point with them once unmounted.
- [x] 3.2 Resolve each cache path with `securejoin` before the attempt's steps, recording each location's device and
  inode, failing the attempt for a location that is occupied, overlaps another cache's, is at or beneath `/dev`, `/proc`
  or `/sys` or reached through one of them, or equals, lies beneath or contains where `/etc/hosts`, `/etc/hostname`,
  `/etc/resolv.conf`, `/run/.containerenv`, `/run/secrets`, `/etc/crypto-policies/back-ends` or
  `/etc/crypto-policies/config` resolves, or a volume the image declares resolves once its path is cleaned, and for a
  `/dev`, `/proc` or `/sys` that is a symbolic link, checking the system roots, bound files and volumes again before
  each `run()` step, with `run` unit tests of a symbolic link that puts a cache at `/run`, of an alias through a
  directory of the image's `/dev`, of a `/etc/resolv.conf` that links into a cache at `/run/systemd`, of a volume
  `/alias/../cache/data` with `/alias` a link elsewhere being refused for a cache at `/cache`, of an image with a
  symbolic link at `/dev` that mounts no cache still building, verified too with "An add() that makes /dev a symbolic
  link", and of an image whose `/dev` is a relative symbolic link to `devices`, with a cache at `/devices/cache`,
  failing and naming `/dev`, and the scenario "A volume the image declares beneath a cache", and, just before the first
  `run()` step, create the mount points, failing naming the path for one that can't be created; before each `run()` step
  and once after all steps, fail it for a cache path that no longer resolves to its location or now resolves through
  `/dev`, `/proc` or `/sys`, whose location is no longer an empty directory with the cache's owner and group, or that
  holds the working directory, and before a `run()` step for a secret's file, a bound file or a volume the image
  declares that resolves at, inside or above a cache, resolving `/etc/crypto-policies/back-ends` and
  `/etc/crypto-policies/config` each, with a `run` unit test of `back-ends` linked to a cache, verified for volumes with
  "A volume that an earlier step moves into a cache". Verify the bound files with "A file every step is given that a
  symbolic link puts in a cache" and a `run` unit test of a first `run()` step that links `/etc/hostname` into a cache,
  and the traversal with "A step that points a cache path through /dev", whose resolved location stays the same, and a
  `run` unit test of a last `run()` step that does so, which the check after the last step catches; before each `add()`
  step, and when collecting output, fail it for a destination or `source` that resolves to or into a cache. Verify with
  "The image already holds files at a cache path", "A cache path through a symbolic link", "A step that moves a cache
  path", "A workdir() beneath a cache", "A workdir() that a variable puts at a cache", "A last workdir() that a variable
  puts at a cache", "Two cache paths that one symbolic link joins", "A cache path that a symbolic link puts beneath
  /dev", "A secret's file inside a cache", "A symbolic link that an earlier step makes into a cache" and "An add()
  destination that a variable puts in a cache", and `run` unit tests of a destination that an earlier `env()` puts in a
  cache, of a task's `source` that a symbolic link resolves into a cache, of a `source` such as `/work/../build` that a
  symlink at `/work` puts outside a cache at `/build` being collected, of a secret's file above a cache, of a last
  `run()` step that moves a cache path, of a mount point beneath a regular file, of an empty directory already at a
  cache path being kept, of a dangling symlink on a cache path, of cache paths that symlinks, nested or relative, lead
  beneath `/proc` and `/sys`, of `/device/cache` being allowed, of an `add()` destination and a `workdir()` whose
  variable and `..` put them in a cache once cleaned, of an owner compared through a user namespace's ID mappings, of an
  `add()` before the first `run()` that copies an empty directory with another owner to a cache's location failing, and
  of an owner the executor can't set. Prepare the mount points just before the first `run()` step in the design's order:
  check the working directory, resolve again, refuse a path that resolves elsewhere than before the steps or through
  `/dev`, `/proc` or `/sys`, before creating anything, with a `run` unit test of an `add()` that makes a missing parent
  a symlink to `/var/lib`, refuse a location that isn't absent or an empty directory that was there before the steps,
  create, set the owner, and only then check for an empty directory with the cache's owner and group; verify with "A
  step that makes a directory at a cache path" and `run` unit tests of an absent mount point being created and of an
  earlier layer's mount point with another owner and mode being kept and given the cache's owner and mode 0755. Give
  each, created or kept, the rule's owner and group, and create missing parents as the image's user, as `workdir()`
  does; verify with "A cache's mount point belongs to its owner", "A cache does not take a workdir() above it from its
  user", "A workdir() above a cache" and "An add() above a cache".
- [x] 3.2a Fail an attempt that mounts caches, before its steps and naming the isolation, when the executor's isolation
  is not chroot, taking the isolation from an executor option that defaults to buildah's own resolution, as the runner
  uses it today, so that a build that mounts no cache keeps its isolation, and that also sets the steps'
  `RunOptions.Isolation`, so that the check and the run agree and a parallel scenario can set it without `t.Setenv`;
  verify with "A cache under OCI isolation" and a `run` test that an attempt that mounts no cache under OCI is given
  `RunOptions.Isolation` OCI, and that with the option unset it is given buildah's own resolution, checking the chosen
  options rather than running a step.
- [x] 3.2b Fail the attempt, before the steps, before each `run()` step and after the last, when the resolution of a
  cache path, the working directory, a secret's file, a bound file or a declared volume passes through a cache's
  location, its own included, before its end. Verify with "A cache path reached through another cache" and "A step that
  re-links a cache path through another cache", and `run` unit tests of a cache path linked through its own cache, of
  `/etc/hostname` linked through a cache to a file outside it, of a last `run()` step that makes such a link, which the
  check after the steps catches, and of a `workdir("/alias/sub")` with `/alias` linked through a cache to a directory
  outside it failing before the next `run()` step.
- [x] 3.3 Bind-mount each cache read-write for every `run()` step, as a `bind` `specs.Mount` in `RunOptions.Mounts`
  rather than in `RunOptions.RunMounts`, which buildah turns into a discarded overlay, at the mount points task 3.2
  creates; give `add()` steps no mount. Verify with the scenarios "A later build sees what an earlier one left", "A
  mount overrides the cache's path", "A step is given no variable for its cache" and "A layer's image keeps no cache
  contents", with the same layer then built again with `--rebuild` on a warm cache, which its step sees a marker in, and
  running, and holding the same files.
- [x] 3.4 Verify the key rules end to end with "A changed cache does not cost a hit", "Moving a mount reruns the target"
  and "Renaming the workspace reuses results", and the workspace scoping with "Two workspaces with the same label keep
  separate caches" and "Two packages share one cache".
- [x] 3.5 Verify the clean build with "A clean build gives every cache empty", "A clean build gives each target its own
  empty cache", "A clean build keeps the cache for later builds" and "An attempt's steps share an empty cache", and the
  owner with "A non-root step writes to its cache", "A non-root step writes to an empty cache", "Changing a cache's
  owner gives it empty" and "Changing a cache's owner back finds what it held".
- [x] 3.6 Record `ToolCaches []ToolCacheUse{Label, Empty}` in `cache.Provenance` from the attempt, with `Empty` true
  only for an attempt's own temporary directory, as under `--no-tool-caches`, on a worker and in coordinator mode.
  Verify with "A clean build's provenance", a `run` test of coordinator mode's provenance, that provenance stays outside
  the key in the existing key tests, and `sched` tests of a clean job joining a warm job's attempt and a warm job
  joining a clean one, each reusing the result and its provenance.
- [x] 3.7 Describe tool caches in [storage.md](../../../docs/design/storage.md) as storage beside the three stores,
  with their trust model, layout, lock and lifetime, and in [run's doc.go](../../../internal/run/doc.go); verify the
  diagrams render and `scripts/check`'s doc checks pass.

## 4. Remote executors

- [x] 4.1 Make a worker's runner mount a fresh empty directory for each cache, whatever the request says, and record
  every cache as given empty, using the temporary directories of task 3.1, which the worker's existing removal of an
  attempt's container deletes; make `removeAttempt` delete the container directory's `tool-caches/` before removing the
  container, outside the store's locks, and take `storage.ErrLayerUnknown` from unmounting as already unmounted, on both
  its paths, going on to remove the container when deleting `tool-caches/` fails and returning that failure, wrapped to
  say it was the temporary caches, with any other, so that a removal that failed after the layer was deleted can be
  finished. Verify with "A remote build gives every cache empty" in the remote feature's scenarios, and `worker` tests
  that they are gone after an attempt whose subprocess was killed and after a start that finds a crashed run's attempt,
  a `run` test that a failed delete of `tool-caches/` still removes the container and returns the wrapped failure with
  any error removing it, and a `run` test that a removal failing after the container's layer and buildah's state are
  gone is finished by the next one, and document it in [remote-execution.md](../../../docs/design/remote-execution.md)
  and the user remote reference.

- [ ] 4.2 Document in [remote-execution.md](../../../docs/design/remote-execution.md) that a crashed attempt whose
  temporary caches take longer than its lease to delete is lost, the attempt counted as a lost one, retried while its
  retry budget allows and otherwise failed as lost. Verify with a `worker` test that the runner deletes an attempt's
  temporary caches before its subprocess ends, while the lease is renewed.

## 5. Pruning

- [x] 5.1 In `Local.prune`, in the `first` callback that `cache.Directory.Prune` runs under the exclusive lock, beside
  `removeCachedImages`, rename `tool-caches/` aside to `tool-caches.trash-<random>`, then count the owner directories of
  what it renamed, a failure to count being a warning, a missing `tool-caches/` counting as none and a failed rename of
  one failing the prune; once `Prune` has taken its lock back shared, and not if it couldn't, delete it with any
  `tool-caches.trash-*` an earlier prune left, reporting a failure to delete as a warning in `api.Pruned.Warnings`. In
  coordinator mode, rename, delete and count nothing under `tool-caches/` or `tool-caches.trash-*`. Add `ToolCaches`,
  the caches removed from use, to `api.Pruned` and the prune command's report. Verify with "A build after a prune starts
  with empty caches", a prune scenario showing the count, `run` tests that a prune finding a leftover
  `tool-caches.trash-*` deletes it and that a failed delete is a warning and not a failure, a `run` test that a prune on
  an executor that never mounted a cache succeeds and counts none, one with two workspaces' caches, a `.tmp-` leftover
  and old trash counting only the caches, one in coordinator mode keeping both `tool-caches/` and old trash and
  reporting none, the scenario "Pruning a local coordinator keeps a standalone build's cache", two owner directories
  under one label counted as two, one whose rename fails failing and counting none, one whose count fails after the
  rename warning, reporting what it read and deleting the tree, one whose `Prune` returns `cache.ErrRelock` leaving the
  trash and the process exiting with an error that says the build cache lost its lock, for a build and for a local
  coordinator, pruning no history and not reported as refused, `cache` tests that `Prune` returns `cache.ErrRelock`, not
  matching `cache.ErrLocked`, when it can't take its lock back after a refused upgrade and after a prune, and the
  existing test that a prune is refused while a build holds the directory, extended so that a cache's marker is still
  there for a fresh action after the refusal.
- [x] 5.2 Update the pruning section of [docs/user/reference/cache.md](../../../docs/user/reference/cache.md) to say
  that a standalone prune deletes tool caches and a local coordinator's leaves them, and verify the documented command
  output matches what prune prints.

## 6. vamos builds itself with a Go build cache

- [x] 6.1 Name the root workspace in `WORKSPACE` if it isn't named, declare `tool_cache(name = "go-build", path =
  "/cache/go-build")` in the root `BUILD`, a path no image holds anything at, and mount it with `mount("go-build")` in
  the `lint`, `vulnerabilities`, `test` and `build` tasks. Run each of their Go commands through `sh -c`, passing its
  arguments, such as `build`'s `-ldflags`, through `"$@"` unchanged, with `sh` given as `$0` (`sh -c '...' sh <args>`),
  with `GOCACHE`, and `GOLANGCI_LINT_CACHE` for `lint`, set to directories beneath the mount named by a digest of the
  image's native build environment: its installed packages, sorted with `LC_ALL=C sort` (`rpm -qa --qf
  '%{NAME}-%{EPOCHNUM}:%{VERSION}-%{RELEASE}.%{ARCH}\n'` on Fedora, which `rpm -qa` alone leaves the epoch out of,
  `dpkg-query -W` on the golang image), and the contents of `/usr/local/include`, `/usr/local/lib` and
  `/usr/local/lib64`, since Go's build cache doesn't detect a change to the C libraries and headers cgo uses, so that a
  base that installs other ones compiles into an empty directory. Give the `test` task's `go test` `-count=1`, since `go
  test` reuses a cached result while the files a test reads keep their size and modification time, and vamos gives every
  added file the same time. Verify that `./vamos build lint` after a one-file source change is much faster than the
  first run and still reports failures, and, in a scratch workspace, that adding a package to the base's install step,
  or a header to `/usr/local/include`, gives the Go commands a new directory.
- [x] 6.2 Remove "No Go build cache" from [self-build.md](../../../.agents/kb/self-build.md), describe the cache there,
  give its table's `test` row `go test -short -count=1 ./...`, and say that its commands run in the
  devcontainer, whose `BUILDAH_ISOLATION=chroot` tool caches need; verify the doc's commands still run as written.

## 7. Integration

- [x] 7.1 Run `scripts/check` with its full tier and `VAMOS_REQUIRE_INTEGRATION=1`, on a host where buildah's chroot
  isolation runs, so that no buildah scenario is skipped, and `openspec validate tool-caches --strict`, and run the
  smoke checks in [smoke-checks.md](../../../.agents/kb/smoke-checks.md) that build examples, confirming that examples
  without caches build and reuse results as before apart from the one-time version rebuild.

## Workflow follow-up

- Archive the change after review, syncing `tool-caches` into `openspec/specs/`, and update `workspace-identity`'s
  Purpose, which says the name never changes what is built or cached, to say it chooses a workspace's tool caches.
