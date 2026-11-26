# Spec Delta

## MODIFIED Requirements

### Requirement: Attribution compares with the nearest entry

A miss that isn't `forced`, `new` or `lost` SHALL be compared with the entry, of the same version of what an action
covers, that differs from it least, counting one for each of: its kind, a step, the command, the entrypoint, the output
directory, the output selection, a source file added, removed or changed, a changed source kept by digest, the base, by
supplier or content, an add whose supplier differs, an added target's files or epoch, and the caches it mounts, as one.
Of equals, the most recent SHALL be compared.

#### Scenario: The nearest entry is compared, not the latest

- **GIVEN** a named workspace that has built `app` with `a.txt` holding `one` and `b.txt` holding `one`, then with both
  holding `two`
- **WHEN** `a.txt` holds `one` and `b.txt` holds `three`
- **AND** I build `app`
- **THEN** its span says the miss cause `own-inputs`, with `b.txt` alone changed

### Requirement: A change of the target's own inputs is own-inputs

A miss whose nearest entry differs in its kind, a step, the command, the entrypoint, the output directory or selection,
a source's files, which target supplies its base or files it adds, or the caches it mounts, where, or with which owner
and group, SHALL have the cause `own-inputs`, whatever else differs. Where a source's files differ, the span SHALL name
those added, removed and changed, by path, up to 100 of each with the count of all, or say a source changed if history
couldn't keep its files or, once any source can't be read, doesn't hold them.

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

#### Scenario: A moved cache mount is own-inputs

- **GIVEN** a named workspace whose task `build` mounts a cache at `/cache`, and that has built `build`
- **WHEN** it mounts the cache at `/other` instead, and I build `build`
- **THEN** its span says the miss cause `own-inputs` and names no file
