# workspace-identity Specification

## Purpose

Lets a workspace declare a stable symbolic name in its `WORKSPACE` file, which every request to an executor carries,
so that a shared coordinator can tell workspaces apart without the name ever changing what is built or cached.

## Requirements

### Requirement: WORKSPACE is a Starlark file that vamos evaluates

vamos SHALL evaluate the `WORKSPACE` file at the workspace's root as Starlark whenever a command opens the workspace,
before it loads any BUILD file. An empty `WORKSPACE`, and a workspace with no `WORKSPACE` file, SHALL remain valid and
unnamed. Only the `WORKSPACE` file of the workspace holding `-C` SHALL be evaluated.

#### Scenario: An empty WORKSPACE is valid

- **GIVEN** a workspace whose `WORKSPACE` file is empty
- **WHEN** I build `app`
- **THEN** it succeeds

#### Scenario: A workspace without a WORKSPACE file is valid

- **GIVEN** a project with a BUILD file and no `WORKSPACE` file
- **WHEN** I build `app`
- **THEN** it succeeds

#### Scenario: A nested workspace's WORKSPACE is not evaluated

- **GIVEN** a workspace whose `WORKSPACE` names it `com.acme.outer`
- **AND** a directory `inner` beneath it whose own `WORKSPACE` file is not valid Starlark
- **WHEN** I build `app` in the outer workspace's root
- **THEN** it succeeds

#### Scenario: A mistake in WORKSPACE fails a command that builds nothing it names

- **GIVEN** a `WORKSPACE` whose second line is not valid Starlark
- **WHEN** I run `vamos update`
- **THEN** it fails naming `WORKSPACE` with line 2 and its column, and the lockfile is unchanged

### Requirement: WORKSPACE declares the name with workspace()

`WORKSPACE` MAY call `workspace(name = "...")` at most once, which SHALL name the workspace. `name` SHALL be given by
keyword and be a string. `WORKSPACE` SHALL offer no other builtin of BUILD files and no `load()`: calling one SHALL be
a mistake.

#### Scenario: workspace() names the workspace

- **GIVEN** a `WORKSPACE` holding `workspace(name = "com.acme.storefront")`
- **AND** a coordinator
- **WHEN** I build `app` on the coordinator
- **THEN** it succeeds and the coordinator was told the workspace is named `com.acme.storefront`

#### Scenario: Calling workspace() twice is a mistake

- **GIVEN** a `WORKSPACE` calling `workspace(name = "a")` on line 1 and `workspace(name = "b")` on line 2
- **WHEN** I build `app`
- **THEN** it fails naming `WORKSPACE` with line 2 and its column, and saying the workspace is already named `a`,
  and no target ran

#### Scenario: A BUILD file's builtin is a mistake in WORKSPACE

- **GIVEN** a `WORKSPACE` whose first line declares `pull(name = "busybox", image = "busybox")`
- **WHEN** I build `app`
- **THEN** it fails naming `WORKSPACE` with line 1 and its column, and `pull`, and no target ran

#### Scenario: A name that isn't a string is a mistake

- **GIVEN** a `WORKSPACE` holding `workspace(name = 1)`
- **WHEN** I build `app`
- **THEN** it fails naming `WORKSPACE` with line 1 and its column, and saying the name must be a string,
  and no target ran

### Requirement: A workspace name follows the rules of a module name

A name SHALL consist of lowercase ASCII letters, digits, `.`, `-` and `_`, start with a letter, and end with a letter
or digit. A name that breaks these rules SHALL fail every command that opens the workspace, naming the file, line and
column of the call, the name, and the rule it breaks.

#### Scenario: Valid names

- **GIVEN** a `WORKSPACE` holding `workspace(name = "<name>")`, for each of `storefront`, `com.acme.storefront`,
  `my-app_2` and `a`
- **WHEN** I build `app`
- **THEN** it succeeds

#### Scenario: Invalid names are refused where they are declared

- **GIVEN** a `WORKSPACE` holding `workspace(name = "<name>")`, for each of `""`, `Storefront`, `2app`, `app-`,
  `app.` and `acme/app`
- **WHEN** I build `app`
- **THEN** it fails naming `WORKSPACE` with line 1 and its column, the name and the rule it breaks,
  and no target ran

### Requirement: vamos derives no workspace name

