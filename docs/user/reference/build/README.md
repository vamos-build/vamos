---
title: "BUILD language reference"
description: "Targets, steps, and file selection in Vamos BUILD files."
---

A `BUILD` file is a Starlark program that declares build targets.
In directories containing a subfolder named `build`, name the file `BUILD.vms`
to prevent naming collisions on case-insensitive filesystems;
see [workspaces](../configuration.md#workspaces).
You can use Starlark lists, dictionaries, functions, and comprehensions to compose target declarations.

Syntax errors and mistakes in how a call is made, such as a misspelled argument or a duplicate name,
halt execution as the file loads.
A mistake in a target's values or references is reported when a command needs the target, before any build steps run.
Error messages report the file, line, and column of the invalid call, which may be in a `.star` file.
Strings passed to targets and steps—including names, arguments, paths, owners, and environment variables—must
not contain NUL bytes.

## Target names and references

Each target within a `BUILD` file requires a unique, nonempty `name`.
Names cannot contain `:` or `/`, and `scratch` is reserved to represent an empty base image.
Target declarations register targets in the build graph; they do not return artifact objects.
Refer to targets by name or by [workspace label](../configuration.md#workspaces), optionally wrapped in `dep()`.

A reference can name a target declared above or below it, in its own file or another package's;
see [cross-package dependencies](../configuration.md#cross-package-dependencies).
The values passed to a rule, or to `dep()`, are frozen as it is called, so a list can't be changed after passing it;
see [cross-package dependencies](../configuration.md#cross-package-dependencies) for both.
Dependency cycles produce an error as the targets are configured.

| Target | Produces | Can be a `base` | Can be an `add()` source |
| --- | --- | --- | --- |
| [`pull()`](pull.md) | Image | Yes | No |
| [`glob()`](glob.md) | Fileset | No | Yes |
| [`task()`](task.md) with output | Fileset | No | Yes |
| [`task()`](task.md) without output | Check result | No | No |
| [`layer()`](layer.md) | Image | Yes | No |
| [`index()`](image-index.md) | Image index | No | No |
| [`publication()`](publication.md) | Release published by `vamos publish` | No | No |
| [`tool_cache()`](tool-cache.md) | A tool's cache, kept between builds and mounted with `mount()` | No | No |

No target can depend on a `publication()`, which can only be requested from the command line.
A `tool_cache()` is not built at all: tasks and layers mount it for their `run()` steps.
All target types except `pull()` and `tool_cache()` accept a `params` dictionary for target parameters.
A target's attributes other than `name` and `params` can be defined as functions of `ctx`, but not a `tool_cache()`'s.
See [configuration](../configuration.md) for details on `param()`, `dep()`, `arg()`, and platform configuration.

## Steps and shared options

Tasks and layers build their contents with [steps](steps.md):
`add()` copies filesets, `run()` executes commands, and `env()`, `build_env()`, and `workdir()` configure the container.
Steps that need credentials receive them as [secrets](secrets.md),
and steps whose tools keep a cache can be lent one that persists between builds as a [tool cache](tool-cache.md).
Globs and task outputs choose their files with the patterns and limits described in
[file selection](glob.md#file-selection).
