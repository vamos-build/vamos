# Spec Delta

## Purpose

Says why each target that wasn't reused had to build, comparing it with the builds of the same target before it, so
that a glob that selects too much, a moved tag or a forced rebuild can be told apart from an ordinary change.

## ADDED Requirements

### Requirement: Every miss has one cause

Every layer or task that a build runs, or joins another build's attempt for, rather than reuses SHALL have exactly one
cause, which its span in the trace SHALL say: `forced`, `new`, `lost`, `own-inputs`, `upstream` or `dependency`.
A reused target SHALL have none.

#### Scenario: A reused target has no cause

- **GIVEN** a named workspace that has built `app`
- **WHEN** I build `app` again
- **THEN** `app` was reused, and its span in the trace says no miss cause

### Requirement: A forced build is forced

A target that runs because `--rebuild` named it SHALL have the cause `forced`, whatever else changed.

#### Scenario: --rebuild forces a target

- **GIVEN** a named workspace that has built `app`
- **WHEN** I build `app` again with `--rebuild`
- **THEN** `app` was rebuilt, and its span says the miss cause `forced`

### Requirement: Target history

An executor SHALL keep, per target of a named workspace, the last 8 distinct actions its builds used, reused or made,
most recent first, or fewer where 8 would pass the history's size cap. A target SHALL be identified by the workspace's
name, its label, its platform and all its parameters' values. A failed or cancelled target SHALL add nothing. Builds of
one target on one executor at once SHALL each add their action, and none SHALL drop another's.

#### Scenario: Two branches' builds don't look like changes

- **GIVEN** a named workspace that has built `app` with its source `a.txt` holding `one`, then holding `two`
- **WHEN** `a.txt` holds `one` again, and the build cache is pruned
- **AND** I build `app`
- **THEN** `app` ran, and its span says the miss cause `lost`

#### Scenario: Concurrent builds each add their action

- **GIVEN** a coordinator, on which two builds of a named workspace's `app` ran at once, with its source holding
  `one` in the first and `two` in the second
- **AND** the coordinator's cache pruned since
- **WHEN** I build `app` on the coordinator with its source holding `one`, and again with it holding `two`
- **THEN** both ran, and `app`'s span says the miss cause `lost` in each

### Requirement: The workspace name keys target history

Target history SHALL be kept apart by workspace name: builds of identically labeled targets in differently named
workspaces SHALL NOT compare with each other's. An unnamed workspace SHALL keep no history: its misses SHALL be
`forced`, `lost` where the result found is unusable, or else `new`.

#### Scenario: Another workspace's history is not compared

- **GIVEN** a workspace named `com.acme.first` that has built `app`
- **WHEN** a workspace named `com.acme.second`, whose `app` adds another file, builds `app`
- **THEN** `app` ran, and its span says the miss cause `new`

#### Scenario: An unnamed workspace's misses are new

- **GIVEN** an unnamed workspace that has built `app`
- **WHEN** a step of `app` changes, and I build `app`
- **THEN** `app` ran, and its span says the miss cause `new`

### Requirement: A target without comparable history is new

A target with no history entry, or none recorded by the same version of what an action covers, SHALL have the cause
`new`, unless it is `forced`, or `lost` for a result found that can't be used.

#### Scenario: A target's first build is new

- **GIVEN** a named workspace that has never built `app`
- **WHEN** I build `app`
- **THEN** its span says the miss cause `new`

#### Scenario: A target in a new configuration is new

- **GIVEN** a named workspace that has built `app` with `mode` set to `debug`
- **WHEN** I build `app` with `mode` set to `release`
- **THEN** its span says the miss cause `new`

### Requirement: A result that is gone is lost

A target whose action is one its history holds, or whose result is found but can't be used, at lookup or when a target
that adds its files finds them unusable and has it run again, SHALL have the cause `lost`, as after a prune or once an
image it names was removed.

#### Scenario: A task whose cached files are corrupt is lost

