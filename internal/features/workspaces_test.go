package features

import (
	"fmt"
	"testing"

	"github.com/vamos-build/vamos/internal/testenv"
)

// newWorkspaceWorld starts a scenario of the workspaces feature, from its background:
// a workspace, a directory with a WORKSPACE file, whose packages are directories with BUILD files.
func newWorkspaceWorld(t *testing.T) *world {
	t.Helper()
	w := newBuildingWorld(t)
	w.GivenTheProjectHolds(files{"WORKSPACE": ""})
	return w
}

// newWorkspaceMistakeWorld starts a scenario of the workspaces feature whose labels or packages are wrong,
// which loading catches before anything runs, so that it needs no buildah.
// Its project is a workspace, with a WORKSPACE file.
func newWorkspaceMistakeWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	w.GivenTheProjectHolds(files{"WORKSPACE": ""})
	return w
}

// theLibPackage declares, in the package lib, the pull "busybox", the glob "code" of lib.sh and the task "make".
const theLibPackage = `
	pull(name="busybox", image="docker.io/library/busybox", tag="1.36")
	glob(name="code", include=["lib.sh"])
	task(name="make", base="busybox", source="/out", include=["made"])
`

// TestWorkspaces_LabelsNameOtherPackagesTargets specifies that labels name the targets of other packages
// wherever a target reference is accepted, such as base, add() and dep(), and that a referenced glob keeps paths
// relative to its own BUILD directory.
func TestWorkspaces_LabelsNameOtherPackagesTargets(t *testing.T) {
	parallelRule(t)
	t.Run("A layer is built on, and adds, another package's targets", func(t *testing.T) {
		w := newWorkspaceWorld(t)
		nonce := testenv.NewNonce()
		w.GivenThePackageDeclares("lib", testenv.PullBusybox)
		w.GivenThePackageDeclares("lib", `
			glob(name="code", include=["lib.sh"])
			task(
			    name="make",
			    base="busybox",
			    steps=[add("code", "/src"), run(["sh", "-c", "mkdir /out && cp /src/lib.sh /out/made.sh"])],
			    source="/out",
			    include=["made.sh"],
			)
		`)
		w.GivenThePackageDeclares("app", `
			glob(name="code", include=["app.sh"])
			layer(
			    name="app",
			    base=dep("//lib:busybox"),
			    steps=[add("code", "/app"), add("//lib:make", "/app"), add("//lib:code", "/srv/lib")],
			)
		`)
		w.GivenTheProjectHolds(files{"lib/lib.sh": "echo " + nonce, "app/app.sh": "echo app"})
		w.GivenIWorkIn("app")
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{task("//lib:make"): ran})
		w.ThenTheImageHolds(under("/app"), files{"/app/app.sh": "echo app", "/app/made.sh": "echo " + nonce})
		w.ThenTheImageHolds(under("/srv/lib"), files{"/srv/lib/lib.sh": "echo " + nonce})
	})

	t.Run("From the root, every target is named by its label", func(t *testing.T) {
		w := newWorkspaceWorld(t)
		w.GivenThePackageDeclares("lib", testenv.PullBusybox)
		w.GivenThePackageDeclares("app", fmt.Sprintf(`
			layer(name="app", base="//lib:busybox", steps=[run(["echo", %q])])
		`, testenv.NewNonce()))
		w.GivenIWorkIn("app")
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("app"): ran})
		w.GivenIWorkIn(".")
		w.WhenIBuild("//app:app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("//app:app"): reused})
	})
}

// TestWorkspaces_OnlyNeededTargetsLoad specifies that vamos loads only the packages and targets
// that the requested targets need.
func TestWorkspaces_OnlyNeededTargetsLoad(t *testing.T) {
	parallelRule(t)
	t.Run("Targets and packages nothing needs are neither run nor checked", func(t *testing.T) {
		w := newWorkspaceWorld(t)
		w.GivenThePackageDeclares("lib", testenv.PullBusybox)
		w.GivenThePackageDeclares("lib", `
			glob(name="docs", include=["README"]) # missing, but nothing needs it
		`)
		w.GivenThePackageDeclares("app", `
			layer(name="app", base="//lib:busybox")
			task(name="test", base="app", steps=[run(["false"])])
			layer(name="later", base=lambda ctx: "//broken:base") # named once configured, which it never is
		`)
		w.GivenThePackageDeclares("broken", `this isn't Starlark`)
		w.GivenIWorkIn("app")
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{task("test"): notBuilt})
	})
}

