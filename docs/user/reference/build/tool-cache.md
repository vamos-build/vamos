---
title: "tool_cache()"
description: "Keeping a tool's own cache, such as Go's build cache, between builds."
---

A tool cache is a directory that the executor keeps between builds
and lends to the `run()` steps of the tasks and layers that mount it,
so that a compiler or package manager finds what it cached the last time,
and an edit rebuilds incrementally.

```python
tool_cache(name = "go-build", path = "/cache/go-build")

task(
    name = "lint",
    base = "tools",
    tool_caches = [mount("go-build")],
    steps = [
        add("source", "/src"),
        workdir("/src"),
        build_env(GOCACHE = "/cache/go-build"),
        run(["go", "vet", "./..."]),
    ],
)
```

| Argument | Description | Default |
| --- | --- | --- |
| `name` | Name of the cache | Required |
| `path` | Absolute container path where the cache is mounted, unless a mount overrides it | Required |
| `owner` | Numeric user ID that owns the cache's directory | `"0"` |
| `group` | Numeric group ID of the cache's directory | `"0"` |

A `tool_cache()` takes no other argument, and its mistakes fail as the `BUILD` file loads,
whether or not a target mounts the cache.
It sets no environment variable:
many tools find their cache at its default path,
and a step that needs a variable to find it sets one with `build_env()`.

## Mounting a cache

[`task()`](task.md) and [`layer()`](layer.md) take `tool_caches`, a list of `mount()` values.
`mount(label)` names a `tool_cache()` by label, from any package of the workspace,
and `mount(label, path = "/other")` mounts it at another path;
`path = None` is the same as leaving it out.
One cache can be mounted at different paths by different targets,
and the cache is shared by every target that mounts it.

```python
tool_cache(name = "go-mod", path = "/go/pkg/mod")

layer(
    name = "deps",
    base = "golang",
    tool_caches = [mount("go-mod", path = "/root/go/pkg/mod")],
    steps = [
        add("modules", "/src"),
        workdir("/src"),
        build_env(GOMODCACHE = "/root/go/pkg/mod"),
        run(["go", "mod", "download"]),
    ],
)
```

A target mounts each cache for every one of its `run()` steps, and for no `add()` step,
so a target that mounts caches needs a `run()` step.
What a step writes beneath a cache's path stays in the cache, never in a layer's image,
which holds an empty directory there.
A tool cache is no target: a command that builds or sends it, and a `base`, `dep()`, `add()` or publication check
that names it, fails.
A misspelled name suggests tool caches to `mount()` alone, and targets alone elsewhere.

## Caches never change what is built

Declaring a cache is your assertion that what it holds affects only how long a build takes, never what it makes,
just as a `run()` that fetches a pinned URL asserts that the URL always gives the same file.
Vamos takes the assertion as given and doesn't verify it.
What a cache holds, and whether it holds anything, is therefore never part of a target's cache key:
a warm cache never turns a hit into a miss, and a cold one never discards a result.
Where each cache is mounted, which cache it is, and its owner and group are part of the key,
so adding, moving or removing a mount reruns the target.

**An executor may give any cache empty**, and a build must succeed either way.
`vamos build` keeps each workspace's caches between builds on the machine it runs on.
A coordinator's workers, and `vamos coordinator --local`, give every cache empty.
`--no-tool-caches` gives each attempt that a build starts empty caches of its own,
which its steps share and which are discarded when it finishes;
a result made with a warm cache is still reused, as is an attempt of another build already running with one,
and `--no-tool-caches --rebuild` reruns the named targets cold.
[`vamos prune`](../cache.md#storage-and-pruning) deletes every workspace's caches,
though a prune of `vamos coordinator --local` leaves them.

Some tools can break the assertion:

- A tool whose cache misses a change it doesn't track can return what it made before the change,
  as Go's build cache misses a change to the C libraries and headers that cgo uses.
  Keep its cache in a directory beneath the mount named by what it doesn't track,
  such as a digest of the image's installed packages.
- A tool that trusts files' sizes and modification times can return a stale result,
  since Vamos gives every file it adds the same time,
  as `go test` does with its cached results; run `go test -count=1`, which keeps the build cache.
- A step that changes its cache's owner or mode must leave it usable by the cache's owner,
  or later builds fail with a warm cache and pass with an empty one.
  `vamos prune` repairs the cache, `--no-tool-caches` gets the attempts a build starts past it,
  though a target can still join another build's attempt that has the cache,
  and `--no-tool-caches --rebuild` reruns a named target cold.

## Workspaces and owners

A cache belongs to its workspace: it is kept by the workspace's name and the cache's label,
so workspaces named differently never share one, and renaming a workspace mounts the caches of the new name,
empty unless a workspace of that name used them, while still reusing its results.
Only a named workspace can mount caches:
a target that mounts one fails to load in a workspace whose `WORKSPACE` file gives no name,
saying that the workspace needs one.
Declaring a `tool_cache()` that no target mounts is allowed.
See [naming a workspace](../configuration.md#naming-a-workspace).

The cache's directory is owned by `owner` and `group`, given as decimal user and group IDs from 0 to 4294967294,
both or neither; a name is refused, since it can mean a different ID in each image.
`None` for either is the same as leaving it out, so that a function can pass on its own defaults.
Give them when the steps that use the cache run as another user than root.
A cache is kept apart for each owner and group,
so changing them starts the cache empty and changing them back finds what it held.
The cache's mount point in the container takes the cache's owner and group,
and any parent directory that Vamos creates for it belongs to the user the image runs as,
as `workdir()` would make it.

## Paths

A cache's path, the rule's or a mount's, is a literal absolute path in clean form, without `$`, and not `/`.
It can't be at or beneath `/dev`, `/proc` or `/sys`, which buildah mounts itself,
nor at, beneath or above `/etc/hosts`, `/etc/hostname`, `/etc/resolv.conf`, `/run/.containerenv`, `/run/secrets`
or `/etc/crypto-policies`, which buildah may bind into a `run()` step.
Loading refuses two mounts of a target at one path or nested within each other, one cache mounted twice,
a secret's file at, within or above a cache,
an `add()` destination or `workdir()` within a cache, and a task's `source` within one.
The paths a variable or a symbolic link decides are checked as the target runs:
a path the image already holds files at, a step that moves a cache's path or puts something at it,
a cache's path that a step couldn't follow, such as through a link to `/absent/../real` when `/absent` is missing,
and a working directory, `add()` destination, secret or `source` that resolves into a cache
each fail the target.

The executor creates a cache's mount point, and its missing parents, just before the first `run()` step,
so the parents exist from then on:

- A `run()` step that makes a cache's parent itself, as `git clone <url> /app` does with a cache at `/app/.cache`,
  fails; make the parent in an earlier step, or mount the cache elsewhere.
- A tool that deletes and remakes a directory holding a cache fails, as `npm ci` does with `node_modules`;
  mount the cache outside such a directory.
- A layer leaves an empty directory at each cache path in its image,
  so a target built on it can't mount a cache at a path above one where its base mounted a cache;
  mount the cache beside it instead.

Tool caches need buildah's chroot isolation, which Vamos's image sets:
a target that mounts caches under another isolation, such as `BUILDAH_ISOLATION=oci`, fails before its steps,
while one that mounts none builds as before.
On a FIPS host, an image whose `/usr/share/crypto-policies/back-ends/FIPS` links to where a cache's mount point will
be makes buildah bind its FIPS files once the mount point exists, which changes what the target's steps see;
keep such links away from a cache's path.