- **GIVEN** a named workspace whose layer `app` adds the files of the task `gen`, and that has built `app`
- **AND** the cached files of `gen` corrupt
- **WHEN** I build `app` with `--rebuild` naming `app`
- **THEN** `gen` ran again, and its span says the miss cause `lost`

#### Scenario: A target built again after a prune is lost

- **GIVEN** a named workspace that has built `app`
- **AND** the build cache pruned since
- **WHEN** I build `app` again
- **THEN** `app` ran, and its span says the miss cause `lost`

### Requirement: Attribution compares with the nearest entry

A miss that isn't `forced`, `new` or `lost` SHALL be compared with the entry, of the same version of what an action
covers, that differs from it least: whether it is a layer or a task, each step, the command, the entrypoint, the output
directory, the output selection, each source file added, removed or changed, the base, and each target's files or their
epoch it adds count one difference. Among entries that differ equally, the most recent SHALL be compared.

#### Scenario: The nearest entry is compared, not the latest

- **GIVEN** a named workspace that has built `app` with `a.txt` holding `one` and `b.txt` holding `one`, then with both
  holding `two`
- **WHEN** `a.txt` holds `one` and `b.txt` holds `three`
- **AND** I build `app`
- **THEN** its span says the miss cause `own-inputs`, with `b.txt` alone changed

### Requirement: A change of the target's own inputs is own-inputs

A miss whose nearest entry differs in a step, the command, the entrypoint, the output directory or selection, a
source's files, or which target supplies its base or files it adds, SHALL have the cause `own-inputs`, whatever else
differs. Where a source's files differ, the span SHALL name the files added, removed and changed, by path, unless the
source was too large for history to keep its files, when it SHALL say the source changed.

#### Scenario: A layer that became a task is own-inputs

- **GIVEN** a named workspace whose layer `app` runs one step, and that has built `app`
- **WHEN** `app` is declared as a task with the same base and step, and I build `app`
- **THEN** its span says the miss cause `own-inputs`

#### Scenario: A changed output directory is own-inputs

- **GIVEN** a named workspace whose task `gen` takes its output from `/one`, and that has built `gen`
- **WHEN** `gen` takes its output from `/two` instead, and I build `gen`
- **THEN** its span says the miss cause `own-inputs` and names no file

#### Scenario: A base switched to another pull is own-inputs

- **GIVEN** a named workspace whose `app` is based on the pull `one`, and that has built `app`
- **WHEN** `app` is based on the pull `two` instead, and I build `app`
- **THEN** its span says the miss cause `own-inputs`

#### Scenario: A changed step is own-inputs

- **GIVEN** a named workspace that has built `app`
- **WHEN** a `run()` step of `app` changes, and I build `app`
- **THEN** its span says the miss cause `own-inputs` and names no file

#### Scenario: A glob that selects too much shows the files it picked up

- **GIVEN** a named workspace whose `app` adds the glob `src`, selecting `src/**`, and that has built `app`
- **WHEN** the editor writes `src/.main.go.swp`, and I build `app`
- **THEN** its span says the miss cause `own-inputs`, with `src/.main.go.swp` added, and no file removed or changed

#### Scenario: Added, removed and changed files are named

- **GIVEN** a named workspace whose `app` adds a glob of `a.txt`, `b.txt` and `c.txt`, and that has built `app`
- **WHEN** `a.txt` changes, `b.txt` is removed and `d.txt` is added, and I build `app`
- **THEN** its span says the miss cause `own-inputs`, with `d.txt` added, `b.txt` removed and `a.txt` changed

#### Scenario: A change of mode alone is a change

- **GIVEN** a named workspace whose `app` adds `run.sh`, and that has built `app`
- **WHEN** `run.sh` is made executable, and I build `app`
- **THEN** its span says the miss cause `own-inputs`, with `run.sh` changed

### Requirement: A moved pull is upstream

A miss whose nearest entry differs only in the image that the pull which is its base names, the same pull of the same
image reference, its own inputs alike, SHALL have the cause `upstream`, and its span SHALL name the pull and the
manifest digests before and after.

