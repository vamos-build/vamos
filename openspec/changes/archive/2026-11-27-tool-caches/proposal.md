# Proposal

## Why

A task that runs `go build`, `tsc` or `vite build` starts from an empty compiler cache on every run,
so a one-line source change costs a full rebuild: vamos's own check tasks take about a minute
where the devcontainer takes seconds.
A dependency layer keyed on a lockfile avoids repeated downloads, but a single version bump refetches every dependency.
Local development, and CI on a persistent runner, need tools' own caches to persist between builds.

## What Changes

- A new `tool_cache` rule declares a **tool cache**: a directory that persists on the executor between builds
  and is lent to the `run()` steps of the targets that mount it.
  The rule requires `path`, the absolute container path where it is mounted by default,
  and takes an optional `owner` and `group`; it names no variable and no tool.
  Its label lets targets in any package of the workspace share it.
- `task` and `layer` take `tool_caches`, a list of `mount(label)` values, mounting each cache for every `run()` step of
  the target, and never for `add()`.
  A mount uses the cache's own path unless it gives `path` to override it,
  so one cache can be mounted at different paths by different targets.
  `mount()` leaves room for more mount options later.
  A step that needs a variable to find a cache sets one with `build_env`; many tools find their default paths unaided.
- **Declaring a cache is the user's assertion that its contents affect only how long a build takes, never what it
  makes**, just as a `run()` that fetches a pinned URL asserts its repeatability.
  The contents of a cache, and whether it holds anything, are therefore never part of an action's key:
  a warm cache never turns a hit into a miss, and a cold one never invalidates a result.
  Each mount's path, the cache's label and its owner and group are part of the key,
  since mounting over a path changes what a layer keeps.
- **An executor may give any cache empty.** That one rule covers a clean CD build, a remote build, pruning and a cache
  in doubt. This change keeps caches on the local executor only: a coordinator's workers mount an empty directory at
  each path, so a BUILD file that mounts caches builds the same everywhere, only slower. `vamos coordinator --local`
  gives every cache empty as well, through a coordinator mode of the local executor, and its prune leaves standalone
  builds' caches. A new build flag, `--no-tool-caches`, asks the local executor to give every attempt it starts its own
  empty caches, discarded when the target finishes and never shared with another target, for a clean build. A result or
  running attempt made with a warm cache may still be reused.
- A cache's contents are never part of a layer's image: what a step writes beneath a cache path stays in the cache.
- A cache belongs to its workspace: its identity is the workspace's name and the cache's label.
  **A workspace without a name cannot mount caches**: a target that mounts one fails to load and the error
  says to name the workspace.
  The workspace name stays outside every key and the graph, as `workspace-identity` requires.
- Loading refuses a cache path, whether the rule's or a mount's, that is not absolute or not in clean form, is `/`,
  contains `$`, is at or beneath `/dev`, `/proc` or `/sys`, which buildah's chroot isolation mounts itself, or equals,
  lies beneath or contains `/etc/hosts`, `/etc/hostname`, `/etc/resolv.conf`, `/run/.containerenv`, `/run/secrets` or
  `/etc/crypto-policies`, which it binds into a `run()` step; a mount whose path equals or overlaps another of the
  same target's mounts, or a secret's file; an `add()` destination or a `workdir()` with no variable at or beneath a
  mount's path once cleaned, or a task's `source` with no `..` component there; one cache mounted twice by a target; a
  target that mounts caches and has no `run()` step; and a `mount()` label that names anything but a `tool_cache` rule.
  A `tool_cache` label is no target: building it, or naming it where a target is expected, fails. Since `tool_caches` is
  a list, not a dict keyed by path, loading enforces unique paths itself. What loading can't see, the executor checks as
  the attempt runs, following symlinks inside the container: a cache path the container already occupies, a step that
  moves a cache path or leaves something at it, and a secret's file, an `add()` destination, the working directory
  before a `run()` step or after the last step, or a task's `source` that variables, `..` or symlinks put in a cache
  path, each fail the attempt.
- Caches need buildah's chroot isolation, which vamos's image sets: an attempt that mounts caches under another
  isolation, such as `BUILDAH_ISOLATION=oci`, fails before its steps, while one that mounts none builds as before.
- `vamos prune` deletes the local executor's caches along with the rest of its cache.
- A result's provenance records which caches its attempt mounted and whether each was given empty,
  for diagnostics; like the rest of provenance, it is outside the key.

## Capabilities

### New Capabilities

- `tool-caches`: the `tool_cache` rule, mounting caches in tasks and layers, the user's assertion and what is and isn't
  in the key, the rule that an executor may give any cache empty, workspace scoping and the unnamed-workspace refusal,
  path conflicts, pruning and provenance.

### Modified Capabilities

- `workspace-identity`: the requirement that the name never affects what is built gains one exception,
  that only a named workspace can build a target that mounts caches. The name still chooses only which cache an
  executor mounts, enters no key, graph or step, and identical targets in workspaces named differently,
  or unnamed, still share results whenever both can build them.
- `cache-miss-attribution`: a change to the caches a target mounts, where it mounts them, or their owner and group,
  is an `own-inputs` miss.

## Impact

- BUILD language ([load](../../../internal/load/doc.go)): the `tool_cache` and `mount` builtins, the `tool_caches`
  argument of `task` and `layer`, label resolution to `tool_cache` rules, and the load-time conflict checks.
- [graph](../../../internal/graph/doc.go): layers and tasks carry their mounts (path, canonical label, owner and
  group), and `graph.Version` advances.
- [sched](../../../internal/sched/doc.go): `Action` carries the mounts and `ActionVersion` advances;
  history's attribution counts a target's mounts among its own inputs; `Request` carries the workspace name the
  scheduler already holds for target history, as execution metadata, outside the key.
- [run](../../../internal/run/doc.go): cache directories beside `cache.Directory`, bind mounts for `run()` steps,
  the checks of each cache path in the container, ownership, the empty-cache mode, pruning and provenance.
- [worker](../../../internal/worker/doc.go): mounts an empty directory for each cache.
- [cli](../../../internal/cli/doc.go) and [api](../../../internal/api/doc.go): the clean-build flag, and the count of
  caches a prune removed.
- Documentation: the BUILD reference (`task`, `layer`, steps, a new page for the `tool_cache` rule), the cache
  reference's pruning section, [storage.md](../../../docs/design/storage.md) and
  [self-build.md](../../../.agents/kb/self-build.md).
- The root `BUILD`'s check tasks mount Go's build cache, the first real use.