// TestWorkspaces_RootIsTheNearestWorkspaceFile specifies that vamos searches for the nearest WORKSPACE
// in -C's directory and its ancestors, and that without one, -C's directory is the workspace root.
// A WORKSPACE that can't be checked stops the search, rather than letting another directory become the root.
func TestWorkspaces_RootIsTheNearestWorkspaceFile(t *testing.T) {
	parallelRule(t)
	t.Run("A WORKSPACE that can't be checked fails the command", func(t *testing.T) {
		w := newWorkspaceMistakeWorld(t)
		w.GivenThePackageDeclares("app", `layer(name="app", base="scratch")`)
		w.GivenTheProjectFileCannotBeChecked("app/WORKSPACE")
		w.GivenIWorkIn("app")
		w.WhenISave("app")
		w.ThenItFailsWith("looking for a WORKSPACE file: stat /project/app/WORKSPACE: permission denied")
		w.ThenNoTargetRan()
		w.ThenNothingIsSaved()
	})

	for _, example := range []struct {
		name       string
		workspaces files  // the WORKSPACE files, none for none
		lib        string // the content of the lib package that the app's label reaches
	}{
		{
			name:       "A WORKSPACE above -C is the root",
			workspaces: files{"WORKSPACE": ""},
			lib:        "outer",
		},
		{
			name:       "A WORKSPACE in -C is the root, rather than one above",
			workspaces: files{"WORKSPACE": "", "app/WORKSPACE": ""},
			lib:        "inner",
		},
		{
			name: "Without a WORKSPACE, -C is the root",
			lib:  "inner",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newBuildingWorld(t)
			w.GivenTheProjectHolds(example.workspaces)
			lib := `glob(name="code", include=["lib.txt"])`
			w.GivenThePackageDeclares("lib", lib)
			w.GivenThePackageDeclares("app/lib", lib)
			w.GivenThePackageDeclares("app", testenv.PullBusybox)
			w.GivenThePackageDeclares("app", `
				layer(name="app", base="busybox", steps=[add("//lib:code", "/srv/lib")])
			`)
			w.GivenTheProjectHolds(files{"lib/lib.txt": "outer", "app/lib/lib.txt": "inner"})
			w.GivenIWorkIn("app")
			w.WhenISave("app")
			w.ThenItSucceeds()
			w.ThenTheImageHolds(under("/srv/lib"), files{"/srv/lib/lib.txt": example.lib})
		})
	}
}

// TestWorkspaces_BuildVmsIsABuildFile specifies that a package's file may be named BUILD.vms,
// where the directory also holds a build directory, and that a directory named BUILD or build is never taken for a
// BUILD file.
func TestWorkspaces_BuildVmsIsABuildFile(t *testing.T) {
	parallelRule(t)
	t.Run("Packages with BUILD.vms files are built by label and from -C", func(t *testing.T) {
		w := newWorkspaceWorld(t)
		w.GivenTheProjectHolds(files{
			"BUILD.vms":       `layer(name="app", base="//react:site")`,
			"react/BUILD.vms": `layer(name="site", base="scratch", command=["serve"])`,
			"react/BUILD/out": "a build directory, as where build is found as BUILD without case",
			"react/build/out": "a build directory",
		})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageCommandIs("serve")
		w.GivenIWorkIn("react")
		w.WhenISave("site")
		w.ThenItSucceeds()
		w.ThenTheImageCommandIs("serve")
	})

	t.Run("A package's BUILD.vms is read as its BUILD file, by label and from -C, as its mistakes show",
		func(t *testing.T) {
			w := newWorkspaceMistakeWorld(t)
			w.GivenTheProjectHolds(files{
				"BUILD.vms":       `layer(name="app", base="//react:site")`,
				"react/BUILD.vms": `layer(name="site", base="busybx")`,
				"react/build/out": "a build directory",
			})
			w.WhenISave("app")
			w.ThenItFailsWith(`/project/react/BUILD.vms:1:6: layer: base "busybx" is not "scratch" or an earlier ` +
				"pull() or layer(); there are none before this")
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
			w.GivenIWorkIn("react")
			w.WhenISave("site")
			w.ThenItFailsWith(`/project/react/BUILD.vms:1:6: layer: base "busybx" is not "scratch" or an earlier ` +
				"pull() or layer(); there are none before this")
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})

	t.Run("A package with both BUILD and BUILD.vms fails, rather than one being ignored", func(t *testing.T) {
		w := newWorkspaceMistakeWorld(t)
		w.GivenTheProjectHolds(files{
			"BUILD.vms":       `layer(name="app", base="//react:site")`,
			"react/BUILD":     `layer(name="site", base="scratch")`,
			"react/BUILD.vms": `layer(name="site", base="scratch")`,
		})
		w.WhenISave("app")
		w.ThenItFailsWith(`/project/BUILD.vms:1:6: layer: base "//react:site": /project/react has both BUILD and ` +
			"BUILD.vms; a package has one BUILD file, so remove one of them")
		w.ThenNoTargetRan()
		w.ThenNothingIsSaved()
	})
}

