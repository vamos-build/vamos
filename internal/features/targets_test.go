package features

import (
	"fmt"
	"testing"

	"github.com/vamos-build/vamos/internal/testenv"
)

// newTargetMistakeWorld starts a scenario of the targets feature whose BUILD file names or refers to a target wrongly,
// which loading catches before anything runs, so that it needs no buildah.
// Every target has a unique name within its BUILD file, and a target refers to another by its name.
// Its project pulls busybox.
func newTargetMistakeWorld(t *testing.T) *world {
	t.Helper()
	w := newEmptyTargetMistakeWorld(t)
	w.GivenAProjectThatPullsBusybox()
	return w
}

// newEmptyTargetMistakeWorld starts a scenario of the targets feature as newTargetMistakeWorld does,
// for a BUILD file that declares no other target before the scenario's: its project holds nothing.
func newEmptyTargetMistakeWorld(t *testing.T) *world {
	t.Helper()
	return newWorld(t)
}

// TestTargets_NamesAreUniqueAndNonempty specifies that every target has a unique, nonempty name within its BUILD file.
func TestTargets_NamesAreUniqueAndNonempty(t *testing.T) {
	parallelRule(t)
	for _, example := range []struct {
		name  string
		build string // after the pull of busybox, on line 2
		error string
	}{
		{
			name:  "A layer with an empty name fails",
			build: `layer(name="", base="busybox")`,
			error: "/project/BUILD:2:6: layer: name must not be empty",
		},
		{
			name:  "A pull with an empty name fails",
			build: `pull(name="", image="busybox")`,
			error: "/project/BUILD:2:5: pull: name must not be empty",
		},
		{
			name:  "A glob with an empty name fails",
			build: `glob(name="", include=["app.sh"])`,
			error: "/project/BUILD:2:5: glob: name must not be empty",
		},
		{
			name:  "A layer with a name already used fails, naming where it was",
			build: `layer(name="busybox", base="busybox")`,
			error: `/project/BUILD:2:6: layer: name "busybox" is already used at /project/BUILD:1:5`,
		},
		{
			name:  "A glob with a name already used fails, naming where it was",
			build: `glob(name="busybox", include=["app.sh"])`,
			error: `/project/BUILD:2:5: glob: name "busybox" is already used at /project/BUILD:1:5`,
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newTargetMistakeWorld(t)
			w.GivenTheBuildFileDeclares(example.build)
			w.WhenISave("app")
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}

// TestTargets_NamesAreNotLabelsOrScratch specifies that names cannot contain : or /,
// and that scratch is reserved for the empty image.
func TestTargets_NamesAreNotLabelsOrScratch(t *testing.T) {
	parallelRule(t)
	for _, example := range []struct {
		name  string
		build string // after the pull of busybox, on line 2
		error string
	}{
		{
			name:  "A name with a colon fails",
			build: `layer(name="a:b", base="busybox")`,
			error: `/project/BUILD:2:6: layer: name "a:b" must not contain ":" or "/", which labels use`,
		},
		{
			name:  "A name with a slash fails",
			build: `layer(name="a/b", base="busybox")`,
			error: `/project/BUILD:2:6: layer: name "a/b" must not contain ":" or "/", which labels use`,
		},
		{
			name:  "A target named scratch fails",
			build: `pull(name="scratch", image="busybox")`,
			error: `/project/BUILD:2:5: pull: name "scratch" is reserved for the empty base image`,
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newTargetMistakeWorld(t)
			w.GivenTheBuildFileDeclares(example.build)
			w.WhenISave("app")
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}

// TestTargets_ReferencesNameTargetsDeclaredAnywhere specifies that a reference names the target its label names,
// whether that target is declared above or below it, as a base, an add() source, a dep() with arguments, an index's
// manifest, or a publication's check or image.
func TestTargets_ReferencesNameTargetsDeclaredAnywhere(t *testing.T) {
	parallelRule(t)
	t.Run("A base declared below in the same file", func(t *testing.T) {
		w := newTargetWorld(t)
		w.GivenTheBuildFileDeclares(fmt.Sprintf(`
			layer(name="app", base="deps", steps=[run(["echo", %q])])
			layer(name="deps", base="busybox")
		`, testenv.NewNonce()))
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("deps"): ranOrReused, layer("app"): ran})
	})

	t.Run("A base declared below, beside a function of ctx", func(t *testing.T) {
		w := newTargetWorld(t)
		w.GivenTheBuildFileDeclares(fmt.Sprintf(`
			layer(name="app", base="deps", steps=lambda ctx: [run(["echo", %q])])
			layer(name="deps", base="busybox")
		`, testenv.NewNonce()))
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("deps"): ranOrReused, layer("app"): ran})
	})

	t.Run("An add() source declared below in the same file", func(t *testing.T) {
		w := newTargetWorld(t)
		w.GivenTheBuildFileDeclares(`
			layer(name="app", base="busybox", steps=[add("code", "/app")])
			glob(name="code", include=["app.sh"])
		`)
		w.GivenTheProjectHolds(files{"app.sh": "echo hi"})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageHolds(under("/app"), files{"/app/app.sh": "echo hi"})
	})

	t.Run("A dep() with arguments of a target declared below", func(t *testing.T) {
		w := newTargetWorld(t)
		w.GivenTheBuildFileDeclares(fmt.Sprintf(`
			layer(name="app", base=dep("deps", flavor="slim"))
			layer(name="deps", params={"flavor": "full"}, base="busybox",
			      steps=lambda ctx: [run(["echo", %q, ctx.flavor])])
		`, testenv.NewNonce()))
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("deps", `flavor="slim"`): ran, layer("app"): ranOrReused})
	})

	t.Run("An add() of a dep() passing on a parameter, to a target declared below", func(t *testing.T) {
		w := newTargetWorld(t)
		nonce := testenv.NewNonce()
		w.GivenTheBuildFileDeclares(fmt.Sprintf(`
			layer(name="app", params={"mode": "release"}, base="busybox",
			      steps=[add(dep("mode", mode=arg("mode")), "/app")])
			task(name="mode", params={"mode": "release"}, base="busybox", source="/out", include=["mode.txt"],
			     steps=lambda ctx: [run(["sh", "-c", "mkdir /out && echo %s " + ctx.mode + " > /out/mode.txt"])])
		`, nonce))
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageHolds(under("/app"), files{"/app/mode.txt": nonce + " release\n"})
		w.WhenISave("app", setting("mode=debug"))
		w.ThenItSucceeds()
		w.ThenTheImageHolds(under("/app"), files{"/app/mode.txt": nonce + " debug\n"})
	})

	t.Run("A publication's check and image, and an index's manifest, declared below", func(t *testing.T) {
		w := newTargetWorld(t)
		w.GivenARegistry()
		w.GivenTheBuildFileDeclares(fmt.Sprintf(`
			publication(name="release", checks=["test"], images={"<registry>/team/server": "server"})
			index(name="server", layer="app", platforms=[%q])
			task(name="test", base="app", steps=[run(["echo", %q])])
			layer(name="app", base="busybox", command=["echo", "app"])
		`, thisPlatform(), testenv.NewNonce()))
		w.WhenIPublish("release", taggedAs("v1"))
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{task("test"): ran, layer("app"): ranOrReused})
		w.ThenTheIndexManifestsAre(manifest{platform: thisPlatform(), command: []string{"echo", "app"}})
	})
}

