package features

import (
	"fmt"
	"testing"

	"github.com/vamos-build/vamos/internal/testenv"
)

// newBuildFileWorld starts a scenario of the BUILD file feature, from its background.
// A BUILD file is a Starlark program that declares named targets, which Vamos loads before anything runs.
// Every scenario's file holds a mistake, which loading catches, so it needs no buildah,
// and saves its layer, to see that the mistake leaves nothing where it would have saved.
// Its project pulls busybox.
func newBuildFileWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	w.GivenAProjectThatPullsBusybox()
	return w
}

// theCodeGlob declares the glob "code" of app.sh, which theCode holds, for a scenario whose layer adds it.
const theCodeGlob = `glob(name="code", include=["app.sh"])`

// theCode is the project file that theCodeGlob selects.
var theCode = files{"app.sh": "echo hi"}

// TestBuildFile_MistakesFailAsItLoads specifies that a mistake in how a BUILD file declares a target that a command
// uses, such as a misspelled argument or a value of the wrong type, fails the command as it loads,
// before anything runs,
// with an error that names the file, line and column of the call that made it.
func TestBuildFile_MistakesFailAsItLoads(t *testing.T) {
	parallelRule(t)
	for _, example := range []struct {
		name  string
		build string // after the pull of busybox and theCodeGlob, on line 3
		error string
	}{
		{
			name:  "A syntax error is reported where the parser finds it",
			build: "layer(\n    name=\"app\",\n    base=\"busybox\",\n    command=[\"echo\", \"hi\"\n)",
			error: "/project/BUILD:7:2: got ')', want ']'",
		},
		{
			name:  "A misspelled function is undefined",
			build: `bulid(name="app", base="busybox")`,
			error: "/project/BUILD:3:1: undefined: bulid",
		},
		{
			name:  "A misspelled argument is unexpected",
			build: `layer(name="app", base="busybox", comand=["echo"])`,
			error: `/project/BUILD:3:6: layer: unexpected keyword argument "comand"`,
		},
		{
			name:  "A misspelled argument is unexpected beside a function of ctx",
			build: `layer(name="app", base="busybox", comand=lambda ctx: [])`,
			error: `/project/BUILD:3:6: layer: unexpected keyword argument "comand"`,
		},
		{
			name:  "An argument that layer() no longer takes is unexpected",
			build: `layer(name="app", base="busybox", sources=["code"], destination="/app")`,
			error: `/project/BUILD:3:6: layer: unexpected keyword argument "sources"`,
		},
		{
			name:  "A task takes no command",
			build: `task(name="app", base="busybox", command=["make"], source="/out", include=["app"])`,
			error: `/project/BUILD:3:5: task: unexpected keyword argument "command"`,
		},
		{
			name:  "A target without a name fails",
			build: `layer(base="busybox")`,
			error: "/project/BUILD:3:6: layer: missing argument for name",
		},
		{
			name:  "A layer without a base fails",
			build: `layer(name="app", command=["echo"])`,
			error: "/project/BUILD:3:6: layer: missing argument for base",
		},
		{
			name:  "An add() without a destination fails",
			build: `layer(name="app", base="busybox", steps=[add("code")])`,
			error: "/project/BUILD:3:45: add: missing argument for dest",
		},
		{
			name:  "Steps that aren't a list fail",
			build: `layer(name="app", base="busybox", steps=run(["true"]))`,
			error: `/project/BUILD:3:6: layer: for parameter "steps": got step, want list`,
		},
		{
			name:  "Steps in a list within the steps fail, as a list",
			build: `layer(name="app", base="busybox", steps=[[run(["true"])]])`,
			error: "/project/BUILD:3:6: layer: steps[0] is list, want a step, such as add() or run()",
		},
		{
			name:  "Steps in a dict fail, as a dict",
			build: `layer(name="app", base="busybox", steps={"a": run(["true"])})`,
			error: `/project/BUILD:3:6: layer: for parameter "steps": got dict, want list`,
		},
		{
			name:  "A step that isn't a step fails",
			build: `layer(name="app", base="busybox", steps=[run(["true"]), "make"])`,
			error: "/project/BUILD:3:6: layer: steps[1] is string, want a step, such as add() or run()",
		},
		{
			name:  "A command that isn't a list fails",
			build: `layer(name="app", base="busybox", command="echo")`,
			error: `/project/BUILD:3:6: layer: for parameter "command": got string, want list`,
		},
		{
			name:  "A command of bytes fails",
			build: `layer(name="app", base="busybox", command=b"echo")`,
			error: `/project/BUILD:3:6: layer: for parameter "command": got bytes, want list`,
		},
		{
			name:  "A command whose argument isn't a string fails",
			build: `layer(name="app", base="busybox", command=["sleep", 5])`,
			error: "/project/BUILD:3:6: layer: command[1] is int, want string",
		},
		{
			name:  "An entrypoint whose argument isn't a string fails",
			build: `layer(name="app", base="busybox", entrypoint=["java", 1])`,
			error: "/project/BUILD:3:6: layer: entrypoint[1] is int, want string",
		},
		{
			name:  "An add() source that isn't a name fails",
			build: `layer(name="app", base="busybox", steps=[add(1, "/app")])`,
			error: "/project/BUILD:3:45: add: source is int, want a label or dep()",
		},
		{
			name:  "An add() source that is a list fails",
			build: `layer(name="app", base="busybox", steps=[add(["code"], "/app")])`,
			error: "/project/BUILD:3:45: add: source is list, want a label or dep()",
		},
		{
			name:  "An add() destination that isn't a string fails",
			build: `layer(name="app", base="busybox", steps=[add("code", ["/app"])])`,
			error: "/project/BUILD:3:45: add: for parameter dest: got list, want string",
		},
		{
			name:  "A run() that isn't given a list fails",
			build: `layer(name="app", base="busybox", steps=[run("make")])`,
			error: "/project/BUILD:3:45: run: for parameter argv: got string, want list",
		},
		{
			name:  "A run() argument that isn't a string fails",
			build: `layer(name="app", base="busybox", steps=[run(["sleep", 5])])`,
			error: "/project/BUILD:3:45: run: argv[1] is int, want string",
		},
		{
			name:  "Secrets that aren't secrets fail",
			build: `layer(name="app", base="busybox", steps=[run(["true"], secrets=["t"])])`,
			error: "/project/BUILD:3:45: run: secrets[0] is string, " +
				`want a secret, such as secret("token", env="TOKEN")`,
		},
		{
			name: "An include that isn't a string fails",
			build: "glob(name=\"other\", include=[1])\n" +
				`layer(name="app", base="busybox", steps=[add("other", "/")])`,
			error: "/project/BUILD:3:5: glob: include[0] is int, want string",
		},
		{
			name: "A mode_mask that isn't an int fails",
			build: "glob(name=\"other\", include=[\".\"], mode_mask=\"755\")\n" +
				`layer(name="app", base="busybox", steps=[add("other", "/")])`,
			error: `/project/BUILD:3:5: glob: for parameter "mode_mask": got string, want int`,
		},
		{
			name: "A limit that isn't an int fails",
			build: "glob(name=\"other\", include=[\".\"], max_file_count=True)\n" +
				`layer(name="app", base="busybox", steps=[add("other", "/")])`,
			error: `/project/BUILD:3:5: glob: for parameter "max_file_count": got bool, want int`,
		},
		{
			name:  "fail() fails the command with its message",
			build: `fail("unsupported platform")`,
			error: "/project/BUILD:3:5: fail: unsupported platform",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newBuildFileWorld(t)
			w.GivenTheBuildFileDeclares(theCodeGlob)
			w.GivenTheBuildFileDeclares(example.build)
			w.GivenTheProjectHolds(theCode)
			w.WhenISave("app")
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}

	t.Run("A mistake in a function that the file calls is reported where the function makes it", func(t *testing.T) {
		w := newBuildFileWorld(t)
		w.GivenTheBuildFileDeclares(`
			def app(src):
			    glob(name="code", include=[src])

			app("main.sh")
			layer(name="app", base="busybox", steps=[add("code", "/app")])
		`)
		w.WhenISave("app")
		w.ThenItFailsWith(`/project/BUILD:3:9: glob: include "main.sh" not found in /project`)
		w.ThenNoTargetRan()
		w.ThenNothingIsSaved()
	})
}

// TestBuildFile_DeclaresTargetsWithoutEvaluatingThem specifies that running a BUILD file declares each target its
// rule calls name, checking only each call's shape: that its keywords are the rule's, its required arguments are
// given, its name is a string unique in the package, its params are valid, and a function of ctx gives a whole
// argument, never a value within one.
// It reads none of the arguments' values, resolves no reference and loads no other package:
// a target's rule is evaluated only once a command needs the target.
func TestBuildFile_DeclaresTargetsWithoutEvaluatingThem(t *testing.T) {
	parallelRule(t)
	for _, example := range []struct {
		name  string
		build string // after the pull of busybox, on line 2
		error string
	}{
		{
			name:  "A missing required argument of an unused target",
			build: "layer(name=\"app\", base=\"scratch\")\nlayer(name=\"unused\")",
			error: "/project/BUILD:3:6: layer: missing argument for base",
		},
		{
			name:  "A duplicate name",
			build: "layer(name=\"app\", base=\"scratch\")\nlayer(name=\"app\", base=\"scratch\")",
			error: `/project/BUILD:3:6: layer: name "app" is already used at /project/BUILD:2:6`,
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newBuildFileWorld(t)
			w.GivenTheBuildFileDeclares(example.build)
			w.WhenIBuild("app")
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
		})
	}

	for _, example := range []struct {
		name   string
		unused string // after the pull of busybox and the layer app, on line 3
	}{
		{
			name:   "An unused target's steps are not walked",
			unused: `layer(name="unused", base="busybox", steps=range(1 << 40))`,
		},
		{
			name:   "An unused target's arg() is not checked",
			unused: `layer(name="unused", base=dep("app", mode=arg("mdoe")))`,
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newBuildFileWorld(t)
			w.GivenTheBuildFileDeclares(`layer(name="app", base="busybox")`)
			w.GivenTheBuildFileDeclares(example.unused)
			w.WhenIShowTheGraph("app")
			w.ThenItSucceeds()
		})
	}

	t.Run("An unused target's values are not read", func(t *testing.T) {
		w := newBuildingWorld(t)
		w.GivenAProjectThatPullsBusybox()
		w.GivenTheBuildFileDeclares(fmt.Sprintf(`
			layer(name="app", base="busybox", steps=[run(["echo", %q])])
			layer(name="unused", base="//missing:nothing", command=42)
		`, testenv.NewNonce()))
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("app"): ran})
	})
}