A workspace whose `WORKSPACE` file does not call `workspace()`, or that has none, SHALL be unnamed. vamos SHALL NOT
derive a name from the workspace's directory, its repository or the repository's remotes.

#### Scenario: An undeclared name is sent as none

- **GIVEN** a workspace in a directory named `storefront` whose `WORKSPACE` file is empty
- **AND** a coordinator
- **WHEN** I build `app` on the coordinator
- **THEN** it succeeds and the coordinator was told no workspace name

### Requirement: The name travels with every request to an executor

Every graph, push, publish and export request that a command sends to an executor SHALL carry the workspace's name,
or none for an unnamed workspace. The name SHALL be trusted as declared: an executor SHALL NOT refuse a request for
its name being in use by another workspace. A request with no name, as an older client sends, SHALL be accepted as
from an unnamed workspace.

#### Scenario: A push carries the name

- **GIVEN** a `WORKSPACE` holding `workspace(name = "com.acme.storefront")`
- **AND** a coordinator and a registry
- **WHEN** I push `app` to the registry, building on the coordinator
- **THEN** it succeeds and the coordinator was told, with the build and with the push, that the workspace is named
  `com.acme.storefront`

#### Scenario: A publish carries the name

- **GIVEN** a `WORKSPACE` holding `workspace(name = "com.acme.storefront")`
- **AND** a coordinator and a registry
- **WHEN** I publish `release`, whose image goes to the registry, building on the coordinator
- **THEN** it succeeds and the coordinator was told, with the build and with the publish, that the workspace is named
  `com.acme.storefront`

#### Scenario: An export carries the name

- **GIVEN** a `WORKSPACE` holding `workspace(name = "com.acme.storefront")`
- **AND** a coordinator
- **WHEN** I save `app`, building on the coordinator
- **THEN** it succeeds and the coordinator was told, with the build and with the export, that the workspace is named
  `com.acme.storefront`

#### Scenario: Two clones send the same name

- **GIVEN** two copies of a workspace whose `WORKSPACE` holds `workspace(name = "com.acme.storefront")`,
  in different directories
- **AND** a coordinator
- **WHEN** I build `app` on the coordinator in each
- **THEN** both succeed and the coordinator was told the same name by both

### Requirement: The name never affects what is built or cached

vamos SHALL NOT put the workspace name into any action's key, the graph or any step of its own accord. Identical targets
in workspaces named differently, or unnamed, SHALL share cached results. A glob that selects the `WORKSPACE` file SHALL
treat it as it does any other file it selects: its contents, the name included, are part of the targets that use the
glob, and a step given them sees them. Whether a workspace has a valid name SHALL affect what it can build only for
targets that mount a tool cache: such a target SHALL fail to load in an unnamed workspace, and a standalone build of a
graph that mounts one SHALL fail before anything runs if its request's name is malformed, or is empty without
`NoToolCaches`, as `tool-caches` requires. A step that mounts a tool cache MAY see where the executor keeps it, which a
digest of the name is part of, as it may see other paths of the executor; the name itself SHALL NOT be part of it.

#### Scenario: Renaming a workspace reuses its cache

- **GIVEN** a workspace named `com.acme.first` that has built `app`, which no glob that selects `WORKSPACE` reaches
- **WHEN** its `WORKSPACE` is changed to name it `com.acme.second`
- **AND** I build `app` again
- **THEN** it succeeds and `app` was reused

#### Scenario: A step does not see the name

- **GIVEN** a workspace named `com.acme.storefront` whose task `probe` reports its environment
- **WHEN** I build `probe`
- **THEN** no variable the step observed holds `com.acme.storefront`

#### Scenario: A glob that selects WORKSPACE builds on its contents

- **GIVEN** a workspace named `com.acme.first` whose root package's `app` adds a glob selecting `WORKSPACE`,
  and that has built `app`
- **WHEN** its `WORKSPACE` is changed to name it `com.acme.second`
- **AND** I build `app` again
- **THEN** it succeeds and `app` ran

#### Scenario: Only a named workspace builds a target that mounts a cache

- **GIVEN** a workspace whose `WORKSPACE` file is empty, and a task `build` that mounts a cache
- **WHEN** I build `build`
- **THEN** it fails before anything runs, naming `build`'s declaration and saying that the workspace needs a name
  in `WORKSPACE`
