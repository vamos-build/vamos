---
title: "Build cache and traces"
description: "How vamos reuses and rebuilds results, prunes its cache, and traces builds."
---

vamos caches target results and reuses them when their effective inputs have not changed.
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
Mounting a volume at `/var/lib/containers` preserves the cache when running vamos inside a container;
see the [tutorial setup](../tutorial.md#2-set-up-vamos).

If a cached layer image or fileset is deleted or corrupted,
vamos reports the issue and automatically rebuilds the target rather than failing the build.

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

Commands that build or deliver targets accept `--trace <file.json>`:

```sh
vamos build lint test app --trace build-trace.json
vamos load app app:dev --trace load-trace.json
```

The resulting trace file can be opened in [Perfetto](https://ui.perfetto.dev).
The viewer parses the file locally in your browser without uploading data.

Traces visualize the complete build lifecycle:
- `BUILD` file parsing and package loading.
- Input glob evaluation and source file hashing.
- Execution steps for pulls, tasks, and layers.
- Cache hits, cache misses, and step replay.
- Delivery operations (pushing to registries, loading into Docker, saving archives).

Arrows in the timeline connect glob and target outputs to the execution steps that consume them.
Trace spans include performance metrics such as file counts, byte sizes, manifest digests, and image IDs.
vamos attempts to write the trace file even if a build step or delivery operation fails.
Writing a trace replaces an existing file at the specified path.
The trace path must not collide with a `save` archive path or reside inside an `export` destination directory.
