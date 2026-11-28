---
title: "Configuration reference"
description: "Workspaces, parameters, platforms, and shared .star files."
---

Vamos uses packages to organize projects, parameters to vary target behavior,
and platforms to manage cross-compilation and container architectures.
This reference explains workspace discovery, target labels, configuration parameters, multi-platform builds,
and shared Starlark modules.

## Workspaces

A **package** is any directory containing a `BUILD` or `BUILD.vms` file.
A **workspace** groups packages beneath a common root directory containing a `WORKSPACE` file (which can be empty),
which may [name the workspace](#naming-a-workspace).

Name a package file `BUILD.vms` if the directory also contains a `build` output directory.
On case-insensitive filesystems (such as macOS default filesystems or Git repositories with `core.ignorecase`),
`BUILD` and `build` represent the same file path.
Using `BUILD.vms` prevents naming collisions
and ensures `.gitignore` patterns for `build` do not ignore the build file.
A package cannot contain both `BUILD` and `BUILD.vms`; having both in one directory is an error.
Labels and target semantics are identical whether a package uses `BUILD` or `BUILD.vms`.

```text
project/
├── WORKSPACE
├── BUILD
├── backend/
│   └── BUILD
└── ui/
    └── BUILD.vms
```

To locate the workspace root, Vamos searches upward from the directory specified by `-C` for a `WORKSPACE` file.
If no `WORKSPACE` file is found, the directory specified by `-C` is treated as the workspace root.

Target labels identify targets across packages:

| Label | Meaning |
| --- | --- |
| `app` or `:app` | Target in the current package's `BUILD` file |
| `//:tools` | Target in the workspace root's `BUILD` file |
| `//backend:schema` | Target in `backend/` beneath the workspace root |

Use labels anywhere target references are accepted, including `base`, `add()`, `dep()`, and index mappings:

```python
task(
    name = "generate",
    base = "tools",
    steps = [
        add("//backend:schema", "/workspace"),
        run(["pnpm", "graphql-codegen"]),
    ],
    source = "/workspace/src/generated",
    include = ["**/*.ts"],
)
```

Label paths must be normalized relative paths without symbolic links.
Globs evaluate paths relative to their declaring package directory.
Vamos loads only the packages needed by the requested build targets and reports dependency cycles as errors.

### Naming a workspace

The `WORKSPACE` file is a Starlark file, which Vamos evaluates whenever a command opens the workspace,
before it loads any `BUILD` file.
An empty `WORKSPACE` file is valid, as is a workspace without one, and either leaves the workspace unnamed.
Only the `WORKSPACE` file at the root of the workspace that holds `-C` is evaluated;
one in a directory beneath it marks another workspace, which Vamos doesn't read.
A `WORKSPACE` that isn't a regular file, such as a directory, marks the root without being evaluated.
A `WORKSPACE` file written for another tool, such as Bazel, is evaluated too,
and fails on what Vamos doesn't offer, such as `load()`;
give Vamos a root of its own, with an empty `WORKSPACE` file, or run it where no such file is above `-C`.

`WORKSPACE` may call `workspace()` once, to give the workspace a symbolic name:

```python
workspace(name = "com.acme.storefront")
```

`name` is given by keyword, and is a string.
`WORKSPACE` offers no other builtin of `BUILD` files, and no `load()`: calling one is a mistake.
A workspace name follows the rules of a Bazel module name:
it consists of lowercase letters, digits, `.`, `-` and `_`,
starts with a letter, and ends with a letter or digit.
A mistake in `WORKSPACE`, such as a name that breaks these rules, fails every command that opens the workspace,
`vamos update` included, naming the file, line and column, as a mistake in a `BUILD` file does.

Every request that a command sends to an executor, to build, push, publish or export, carries the name,
or none for an unnamed workspace, as metadata that changes nothing the executor builds.
The name chooses the workspace's [tool caches](build/tool-cache.md),
which `vamos build` keeps by the name and each cache's label,
so only a named workspace can mount them:
a target that mounts one fails to load in an unnamed workspace.
Vamos derives no name from anything else: not the workspace's directory, its repository or the repository's remotes.
A workspace that doesn't call `workspace()` is unnamed.
A name is trusted as declared, so choosing one that no other workspace uses is up to the workspace;
a reverse-DNS name of a domain you control, such as `com.acme.storefront`, is one.

Vamos never adds the name to an action's key, the graph or a step,
so identical targets in workspaces named differently, or unnamed, share cached results,
and renaming a workspace rebuilds nothing, though its targets then mount the tool caches of the new name.
A glob that selects the `WORKSPACE` file, such as `**` in the root package, builds on its contents
as on any other file's, the name included,
so renaming the workspace rebuilds the targets that use that glob.

### Cross-package dependencies

Packages can reference each other's targets, declared anywhere in either package.
Running a `BUILD` file only declares its targets:
each call checks its shape, such as the names of its arguments and the target's own,
and keeps its arguments.
A target is evaluated when a command needs it,
because the command names it or a target being configured references it:
its arguments are read, the targets they name are found, and the packages declaring those are loaded.
So a reference can name a target declared above or below it, in its own package or another,
and whether a target builds doesn't depend on the order targets are declared in or `BUILD` files run in.
A mistake in a target is reported when a command builds it, at the call that makes it, which may be in a `.star` file,
and a target that no command needs is checked only for its shape,
but by a full [`vamos update`](cli/update.md#workspace-discovery),
which checks the rule of every target of the packages it walks, and of the targets those use as given.

The values passed to a rule that declares a target, and to `dep()`, are frozen as the call is made,
with every list and dictionary they hold,
so that the target is built from the values the call was given:
changing one afterwards fails, at the change, with Starlark's error for a frozen value.
A function of `ctx` gives a whole argument, never a value within a list or dictionary,
and isn't frozen; it may run more than once for a target, so it should give the same result each time.

Targets that use each other in a cycle fail as they are configured, naming the cycle,
at the declaration of the target whose reference closes it.

On the command line, unqualified target names resolve against the package selected by `-C`:

```sh
vamos build //backend:test //ui:lint -C project
vamos load app ui:dev -C project/ui
```

## Parameters

All targets other than `pull()` accept a `params` dictionary.
Providing a default value declares both the parameter's type and its default:

```python
params = {
    "mode": "release",
    "verbose": False,
    "workers": 4,
    "languages": ["en"],
}
```

Supported parameter types are strings, booleans, integers, and lists of strings.
Use `param()` to constrain allowed values or require an explicit argument:

```python
params = {
    "mode": param(default = "release", values = ["debug", "release"]),
    "languages": param(default = ["en"], values = ["en", "es", "fr"]),
    "version": param(type = "string"),
}
```

| Argument | Description |
| --- | --- |
| `default` | Default value if omitted; determines the parameter type |
| `values` | Nonempty list of permitted string values |
| `type` | `"string"`, `"bool"`, `"int"`, or `"list"`; required when omitting `default` |

Provide either a `default` or an explicit `type`.
If both are specified, their types must match.
A parameter without a default must be supplied whenever the target is built or referenced.
For list parameters, `values` constrains each individual list element.

### Functions of `ctx`

Target attributes can be defined as functions of `ctx` that receive the target's configuration:

```python
glob(
    name = "translations",
    params = {"languages": ["en"]},
    include = lambda ctx: ["locales/%s" % language for language in ctx.languages],
)
```

Read configured parameters as `ctx.<name>`.
Functions of `ctx` return entire attribute values (such as a full `steps` list or `include` list);
individual list items cannot be functions of `ctx`.
Functions of `ctx` configure existing targets; they cannot declare new targets.
The target's `name` and `params` cannot be functions of `ctx`.
Errors inside `ctx` functions display the target name and parameter values being evaluated.

### `dep()` and `arg()`

`dep(label, platform = ..., **arguments)` references a target with explicit parameter values.
It is accepted wherever a plain target label is permitted:

```python
add(dep("translations", languages = ["en", "es"]), "/app")
base = dep("tools", platform = EXEC_PLATFORM)
```

Omit `platform` to inherit the caller's target platform.

`arg("name")` forwards a parameter from the caller to a dependency:

```python
layer(
    name = "app",
    params = {"mode": "release"},
    base = "scratch",
    steps = [add(dep("compile", mode = arg("mode")), "/")],
    command = ["/app"],
)
```

The downstream target must declare the forwarded parameter name.
Parameters are not forwarded automatically beyond that target.
Plain target labels use the referenced target's default parameter values.

### Command-line overrides

Override parameters from the command line using `--set <name>=<value>`:

```sh
vamos build compile --set mode=debug
vamos build bundle --set languages=en,es --set verbose=true --set workers=8
```

- String parameters accept plain text values.
- Boolean parameters accept lowercase `true` or `false`.
- Integer parameters accept integer values.
- List parameters accept comma-separated strings (for example, `--set languages=en,es`);
  an empty value (`--set languages=`) produces an empty list.
- Parameters with `values` constraints validate each supplied value against the allowed list.

Each `--set` flag applies to all requested top-level targets that declare matching parameter names.
A `--set` flag that matches no requested target produces an error.
Parameters affect cache reuse only when they change configured inputs, environment variables, or commands.

## Platforms

A platform string specifies the operating system, architecture, and optional variant:
`os/arch` (such as `linux/amd64`, `linux/arm64`) or `os/arch/variant` (such as `linux/arm/v7`).
Targets inherit their platform from the requesting command or caller.
Build commands default to the host platform unless overridden by `--platform`:

```sh
vamos build app --platform linux/arm64
```

Functions of `ctx` can inspect built-in platform properties:

| Field | Description | Example |
| --- | --- | --- |
| `ctx.platform` | Full platform string | `"linux/arm64"` |
| `ctx.os` | Target operating system | `"linux"` |
| `ctx.arch` | Target CPU architecture | `"arm64"` |
| `ctx.variant` | Architecture variant, or an empty string | `"v7"` |
| `ctx.exec_platform` | Build execution platform (matching `EXEC_PLATFORM`) | `"linux/amd64"` |

These field names are reserved and cannot be used as custom parameter names.
Globs are shared across platforms and cannot inspect `ctx.platform`, `ctx.os`, `ctx.arch`, or `ctx.variant`.

### Native tools and cross-compilation

`EXEC_PLATFORM` requests the execution platform of the build engine rather than the target platform:

```python
task(
    name = "compile",
    base = dep("golang", platform = EXEC_PLATFORM),
    steps = lambda ctx: [
        add("source", "/src"),
        workdir("/src"),
        build_env(CGO_ENABLED = "0", GOOS = ctx.os, GOARCH = ctx.arch),
        run(["go", "build", "-o", "/out/app", "."]),
    ],
    source = "/out",
    include = ["app"],
)
```

This pattern runs compiler tools natively on the host architecture while producing binaries for the target platform.
A task using native tools can share a single cached result across target architectures
if its inputs and commands are platform-independent (such as bundled web assets).
When compiler environment variables depend on `ctx.os` or `ctx.arch`,
Vamos generates a separate cached result for each target architecture.
A layer's base image must match the layer's platform; use a task to decouple build tools from the final runtime image.

#### Execution platform selection

The execution platform is resolved before reading `BUILD` files:

1. The platform specified by `--exec-platform`.
2. The coordinator's `--default-exec-platform` when building remotely.
3. The host platform if supported by the executor.
4. The sole platform offered by the executor.

If the executor offers multiple platforms and none matches the host, pass `--exec-platform` explicitly.

When using a remote coordinator with `--default-exec-platform`,
clients on different host architectures share identical cache keys for `EXEC_PLATFORM` targets.

If a `run()` step executes binaries for a foreign architecture without cross-compilation,
the host kernel requires registered QEMU emulators (`binfmt_misc`).
Without registered emulators, foreign binary execution fails with `exec format error`.
On a remote coordinator, targets requiring emulation fail before execution unless `--emulate` is enabled
(see [offered platforms and emulation](remote.md#offered-platforms-and-emulation)).

### Index delivery

An [index](build/image-index.md) defines its own platform list, so it cannot be combined with `--platform`.

| Command | Action for index targets |
| --- | --- |
| `vamos build` | Builds all platform images into the build cache |
| `vamos push` | Builds all images and pushes the multi-platform index to the registry |
| `vamos publish` | Builds all images and publishes the index to each repository under every `--tag` |
| `vamos load` | Builds and loads only the image matching the local Docker daemon's platform |
| `vamos save` | Builds and exports only the image matching the host platform into the tarball |

To export or load a specific foreign platform from an index,
reference the underlying pull or layer target directly and pass `--platform`.

## Shared `.star` files

Use the Starlark `load()` statement to share functions and constants across `BUILD` files:

```python
load("//:defs.star", "go_checks")

go_checks(name = "test", base = "tools", source = "source")
```

For example, `defs.star` can define reusable target macros:

```python
def go_checks(name, base, source):
    task(
        name = name,
        base = base,
        steps = [
            add(source, "/workspace"),
            workdir("/workspace"),
            run(["go", "test", "./..."]),
        ],
    )
```

- Module labels begin with `//` and reference a `.star` file relative to the workspace root.
- Modules cannot declare targets at top-level while loading;
  macro functions declare targets when called from a `BUILD` file.
- Labels within macro functions resolve relative to the calling `BUILD` file's package.
- References in functions of `ctx` resolve relative to the target being configured.
- Circular module dependencies produce an error during loading.
