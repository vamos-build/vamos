# Spec Delta

## MODIFIED Requirements

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