#### Scenario: A tag that moved, updated with vamos update

- **GIVEN** a named workspace whose `app` is based on the pinned pull `base` of a tag, and that has built `app`
- **WHEN** the tag is moved to another image, I run `vamos update base`, and I build `app`
- **THEN** its span says the miss cause `upstream`, naming `base` and both digests

#### Scenario: An updated pin

- **GIVEN** a named workspace whose `app` is based on a pinned pull `base`, and that has built `app`
- **WHEN** `vamos.lock` pins `base` to another digest, and I build `app`
- **THEN** its span says the miss cause `upstream`

### Requirement: A change only in what a dependency supplies is dependency

A miss whose nearest entry differs only in what a target it depends on supplies, the image of its base layer, the
image reference its base pull declares, or the files or epoch of a task it adds, and not in its own inputs or as
`upstream`, SHALL have the cause `dependency`, and its span SHALL name each such target. That target's own cause SHALL
be its own, whether it hit or missed.

#### Scenario: A layer on a changed layer

- **GIVEN** a named workspace whose layer `app` is based on the layer `runtime`, and that has built `app`
- **WHEN** a step of `runtime` changes, and I build `app`
- **THEN** `runtime`'s span says the miss cause `own-inputs`, and `app`'s says `dependency`, naming `runtime`

#### Scenario: A pull whose tag was edited

- **GIVEN** a named workspace whose `app` is based on the pull `base` of `busybox:1.36`, and that has built `app`
- **WHEN** `base` is changed to pull `busybox:1.37`, and I build `app`
- **THEN** its span says the miss cause `dependency`, naming `base`

#### Scenario: A rebuilt task changes its epoch alone

- **GIVEN** a named workspace whose layer `app` adds the files of the task `gen`, and that has built `app`
- **WHEN** I build `gen` with `--rebuild` and a later `--timestamp`, which makes the same files stamped later
- **AND** I build `app`
- **THEN** `app`'s span says the miss cause `dependency`, naming `gen`

#### Scenario: A base switched to an identical layer, then changed

- **GIVEN** a named workspace whose layer `app` is based on the layer `one`, and that has built `app`
- **AND** `app` based instead on the layer `two`, which makes the same image, and built, reusing its result
- **WHEN** a step of `two` changes, and I build `app`
- **THEN** its span says the miss cause `dependency`, naming `two`

#### Scenario: A moved pull and a changed task together are dependency

- **GIVEN** a named workspace whose `app` is based on the pinned pull `base` of a tag, and adds the files of the task
  `gen`, and that has built `app`
- **WHEN** `vamos.lock` pins `base` to another digest, a step of `gen` changes, and I build `app`
- **THEN** `app`'s span says the miss cause `dependency`, naming `base` and `gen`

### Requirement: History survives a prune

Target history SHALL hold inline everything attribution compares, referring to no content blob. A prune SHALL clear the
history of no target built or reused in the 90 days before it, so that a miss after a prune whose action changed is
still attributed to what changed, and SHALL remove the history of targets neither built nor reused in the 120 days
before it.

#### Scenario: History of a target neither built nor reused for 120 days goes at a prune

- **GIVEN** a named workspace that built `app` more than 120 days ago, and has neither built nor reused it since
- **AND** the build cache pruned since
- **WHEN** I build `app`
- **THEN** its span says the miss cause `new`

#### Scenario: History of a target reused within 90 days stays at a prune

- **GIVEN** a named workspace whose `app` adds a glob, and that built `app` more than 120 days ago and last reused it 30
  days ago
- **AND** the build cache pruned since
- **WHEN** a file of the glob changes, and I build `app`
- **THEN** its span says the miss cause `own-inputs`, naming the file changed

#### Scenario: A changed source after a prune is still own-inputs

- **GIVEN** a named workspace whose `app` adds a glob, and that has built `app`
- **AND** the build cache pruned since
- **WHEN** a file of the glob changes, and I build `app`
- **THEN** its span says the miss cause `own-inputs`, naming the file changed
