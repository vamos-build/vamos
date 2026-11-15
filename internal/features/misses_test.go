package features

import (
	"fmt"
	"testing"
	"time"

	"github.com/vamos-build/vamos/internal/testenv"
)

// The miss attribution feature: why each layer or task that runs, rather than being reused, had to, as its span in
// the trace says, from the history the executor keeps of a named workspace's targets.

// newMissWorld starts a scenario of the miss attribution feature: a building world in the workspace named
// com.acme.app, whose WORKSPACE file says so, on stores of its own, so that its targets' history and cache are its
// alone and can be pruned and aged.
func newMissWorld(t *testing.T) *world {
	t.Helper()
	w := newUnnamedMissWorld(t)
	w.GivenTheProjectHolds(files{"WORKSPACE": `workspace(name = "com.acme.app")`})
	return w
}

// newUnnamedMissWorld starts a scenario of the miss attribution feature in a workspace without a name, on stores of
// its own.
func newUnnamedMissWorld(t *testing.T) *world {
	t.Helper()
	w := newBuildingWorld(t)
	w.GivenStoresOfTheScenariosOwn()
	return w
}

// anAppOfAGlob declares the glob "src" of the files named in include, and the layer "app" on scratch, which adds it,
// with nonce, so that its first build runs.
func anAppOfAGlob(nonce, include string) string {
	return `
		glob(name="src", include=[` + include + `])
		layer(name="app", base="scratch", steps=[env(NONCE="` + nonce + `"), add("src", "/src")])
	`
}

// anAppOnThePull declares the pull "base" of the scenario's registry's base:tag, and the layer "app" on it, with nonce,
// so that its first build runs.
func anAppOnThePull(nonce, tag string) string {
	return `
		pull(name="base", image="<registry>/base", tag="` + tag + `")
		layer(name="app", base="base", steps=[env(NONCE="` + nonce + `")])
	`
}

// TestMisses_EveryMissHasOneCause specifies, as the cache reference says, that "each layer or task that runs, rather
// than being reused, gets one cause in its trace".
func TestMisses_EveryMissHasOneCause(t *testing.T) {
	t.Run("A reused target has no cause", func(t *testing.T) {
		w := newMissWorld(t)
		w.GivenTheBuildFileDeclares(anAppOfAGlob(testenv.NewNonce(), `"a.txt"`))
		w.GivenTheProjectHolds(files{"a.txt": "one"})
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.WhenIBuild("app", traced())
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("app"): reused})
		w.ThenTheTraceSpanSaysNothingOf("executor", `layer "app"`, "miss")
	})
}

// TestMisses_AForcedBuildIsForced specifies, as the cache reference says, that a target is "forced" when "--rebuild
// named it, whatever else changed".
func TestMisses_AForcedBuildIsForced(t *testing.T) {
	t.Run("--rebuild forces a target", func(t *testing.T) {
		w := newMissWorld(t)
		w.GivenTheBuildFileDeclares(anAppOfAGlob(testenv.NewNonce(), `"a.txt"`))
		w.GivenTheProjectHolds(files{"a.txt": "one"})
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.GivenTheProjectHolds(files{"a.txt": "two"}) // whatever else changed
		w.WhenIBuild("app", rebuilding(), traced())
		w.ThenItSucceeds()
		w.ThenTheTraceSpanSaysAtLeast("executor", `layer "app"`, traceArgs{"miss": "forced"})
	})
}

// TestMisses_TargetHistory specifies, as the cache reference says, that "builds of different branches don't inflate
// each other's changes": a build that returns to an earlier branch's action is compared with that branch's build.
func TestMisses_TargetHistory(t *testing.T) {
	t.Run("Two branches' builds don't look like changes", func(t *testing.T) {
		w := newMissWorld(t)
		w.GivenTheBuildFileDeclares(anAppOfAGlob(testenv.NewNonce(), `"a.txt"`))
		w.GivenTheProjectHolds(files{"a.txt": "one"})
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.GivenTheProjectHolds(files{"a.txt": "two"})
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.GivenTheProjectHolds(files{"a.txt": "one"})
		w.WhenIPrune()
		w.ThenItSucceeds()
		w.WhenIBuild("app", traced())
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("app"): ran})
		w.ThenTheTraceSpanSaysAtLeast("executor", `layer "app"`, traceArgs{"miss": "lost"})
	})
}

