# target-references Specification

## Purpose

Says how BUILD files declare targets and when their rules are evaluated, so that a reference can name any target of
the workspace wherever it is declared, and whether a target builds never depends on the order targets are declared in
or BUILD files run in.

## Requirements

### Requirement: Running a BUILD file declares its targets without evaluating them

Running a BUILD file SHALL declare each target its rule calls name, checking only each call's shape: that its keywords
are the rule's, its required arguments are given, its name is a string unique in the package, its `params` are valid,
and a function of `ctx` gives a whole argument, never a value within one. It SHALL NOT otherwise read the arguments'
values, resolve references or load other packages. A `tool_cache()` call,
which declares no target, SHALL still be checked in full as it is made.

#### Scenario: A missing required argument of an unused target

- **GIVEN** a BUILD file declaring `layer(name="app", base="scratch")` and `layer(name="unused")`
- **WHEN** I build `app`
- **THEN** it fails at the second `layer()` call, saying `base` is missing

#### Scenario: A duplicate name

- **GIVEN** a BUILD file declaring two targets named `app`
- **WHEN** I build `app`
- **THEN** it fails at the second call, saying `app` is already declared

#### Scenario: An unused target's values are not read

- **GIVEN** a BUILD file declaring `layer(name="app", base="scratch")` and
  `layer(name="unused", base="//missing:nothing", command=42)`
- **WHEN** I build `app`
- **THEN** it succeeds, and no package `missing` is loaded

### Requirement: A rule is evaluated when its target is configured

A target's rule SHALL be evaluated when its target is configured, because the command names it or a target being
configured references it, with that configuration. Evaluating it SHALL read its values, resolve its references and
load the packages they name. A rule with no function of `ctx` SHALL be evaluated once, and its mistakes reported
without the configuration.

#### Scenario: A workspace whose packages name each other's later targets builds from every entry point

- **GIVEN** a root BUILD file pulling `base` and declaring `layer(name="app", base="//app:image")`
- **AND** `app/BUILD` declaring `layer(name="image", base="//:base", steps=[add("//lib:files", "/app")])`
- **AND** `lib/BUILD` declaring `layer(name="tools", base="//:base")` and then `glob(name="files", include=["src"])`
- **WHEN** I build `//:app`, and separately `//lib:tools`
- **THEN** both succeed

#### Scenario: A mistake in a target is reported when it is built

- **GIVEN** a root BUILD file declaring `layer(name="app", base="//lib:base")`
- **AND** `lib/BUILD` declaring `layer(name="base", base="scratch")` and `layer(name="other", base="//:missing")`
- **WHEN** I build `//:app`, and then `//lib:other`
- **THEN** the first succeeds, and the second fails at `other`'s `layer()` call, naming `"//:missing"`

#### Scenario: A mistake in a target used in two configurations

- **GIVEN** a layer `tool` whose `command` is `42`, used by `dep("tool", platform="linux/amd64")` and
  `dep("tool", platform="linux/arm64")` from a target `app`
- **WHEN** I build `app`
- **THEN** it fails at `tool`'s `layer()` call once, without naming a configuration

#### Scenario: A function of ctx names a package loaded only for it

- **GIVEN** a root BUILD file declaring `layer(name="app", base=lambda ctx: "//scala:x")`
- **AND** `scala/BUILD` declaring `layer(name="x", base="scratch")`
- **WHEN** I build `app`
- **THEN** it succeeds, building `//scala:x` and `//:app`

### Requirement: A reference names a target declared anywhere

A target reference SHALL resolve to the target its label names whether that target is declared above or below it, in
the same BUILD file or another package's. This SHALL hold for a `base`, an `add()` source, a `dep()`, an `index()`
manifest, a publication's checks and images, and a `mount()` in `tool_caches`, made in the BUILD file, in a `.star`
function it calls, or by a function of `ctx`.

#### Scenario: A base declared below in the same file

- **GIVEN** a BUILD file declaring `layer(name="app", base="deps")` and then `layer(name="deps", base="busybox")`
- **WHEN** I build `app`
- **THEN** it succeeds, building `deps` before `app`

#### Scenario: An add() source declared below in the same file

- **GIVEN** a BUILD file declaring a layer `app` with `steps=[add("code", "/app")]` and then
  `glob(name="code", include=["app.sh"])`
- **WHEN** I save `app`
- **THEN** it succeeds and the image holds `/app/app.sh`

#### Scenario: A dep() with arguments of a target declared below

- **GIVEN** a BUILD file declaring a layer `app` with `base=dep("deps", flavor="slim")` and then a layer `deps` with
  a parameter `flavor`
- **WHEN** I build `app`
- **THEN** it succeeds, configuring `deps` with `flavor` set to `"slim"`

#### Scenario: An add() of a dep() passing on a parameter, to a target declared below

- **GIVEN** a BUILD file declaring a layer `app` with `params={"mode": "release"}` and
  `steps=[add(dep("mode", mode=arg("mode")), "/app")]`
- **AND** then a task `mode` with a parameter `mode`, whose output is a file holding its mode
- **WHEN** I save `app`, and separately `app` with `--set mode=debug`
- **THEN** both succeed, the first image holding `release` under `/app` and the second `debug`

#### Scenario: A tool cache declared below

- **GIVEN** a BUILD file declaring a task that mounts `mount("go-build")` and then `tool_cache(name="go-build", ...)`
- **WHEN** I build the task
- **THEN** it succeeds with the cache mounted at its path