// TestBuildFile_ArgumentsAreFrozenWhenTheRuleIsCalled specifies that the values passed to a rule are frozen as it is
// called, with every list and dict they hold, so that the target is made from the values the call was given,
// and changing one afterwards fails at the change.
// The check that every run() step is in some target's steps reads the same values.
func TestBuildFile_ArgumentsAreFrozenWhenTheRuleIsCalled(t *testing.T) {
	parallelRule(t)
	for _, example := range []struct {
		name  string
		build string // after the pull of busybox, on line 2
		error string
	}{
		{
			name: "Appending to a list after passing it as steps",
			build: `
				s = run(["true"])
				st = []
				layer(name="x", base="scratch", command=lambda ctx: [], steps=st)
				st.append(s)
			`,
			error: "/project/BUILD:5:10: append: cannot append to frozen list",
		},
		{
			name: "Appending to a list after passing it to dep()",
			build: `
				flags = ["-v"]
				layer(name="lib", params={"flags": []}, base="busybox", command=lambda ctx: ctx.flags)
				layer(name="x", base=dep("lib", flags=flags))
				flags.append("-q")
			`,
			error: "/project/BUILD:5:13: append: cannot append to frozen list",
		},
		{
			name: "Passing a list that holds a function",
			build: `
				st = [print, run(["true"])]
				layer(name="x", base="scratch", steps=st)
			`,
			error: "/project/BUILD:3:6: layer: steps[0] is a function; " +
				"a function of ctx must give the whole steps, not a value within it",
		},
		{
			name: "A tuple held many times over, which is walked once",
			build: `
				def shared():
				    t = ()
				    for _ in range(64):
				        t = (t, t)
				    return t
				layer(name="x", base="busybox", command=lambda ctx: [], entrypoint=shared())
			`,
			error: `/project/BUILD:7:6: layer: for parameter "entrypoint": got tuple, want list (configuring //:x)`,
		},
		{
			name: "Passing a dep() whose argument holds a function",
			build: `
				glob(name="code", params={"a": []}, include=["."])
				layer(name="x", base="scratch", steps=[add(dep("code", a=[print]), "/app")])
			`,
			error: "/project/BUILD:3:47: dep: a[0] is a function, which no parameter's value can be",
		},
		{
			name: "Passing a dict keyed by a function",
			build: `
				e = {print: "1"}
				layer(name="x", base="busybox", command=lambda ctx: [], entrypoint=e)
			`,
			error: "/project/BUILD:3:6: layer: entrypoint's key <built-in function print> is a function; " +
				"a function of ctx must give the whole entrypoint, not a value within it",
		},
		{
			name: "Appending to a list after passing it to dep(), before the rule",
			build: `
				flags = ["-v"]
				layer(name="lib", params={"flags": []}, base="busybox", command=lambda ctx: ctx.flags)
				d = dep("lib", flags=flags)
				flags.append("-q")
				layer(name="x", base=d)
			`,
			error: "/project/BUILD:5:13: append: cannot append to frozen list",
		},
		{
			name: "Removing from a list after passing it as steps",
			build: `
				s = run(["true"])
				st = [s]
				layer(name="x", base="scratch", steps=st)
				st.pop()
			`,
			error: "/project/BUILD:5:7: pop: cannot pop from frozen list",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newBuildFileWorld(t)
			w.GivenTheBuildFileDeclares(example.build)
			w.WhenIShowTheGraph("x")
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
		})
	}

	t.Run("A function of ctx that refers to itself is passed as it is", func(t *testing.T) {
		w := newBuildFileWorld(t)
		w.GivenTheBuildFileDeclares(`
			def command():
			    def f(ctx):
			        return [] if f == None else ["echo"]
			    return f

			layer(name="x", base="busybox", command=command())
		`)
		w.WhenIShowTheGraph("x")
		w.ThenItSucceeds()
	})

	t.Run("A function of ctx holding a method bound to a list that holds a function referring to itself",
		func(t *testing.T) {
			w := newBuildFileWorld(t)
			w.GivenTheBuildFileDeclares(`
				def command():
				    def f(ctx):
				        return [] if f == None else ["echo"]
				    keep = [f].append
				    return lambda ctx: [] if keep == None else ["echo"]
				layer(name="x", base="busybox", command=command())
			`)
			w.WhenIShowTheGraph("x")
			w.ThenItSucceeds()
		})

	for _, example := range []struct {
		name  string
		build string // after the pull of busybox, on line 2
	}{
		{
			name: "Functions of ctx that capture each other, one through a list",
			build: `
				def steps():
				    l = []
				    def f(ctx):
				        return [] if (l, g) == None else []
				    def g(ctx):
				        return f
				    l.append(g)
				    return f
				layer(name="x", base="busybox", steps=steps())
			`,
		},
		{
			name: "Functions of ctx that share what they capture, many times over",
			build: `
				def shared():
				    f = lambda ctx: []
				    for _ in range(64):
				        f = (lambda a, b: lambda ctx: [] if (a, b) == None else [])(f, f)
				    return f
				layer(name="x", base="busybox", command=shared())
			`,
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newBuildFileWorld(t)
			w.GivenTheBuildFileDeclares(example.build)
			w.WhenIShowTheGraph("x")
			w.ThenItSucceeds()
		})
	}

	t.Run("A dep() that others hold many times over is frozen once", func(t *testing.T) {
		w := newBuildFileWorld(t)
		w.GivenTheBuildFileDeclares(`
			def chain():
			    d = dep("x")
			    for _ in range(64):
			        d = dep("x", a=d, b=d)
			    return d
			layer(name="x", base="busybox")
			layer(name="unused", base=chain())
		`)
		w.WhenIShowTheGraph("x")
		w.ThenItSucceeds()
	})

	t.Run("A list shared by two targets", func(t *testing.T) {
		w := newBuildingWorld(t)
		w.GivenAProjectThatPullsBusybox()
		w.GivenTheBuildFileDeclares(fmt.Sprintf(`
			common = [run(["sh", "-c", "echo observed common=ran # %s"])]
			layer(name="a", base="busybox", steps=common)
			layer(name="b", base="busybox", steps=common + [run(["sh", "-c", "echo observed b=ran"])])
		`, testenv.NewNonce()))
		w.WhenIBuild("b")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("b"): ran})
		w.ThenTheStepsObserved(observations{"common": "ran", "b": "ran"})
	})
}