// TestMisses_ConcurrentBuildsEachAddTheirAction specifies, as the cache reference says, that "builds of a target that
// run at once each add their action to its history".
func TestMisses_ConcurrentBuildsEachAddTheirAction(t *testing.T) {
	t.Run("Concurrent builds each add their action", func(t *testing.T) {
		w := newMissWorld(t)
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.GivenAProjectThatPullsBusybox()
		w.GivenTheBuildFileDeclares(`
			glob(name="src", include=["a.txt"])
			task(name="app", base="busybox", steps=[env(NONCE="` + testenv.NewNonce() + `"), add("src", "/src"),
			    run(["sleep", "2"])])
		`)
		w.GivenTwoClientsBuiltAtOnceOnTheCoordinator("app", files{"a.txt": "one"}, files{"a.txt": "two"},
			onTheCoordinator("<coordinator>"))
		w.WhenIPrune(onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		for _, source := range []string{"one", "two"} {
			w.GivenTheProjectHolds(files{"a.txt": source})
			w.WhenIBuild("app", onTheCoordinator("<coordinator>"), traced())
			w.ThenItSucceeds()
			w.ThenTheTargetsWere(statuses{task("app"): ran})
			w.ThenTheTraceSpanSaysAtLeast("executor", `task "app"`, traceArgs{"miss": "lost"})
		}
	})
}

// TestMisses_HistoryKeepsTheLastEightActions specifies, as the cache reference says, that history holds "the last 8
// actions its builds used".
func TestMisses_HistoryKeepsTheLastEightActions(t *testing.T) {
	t.Run("A ninth action makes the first be forgotten", func(t *testing.T) {
		w := newMissWorld(t)
		w.GivenTheBuildFileDeclares(anAppOfAGlob(testenv.NewNonce(), `"a.txt"`))
		for i := range 9 {
			w.GivenTheProjectHolds(files{"a.txt": fmt.Sprint("version ", i)})
			w.WhenIBuild("app")
			w.ThenItSucceeds()
		}
		w.WhenIPrune()
		w.ThenItSucceeds()
		// version 1 before 0: building 0 adds an entry, which would push version 1 out of the history
		for _, check := range []struct {
			version int
			want    string
		}{{1, "lost"}, {0, "own-inputs"}} {
			w.GivenTheProjectHolds(files{"a.txt": fmt.Sprint("version ", check.version)})
			w.WhenIBuild("app", traced())
			w.ThenItSucceeds()
			w.ThenTheTraceSpanSaysAtLeast("executor", `layer "app"`, traceArgs{"miss": check.want})
		}
	})
}

// TestMisses_TheWorkspaceNameKeysTargetHistory specifies, as the cache reference says, that history is kept "in a
// named workspace", whose name keys it, so that a target is "new" in "an unnamed workspace".
func TestMisses_TheWorkspaceNameKeysTargetHistory(t *testing.T) {
	t.Run("Another workspace's history is not compared", func(t *testing.T) {
		w := newMissWorld(t)
		nonce := testenv.NewNonce()
		w.GivenTheBuildFileDeclares(anAppOfAGlob(nonce, `"a.txt"`))
		w.GivenTheProjectHolds(files{"a.txt": "one", "b.txt": "b"})
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.GivenTheProjectHolds(files{"WORKSPACE": `workspace(name = "com.acme.second")`})
		w.GivenTheBuildFileChanges(`include=["a.txt"]`, `include=["a.txt", "b.txt"]`)
		w.WhenIBuild("app", traced())
		w.ThenItSucceeds()
		w.ThenTheTraceSpanSaysAtLeast("executor", `layer "app"`, traceArgs{"miss": "new"})
	})

	t.Run("An unnamed workspace's misses are new", func(t *testing.T) {
		w := newUnnamedMissWorld(t)
		nonce := testenv.NewNonce()
		w.GivenTheBuildFileDeclares(anAppOfAGlob(nonce, `"a.txt"`))
		w.GivenTheProjectHolds(files{"a.txt": "one"})
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.GivenTheBuildFileChanges(`env(NONCE="`+nonce+`")`, `env(NONCE="`+nonce+`", MODE="changed")`)
		w.WhenIBuild("app", traced())
		w.ThenItSucceeds()
		w.ThenTheTraceSpanSaysAtLeast("executor", `layer "app"`, traceArgs{"miss": "new"})
	})
}

// TestMisses_ATargetWithoutComparableHistoryIsNew specifies, as the cache reference says, that a target is "new" when
// "it has no earlier build to compare with: a first build, a new configuration, or an unnamed workspace".
func TestMisses_ATargetWithoutComparableHistoryIsNew(t *testing.T) {
	t.Run("A target's first build is new", func(t *testing.T) {
		w := newMissWorld(t)
		w.GivenTheBuildFileDeclares(anAppOfAGlob(testenv.NewNonce(), `"a.txt"`))
		w.GivenTheProjectHolds(files{"a.txt": "one"})
		w.WhenIBuild("app", traced())
		w.ThenItSucceeds()
		w.ThenTheTraceSpanSaysAtLeast("executor", `layer "app"`, traceArgs{"miss": "new"})
	})

	t.Run("A target in a new configuration is new", func(t *testing.T) {
		w := newMissWorld(t)
		w.GivenTheBuildFileDeclares(`
			layer(name="app", base="scratch", params={"mode": "debug"},
			    steps=lambda ctx: [env(NONCE="` + testenv.NewNonce() + `", MODE=ctx.mode)])
		`)
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.WhenIBuild("app", setting("mode=release"), traced())
		w.ThenItSucceeds()
		w.ThenTheTraceSpanSaysAtLeast("executor", `layer "app" (mode="release")`, traceArgs{"miss": "new"})
	})
}

// TestMisses_AResultThatIsGoneIsLost specifies, as the cache reference says, that a target is "lost" when "its action
// is one it built before, but the result is gone or can't be used, as after a prune".
func TestMisses_AResultThatIsGoneIsLost(t *testing.T) {
	t.Run("A target built again after a prune is lost", func(t *testing.T) {
		w := newMissWorld(t)
		w.GivenTheBuildFileDeclares(anAppOfAGlob(testenv.NewNonce(), `"a.txt"`))
		w.GivenTheProjectHolds(files{"a.txt": "one"})
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.WhenIPrune()
		w.ThenItSucceeds()
		w.WhenIBuild("app", traced())
		w.ThenItSucceeds()
		w.ThenTheTraceSpanSaysAtLeast("executor", `layer "app"`, traceArgs{"miss": "lost"})
	})
}

// TestMisses_AttributionComparesWithTheNearestEntry specifies, as the cache reference says, that "a miss is compared
// with the history entry that differs from it least".
func TestMisses_AttributionComparesWithTheNearestEntry(t *testing.T) {
	t.Run("The nearest entry is compared, not the latest", func(t *testing.T) {
		w := newMissWorld(t)
		w.GivenTheBuildFileDeclares(anAppOfAGlob(testenv.NewNonce(), `"a.txt", "b.txt"`))
		w.GivenTheProjectHolds(files{"a.txt": "one", "b.txt": "one"})
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.GivenTheProjectHolds(files{"a.txt": "two", "b.txt": "two"})
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.GivenTheProjectHolds(files{"a.txt": "one", "b.txt": "three"})
		w.WhenIBuild("app", traced())
		w.ThenItSucceeds()
		w.ThenTheTraceSpanSaysAtLeast("executor", `layer "app"`,
			traceArgs{"miss": "own-inputs", "changed": []any{"b.txt"}})
		w.ThenTheTraceSpanSaysNothingOf("executor", `layer "app"`, "added", "removed")
	})
}

// TestMisses_AChangeOfTheTargetsOwnInputsIsOwnInputs specifies, as the cache reference says, that a target misses for
// "own-inputs" when "a step, command, output, source file, or which target supplies its base or files changed; a
// source's added, removed and changed files are named".
func TestMisses_AChangeOfTheTargetsOwnInputsIsOwnInputs(t *testing.T) {
	t.Run("A layer that became a task is own-inputs", func(t *testing.T) {
		w := newMissWorld(t)
		nonce := testenv.NewNonce()
		w.GivenTheBuildFileDeclares(`layer(name="app", base="scratch", steps=[env(NONCE="` + nonce + `")])`)
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.GivenTheBuildFileChanges(`layer(name="app", base="scratch",`, `task(name="app", base="scratch",`)
		w.WhenIBuild("app", traced())
		w.ThenItSucceeds()
		w.ThenTheTraceSpanSaysAtLeast("executor", `task "app"`, traceArgs{"miss": "own-inputs"})
	})

	t.Run("A changed output directory is own-inputs", func(t *testing.T) {
		w := newMissWorld(t)
		w.GivenTheBuildFileDeclares(`
			glob(name="src", include=["a.txt"])
			task(name="gen", base="scratch", steps=[env(NONCE="` + testenv.NewNonce() + `"), add("src", "/one"),
			    add("src", "/two")], source="/one", include=["**"])
		`)
		w.GivenTheProjectHolds(files{"a.txt": "a"})
		w.WhenIBuild("gen")
		w.ThenItSucceeds()
		w.GivenTheBuildFileChanges(`source="/one"`, `source="/two"`)
		w.WhenIBuild("gen", traced())
		w.ThenItSucceeds()
		w.ThenTheTraceSpanSaysAtLeast("executor", `task "gen"`, traceArgs{"miss": "own-inputs"})
		w.ThenTheTraceSpanSaysNothingOf("executor", `task "gen"`, "added", "removed", "changed")
	})

	t.Run("A changed step is own-inputs", func(t *testing.T) {
		w := newMissWorld(t)
		nonce := testenv.NewNonce()
		w.GivenTheBuildFileDeclares(anAppOfAGlob(nonce, `"a.txt"`))
		w.GivenTheProjectHolds(files{"a.txt": "one"})
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.GivenTheBuildFileChanges(`env(NONCE="`+nonce+`")`, `env(NONCE="`+nonce+`", MODE="changed")`)
		w.WhenIBuild("app", traced())
		w.ThenItSucceeds()
		w.ThenTheTraceSpanSaysAtLeast("executor", `layer "app"`, traceArgs{"miss": "own-inputs"})
		w.ThenTheTraceSpanSaysNothingOf("executor", `layer "app"`, "added", "removed", "changed")
	})

	t.Run("A base switched to another layer is own-inputs", func(t *testing.T) {
		w := newMissWorld(t)
		nonce := testenv.NewNonce()
		w.GivenTheBuildFileDeclares(`
			layer(name="one", base="scratch", steps=[env(ONE="` + nonce + `")])
			layer(name="two", base="scratch", steps=[env(TWO="` + nonce + `")])
			layer(name="app", base="one", steps=[env(APP="app")])
		`)
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.GivenTheBuildFileChanges(`layer(name="app", base="one"`, `layer(name="app", base="two"`)
		w.WhenIBuild("app", traced())
		w.ThenItSucceeds()
		w.ThenTheTraceSpanSaysAtLeast("executor", `layer "app"`, traceArgs{"miss": "own-inputs"})
	})

	t.Run("A base switched to another pull is own-inputs", func(t *testing.T) {
		w := newMissWorld(t)
		w.GivenARegistry()
		w.GivenTheRegistryHoldsAnImage("<registry>/one:v1", "one", thisPlatform())
		w.GivenTheRegistryHoldsAnImage("<registry>/two:v1", "two", thisPlatform())
		w.GivenTheBuildFileDeclares(`
			pull(name="one", image="<registry>/one", tag="v1")
			pull(name="two", image="<registry>/two", tag="v1")
			layer(name="app", base="one", steps=[env(NONCE="` + testenv.NewNonce() + `")])
		`)
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.GivenTheBuildFileChanges(`layer(name="app", base="one"`, `layer(name="app", base="two"`)
		w.WhenIBuild("app", traced())
		w.ThenItSucceeds()
		w.ThenTheTraceSpanSaysAtLeast("executor", `layer "app"`, traceArgs{"miss": "own-inputs"})
		w.ThenTheTraceSpanSaysNothingOf("executor", `layer "app"`, "pull", "dependencies")
	})

	t.Run("A change of mode alone is a change", func(t *testing.T) {
		w := newMissWorld(t)
		w.GivenTheBuildFileDeclares(anAppOfAGlob(testenv.NewNonce(), `"run.sh"`))
		w.GivenTheProjectHolds(files{"run.sh": "echo hello"})
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.GivenTheProjectModes(modes{"run.sh": 0o755})
		w.WhenIBuild("app", traced())
		w.ThenItSucceeds()
		w.ThenTheTraceSpanSaysAtLeast("executor", `layer "app"`,
			traceArgs{"miss": "own-inputs", "changed": []any{"run.sh"}})
	})

	t.Run("A glob that selects too much shows the files it picked up", func(t *testing.T) {
		w := newMissWorld(t)
		w.GivenTheBuildFileDeclares(anAppOfAGlob(testenv.NewNonce(), `"src/**"`))
		w.GivenTheProjectHolds(files{"src/main.go": "package main"})
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.GivenTheProjectHolds(files{"src/.main.go.swp": "swap"})
		w.WhenIBuild("app", traced())
		w.ThenItSucceeds()
		w.ThenTheTraceSpanSaysAtLeast("executor", `layer "app"`,
			traceArgs{"miss": "own-inputs", "added": []any{"src/.main.go.swp"}})
		w.ThenTheTraceSpanSaysNothingOf("executor", `layer "app"`, "removed", "changed")
	})

	t.Run("Added, removed and changed files are named", func(t *testing.T) {
		w := newMissWorld(t)
		w.GivenTheBuildFileDeclares(anAppOfAGlob(testenv.NewNonce(), `"*.txt"`))
		w.GivenTheProjectHolds(files{"a.txt": "a", "b.txt": "b", "c.txt": "c"})
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.GivenTheProjectHolds(files{"a.txt": "a, edited", "d.txt": "d"})
		w.GivenTheProjectLacks("b.txt")
		w.WhenIBuild("app", traced())
		w.ThenItSucceeds()
		w.ThenTheTraceSpanSaysAtLeast("executor", `layer "app"`, traceArgs{"miss": "own-inputs",
			"added": []any{"d.txt"}, "removed": []any{"b.txt"}, "changed": []any{"a.txt"}})
	})
}

// TestMisses_AChangeOnlyInWhatADependencySuppliesIsDependency specifies, as the cache reference says, that a target
// misses for "dependency" when "only what a target it depends on supplies changed, as a base layer's image or an added
// task's files; each is named".
func TestMisses_AChangeOnlyInWhatADependencySuppliesIsDependency(t *testing.T) {
	t.Run("A layer on a changed layer", func(t *testing.T) {
		w := newMissWorld(t)
		nonce := testenv.NewNonce()
		w.GivenTheBuildFileDeclares(`
			layer(name="runtime", base="scratch", steps=[env(NONCE="` + nonce + `")])
			layer(name="app", base="runtime", steps=[env(APP="app")])
		`)
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.GivenTheBuildFileChanges(`env(NONCE="`+nonce+`")`, `env(NONCE="`+nonce+`", MODE="changed")`)
		w.WhenIBuild("app", traced())
		w.ThenItSucceeds()
		w.ThenTheTraceSpanSaysAtLeast("executor", `layer "runtime"`, traceArgs{"miss": "own-inputs"})
		w.ThenTheTraceSpanSaysAtLeast("executor", `layer "app"`,
			traceArgs{"miss": "dependency", "dependencies": []any{`layer "runtime"`}})
	})

	t.Run("A changed base layer and a rebuilt task together are dependency, naming both", func(t *testing.T) {
		w := newMissWorld(t)
		nonce := testenv.NewNonce()
		w.GivenTheBuildFileDeclares(`
			glob(name="src", include=["a.txt"])
			layer(name="runtime", base="scratch", steps=[env(NONCE="` + nonce + `")])
			task(name="gen", base="scratch", steps=[env(GEN="` + nonce + `"), add("src", "/out")],
			    source="/out", include=["**"])
			layer(name="app", base="runtime", steps=[add("gen", "/gen")])
		`)
		w.GivenTheProjectHolds(files{"a.txt": "a"})
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.GivenTheBuildFileChanges(`env(NONCE="`+nonce+`")`, `env(NONCE="`+nonce+`", MODE="changed")`)
		w.WhenIBuild("gen", rebuilding(), stampedAt("2030-01-01T00:00:00Z"))
		w.ThenItSucceeds()
		w.WhenIBuild("app", traced())
		w.ThenItSucceeds()
		w.ThenTheTraceSpanSaysAtLeast("executor", `layer "app"`,
			traceArgs{"miss": "dependency", "dependencies": []any{`layer "runtime"`, `task "gen"`}})
	})

	t.Run("A pull whose tag was edited", func(t *testing.T) {
		w := newMissWorld(t)
		w.GivenARegistry()
		w.GivenTheRegistryHoldsAnImage("<registry>/base:v1", "v1", thisPlatform())
		w.GivenTheRegistryHoldsAnImage("<registry>/base:v2", "v2", thisPlatform())
		w.GivenTheBuildFileDeclares(anAppOnThePull(testenv.NewNonce(), "v1"))
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.GivenTheBuildFileChanges(`tag="v1"`, `tag="v2"`)
		w.WhenIBuild("app", traced())
		w.ThenItSucceeds()
		w.ThenTheTraceSpanSaysAtLeast("executor", `layer "app"`,
			traceArgs{"miss": "dependency", "dependencies": []any{`pull "base"`}})
	})

	t.Run("A base switched to an identical layer, then changed", func(t *testing.T) {
		w := newMissWorld(t)
		nonce := testenv.NewNonce()
		w.GivenTheBuildFileDeclares(`
			layer(name="one", base="scratch", steps=[env(SAME="` + nonce + `")])
			layer(name="two", base="scratch", steps=[env(SAME="` + nonce + `")])
			layer(name="app", base="one", steps=[env(APP="app")])
		`)
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.GivenTheBuildFileChanges(`layer(name="app", base="one"`, `layer(name="app", base="two"`)
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("app"): reused})
		w.GivenTheBuildFileChanges(`layer(name="two", base="scratch", steps=[env(SAME="`+nonce+`")])`,
			`layer(name="two", base="scratch", steps=[env(SAME="`+nonce+`", MODE="changed")])`)
		w.WhenIBuild("app", traced())
		w.ThenItSucceeds()
		w.ThenTheTraceSpanSaysAtLeast("executor", `layer "app"`,
			traceArgs{"miss": "dependency", "dependencies": []any{`layer "two"`}})
	})

	t.Run("A moved pull and a changed task together are dependency", func(t *testing.T) {
		w := newMissWorld(t)
		w.GivenARegistry()
		w.GivenTheRegistryHoldsAnImage("<registry>/base:v1", "first", thisPlatform())
		nonce := testenv.NewNonce()
		w.GivenTheBuildFileDeclares(`
			glob(name="src", include=["a.txt"])
			pull(name="base", image="<registry>/base", tag="v1")
			task(name="gen", base="scratch", steps=[env(GEN="` + nonce + `"), add("src", "/out")],
			    source="/out", include=["**"])
			layer(name="app", base="base", steps=[add("gen", "/gen")])
		`)
		w.GivenTheProjectHolds(files{"a.txt": "a"})
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.GivenTheRegistryHoldsAnImage("<registry>/base:v1", "moved", thisPlatform())
		w.GivenTheLockfileHolds(lockfilePinning(pin{"<registry>/base:v1", "v1", "<digest of moved>"}))
		w.GivenTheProjectHolds(files{"a.txt": "a, edited"}) // which changes the files gen makes
		w.WhenIBuild("app", traced())
		w.ThenItSucceeds()
		w.ThenTheTraceSpanSaysAtLeast("executor", `layer "app"`,
			traceArgs{"miss": "dependency", "dependencies": []any{`pull "base"`, `task "gen"`}})
	})

	t.Run("A rebuilt task changes its epoch alone", func(t *testing.T) {
		w := newMissWorld(t)
		w.GivenTheBuildFileDeclares(`
			glob(name="src", include=["a.txt"])
			task(name="gen", base="scratch", steps=[env(NONCE="` + testenv.NewNonce() + `"), add("src", "/out")],
			    source="/out", include=["**"])
			layer(name="app", base="scratch", steps=[add("gen", "/gen")])
		`)
		w.GivenTheProjectHolds(files{"a.txt": "a"})
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.WhenIBuild("gen", rebuilding(), stampedAt("2030-01-01T00:00:00Z"))
		w.ThenItSucceeds()
		w.WhenIBuild("app", traced())
		w.ThenItSucceeds()
		w.ThenTheTraceSpanSaysAtLeast("executor", `layer "app"`,
			traceArgs{"miss": "dependency", "dependencies": []any{`task "gen"`}})
	})
}