#### Scenario: A package names a target that the package naming it declares below

- **GIVEN** a root BUILD file declaring `layer(name="app", base="//scala:x")` and then
  `layer(name="y", base="scratch")`
- **AND** `scala/BUILD` declaring `layer(name="x", base="//:y")`
- **WHEN** I build `app`
- **THEN** it succeeds, building `//:y`, `//scala:x` and `//:app`

### Requirement: A reference's mistake is reported where it is made

A reference whose label names no target of its package, or a target of a kind it can't use, SHALL fail with an error
giving the file, line and column of the call making it: the rule's call for a `base`, `dep()`, manifest, check or
mount, and the `add()` call for its source, at the innermost BUILD or `.star` frame. A missing target SHALL be reported
with the names it could be, in any package.

#### Scenario: A misspelled base in the same file

- **GIVEN** a BUILD file pulling `busybox` on line 1 and declaring `layer(name="app", base="busybx")` on line 2
- **WHEN** I build `app`
- **THEN** it fails with `/project/BUILD:2:6: layer: base "busybx": /project/BUILD has no target "busybx"; did you mean
  "busybox"?`, and no target ran

#### Scenario: A misspelled add() source in another package

- **GIVEN** `app/BUILD` declaring a layer whose steps `add("//lib:code", "/app")` at column 45 of line 1
- **AND** `lib/BUILD` declaring `glob(name="cod", include=["src"])`
- **WHEN** I build `//app:app`
- **THEN** it fails naming `/project/app/BUILD:1:45`, `add: source "//lib:code"` and suggesting `"cod"`

#### Scenario: A reference of the wrong kind declared below

- **GIVEN** a BUILD file declaring a layer `app` with `steps=[add("img", "/app")]` and then a layer `img`
- **WHEN** I build `app`
- **THEN** it fails at the `add()` call saying that `"img"` is a layer(), not a glob() or task()

#### Scenario: A reference made in a .star function

- **GIVEN** a `.star` file whose function calls `add("//lib:nope", "/x")`, called from a BUILD file's layer `app`
- **WHEN** I build `app`
- **THEN** it fails at the `add()` call's line and column in the `.star` file

### Requirement: Targets whose references form a cycle fail as they are configured

Targets whose references form a cycle SHALL fail the command configuring them, naming every label of the cycle in
order, at the declaration of the target whose reference closes it, naming that reference, such as `add: source "b"`,
whether the references are given as the BUILD file runs or by functions of `ctx`. A label repeated only through a
target configured already need not be found.

#### Scenario: A target that is its own base

- **GIVEN** a BUILD file declaring `layer(name="app", base="app")`
- **WHEN** I build `app`
- **THEN** it fails at the `layer()` call with `targets use each other in a cycle: app -> app`

#### Scenario: Targets of two packages that name each other in a cycle fail

- **GIVEN** a root BUILD file declaring `layer(name="app", base="//lib:base")`
- **AND** `lib/BUILD` declaring `layer(name="base", base="//:app")`
- **WHEN** I build `app`
- **THEN** it fails with `targets use each other in a cycle: app -> //lib:base -> app`, at `//lib:base`'s `layer()` call,
  naming its `base "//:app"`

#### Scenario: Targets of three packages that use each other in a cycle fail

- **GIVEN** packages `.`, `b` and `c` whose layers `app`, `b` and `c` take as base `//b:b`, `//c:c` and `//:app`
- **WHEN** I build `app`
- **THEN** it fails with `targets use each other in a cycle: app -> //b:b -> //c:c -> app`, at `//c:c`'s `layer()`
  call, naming its `base "//:app"`

### Requirement: A rule's arguments are frozen when it is called

The values passed to a rule call SHALL be frozen as the call is made, with every list and dict they hold, so that the
target is evaluated from the values the call was given. Changing one afterwards SHALL fail at the change, with
Starlark's error for a frozen value. A function of `ctx` given as an argument, and the variables it captures, SHALL be
left as they are. The check that every `run()` step is in some target's `steps` SHALL read the same
values.

#### Scenario: Appending to a list after passing it as steps

- **GIVEN** a BUILD file with `s = run(["true"])`, `st = []`,
  `layer(name="x", base="scratch", command=lambda ctx: [], steps=st)` and then `st.append(s)` on line 4
- **WHEN** I run `vamos graph x`
- **THEN** it fails at line 4, saying the list is frozen

#### Scenario: Removing from a list after passing it as steps

- **GIVEN** a BUILD file with `s = run(["true"])`, `st = [s]`, a layer `x` given `steps=st`, and then `st.pop()`
- **WHEN** I run `vamos graph x`
- **THEN** it fails at the `pop()`, saying the list is frozen

#### Scenario: A function within an argument

- **GIVEN** a BUILD file with `st = [print, run(["true"])]` and a layer `x` given `steps=st`
- **WHEN** I run `vamos graph x`
- **THEN** it fails at the `layer()` call, saying `steps[0]` is a function and that a function of `ctx` must give the
  whole `steps`

#### Scenario: A list shared by two targets

- **GIVEN** a BUILD file with `common = [run(["true"])]`, a layer `a` given `steps=common` and a layer `b` given
  `steps=common + [run(["echo", "b"])]`
- **WHEN** I build `b`
- **THEN** it succeeds, `b` running both steps