// TestTargets_ReferenceMistakesAreReportedWhereTheyAreMade specifies that a reference naming no target of its
// package, or one of a kind it can't use, fails at the call making it: the rule's call for a base, and the add() call
// for its source, even in a .star file; and that a missing target is reported with the names it could be, of the kinds
// the reference can use, but the target naming it.
func TestTargets_ReferenceMistakesAreReportedWhereTheyAreMade(t *testing.T) {
	parallelRule(t)
	for _, example := range []struct {
		name    string
		build   string // after the pull of busybox, on line 2
		project files  // the project's other files
		error   string
	}{
		{
			name:  "A misspelled base fails, naming the one meant",
			build: `layer(name="app", base="busybx")`,
			error: `/project/BUILD:2:6: layer: base "busybx": /project/BUILD has no target "busybx"; ` +
				`did you mean "busybox"?`,
		},
		{
			name:  "A base misspelled from scratch fails, naming scratch",
			build: `layer(name="app", base="scrach")`,
			error: `/project/BUILD:2:6: layer: base "scrach": /project/BUILD has no target "scrach"; ` +
				`did you mean "scratch", the empty image?`,
		},
		{
			name:  "A dep() misspelled from scratch fails, without naming scratch, which takes no arguments",
			build: `layer(name="app", base=dep("scrach"))`,
			error: `/project/BUILD:2:6: layer: base "scrach": /project/BUILD has no target "scrach"; ` +
				`its pulls and layers are "busybox"`,
		},
		{
			name:  "A base misspelled from the target itself fails, without suggesting it",
			build: `layer(name="app", base="ap")`,
			error: `/project/BUILD:2:6: layer: base "ap": /project/BUILD has no target "ap"; ` +
				`its pulls and layers are "busybox"`,
		},
		{
			name:  "A base that names an image rather than a target fails, saying to pull it",
			build: `layer(name="app", base="docker.io/library/busybox:1.36")`,
			error: `/project/BUILD:2:6: layer: base "docker.io/library/busybox:1.36": /project/BUILD has no target ` +
				`"docker.io/library/busybox:1.36"; to build on an image, declare a pull() of it, ` +
				`or name a target of another package by a label starting with "//"`,
		},
		{
			name: "A misspelled add() source fails, naming the one meant",
			build: "glob(name=\"code\", include=[\"app.sh\"])\n" +
				`layer(name="app", base="busybox", steps=[add("cod", "/app")])`,
			error: `/project/BUILD:3:45: add: source "cod": /project/BUILD has no target "cod"; did you mean "code"?`,
		},
		{
			name:  "An add() source misspelled from a target it can't use fails, without suggesting it",
			build: `layer(name="app", base="busybox", steps=[add("busybx", "/app")])`,
			error: `/project/BUILD:2:45: add: source "busybx": /project/BUILD has no target "busybx"; ` +
				"it declares no globs or tasks with output",
		},
		{
			name:    "An add() source that names a file rather than a target fails",
			build:   `layer(name="app", base="busybox", steps=[add("app.sh", "/app")])`,
			project: files{"app.sh": "echo hi"},
			error: `/project/BUILD:2:45: add: source "app.sh": /project/BUILD has no target "app.sh"; ` +
				"it declares no globs or tasks with output",
		},
		{
			name: "A reference of the wrong kind declared below fails, saying what it is",
			build: `layer(name="app", base="busybox", steps=[add("img", "/app")])` + "\n" +
				`layer(name="img", base="busybox")`,
			error: `/project/BUILD:2:45: add: source "img" is a layer(), not a glob() or task()`,
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newTargetMistakeWorld(t)
			w.GivenTheBuildFileDeclares(example.build)
			w.GivenTheProjectHolds(example.project)
			w.WhenISave("app")
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}

	t.Run("A base in a file with no other targets fails, saying there are none", func(t *testing.T) {
		w := newEmptyTargetMistakeWorld(t)
		w.GivenTheBuildFileDeclares(`layer(name="app", base="busybx")`)
		w.WhenISave("app")
		w.ThenItFailsWith(`/project/BUILD:1:6: layer: base "busybx": /project/BUILD has no target "busybx"; ` +
			"it declares no pulls or layers")
		w.ThenNoTargetRan()
		w.ThenNothingIsSaved()
	})

	t.Run("A reference made in a .star function fails where the function makes it", func(t *testing.T) {
		w := newEmptyTargetMistakeWorld(t)
		w.GivenTheProjectHolds(files{
			"defs.star":  "def lib():\n    return add(\"//lib:nope\", \"/x\")\n",
			"lib/BUILD":  `glob(name="code", include=["lib.sh"])`,
			"lib/lib.sh": "echo lib",
		})
		w.GivenTheBuildFileDeclares(`
			load("//:defs.star", "lib")
			layer(name="app", base="scratch", steps=[lib()])
		`)
		w.WhenISave("app")
		w.ThenItFailsWith(`/project/defs.star:2:15: add: source "//lib:nope": /project/lib/BUILD has no target ` +
			`"nope"; did you mean "code"?`)
		w.ThenNoTargetRan()
		w.ThenNothingIsSaved()
	})
}

// TestTargets_RulesAreEvaluatedWhenConfigured specifies that a target's rule is evaluated as its target is configured,
// once if no argument is a function of ctx, so that a mistake in it is reported without the configuration, even for a
// target used in several configurations.
func TestTargets_RulesAreEvaluatedWhenConfigured(t *testing.T) {
	parallelRule(t)
	t.Run("A mistake in a target used in two configurations", func(t *testing.T) {
		w := newTargetMistakeWorld(t)
		w.GivenTheBuildFileDeclares(`
			layer(name="tool", base="busybox", command=42)
			index(name="app", layer="tool", platforms=["linux/amd64", "linux/arm64"])
		`)
		w.WhenIBuild("app")
		w.ThenItFailsWith(`/project/BUILD:2:6: layer: for parameter "command": got int, want list`)
		w.ThenTheCommandDoesNotSay("configuring")
		w.ThenNoTargetRan()
	})
}

// TestTargets_KindsHaveTheirUses specifies what each kind of target can be used as:
// only a pull or a layer can be a base, and only a glob or a task with output can be an add() source.
func TestTargets_KindsHaveTheirUses(t *testing.T) {
	parallelRule(t)
	for _, example := range []struct {
		name  string
		build string // after the pull of busybox, on line 2
		error string
	}{
		{
			name:  "A glob as a base fails",
			build: "glob(name=\"code\", include=[\"app.sh\"])\n" + `layer(name="app", base="code")`,
			error: `/project/BUILD:3:6: layer: base "code" is a glob(), not a pull() or layer()`,
		},
		{
			name: "A task as a base fails",
			build: `task(name="make", base="busybox", source="/out", include=["app"])` + "\n" +
				`layer(name="app", base="make")`,
			error: `/project/BUILD:3:6: layer: base "make" is a task(), not a pull() or layer()`,
		},
		{
			name:  "A pull as an add() source fails",
			build: `layer(name="app", base="busybox", steps=[add("busybox", "/app")])`,
			error: `/project/BUILD:2:45: add: source "busybox" is a pull(), not a glob() or task()`,
		},
		{
			name:  "A glob that dep() names as a base fails",
			build: "glob(name=\"code\", include=[\"app.sh\"])\n" + `layer(name="app", base=dep("code"))`,
			error: `/project/BUILD:3:6: layer: base "code" is a glob(), not a pull() or layer()`,
		},
		{
			name: "A task without output as an add() source fails, even with its source given by a function",
			build: `task(name="make", params={"out": ""}, base="busybox", source=lambda ctx: ctx.out)` + "\n" +
				`layer(name="app", base="busybox", steps=[add("make", "/app")])`,
			error: `/project/BUILD:3:45: add: source "make" is a task() without output; give it source and include`,
		},
		{
			name: "A task without output as an add() source fails",
			build: `task(name="test", base="busybox", steps=[run(["true"])])` + "\n" +
				`layer(name="app", base="busybox", steps=[add("test", "/app")])`,
			error: `/project/BUILD:3:45: add: source "test" is a task() without output; give it source and include`,
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newTargetMistakeWorld(t)
			w.GivenTheBuildFileDeclares(example.build)
			w.WhenISave("app")
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}

// TestTargets_CommandsNameTheBuildFilesTargets specifies that the targets a command names are names in the current
// directory's BUILD file, or workspace labels: any other name fails, before anything runs,
// suggesting the name that was meant where one is close, and otherwise naming the targets there are.
func TestTargets_CommandsNameTheBuildFilesTargets(t *testing.T) {
	parallelRule(t)
	for _, example := range []struct {
		name    string
		targets []string // the first, and those after it
		error   string
	}{
		{
			name:    "A misspelled target fails, naming the one meant",
			targets: []string{"pakcages"},
			error:   `/project/BUILD has no target "pakcages"; did you mean "packages"?`,
		},
		{
			name:    "An unknown target fails, naming the targets there are",
			targets: []string{"packages", "server"},
			error:   `/project/BUILD has no target "server"; its targets are "busybox", "code", "packages"`,
		},
		{
			name:    "A label without its leading slashes fails, saying how a label starts",
			targets: []string{"backend:test"},
			error:   `/project/BUILD has no target "backend:test"; a label starts with "//", as in "//backend:test"`,
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newTargetMistakeWorld(t)
			w.GivenTheBuildFileDeclares(`
				glob(name="code", include=["app.sh"])
				layer(name="packages", base="busybox", steps=[add("code", "/app")])
			`)
			w.WhenIBuild(example.targets[0], alongWith(example.targets[1:]...))
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
		})
	}

	t.Run("A target of a BUILD file that declares none fails, saying so", func(t *testing.T) {
		w := newEmptyTargetMistakeWorld(t)
		w.GivenTheBuildFileDeclares("")
		w.WhenISave("app")
		w.ThenItFailsWith(`/project/BUILD has no target "app"; it declares none`)
		w.ThenNoTargetRan()
		w.ThenNothingIsSaved()
	})
}

// TestTargets_GlobsAreNotNamedByCommands specifies that globs provide input filesets to add(),
// and cannot be named directly by a command.
func TestTargets_GlobsAreNotNamedByCommands(t *testing.T) {
	parallelRule(t)
	t.Run("Saving a glob fails, saying its files are read by the targets that add them", func(t *testing.T) {
		w := newTargetMistakeWorld(t)
		w.GivenTheBuildFileDeclares(`glob(name="code", include=["app.sh"])`)
		w.WhenISave("code")
		w.ThenItFailsWith(`"code" is a glob(), whose files are read by the targets that add them, not an image`)
		w.ThenNoTargetRan()
		w.ThenNothingIsSaved()
	})

	t.Run("Building a glob fails, saying its files are read by the targets that add them", func(t *testing.T) {
		w := newTargetMistakeWorld(t)
		w.GivenTheBuildFileDeclares(`glob(name="code", include=["app.sh"])`)
		w.WhenIBuild("code")
		w.ThenItFailsWith(`"code" is a glob(), whose files are read by the targets that add them`)
		w.ThenNoTargetRan()
	})
}

// newTargetWorld starts a scenario of the targets feature that builds, which it skips where buildah can't.
// Its project pulls busybox.
func newTargetWorld(t *testing.T) *world {
	t.Helper()
	w := newBuildingWorld(t)
	w.GivenAProjectThatPullsBusybox()
	return w
}

// TestTargets_CyclesAreErrors specifies that targets that use each other in a cycle are errors,
// reported at the declaration of the target whose reference closes the cycle,
// naming the reference and the configuration that a function of ctx made it in,
// whether the references are given as they are or by functions of ctx, which may name a target declared after them.
func TestTargets_CyclesAreErrors(t *testing.T) {
	parallelRule(t)
	t.Run("A function of ctx names a target declared after it", func(t *testing.T) {
		w := newTargetWorld(t)
		w.GivenTheBuildFileDeclares(`
			layer(name="app", base=lambda ctx: "later")
			layer(name="later", base="busybox")
		`)
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("later"): ranOrReused, layer("app"): ranOrReused})
	})

	for _, example := range []struct {
		name  string
		build string // after the pull of busybox, on line 2
		error string
	}{
		{
			name:  "A target that is its own base fails, naming the cycle",
			build: `layer(name="app", base="app")`,
			error: `/project/BUILD:2:6: layer: base "app": targets use each other in a cycle: app -> app`,
		},
		{
			name: "Targets that use each other through a function fail, naming the cycle",
			build: `
				layer(name="a", base=lambda ctx: "b")
				layer(name="b", base="a")
				layer(name="app", base="b")
			`,
			error: `/project/BUILD:2:6: layer: base "b": targets use each other in a cycle: b -> a -> b ` +
				"(configuring //:a)",
		},
		{
			name: "Targets that use each other through another configuration fail, naming the cycle",
			build: `
				layer(name="a", params={"mode": "release"}, base=lambda ctx: dep("b", mode="debug"))
				layer(name="b", params={"mode": "release"}, base=dep("a", mode=arg("mode")))
				layer(name="app", base="a")
			`,
			error: `/project/BUILD:3:6: layer: base "a": targets use each other in a cycle: a -> b -> a`,
		},
		{
			name: "Tasks that add each other's output through a function fail, naming the cycle",
			build: `
				task(name="a", base="busybox", steps=lambda ctx: [add("b", "/b")], source="/a", include=["."])
				task(name="b", base="busybox", steps=[add("a", "/a")], source="/b", include=["."])
				layer(name="app", base="busybox", steps=[add("b", "/app")])
			`,
			error: `/project/BUILD:2:5: task: add: source "b": targets use each other in a cycle: b -> a -> b ` +
				"(configuring //:a)",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newTargetMistakeWorld(t)
			w.GivenTheBuildFileDeclares(example.build)
			w.WhenISave("app")
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}