// TestMisses_AMovedPullIsUpstream specifies, as the cache reference says, that a target misses for "upstream" when
// "only the image its base pull's reference names moved: a tag that moved, or an updated pin; the pull and both
// digests are named".
func TestMisses_AMovedPullIsUpstream(t *testing.T) {
	t.Run("A tag that moved, updated with vamos update", func(t *testing.T) {
		w := newMissWorld(t)
		w.GivenARegistry()
		w.GivenTheRegistryHoldsAnImage("<registry>/base:v1", "first", thisPlatform())
		w.GivenTheBuildFileDeclares(anAppOnThePull(testenv.NewNonce(), "v1"))
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.GivenTheRegistryHoldsAnImage("<registry>/base:v1", "moved", thisPlatform())
		w.WhenIUpdate(onlyThePulls("base"))
		w.ThenItSucceeds()
		w.WhenIBuild("app", traced())
		w.ThenItSucceeds()
		w.ThenTheTraceSpanSaysAtLeast("executor", `layer "app"`, traceArgs{"miss": "upstream",
			"pull": `pull "base"`, "before": "<digest of first>", "after": "<digest of moved>"})
	})

	t.Run("An updated pin", func(t *testing.T) {
		w := newMissWorld(t)
		w.GivenARegistry()
		w.GivenTheRegistryHoldsAnImage("<registry>/base:v1", "first", thisPlatform())
		w.GivenTheBuildFileDeclares(anAppOnThePull(testenv.NewNonce(), "v1"))
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.GivenTheRegistryHoldsAnImage("<registry>/base:v1", "moved", thisPlatform())
		w.GivenTheLockfileHolds(lockfilePinning(pin{"<registry>/base:v1", "v1", "<digest of moved>"}))
		w.WhenIBuild("app", traced())
		w.ThenItSucceeds()
		w.ThenTheTraceSpanSaysAtLeast("executor", `layer "app"`, traceArgs{"miss": "upstream",
			"pull": `pull "base"`, "before": "<digest of first>", "after": "<digest of moved>"})
	})

	t.Run("A tag moved to another manifest of the same image is compared from there", func(t *testing.T) {
		w := newMissWorld(t)
		w.GivenARegistry()
		w.GivenTheRegistryHoldsAnImage("<registry>/base:v1", "first", thisPlatform())
		w.GivenTheBuildFileDeclares(anAppOnThePull(testenv.NewNonce(), "v1"))
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.GivenTheRegistryHoldsAnIndexOf("<registry>/base:v1", "same", "first", thisPlatform())
		w.WhenIUpdate(onlyThePulls("base"))
		w.ThenItSucceeds()
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("app"): reused})
		w.GivenTheRegistryHoldsAnImage("<registry>/base:v1", "moved", thisPlatform())
		w.WhenIUpdate(onlyThePulls("base"))
		w.ThenItSucceeds()
		w.WhenIBuild("app", traced())
		w.ThenItSucceeds()
		w.ThenTheTraceSpanSaysAtLeast("executor", `layer "app"`, traceArgs{"miss": "upstream",
			"pull": `pull "base"`, "before": "<digest of same>", "after": "<digest of moved>"})
	})
}

