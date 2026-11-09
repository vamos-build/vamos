---
title: "BUILD language reference"
description: "Targets, steps, and file selection in vamos BUILD files."
---

A `BUILD` file is a Starlark program that declares build targets.
In directories containing a subfolder named `build`, name the file `BUILD.vms`
to prevent naming collisions on case-insensitive filesystems;
see [workspaces](../configuration.md#workspaces).
You can use Starlark lists, dictionaries, functions, and comprehensions to compose target declarations.

Syntax errors or invalid target arguments halt execution as the file loads, before any build steps run.
Error messages report the file, line, and column of the invalid call.
Strings passed to targets and steps—including names, arguments, paths, owners, and environment variables—must
not contain NUL bytes.

## Target names and references

Each target within a `BUILD` file requires a unique, nonempty `name`.
Names cannot contain `:` or `/`, and `scratch` is reserved to represent an empty base image.
Target declarations register targets in the build graph; they do not return artifact objects.
Refer to targets by name or by [workspace label](../configuration.md#workspaces), optionally wrapped in `dep()`.

Declare local targets before any targets that consume them.
References created inside a function of `ctx` can resolve targets declared later in the file.
Dependency cycles produce an error during graph construction.

| Target | Produces | Can be a `base` | Can be an `add()` source |
| --- | --- | --- | --- |
| [`pull()`](pull.md) | Image | Yes | No |
| [`glob()`](glob.md) | Fileset | No | Yes |
| [`task()`](task.md) with output | Fileset | No | Yes |
| [`task()`](task.md) without output | Check result | No | No |
| [`layer()`](layer.md) | Image | Yes | No |
| [`index()`](image-index.md) | Image index | No | No |
| [`publication()`](publication.md) | Release published by `vamos publish` | No | No |

No target can depend on a `publication()`, which can only be requested from the command line.
All target types except `pull()` accept a `params` dictionary for target parameters.
Attributes other than `name` and `params` can be defined as functions of `ctx`.
See [configuration](../configuration.md) for details on `param()`, `dep()`, `arg()`, and platform configuration.

## Steps and shared options

Tasks and layers build their contents with [steps](steps.md):
`add()` copies filesets, `run()` executes commands, and `env()`, `build_env()`, and `workdir()` configure the container.
Steps that need credentials receive them as [secrets](secrets.md).
Globs and task outputs choose their files with the patterns and limits described in
[file selection](glob.md#file-selection).
