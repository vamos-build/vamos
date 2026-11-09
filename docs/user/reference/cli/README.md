---
title: "CLI reference"
description: "The vamos commands for building, publishing, loading, saving, and exporting images."
---

Run `vamos --help` or `vamos <command> --help` for command-line help; invoking `vamos` without arguments shows help too.
Targets are declared names in the current directory's `BUILD` file,
or [workspace labels](../configuration.md#workspaces).
If a target name is misspelled, vamos suggests close matches or lists available targets in the package.

The `-C <dir>` flag selects the working directory whose targets can be referenced without a package label.
Relative paths passed to `-C` are evaluated relative to where you invoke vamos.
The `-C` flag does not change the process's current working directory:
output paths on your host remain relative to where you run vamos.

## Commands

All commands that build targets reuse the [build cache](../cache.md)
and accept the [shared flags](flags.md).
Delivery commands build the requested target and its dependencies before delivering the final artifact.

| Command | Accepted targets | Description |
| --- | --- | --- |
| [`vamos build <target>...`](build.md) | Pulls, layers, tasks, indexes, publications | Build targets and store results in the build cache |
| [`vamos push <target> <image>`](push.md) | Pulls, layers, indexes | Publish an image or index to a container registry |
| [`vamos publish <publication> --tag <tag>...`](publish.md) | Publications | Execute checks and publish release images under each specified tag |
| [`vamos load <target> <image>`](load.md) | Pulls, layers, indexes | Load a container image into Docker |
| [`vamos save <target> <file.tar>`](save.md) | Pulls, layers, indexes | Write an image archive importable by `docker load` |
| [`vamos export <task> <folder>`](export.md) | Tasks with output | Write a task fileset into a local directory |
| [`vamos update [pull...]`](update.md) | Pulls, or all pulls if omitted | Pin pull targets to manifest digests in `vamos.lock` |
| [`vamos prune`](prune.md) | None | Remove cached build results |
| [`vamos coordinator`](coordinator.md) | None | Run a coordinator to schedule builds across workers |
| [`vamos worker`](worker.md) | None | Run a build worker managed by a coordinator |

Registry operations authenticate with your local [registry credentials](registry-credentials.md).
See [output and troubleshooting](output.md) for progress states, error reporting, and common problems.