// TestMisses_HistorySurvivesAPrune specifies, as the cache reference says, that "history names no file of the cache,
// so it survives a prune", which "keeps the history of a target built or reused in the last 90 days, and removes it
// once its target has been neither built nor reused for 100".
func TestMisses_HistorySurvivesAPrune(t *testing.T) {
	t.Run("A changed source after a prune is still own-inputs", func(t *testing.T) {
		w := newMissWorld(t)
		w.GivenTheBuildFileDeclares(anAppOfAGlob(testenv.NewNonce(), `"*.txt"`))
		w.GivenTheProjectHolds(files{"a.txt": "a"})
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.WhenIPrune()
		w.ThenItSucceeds()
		w.GivenTheProjectHolds(files{"a.txt": "a, edited"})
		w.WhenIBuild("app", traced())
		w.ThenItSucceeds()
		w.ThenTheTraceSpanSaysAtLeast("executor", `layer "app"`,
			traceArgs{"miss": "own-inputs", "changed": []any{"a.txt"}})
	})

	t.Run("History of a target reused since it aged stays at a prune", func(t *testing.T) {
		w := newMissWorld(t)
		w.GivenTheBuildFileDeclares(anAppOfAGlob(testenv.NewNonce(), `"*.txt"`))
		w.GivenTheProjectHolds(files{"a.txt": "a"})
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.GivenTheHistoryOfWasLastUsed("com.acme.app", "//:app", 120*24*time.Hour)
		w.WhenIBuild("app") // reused, which uses the history again
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("app"): reused})
		w.WhenIPrune()
		w.ThenItSucceeds()
		w.GivenTheProjectHolds(files{"a.txt": "a, edited"})
		w.WhenIBuild("app", traced())
		w.ThenItSucceeds()
		w.ThenTheTraceSpanSaysAtLeast("executor", `layer "app"`,
			traceArgs{"miss": "own-inputs", "changed": []any{"a.txt"}})
	})

	for _, example := range []struct {
		name string
		ago  time.Duration
		want traceArgs
	}{
		{name: "History last used 99 days ago stays at a prune", ago: 99 * 24 * time.Hour,
			want: traceArgs{"miss": "own-inputs", "changed": []any{"a.txt"}}},
		{name: "History last used 101 days ago goes at a prune",
			ago: 101 * 24 * time.Hour, want: traceArgs{"miss": "new"}},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newMissWorld(t)
			w.GivenTheBuildFileDeclares(anAppOfAGlob(testenv.NewNonce(), `"*.txt"`))
			w.GivenTheProjectHolds(files{"a.txt": "a"})
			w.WhenIBuild("app")
			w.ThenItSucceeds()
			w.GivenTheHistoryOfWasLastUsed("com.acme.app", "//:app", example.ago)
			w.WhenIPrune()
			w.ThenItSucceeds()
			w.GivenTheProjectHolds(files{"a.txt": "a, edited"})
			w.WhenIBuild("app", traced())
			w.ThenItSucceeds()
			w.ThenTheTraceSpanSaysAtLeast("executor", `layer "app"`, example.want)
		})
	}
}