// TestBuildFile_StringsHoldNoNULByte specifies that no string that a BUILD file gives a target or step,
// such as a name, an argument, a path, an owner or a variable, may hold a NUL byte, which can't reach a container.
func TestBuildFile_StringsHoldNoNULByte(t *testing.T) {
	parallelRule(t)
	for _, example := range []struct {
		name  string
		build string // after the pull of busybox and theCodeGlob, on line 3
		error string
	}{
		{
			name:  "In a target's name",
			build: `layer(name="a\x00b", base="busybox")`,
			error: `/project/BUILD:3:6: layer: name "a\x00b" must not contain a NUL byte`,
		},
		{
			name:  "In a run() argument",
			build: `layer(name="app", base="busybox", steps=[run(["echo", "a\x00"])])`,
			error: "/project/BUILD:3:45: run: argv[1] must not contain a NUL byte",
		},
		{
			name:  "In a command",
			build: `layer(name="app", base="busybox", command=["a\x00"])`,
			error: "/project/BUILD:3:6: layer: command[0] must not contain a NUL byte",
		},
		{
			name:  "In a workdir() path",
			build: `layer(name="app", base="busybox", steps=[workdir("/a\x00")])`,
			error: `/project/BUILD:3:49: workdir: path "/a\x00" must not contain a NUL byte`,
		},
		{
			name:  "In an owner",
			build: `layer(name="app", base="busybox", steps=[add("code", "/app", owner="a\x00")])`,
			error: `/project/BUILD:3:45: add: owner "a\x00" must not contain a NUL byte`,
		},
		{
			name:  "In a task's source",
			build: `task(name="app", base="busybox", source="/a\x00", include=["."])`,
			error: `/project/BUILD:3:5: task: source "/a\x00" must not contain a NUL byte`,
		},
		{
			name:  "In a variable's name",
			build: `layer(name="app", base="busybox", steps=[env(**{"A\x00B": "x"})])`,
			error: `/project/BUILD:3:45: env: variable name "A\x00B" must not contain "=" or a NUL byte`,
		},
		{
			name:  "In a variable's value",
			build: `layer(name="app", base="busybox", steps=[env(A="x\x00y")])`,
			error: "/project/BUILD:3:45: env: A's value must not contain a NUL byte",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newBuildFileWorld(t)
			w.GivenTheBuildFileDeclares(theCodeGlob)
			w.GivenTheBuildFileDeclares(example.build)
			w.GivenTheProjectHolds(theCode)
			w.WhenISave("app")
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}

// TestBuildFile_StepsAreMadeInAStepsList specifies that steps are made in the steps list of a task or layer,
// and that a step made anywhere else is an error.
func TestBuildFile_StepsAreMadeInAStepsList(t *testing.T) {
	parallelRule(t)
	for _, example := range []struct {
		name  string
		build string // after the pull of busybox and theCodeGlob, on line 3
		error string
	}{
		{
			name:  "A run() outside any steps fails",
			build: "run([\"true\"])\nlayer(name=\"app\", base=\"busybox\")",
			error: `/project/BUILD:3:4: run: run(["true"]) is not in the steps of any layer() or task()`,
		},
		{
			name: "A run() outside any steps fails beside steps that a function gives",
			build: `layer(name="lib", base="busybox", steps=[run(["true"])], command=lambda ctx: [])` + "\n" +
				"run([\"false\"])\nlayer(name=\"app\", base=\"busybox\")",
			error: `/project/BUILD:4:4: run: run(["false"]) is not in the steps of any layer() or task()`,
		},
		{
			name:  "An add() outside any steps fails",
			build: "add(\"code\", \"/app\")\nlayer(name=\"app\", base=\"busybox\")",
			error: `/project/BUILD:3:4: add: add("code", "/app") is not in the steps of any layer() or task()`,
		},
		{
			name: "An add() outside any steps, given a dep() of a dep(), fails, showing both",
			build: "add(dep(\"code\", a=dep(\"lib\", b=\"x\")), \"/app\")\n" +
				`layer(name="app", base="busybox")`,
			error: `/project/BUILD:3:4: add: add(dep("code", a = dep("lib", b = "x")), "/app") ` +
				"is not in the steps of any layer()",
		},
		{
			name:  "A list given to dep() can't be changed to hold it",
			build: "l = []\nd = dep(\"code\", a=l)\nl.append(d)\nlayer(name=\"app\", base=\"busybox\")",
			error: "/project/BUILD:5:9: append: cannot append to frozen list",
		},
		{
			name: "An env() kept in a variable but never used fails",
			build: `layer(name="app", base="busybox", steps=[run(["true"])])` + "\n" +
				`setup = [env(**{"A": "1", "a.b": "2"})]`,
			error: `/project/BUILD:4:13: env: env(A="1", **{"a.b": "2"}) is not in the steps of any layer() or task()`,
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newBuildFileWorld(t)
			w.GivenTheBuildFileDeclares(theCodeGlob)
			w.GivenTheBuildFileDeclares(example.build)
			w.GivenTheProjectHolds(theCode)
			w.WhenISave("app")
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}
