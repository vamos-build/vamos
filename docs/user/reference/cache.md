---
title: "Build cache and traces"
description: "How Vamos reuses and rebuilds results, prunes its cache, and traces builds."
---

Vamos caches target results and reuses them when their effective inputs have not changed.
Delivery commands (`push`, `publish`, `load`, `save`, `export`) use the same cache as `build`.
This reference describes cache key composition, rebuild rules, cache storage, pruning, timestamps,
and performance tracing.

## What changes a result

A target's cache key is determined by its configured inputs:
- The base image identity and layers.
- Configured steps, executable paths, and arguments.
- Contents, permission modes, and symbolic links of added filesets.
- Timestamps of added task outputs (even if file contents are identical).
- Secret IDs and the container environment variables or paths assigned to them (excluding secret values).
- The execution platform for targets configured with `EXEC_PLATFORM`.

The following factors do not affect cache keys:
- Secret values supplied via `--secret`.
- Modification times (mtimes) of source files on your host.
- Target names.

Touching a source file without altering its contents or renaming a target does not force it to rerun.
Changing a parameter invalidates the cache only if it alters the target's configured inputs or steps.

The CLI reports target status during execution:

```text
layer "tools": cached
task "test": cached
task "compile": building
layer "app": building
```

Cached targets replay their previous console output.
Check tasks (tasks without output declarations) are cached on success.
Failed targets do not produce cached results,
though dependencies that completed successfully before a failure remain cached.

## Refreshing results

Cached results are reused even if external network dependencies have changed
(such as newer packages in a remote package repository).
Delivery commands (`push`, `publish`, `load`, `save`, `export`) do not force cached targets to rerun.
For example, running `vamos publish` republishes release images to registries,
reusing cached check tasks and built layers.

To force a target to rerun explicitly:

```sh
vamos build packages --rebuild
```

The `--rebuild` flag rebuilds `packages`, reuses its unchanged dependencies where possible,
and replaces its cached result.
Downstream targets that depend on `packages` rebuild automatically if its output changes.
To refresh a dependency, pass its name explicitly.

Pull targets are refreshed using [`vamos update`](cli/update.md) rather than `--rebuild`:

```sh
vamos update node
```

The workspace lockfile (`vamos.lock`) records the manifest digest for each pulled tag.
The build cache retains pulled images under their digest so subsequent builds avoid registry round-trips.
Running `vamos build <pull> --rebuild` does not contact the registry;
it informs you that `vamos update` is used to update pulls.

To ensure test and lint checks run on every CI execution:

```sh
vamos build lint test --rebuild
```

## Storage and pruning

Locally cached layer images are stored in the local container store under the name `localhost/vamos-cache:<id>`.
Metadata, logs, and captured workspace files are stored alongside the container store,
typically under `/var/lib/containers/vamos`.
Mounting a volume at `/var/lib/containers` preserves the cache when running Vamos inside a container;
see the [installation requirements](../install/README.md).

If a cached layer image or fileset is deleted or corrupted,
Vamos reports the issue and automatically rebuilds the target rather than failing the build.

To remove cached build outputs:

```sh
vamos prune
```

`prune` deletes all cached layer images and intermediate build outputs.
It preserves pulled base images, recorded tag resolutions, and lockfile pins.
The command reports the count of removed images, target results, and files.
Subsequent builds regenerate required targets.

When building through a [remote coordinator](remote.md),
the build cache resides on the coordinator (`--cache-dir`)
and intermediate layer images are stored in the shared cache repository registry.
Running `vamos prune --coordinator <URL>` removes coordinator build results
and deletes cached layer tags from the cache repository.

## Timestamps

Files added to a container and the resulting image metadata receive deterministic timestamps.
Each target derives its timestamp from the latest timestamp among its base image and any added task outputs,
starting from the creation timestamps of pulled base images.
Using deterministic timestamps ensures identical inputs produce byte-identical images across builds.

Use `--timestamp` to establish a minimum timestamp for newly built targets:

```sh
vamos build app --timestamp 2026-01-01T00:00:00Z
vamos build app --timestamp 1767225600
```

Both examples specify the same UTC moment.
The `--timestamp` flag is not a cache key input, so cached results retain their existing timestamps.
To apply a new timestamp to a cached target, rebuild it explicitly:

```sh
vamos build app --rebuild --timestamp 2026-01-01T00:00:00Z
```

Rebuilding a task with a changed timestamp invalidates downstream targets that add its files,
even if the file contents did not change.
Exported task files retain the task's timestamp.

## Traces

Every command that builds is traced, whether or not you ask: `build`, `push`, `publish`, `load`, `save` and
`export`.
Once its jobs end, a command prints its trace's digest on standard error, whether it succeeded or failed:

```text
vamos: trace sha256:4f2a…
```

`vamos trace` writes a stored trace to a file, by that digest, or by a job's ID on a coordinator,
and `--trace <file.json>` writes the command's own as it ends:

```sh
vamos build lint test app --trace build-trace.json
vamos trace sha256:4f2a… build-trace.json
```

Either file opens in [Perfetto](https://ui.perfetto.dev), which reads it in your browser without uploading it.

A command's trace holds all of its work: this process's, and each job's on its executor, local or a coordinator and
its workers.
It shows:
- `BUILD` file parsing and package loading.
- Input glob evaluation and source file hashing.
- Execution steps for pulls, tasks, and layers, with their files and bytes.
- Cache hits, with when the result was made, how long making it took, and the time reusing it saved,
  and cache misses, with why each target missed (see [why a target missed](#why-a-target-missed)).
- Delivery operations (pushing to registries, loading into Docker, saving archives).

Arrows in the timeline connect glob and target outputs to the execution steps that consume them.

What a trace keeps lives in the executor's build cache:
the trace of each attempt that ran a target, kept with its result, each job's trace, and each command's.
A build that waits for another build's run of the same target shows that run's steps,
on a track named for the target with `(shared)`;
a build that left such a run while it went on says so on the target's track.
Each stored trace is kept whole within 16 MiB, as a log is: past that, the latest-started spans are dropped,
and the trace says it was cut short and how many it dropped.
A result whose trace is gone is reused all the same; a trace that would show it says the trace is gone.
`vamos prune` removes every stored trace.

`--trace` writes a file even when the command fails.
Writing a trace replaces an existing file at the specified path.
The trace path must not collide with a `save` archive path or reside inside an `export` destination directory.

### Why a target missed

In a [named workspace](configuration.md), the executor keeps each target's history: the last 8 actions its builds
used, per target, platform and parameters.
Each layer or task that runs, rather than being reused, gets one cause in its trace:

| Cause | Meaning |
| --- | --- |
| `forced` | `--rebuild` named it, whatever else changed. |
| `lost` | Its action is one it built before, but the result is gone or can't be used, as after a prune. |
| `new` | It has no earlier build to compare with: a first build, a new configuration, or an unnamed workspace. |
| `own-inputs` | A step, command, output, source file, or which target supplies its base or files changed; a source's added, removed and changed files are named. |
| `upstream` | Only the image that its base pull's tag names moved, as after `vamos update`; both manifests are named. |
| `dependency` | Only what a target it depends on supplies changed, as a base layer's image or an added task's files; each is named. |

A miss is compared with the history entry that differs from it least, so builds of different branches don't
inflate each other's changes.
History names no file of the cache, so it survives a prune.
A prune keeps the history of a target built or reused in the last 90 days, and removes it once its target has been
neither built nor reused for 100: a reuse renews it at least once a week.