// TestWorkspaces_PackagesMayNameEachOthersTargets specifies that packages may name each other's targets:
// a package's BUILD file runs when another first names one of its targets, while that other file is still running,
// and can then name the other file's targets declared above that point: a target declared below that point isn't
// declared yet, which is an error saying where the first file stopped, and targets that name each other in a cycle
// are an error.
func TestWorkspaces_PackagesMayNameEachOthersTargets(t *testing.T) {
	parallelRule(t)
	for _, example := range []struct {
		name     string
		packages map[string]string // BUILD files, by package
		statuses statuses          // of the layers built
	}{
		{
			name: "Two packages build on each other's layers",
			packages: map[string]string{
				".":     "layer(name=\"y\", base=\"scratch\")\nlayer(name=\"app\", base=\"//scala:x\")",
				"scala": `layer(name="x", base="//:y")`,
			},
			statuses: statuses{layer("y"): ranOrReused, layer("//scala:x"): ranOrReused, layer("app"): ranOrReused},
		},
		{
			name: "Three packages in a ring build on each other's layers",
			packages: map[string]string{
				".": "layer(name=\"a\", base=\"scratch\")\nlayer(name=\"app\", base=\"//b:b\")",
				"b": `layer(name="b", base="//c:c")`,
				"c": `layer(name="c", base="//:a")`,
			},
			statuses: statuses{
				layer("a"):     ranOrReused,
				layer("//c:c"): ranOrReused,
				layer("//b:b"): ranOrReused,
				layer("app"):   ranOrReused,
			},
		},
		{
			name: "A function of ctx names a target declared below, once every BUILD file has run",
			packages: map[string]string{
				".":     "layer(name=\"app\", base=\"//scala:x\")\nlayer(name=\"y\", base=\"scratch\")",
				"scala": `layer(name="x", base=lambda ctx: "//:y")`,
			},
			statuses: statuses{layer("y"): ranOrReused, layer("//scala:x"): ranOrReused, layer("app"): ranOrReused},
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newWorkspaceWorld(t)
			for dir, build := range example.packages {
				w.GivenThePackageDeclares(dir, build)
			}
			w.WhenIBuild("app")
			w.ThenItSucceeds()
			w.ThenTheTargetsWere(example.statuses)
		})
	}

	t.Run("Packages may add each other's files", func(t *testing.T) {
		w := newWorkspaceWorld(t)
		w.GivenThePackageDeclares(".", testenv.PullBusybox)
		w.GivenThePackageDeclares(".", `
			glob(name="src", include=["src.txt"])
			layer(name="app", base="busybox", steps=[add("//scala:t", "/t")])
		`)
		w.GivenThePackageDeclares("scala", `
			task(name="t", base="//:busybox", steps=[add("//:src", "/s")], source="/s", include=["."])
		`)
		w.GivenTheProjectHolds(files{"src.txt": "from the root"})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageHolds(under("/t"), files{"/t/src.txt": "from the root"})
	})

	for _, example := range []struct {
		name     string
		packages map[string]string // BUILD files, by package
		error    string
	}{
		{
			name: "A target declared below the point where its file loaded the other package fails",
			packages: map[string]string{
				".":     "layer(name=\"app\", base=\"//scala:x\")\nlayer(name=\"y\", base=\"scratch\")",
				"scala": `layer(name="x", base="//:y")`,
			},
			error: `/project/scala/BUILD:1:6: layer: base "//:y": //:y isn't declared yet: its BUILD file is stopped ` +
				"at /project/BUILD:1:6, loading this package; declare y above that line, or name it from a function " +
				"of ctx, which runs once every BUILD file has run",
		},
		{
			name: "Targets of three packages that name each other in a cycle fail, naming the cycle",
			packages: map[string]string{
				".": `layer(name="app", base="//b:b")`,
				"b": `layer(name="b", base="//c:c")`,
				"c": `layer(name="c", base="//:app")`,
			},
			error: `/project/c/BUILD:1:6: layer: base "//:app": targets name each other in a cycle: ` +
				"//:app -> //b:b -> //c:c -> //:app",
		},
		{
			name: "Targets of two packages that name each other in a cycle fail, naming the cycle",
			packages: map[string]string{
				".":   `layer(name="app", base="//lib:base")`,
				"lib": `layer(name="base", base="//:app")`,
			},
			error: `/project/lib/BUILD:1:6: layer: base "//:app": targets name each other in a cycle: ` +
				"//:app -> //lib:base -> //:app",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newWorkspaceMistakeWorld(t)
			for dir, build := range example.packages {
				w.GivenThePackageDeclares(dir, build)
			}
			w.WhenISave("app")
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}

// TestWorkspaces_LabelsNameTargetsOfPackagesWithinTheWorkspace specifies that a label, such as //backend:schema,
// names a target of a package beneath the workspace root, by a clean path to its directory, which is no symbolic link,
// and the target's name: any other label fails as the BUILD file loads, saying why.
func TestWorkspaces_LabelsNameTargetsOfPackagesWithinTheWorkspace(t *testing.T) {
	parallelRule(t)
	for _, example := range []struct {
		name  string
		app   string // the package app's BUILD file
		error string
	}{
		{
			name:  "A label to a directory without a BUILD file fails",
			app:   `layer(name="app", base="//nope:busybox")`,
			error: `/project/app/BUILD:1:6: layer: base "//nope:busybox": no BUILD or BUILD.vms file in /project/nope`,
		},
		{
			name: "A label to a misspelled target fails, naming the one meant",
			app:  `layer(name="app", base="scratch", steps=[add("//lib:cdoe", "/app")])`,
			error: `/project/app/BUILD:1:45: add: source "//lib:cdoe": /project/lib/BUILD has no target "cdoe"; ` +
				`did you mean "code"?`,
		},
		{
			name:  "A label without a target's name fails",
			app:   `layer(name="app", base="//lib")`,
			error: `/project/app/BUILD:1:6: layer: base "//lib": label "//lib" must end with ":" and a target's name`,
		},
		{
			name: "A label outside the workspace fails",
			app:  `layer(name="app", base="//../lib:busybox")`,
			error: `/project/app/BUILD:1:6: layer: base "//../lib:busybox": label "//../lib:busybox": ` +
				`"../lib" is not a clean path to a directory within the workspace`,
		},
		{
			name: "A label that names the root as a dot fails",
			app:  `layer(name="app", base="//.:busybox")`,
			error: `/project/app/BUILD:1:6: layer: base "//.:busybox": label "//.:busybox": "." is not a clean path ` +
				"to a directory within the workspace",
		},
		{
			name: "A label whose path isn't clean fails",
			app:  `layer(name="app", base="//lib/:busybox")`,
			error: `/project/app/BUILD:1:6: layer: base "//lib/:busybox": label "//lib/:busybox": ` +
				`"lib/" is not a clean path to a directory within the workspace`,
		},
		{
			name: "A label without its leading slashes fails, saying how a label starts",
			app:  `layer(name="app", base="scratch", steps=[add("lib:code", "/app")])`,
			error: `/project/app/BUILD:1:45: add: source "lib:code" is not an earlier glob() or task(); ` +
				`a label starts with "//", as in "//lib:code"`,
		},
		{
			name: "A label to a target of its own package, declared later, fails",
			app:  "layer(name=\"app\", base=\":base\")\nlayer(name=\"base\", base=\"scratch\")",
			error: `/project/app/BUILD:1:6: layer: base ":base" is not "scratch" or an earlier pull() or layer(); ` +
				"there are none before this",
		},
		{
			name: "A full label to a target of its own package, declared later, fails",
			app:  "layer(name=\"app\", base=\"//app:base\")\nlayer(name=\"base\", base=\"scratch\")",
			error: `/project/app/BUILD:1:6: layer: base "//app:base" is not "scratch" or an earlier pull() ` +
				"or layer(); there are none before this",
		},
		{
			name:  "A label to another package's glob as a base fails",
			app:   `layer(name="app", base="//lib:code")`,
			error: `/project/app/BUILD:1:6: layer: base "//lib:code" is a glob(), not a pull() or layer()`,
		},
		{
			name:  "A label to another package's pull as an add() source fails",
			app:   `layer(name="app", base="scratch", steps=[add("//lib:busybox", "/app")])`,
			error: `/project/app/BUILD:1:45: add: source "//lib:busybox" is a pull(), not a glob() or task()`,
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newWorkspaceMistakeWorld(t)
			w.GivenThePackageDeclares("lib", theLibPackage)
			w.GivenThePackageDeclares("app", example.app)
			w.GivenTheProjectHolds(files{"lib/lib.sh": "echo lib"})
			w.GivenIWorkIn("app")
			w.WhenISave("app")
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}

	for _, example := range []struct {
		name  string
		label string
		error string
	}{
		{
			name:  "A command's label to a directory without a BUILD file fails",
			label: "//nope:app",
			error: "no BUILD or BUILD.vms file in /project/nope",
		},
		{
			name:  "A command's label to a misspelled target fails, naming the one meant",
			label: "//lib:mkae",
			error: `/project/lib/BUILD has no target "mkae"; did you mean "make"?`,
		},
		{
			name:  "A command's label without a target's name fails",
			label: "//lib",
			error: `label "//lib" must end with ":" and a target's name`,
		},
		{
			name:  "Saving another package's glob by its label fails",
			label: "//lib:code",
			error: `"//lib:code" is a glob(), whose files are read by the targets that add them, not an image`,
		},
		{
			name:  "Saving another package's task by its label fails",
			label: "//lib:make",
			error: `"//lib:make" is a task(), whose image is discarded once it runs; ` +
				"name a pull(), layer() or index() to send",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newWorkspaceMistakeWorld(t)
			w.GivenThePackageDeclares("lib", theLibPackage)
			w.WhenISave(example.label, to("app.tar"))
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}

	t.Run("A label to a package reached through a symbolic link fails", func(t *testing.T) {
		w := newWorkspaceMistakeWorld(t)
		w.GivenAFileOutsideTheProjectHolds("lib/BUILD", `glob(name="code", include=["lib.sh"])`)
		w.GivenTheProjectLinks(links{"vendor": "<outside>"})
		w.GivenThePackageDeclares("app", `layer(name="app", base="scratch", steps=[add("//vendor/lib:code", "/lib")])`)
		w.GivenIWorkIn("app")
		w.WhenISave("app")
		w.ThenItFailsWith(`/project/app/BUILD:1:45: add: source "//vendor/lib:code": package "vendor/lib": ` +
			"/project/vendor is a symbolic link; a package must be a directory within the workspace")
		w.ThenNoTargetRan()
		w.ThenNothingIsSaved()
	})

	t.Run("A command in a directory without a BUILD file fails", func(t *testing.T) {
		w := newWorkspaceMistakeWorld(t)
		w.WhenISave("app")
		w.ThenItFailsWith("no BUILD or BUILD.vms file in /project")
		w.ThenNoTargetRan()
		w.ThenNothingIsSaved()
	})
}

// TestWorkspaces_MistakesAreReportedWhereTheyAre specifies that a mistake in a BUILD file that loads because another
// names one of its targets is reported where it is, as if that file were built itself.
func TestWorkspaces_MistakesAreReportedWhereTheyAre(t *testing.T) {
	parallelRule(t)
	t.Run("A mistake in another package's BUILD file names that file alone", func(t *testing.T) {
		w := newWorkspaceMistakeWorld(t)
		w.GivenThePackageDeclares("lib", testenv.PullBusybox)
		w.GivenThePackageDeclares("lib", `layer(name="base", base="busybx")`)
		w.GivenThePackageDeclares("app", `layer(name="app", base="//lib:base")`)
		w.GivenIWorkIn("app")
		w.WhenISave("app")
		w.ThenItFailsWith(`/project/lib/BUILD:2:6: layer: base "busybx" is not "scratch" or an earlier pull()`)
		w.ThenTheCommandDoesNotSay("app/BUILD")
		w.ThenNoTargetRan()
		w.ThenNothingIsSaved()
	})
}

// anApp declares the layer "app" on scratch, which builds without pulling anything.
const anApp = `layer(name="app", base="scratch")`

// TestWorkspaces_WorkspaceIsAStarlarkFileThatVamosEvaluates specifies that the WORKSPACE file is a Starlark file,
// which vamos evaluates whenever a command opens the workspace, before it loads any BUILD file;
// that an empty WORKSPACE file is valid, as is a workspace without one, and either leaves the workspace unnamed;
// that only the WORKSPACE file at the root of the workspace that holds -C is evaluated;
// and that a WORKSPACE that isn't a regular file, such as a directory, marks the root without being evaluated.
func TestWorkspaces_WorkspaceIsAStarlarkFileThatVamosEvaluates(t *testing.T) {
	parallelRule(t)
	t.Run("An empty WORKSPACE is valid", func(t *testing.T) {
		w := newWorkspaceWorld(t)
		w.GivenThePackageDeclares(".", anApp)
		w.WhenIBuild("app")
		w.ThenItSucceeds()
	})

	t.Run("A workspace without a WORKSPACE file is valid", func(t *testing.T) {
		w := newBuildingWorld(t)
		w.GivenThePackageDeclares(".", anApp)
		w.WhenIBuild("app")
		w.ThenItSucceeds()
	})

	t.Run("A nested workspace's WORKSPACE is not evaluated", func(t *testing.T) {
		w := newWorkspaceWorld(t)
		w.GivenTheProjectHolds(files{
			"WORKSPACE":       `workspace(name = "com.acme.outer")`,
			"inner/WORKSPACE": `this isn't Starlark`,
		})
		w.GivenThePackageDeclares(".", anApp)
		w.WhenIBuild("app")
		w.ThenItSucceeds()
	})

	t.Run("A WORKSPACE above the workspace's own is not evaluated", func(t *testing.T) {
		w := newWorkspaceWorld(t)
		w.GivenTheProjectHolds(files{
			"WORKSPACE":       `this isn't Starlark`,
			"inner/WORKSPACE": `workspace(name = "com.acme.inner")`,
		})
		w.GivenThePackageDeclares("inner", anApp)
		w.GivenIWorkIn("inner")
		w.WhenIBuild("app")
		w.ThenItSucceeds()
	})

	t.Run("A WORKSPACE that isn't a regular file marks the root without being evaluated", func(t *testing.T) {
		w := newBuildingWorld(t)
		w.GivenTheProjectHolds(files{"WORKSPACE/notes.txt": "this isn't Starlark"})
		w.GivenThePackageDeclares(".", anApp)
		w.GivenThePackageDeclares("lib", `layer(name="app", base="//:app")`)
		w.GivenIWorkIn("lib")
		w.WhenIBuild("app")
		w.ThenItSucceeds()
	})

	t.Run("A mistake in WORKSPACE fails before any BUILD file loads", func(t *testing.T) {
		w := newWorkspaceMistakeWorld(t)
		w.GivenTheProjectHolds(files{"WORKSPACE": "# the workspace\nthis isn't Starlark\n"})
		w.GivenThePackageDeclares(".", `this isn't Starlark either`)
		w.WhenIBuild("app")
		w.ThenItFailsWith("/project/WORKSPACE:2:9: got identifier, want newline")
		w.ThenNoTargetRan()
	})
}

// TestWorkspaces_WorkspaceDeclaresTheNameWithWorkspace specifies that WORKSPACE may call workspace() once,
// to give the workspace a symbolic name, which is given by keyword, and is a string,
// and that WORKSPACE offers no other builtin of BUILD files, and no load(): calling one is a mistake.
func TestWorkspaces_WorkspaceDeclaresTheNameWithWorkspace(t *testing.T) {
	parallelRule(t)
	t.Run("workspace() names the workspace", func(t *testing.T) {
		w := newWorkspaceWorld(t)
		w.GivenACoordinator()
		w.GivenTheProjectHolds(files{"WORKSPACE": `workspace(name = "com.acme.storefront")`})
		w.GivenThePackageDeclares(".", anApp)
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheCoordinatorWasToldTheWorkspaceIs("com.acme.storefront", "graph")
	})

	for _, example := range []struct {
		name      string
		workspace string // the WORKSPACE file
		error     string
	}{
		{
			name:      "Calling workspace() twice is a mistake",
			workspace: "workspace(name = \"a\")\nworkspace(name = \"b\")\n",
			error: `/project/WORKSPACE:2:10: workspace: the workspace is already named "a", ` +
				"at /project/WORKSPACE:1:10",
		},
		{
			name:      "A BUILD file's builtin is a mistake in WORKSPACE",
			workspace: `pull(name = "busybox", image = "busybox")`,
			error:     "/project/WORKSPACE:1:1: undefined: pull",
		},
		{
			name:      "load() is a mistake in WORKSPACE",
			workspace: `load("//:defs.star", "x")`,
			error:     "/project/WORKSPACE:1:1: load not implemented by this application",
		},
		{
			name:      "A name that isn't a string is a mistake",
			workspace: `workspace(name = 1)`,
			error:     "/project/WORKSPACE:1:10: workspace: name must be a string, not int",
		},
		{
			name:      "A name given by position is a mistake",
			workspace: `workspace("com.acme.storefront")`,
			error:     `/project/WORKSPACE:1:10: workspace: give the name by keyword, as name = "com.example.app"`,
		},
		{
			name:      "A workspace() without a name is a mistake",
			workspace: `workspace()`,
			error:     "/project/WORKSPACE:1:10: workspace: missing argument for name",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newWorkspaceMistakeWorld(t)
			w.GivenTheProjectHolds(files{"WORKSPACE": example.workspace})
			w.GivenThePackageDeclares(".", anApp)
			w.WhenISave("app")
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}

// TestWorkspaces_AWorkspaceNameFollowsTheRulesOfAModuleName specifies that a workspace name follows the rules of a
// Bazel module name: it consists of lowercase letters, digits, ".", "-" and "_",
// starts with a letter, and ends with a letter or digit;
// and that a mistake in WORKSPACE, such as a name that breaks these rules, fails every command that opens the
// workspace, naming the file, line and column, as a mistake in a BUILD file does.
func TestWorkspaces_AWorkspaceNameFollowsTheRulesOfAModuleName(t *testing.T) {
	parallelRule(t)
	for _, name := range []string{"storefront", "com.acme.storefront", "my-app_2", "a"} {
		t.Run("A valid name: "+name, func(t *testing.T) {
			w := newWorkspaceWorld(t)
			w.GivenTheProjectHolds(files{"WORKSPACE": fmt.Sprintf("workspace(name = %q)", name)})
			w.GivenThePackageDeclares(".", anApp)
			w.WhenIBuild("app")
			w.ThenItSucceeds()
		})
	}

	allowed := `; a workspace name holds only lowercase letters, digits, ".", "-" and "_"`
	for _, example := range []struct {
		name string
		rule string // that the name breaks, as the error says it
	}{
		{
			name: "",
			rule: "name must not be empty",
		},
		{
			name: "Storefront",
			rule: `name "Storefront" must start with a lowercase letter`,
		},
		{
			name: "2app",
			rule: `name "2app" must start with a lowercase letter`,
		},
		{
			name: "app-",
			rule: `name "app-" must end with a lowercase letter or digit`,
		},
		{
			name: "app.",
			rule: `name "app." must end with a lowercase letter or digit`,
		},
		{
			name: "acme/app",
			rule: `name "acme/app" must not contain "/"` + allowed,
		},
	} {
		t.Run(fmt.Sprintf("An invalid name is refused where it is declared: %q", example.name), func(t *testing.T) {
			w := newWorkspaceMistakeWorld(t)
			w.GivenTheProjectHolds(files{"WORKSPACE": fmt.Sprintf("workspace(name = %q)", example.name)})
			w.GivenThePackageDeclares(".", anApp)
			w.WhenISave("app")
			w.ThenItFailsWith("/project/WORKSPACE:1:10: workspace: " + example.rule)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}

// anAppOfAFile declares the layer "app" on scratch, which adds the glob "src" of hello.txt, so that it makes an image
// that a command can send, and a new hello.txt makes it run.
const anAppOfAFile = `
	glob(name="src", include=["hello.txt"])
	layer(name="app", base="scratch", steps=[add("src", "/data")])
`

// TestWorkspaces_VamosDerivesNoWorkspaceName specifies that vamos derives no name from anything else:
// not the workspace's directory, its repository or the repository's remotes;
// and that a workspace that doesn't call workspace() is unnamed.
func TestWorkspaces_VamosDerivesNoWorkspaceName(t *testing.T) {
	parallelRule(t)
	t.Run("An undeclared name is sent as none", func(t *testing.T) {
		w := newBuildingWorld(t)
		w.GivenACoordinator()
		w.GivenTheProjectHolds(files{"storefront/WORKSPACE": ""})
		w.GivenThePackageDeclares("storefront", anApp)
		w.GivenIWorkIn("storefront")
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheCoordinatorWasToldTheWorkspaceIs("", "graph")
	})
}

// TestWorkspaces_TheNameTravelsWithEveryRequestToAnExecutor specifies that every request that a command sends to an
// executor, to build, push, publish or export, carries the name, or none for an unnamed workspace,
// as metadata that changes nothing the executor does, and that a name is trusted as declared.
func TestWorkspaces_TheNameTravelsWithEveryRequestToAnExecutor(t *testing.T) {
	parallelRule(t)
	t.Run("A push carries the name", func(t *testing.T) {
		w := newWorkspaceWorld(t)
		w.GivenARegistry()
		w.GivenACoordinator()
		w.GivenTheProjectHolds(files{"WORKSPACE": `workspace(name = "com.acme.storefront")`, "hello.txt": "hello"})
		w.GivenThePackageDeclares(".", anAppOfAFile)
		w.WhenIPush("app", "<registry>/app:v1", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheCoordinatorWasToldTheWorkspaceIs("com.acme.storefront", "graph", "push")
	})

	t.Run("A publish carries the name", func(t *testing.T) {
		w := newWorkspaceWorld(t)
		w.GivenARegistry()
		w.GivenACoordinator()
		w.GivenTheProjectHolds(files{"WORKSPACE": `workspace(name = "com.acme.storefront")`, "hello.txt": "hello"})
		w.GivenTheBuildFileDeclares(`
			glob(name="src", include=["hello.txt"])
			layer(name="app", base="scratch", steps=[add("src", "/data")])
			publication(name="release", images={"<registry>/team/app": "app"})
		`)
		w.WhenIPublish("release", taggedAs("v1"), onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheCoordinatorWasToldTheWorkspaceIs("com.acme.storefront", "graph", "publish")
	})

	t.Run("An export carries the name", func(t *testing.T) {
		w := newWorkspaceWorld(t)
		w.GivenACoordinator()
		w.GivenTheProjectHolds(files{"WORKSPACE": `workspace(name = "com.acme.storefront")`, "hello.txt": "hello"})
		w.GivenThePackageDeclares(".", anAppOfAFile)
		w.WhenISave("app", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheCoordinatorWasToldTheWorkspaceIs("com.acme.storefront", "graph", "export")
	})

	t.Run("Two clones send the same name, and share what they build", func(t *testing.T) {
		w := newBuildingWorld(t)
		w.GivenACoordinator()
		hello := testenv.NewNonce()
		for _, clone := range []string{"first", "second"} {
			w.GivenTheProjectHolds(files{
				clone + "/WORKSPACE": `workspace(name = "com.acme.storefront")`,
				clone + "/hello.txt": hello,
			})
			w.GivenThePackageDeclares(clone, anAppOfAFile)
		}
		w.GivenIWorkIn("first")
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("app"): ran})
		w.ThenTheCoordinatorWasToldTheWorkspaceIs("com.acme.storefront", "graph")
		w.GivenIWorkIn("second")
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("app"): reused})
		w.ThenTheCoordinatorWasToldTheWorkspaceIs("com.acme.storefront", "graph")
	})
}

// TestWorkspaces_TheNameNeverAffectsWhatIsBuiltOrCached specifies that vamos never adds the name to an action's key,
// the graph or a step, so identical targets in workspaces named differently, or unnamed, share cached results,
// and renaming a workspace rebuilds nothing;
// and that a glob that selects the WORKSPACE file builds on its contents as on any other file's, the name included,
// so renaming the workspace rebuilds the targets that use that glob.
func TestWorkspaces_TheNameNeverAffectsWhatIsBuiltOrCached(t *testing.T) {
	parallelRule(t)
	t.Run("Renaming a workspace reuses its cache", func(t *testing.T) {
		w := newWorkspaceWorld(t)
		w.GivenTheProjectHolds(files{
			"WORKSPACE": `workspace(name = "com.acme.first")`,
			"hello.txt": testenv.NewNonce(),
		})
		w.GivenThePackageDeclares(".", anAppOfAFile)
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("app"): ran})
		w.GivenTheProjectHolds(files{"WORKSPACE": `workspace(name = "com.acme.second")`})
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("app"): reused})
	})

	t.Run("Renaming a workspace reuses its cache on a coordinator", func(t *testing.T) {
		w := newWorkspaceWorld(t)
		w.GivenACoordinator()
		w.GivenTheProjectHolds(files{
			"WORKSPACE": `workspace(name = "com.acme.first")`,
			"hello.txt": testenv.NewNonce(),
		})
		w.GivenThePackageDeclares(".", anAppOfAFile)
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("app"): ran})
		w.GivenTheProjectHolds(files{"WORKSPACE": `workspace(name = "com.acme.second")`})
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("app"): reused})
		w.ThenTheCoordinatorWasToldTheWorkspaceIs("com.acme.second", "graph")
	})

	t.Run("A step does not see the name", func(t *testing.T) {
		w := newWorkspaceWorld(t)
		w.GivenTheProjectHolds(files{"WORKSPACE": `workspace(name = "com.acme.storefront")`})
		w.GivenThePackageDeclares(".", testenv.PullBusybox)
		w.GivenThePackageDeclares(".", fmt.Sprintf(`
			task(name="probe", base="busybox", steps=[run(["sh", "-c", %q])])
		`, "echo "+testenv.NewNonce()+"; echo \"observed variables naming it=$(env | grep -c com.acme.storefront)\""))
		w.WhenIBuild("probe")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{task("probe"): ran})
		w.ThenTheStepsObserved(observations{"variables naming it": "0"})
	})

	t.Run("A glob that selects WORKSPACE builds on its contents", func(t *testing.T) {
		w := newWorkspaceWorld(t)
		nonce := testenv.NewNonce() // in both WORKSPACE files, so that neither was built before
		w.GivenTheProjectHolds(files{"WORKSPACE": "# " + nonce + "\nworkspace(name = \"com.acme.first\")"})
		w.GivenThePackageDeclares(".", `
			glob(name="src", include=["WORKSPACE"])
			layer(name="app", base="scratch", steps=[add("src", "/data")])
		`)
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("app"): ran})
		w.GivenTheProjectHolds(files{"WORKSPACE": "# " + nonce + "\nworkspace(name = \"com.acme.second\")"})
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("app"): ran})
	})
}
