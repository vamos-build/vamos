package features

import (
	"fmt"
	"path"
	"strings"
	"testing"

	"github.com/vamos-build/vamos/internal/testenv"
)

// newToolCacheMistakeWorld starts a scenario of the tool caches feature that fails before anything is built,
// so that it needs no buildah: a workspace named com.acme.shop, whose BUILD files declare tool caches with
// tool_cache() and mount them in tasks and layers with tool_caches.
func newToolCacheMistakeWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	w.GivenTheProjectHolds(files{"WORKSPACE": `workspace(name = "com.acme.shop")`})
	return w
}

// newToolCacheWorld starts a scenario of the tool caches feature, from its background: a workspace named
// com.acme.shop, whose BUILD file pulls busybox and declares tool caches with tool_cache() and mounts them in tasks
// and layers with tool_caches.
func newToolCacheWorld(t *testing.T) *world {
	t.Helper()
	w := newBuildingWorld(t)
	w.GivenTheProjectHolds(files{"WORKSPACE": `workspace(name = "com.acme.shop")`})
	w.GivenAProjectThatPullsBusybox()
	return w
}

// aBuildMountingTool declares the tool cache "tool" at /cache, with the owner and group owner gives, if any, and the
// task "build", which mounts it as mount gives and echoes nonce, so that it runs the first time.
func aBuildMountingTool(owner, mount, nonce string) string {
	return fmt.Sprintf(`
		tool_cache(name = "tool", path = "/cache"%s)
		task(name = "build", base = "busybox", tool_caches = [%s], steps = [run(["echo", %q])])
	`, owner, mount, nonce)
}

// theGoBuildCache declares, in the package tools, the tool cache "go-build" at /cache/go-build.
const theGoBuildCache = `tool_cache(name = "go-build", path = "/cache/go-build")`

// TestToolCaches_TheToolCacheRuleDeclaresACache specifies that a BUILD file declares a tool cache with
// tool_cache(name, path), and optionally owner and group, and no other argument, and that a mistake in its arguments
// fails as its BUILD file is evaluated, whether or not any target mounts the cache.
func TestToolCaches_TheToolCacheRuleDeclaresACache(t *testing.T) {
	parallelRule(t)
	t.Run("A tool_cache rule needs a path", func(t *testing.T) {
		w := newToolCacheMistakeWorld(t)
		w.GivenTheBuildFileDeclares(`
			tool_cache(name = "go-build")
			task(name = "app", base = "scratch", steps = [run(["true"])])
		`)
		w.WhenIBuild("app")
		w.ThenItFailsWith("/project/BUILD:1:11: tool_cache: missing argument for path")
		w.ThenNoTargetRan()
	})

	t.Run("A tool_cache rule names no variable", func(t *testing.T) {
		w := newToolCacheMistakeWorld(t)
		w.GivenTheBuildFileDeclares(`
			tool_cache(name = "go-build", path = "/cache/go-build", env = "GOCACHE")
			task(name = "app", base = "scratch", steps = [run(["true"])])
		`)
		w.WhenIBuild("app")
		w.ThenItFailsWith(`/project/BUILD:1:11: tool_cache: unexpected keyword argument "env"`)
		w.ThenNoTargetRan()
	})
}

// TestToolCaches_AToolCacheIsMountedNeverBuilt specifies that a tool_cache rule is no target: every command that takes
// a target refuses its label, and a base, dep(), add() or publication check that names it fails, each saying that a
// tool cache is mounted with mount(); that vamos update passes over it; and that a name that is neither suggests
// tool caches only to mount(), and targets only elsewhere.
func TestToolCaches_AToolCacheIsMountedNeverBuilt(t *testing.T) {
	parallelRule(t)
	t.Run("Building a tool cache", func(t *testing.T) {
		w := newToolCacheMistakeWorld(t)
		w.GivenThePackageDeclares("tools", theGoBuildCache)
		w.WhenIBuild("//tools:go-build")
		w.ThenItFailsWith(`"//tools:go-build" is a tool_cache(), which is no target: ` +
			"a tool cache is mounted with mount(), in a task's or layer's tool_caches")
		w.ThenNoTargetRan()
	})

	t.Run("Updating a tool cache", func(t *testing.T) {
		w := newToolCacheMistakeWorld(t)
		w.GivenThePackageDeclares("tools", theGoBuildCache)
		w.WhenIUpdate(onlyThePulls("//tools:go-build"))
		w.ThenItFailsWith(`"//tools:go-build" is a tool_cache(), which is no target: ` +
			"a tool cache is mounted with mount(), in a task's or layer's tool_caches")
		w.ThenThereIsNoLockfile()
	})

	t.Run("A workspace-wide update passes over tool caches", func(t *testing.T) {
		w := newToolCacheMistakeWorld(t)
		w.GivenTheRegistryListsTags("<registry>/base", "v1")
		w.GivenTheLockfileHolds(lockfilePinning(
			pin{"<registry>/base:v1", "v1", "<digest of <registry>/base:v1>"},
			pin{"<registry>/old:v1", "v1", "sha256:" + strings.Repeat("0", 64)},
		))
		w.GivenTheBuildFileDeclares(pullOfBase("v1"))
		w.GivenTheBuildFileDeclares(`
			tool_cache(name = "tool", path = "/cache")
			task(name = "app", base = "base", tool_caches = [mount("tool")], steps = [run(["true"])])
		`)
		w.WhenIUpdate()
		w.ThenItSucceeds()
		w.ThenTheLockfileIs(lockfilePinning(pin{"<registry>/base:v1", "v1", "<digest of <registry>/base:v1>"}))
	})

	t.Run("A misspelled target suggests targets alone", func(t *testing.T) {
		w := newToolCacheMistakeWorld(t)
		w.GivenTheBuildFileDeclares(`
			tool_cache(name = "gocache", path = "/cache")
			layer(name = "app", base = "scratch")
		`)
		w.WhenIBuild("gocahce")
		w.ThenItFailsWith(`/project/BUILD has no target "gocahce"; its targets are "app"`)
		w.ThenNoTargetRan()
	})

	t.Run("A misspelled cache that a function of ctx mounts suggests tool caches alone", func(t *testing.T) {
		w := newToolCacheMistakeWorld(t)
		w.GivenTheBuildFileDeclares(`
			tool_cache(name = "gocache", path = "/cache")
			layer(name = "gobuild", base = "scratch")
			task(name = "app", base = "scratch", tool_caches = lambda ctx: [mount("gobuld")], steps = [run(["true"])])
		`)
		w.WhenIBuild("app")
		w.ThenItFailsWith(`/project/BUILD:3:5: task "app": tool_caches[0] "gobuld": ` +
			`/project/BUILD has no tool cache "gobuld"; its tool caches are "gocache" (configuring //:app)`)
		w.ThenNoTargetRan()
	})

	t.Run("Depending on a tool cache", func(t *testing.T) {
		w := newToolCacheMistakeWorld(t)
		w.GivenTheBuildFileDeclares(`
			tool_cache(name = "tool", path = "/cache")
			task(name = "app", base = "scratch", steps = [add(dep("tool"), "/tool"), run(["true"])])
		`)
		w.WhenIBuild("app")
		w.ThenItFailsWith(`/project/BUILD:2:54: dep: "tool" is a tool_cache(), which is no target: ` +
			"a tool cache is mounted with mount(), in a task's or layer's tool_caches")
		w.ThenNoTargetRan()
	})

	for _, example := range []struct {
		name  string
		build string // a target that names the tool cache "tool"
		error string // what building app reports
	}{
		{
			name:  "A base that names a tool cache",
			build: `layer(name = "app", base = "tool")`,
			error: `/project/BUILD:2:6: layer: base: "tool" is a tool_cache()`,
		},
		{
			name: "A publication's check that names a tool cache",
			build: `
				layer(name = "image", base = "scratch")
				publication(name = "app", images = {"registry.example/app": "image"}, checks = ["tool"])
			`,
			error: `/project/BUILD:3:12: publication: checks[0]: "tool" is a tool_cache()`,
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newToolCacheMistakeWorld(t)
			w.GivenTheBuildFileDeclares(`tool_cache(name = "tool", path = "/cache")`)
			w.GivenTheBuildFileDeclares(example.build)
			w.WhenIBuild("app")
			w.ThenItFailsWith(example.error + ", which is no target: " +
				"a tool cache is mounted with mount(), in a task's or layer's tool_caches")
			w.ThenNoTargetRan()
		})
	}
}

// TestToolCaches_WhatACacheHoldsIsOutsideEveryKey specifies that a cache's contents, whether it holds anything, where
// the executor keeps it and the workspace's name are no part of any action's key, while the path each cache is mounted
// at, whether the cache's or the mount's, the cache's label, and its owner and group are.
func TestToolCaches_WhatACacheHoldsIsOutsideEveryKey(t *testing.T) {
	parallelRule(t)
	t.Run("Overriding a cache's path with its own path keeps the hit", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenTheBuildFileDeclares(aBuildMountingTool("", `mount("tool")`, testenv.NewNonce()))
		w.WhenIBuild("build")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{task("build"): ran})
		w.GivenTheBuildFileChanges(`mount("tool")`, `mount("tool", path = "/cache")`)
		w.WhenIBuild("build")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{task("build"): reused})
	})

	t.Run("A changed cache does not cost a hit", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenTheBuildFileDeclares(aBuildMountingTool("", `mount("tool")`, testenv.NewNonce()))
		w.GivenTheBuildFileDeclares(`
			task(name = "other", base = "busybox", tool_caches = [mount("tool")],
			     steps = [run(["sh", "-c", "echo ` + testenv.NewNonce() + ` > /cache/other"])])
		`)
		w.WhenIBuild("build")
		w.ThenItSucceeds()
		w.WhenIBuild("other")
		w.ThenItSucceeds()
		w.WhenIBuild("build")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{task("build"): reused})
	})

	t.Run("Moving a mount reruns the target", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenTheBuildFileDeclares(aBuildMountingTool("", `mount("tool")`, testenv.NewNonce()))
		w.WhenIBuild("build")
		w.ThenItSucceeds()
		w.GivenTheBuildFileChanges(`mount("tool")`, `mount("tool", path = "/other")`)
		w.WhenIBuild("build")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{task("build"): ran})
	})

	t.Run("Renaming the workspace reuses results", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenTheProjectHolds(files{"WORKSPACE": `workspace(name = "com.acme.first")`})
		w.GivenTheBuildFileDeclares(aBuildMountingTool("", `mount("tool")`, testenv.NewNonce()))
		w.WhenIBuild("build")
		w.ThenItSucceeds()
		w.GivenTheProjectHolds(files{"WORKSPACE": `workspace(name = "com.acme.second")`})
		w.WhenIBuild("build")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{task("build"): reused})
	})
}

// TestToolCaches_ACacheBelongsToItsWorkspace specifies that each cache is identified by the name of its workspace and
// its label, so that no attempt is given the cache of a workspace named differently from its job's.
func TestToolCaches_ACacheBelongsToItsWorkspace(t *testing.T) {
	parallelRule(t)
	t.Run("Two workspaces with the same label keep separate caches", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenTheProjectHolds(files{
			"first/WORKSPACE":  `workspace(name = "com.acme.first")`,
			"second/WORKSPACE": `workspace(name = "com.acme.second")`,
		})
		for _, dir := range []string{"first", "second"} {
			w.GivenThePackageDeclares(dir, testenv.PullBusybox)
			w.GivenThePackageDeclares(dir, theProbe(`mount("tool")`))
		}
		w.GivenIWorkIn("first")
		w.WhenIBuild("probe", setting("file=one"))
		w.ThenItSucceeds()
		w.GivenIWorkIn("second")
		w.WhenIBuild("probe", setting("file=two"))
		w.ThenItSucceeds()
		w.ThenTheStepsObserved(observations{"listing": ""})
	})
}

// TestToolCaches_ALocalCoordinatorGivesEveryCacheEmpty specifies that the in-process executor of vamos coordinator
// --local gives every cache empty, whatever workspace name a request carries.
func TestToolCaches_ALocalCoordinatorGivesEveryCacheEmpty(t *testing.T) {
	parallelRule(t)
	t.Run("A local coordinator keeps no cache between builds", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenALocalCoordinator()
		w.GivenTheBuildFileDeclares(theProbe(`mount("tool")`))
		w.WhenIBuild("probe", setting("file=one"), onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheStepsObserved(observations{"listing": ""})
		w.WhenIBuild("probe", setting("file=two"), onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheStepsObserved(observations{"listing": ""})
	})
}

// TestToolCaches_NoToolCachesGivesEachTargetItsOwnEmptyCaches specifies that with --no-tool-caches each attempt the
// build starts is given empty caches of its own, shared by its steps and discarded when it ends, and that a result
// made with a warm cache may still be reused.
func TestToolCaches_NoToolCachesGivesEachTargetItsOwnEmptyCaches(t *testing.T) {
	parallelRule(t)
	t.Run("A clean rebuild reuses a dependency built warm", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenTheBuildFileDeclares(`
			tool_cache(name = "tool", path = "/cache")
			layer(name = "deps", base = "busybox", tool_caches = [mount("tool")],
			      steps = [run(["sh", "-c", "echo ` + testenv.NewNonce() + ` > /cache/deps"])])
			task(name = "app", base = "deps", tool_caches = [mount("tool")], steps = [run(["sh", "-c",
			     'echo "observed listing=$(echo $(ls -A /cache))"; touch /cache/app'])])
		`)
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.WhenIBuild("app", withNoToolCaches(), rebuilding())
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("deps"): reused, task("app"): rebuilt})
		w.ThenTheStepsObserved(observations{"listing": ""})
	})

	t.Run("A clean build gives every cache empty", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenTheBuildFileDeclares(theProbe(`mount("tool")`))
		w.WhenIBuild("probe", setting("file=one"))
		w.ThenItSucceeds()
		w.WhenIBuild("probe", setting("file=two"), withNoToolCaches())
		w.ThenItSucceeds()
		w.ThenTheStepsObserved(observations{"listing": ""})
	})

	t.Run("A clean build gives each target its own empty cache", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenTheBuildFileDeclares(`
			tool_cache(name = "tool", path = "/cache")
			layer(name = "first", base = "busybox", tool_caches = [mount("tool")],
			      steps = [run(["sh", "-c", "echo ` + testenv.NewNonce() + ` > /cache/marker"])])
			task(name = "second", base = "first", tool_caches = [mount("tool")],
			     steps = [run(["sh", "-c", 'echo "observed listing=$(echo $(ls -A /cache))"'])])
		`)
		w.WhenIBuild("second", withNoToolCaches())
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("first"): ran, task("second"): ran})
		w.ThenTheStepsObserved(observations{"listing": ""})
	})

	t.Run("A clean build keeps the cache for later builds", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenTheBuildFileDeclares(theProbe(`mount("tool")`))
		w.WhenIBuild("probe", setting("file=one"))
		w.ThenItSucceeds()
		w.WhenIBuild("probe", setting("file=two"), withNoToolCaches())
		w.ThenItSucceeds()
		w.WhenIBuild("probe", setting("file=three"))
		w.ThenItSucceeds()
		w.ThenTheStepsObserved(observations{"listing": "one"})
	})

	t.Run("An attempt's steps share an empty cache", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenTheBuildFileDeclares(`
			tool_cache(name = "tool", path = "/cache")
			task(name = "probe", base = "busybox", tool_caches = [mount("tool")], steps = [
			    run(["sh", "-c", "echo ` + testenv.NewNonce() + ` > /cache/marker"]),
			    run(["cat", "/cache/marker"]),
			])
		`)
		w.WhenIBuild("probe", withNoToolCaches())
		w.ThenItSucceeds()
	})
}

// TestToolCaches_ProvenanceRecordsTheCachesAnAttemptMounted specifies that a result's provenance records the label of
// each cache its attempt mounted and whether the executor gave it empty.
func TestToolCaches_ProvenanceRecordsTheCachesAnAttemptMounted(t *testing.T) {
	parallelRule(t)
	t.Run("A clean build's provenance", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenThePackageDeclares("tools", `tool_cache(name = "go-build", path = "/cache/go-build")`)
		w.GivenTheBuildFileDeclares(`
			task(name = "build", base = "busybox", tool_caches = [mount("//tools:go-build")],
			     steps = [run(["echo", "` + testenv.NewNonce() + `"])])
		`)
		w.WhenIBuild("build", withNoToolCaches())
		w.ThenItSucceeds()
		w.ThenTheCachedResultsRecordTheToolCaches(toolCacheUses{"//tools:go-build": true})
	})
}

// TestToolCaches_AStandaloneBuildRefusesARequestWithoutAValidWorkspaceName specifies that a standalone local build
// whose graph mounts caches fails before anything runs when its request's workspace name breaks the rules a name
// follows, or is empty and the request doesn't ask for empty tool caches.
func TestToolCaches_AStandaloneBuildRefusesARequestWithoutAValidWorkspaceName(t *testing.T) {
	parallelRule(t)
	t.Run("An API request without a workspace name", func(t *testing.T) {
		w := newToolCacheMistakeWorld(t)
		w.GivenTheBuildFileDeclares(`
			tool_cache(name = "tool", path = "/cache")
			task(name = "build", base = "scratch", tool_caches = [mount("tool")], steps = [run(["true"])])
		`)
		w.WhenIShowTheGraph("build")
		w.WhenAnAPIClientSendsTheGraphWithoutAWorkspaceName()
		w.ThenItFailsWith("the build mounts tool caches, so the workspace needs a name in WORKSPACE")
		w.ThenNoTargetRan()
	})
}

// TestToolCaches_LoadingRefusesACachePathItCannotMount specifies that loading refuses a cache's or a mount's path that
// is not absolute or not in clean form, is /, contains a variable, is at or beneath /dev, /proc or /sys, or equals,
// lies beneath or contains a file that buildah may bind into a run() step, naming the path.
func TestToolCaches_LoadingRefusesACachePathItCannotMount(t *testing.T) {
	parallelRule(t)
	t.Run("A cache path beneath /dev", func(t *testing.T) {
		w := newToolCacheMistakeWorld(t)
		w.GivenTheBuildFileDeclares(`
			tool_cache(name = "tool", path = "/cache")
			task(name = "build", base = "scratch", tool_caches = [mount("tool", path = "/dev/shm/cache")],
			     steps = [run(["true"])])
		`)
		w.WhenIBuild("build")
		w.ThenItFailsWith(`/project/BUILD:2:5: task "build": mount("tool", path = "/dev/shm/cache"): ` +
			`path "/dev/shm/cache" is within /dev, which buildah mounts itself in every run() step`)
		w.ThenNoTargetRan()
	})

	t.Run("A cache path that holds a file every step is given", func(t *testing.T) {
		w := newToolCacheMistakeWorld(t)
		w.GivenTheBuildFileDeclares(`
			tool_cache(name = "tool", path = "/run")
			task(name = "build", base = "scratch", tool_caches = [mount("tool")], steps = [run(["true"])])
		`)
		w.WhenIBuild("build")
		w.ThenItFailsWith(`/project/BUILD:1:11: tool_cache: path "/run" contains /run/.containerenv, ` +
			"which buildah binds into run() steps")
		w.ThenNoTargetRan()
	})

	t.Run("A cache path not in clean form", func(t *testing.T) {
		w := newToolCacheMistakeWorld(t)
		w.GivenTheBuildFileDeclares(`
			tool_cache(name = "tool", path = "/cache/")
			task(name = "app", base = "scratch", steps = [run(["true"])])
		`)
		w.WhenIBuild("app")
		w.ThenItFailsWith(`/project/BUILD:1:11: tool_cache: path "/cache/" must be in clean form, as "/cache"`)
		w.ThenNoTargetRan()
	})
}

// TestToolCaches_LoadingRefusesACachePathThatConflicts specifies that loading refuses a mounted path that equals,
// contains or lies beneath another mounted path of the target or a secret's file; an add() destination or a workdir()
// with no variable that, once cleaned, equals or lies beneath a mounted path, or a source with no ".." component that,
// once cleaned, does; one cache mounted twice by a target; a target that mounts caches and has no run() step; and a
// label naming anything but a tool_cache rule.
func TestToolCaches_LoadingRefusesACachePathThatConflicts(t *testing.T) {
	parallelRule(t)
	for _, example := range []struct {
		name   string
		build  string // declaring the target, which loading refuses
		target string
		error  string
	}{
		{
			name: "A task's output inside a cache",
			build: `
				tool_cache(name = "tool", path = "/build")
				task(name = "build", base = "scratch", tool_caches = [mount("tool")], steps = [run(["true"])],
				     source = "/build/out", include = ["*"])
			`,
			target: "build",
			error: `/project/BUILD:2:5: task "build": source "/build/out" is within the tool cache tool, ` +
				"mounted at /build; a task's output can't be in a tool cache",
		},
		{
			name: "Two mounts at one path",
			build: `
				tool_cache(name = "go", path = "/cache")
				tool_cache(name = "npm", path = "/cache")
				task(name = "build", base = "scratch", tool_caches = [mount("go"), mount("npm")],
				     steps = [run(["true"])])
			`,
			target: "build",
			error: `/project/BUILD:3:5: task "build" mounts go and npm both at /cache; ` +
				"each tool cache needs a path of its own",
		},
		{
			name: "A workdir() in a cache",
			build: `
				tool_cache(name = "tool", path = "/cache")
				task(name = "build", base = "scratch", tool_caches = [mount("tool")],
				     steps = [workdir("/cache/sub"), run(["true"])])
			`,
			target: "build",
			error: `/project/BUILD:2:5: task "build": workdir("/cache/sub") is within the tool cache tool, ` +
				"mounted at /cache; the working directory can't be in a tool cache",
		},
		{
			name: "A layer with caches and no run() step",
			build: `
				tool_cache(name = "tool", path = "/cache")
				glob(name = "src", include = ["*.txt"])
				layer(name = "files", base = "scratch", tool_caches = [mount("tool")], steps = [add("src", "/src")])
			`,
			target: "files",
			error: `/project/BUILD:3:6: layer "files" mounts tool caches but has no run() step; ` +
				"tool caches are mounted only for run() steps",
		},
		{
			name: "One cache mounted twice",
			build: `
				tool_cache(name = "tool", path = "/cache")
				task(name = "build", base = "scratch", tool_caches = [mount("tool"), mount("tool", path = "/other")],
				     steps = [run(["true"])])
			`,
			target: "build",
			error: `/project/BUILD:2:5: task "build" mounts tool twice, at /cache and /other; ` +
				"a target mounts each tool cache once",
		},
		{
			name: "Nested caches",
			build: `
				tool_cache(name = "all", path = "/cache")
				tool_cache(name = "go", path = "/cache/go")
				task(name = "build", base = "scratch", tool_caches = [mount("all"), mount("go")],
				     steps = [run(["true"])])
			`,
			target: "build",
			error: `/project/BUILD:3:5: task "build" mounts go at /cache/go, within all at /cache; ` +
				"tool caches can't be nested",
		},
		{
			name: "A label that is no cache",
			build: `
				layer(name = "tools", base = "scratch")
				task(name = "build", base = "tools", tool_caches = [mount("tools")], steps = [run(["true"])])
			`,
			target: "build",
			error: `/project/BUILD:2:5: task "build": tool_caches[0] "tools" is a layer(), not a tool_cache(); ` +
				"mount() names a tool cache",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newToolCacheMistakeWorld(t)
			w.GivenTheBuildFileDeclares(example.build)
			w.WhenIBuild(example.target)
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
		})
	}

	t.Run("An add() above a cache", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenTheBuildFileDeclares(`
			glob(name = "code", include = ["main.go"])
			tool_cache(name = "tool", path = "/src/.cache")
			layer(name = "src", base = "busybox", tool_caches = [mount("tool")],
			      steps = [add("code", "/src"),
			               run(["sh", "-c", "echo ` + testenv.NewNonce() + ` > /src/.cache/file"])])
		`)
		w.GivenTheProjectHolds(files{"main.go": "package main"})
		w.WhenISave("src")
		w.ThenItSucceeds()
		w.ThenTheImageHasTheDirectory("/src/.cache")
		w.ThenTheImageHoldsNothing(under("/src/.cache"))
	})

	t.Run("A workdir() above a cache", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenTheBuildFileDeclares(`
			tool_cache(name = "tool", path = "/app/.cache")
			task(name = "build", base = "busybox", tool_caches = [mount("tool")],
			     steps = [workdir("/app"), run(["sh", "-c", "echo ` + testenv.NewNonce() + ` > /app/.cache/file"])])
		`)
		w.WhenIBuild("build")
		w.ThenItSucceeds()
	})
}

// TestToolCaches_AnUnnamedWorkspaceCannotMountCaches specifies that a target that mounts a cache fails to load in a
// workspace whose WORKSPACE does not name it, saying where the target is declared and that the workspace needs a
// name, and that declaring a tool_cache rule that no loaded target mounts does not fail.
func TestToolCaches_AnUnnamedWorkspaceCannotMountCaches(t *testing.T) {
	parallelRule(t)
	t.Run("Mounting a cache in an unnamed workspace", func(t *testing.T) {
		w := newWorld(t)
		w.GivenTheProjectHolds(files{"WORKSPACE": ""})
		w.GivenTheBuildFileDeclares(`
			tool_cache(name = "tool", path = "/cache")
			task(name = "build", base = "scratch", tool_caches = [mount("tool")], steps = [run(["true"])])
		`)
		w.WhenIBuild("build")
		w.ThenItFailsWith(`/project/BUILD:2:5: task "build" mounts tool caches, so the workspace needs a name in ` +
			`WORKSPACE, such as workspace(name = "com.example.app")`)
		w.ThenNoTargetRan()
	})

	t.Run("An unmounted cache in an unnamed workspace", func(t *testing.T) {
		w := newBuildingWorld(t)
		w.GivenTheProjectHolds(files{"WORKSPACE": ""})
		w.GivenAProjectThatPullsBusybox()
		w.GivenTheBuildFileDeclares(`
			tool_cache(name = "tool", path = "/cache")
			task(name = "app", base = "busybox", steps = [run(["echo", "` + testenv.NewNonce() + `"])])
		`)
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{task("app"): ran})
	})
}

// TestToolCaches_ACachesOwnerIsFixedByItsRule specifies that a tool_cache rule gives its owner and group both or
// neither, each as decimal digits from 0 to 4294967294, and that a name or any other value is refused as it loads.
func TestToolCaches_ACachesOwnerIsFixedByItsRule(t *testing.T) {
	parallelRule(t)
	for _, example := range []struct {
		name  string
		rule  string // the tool_cache rule
		error string
	}{
		{
			name: "An owner given by name",
			rule: `tool_cache(name = "tool", path = "/cache", owner = "node", group = "1000")`,
			error: `tool_cache: owner "node" must be a numeric ID, such as "1000", ` +
				"since a name can mean another ID in each image",
		},
		{
			name:  "An owner out of range",
			rule:  `tool_cache(name = "tool", path = "/cache", owner = "4294967295", group = "0")`,
			error: `tool_cache: owner "4294967295" must be from 0 to 4294967294`,
		},
		{
			name: "An owner without a group",
			rule: `tool_cache(name = "tool", path = "/cache", owner = "1000")`,
			error: "tool_cache: owner and group are given together, or neither, " +
				"since either alone would leave the other to a default",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newToolCacheMistakeWorld(t)
			w.GivenTheBuildFileDeclares(example.rule)
			w.GivenTheBuildFileDeclares(`
				task(name = "app", base = "scratch", tool_caches = [mount("tool")], steps = [run(["true"])])
			`)
			w.WhenIBuild("app")
			w.ThenItFailsWith("/project/BUILD:1:11: " + example.error)
			w.ThenNoTargetRan()
		})
	}

	t.Run("Owners written differently are one owner", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenTheBuildFileDeclares(aBuildMountingTool(`, owner = "1000", group = "1000"`, `mount("tool")`,
			testenv.NewNonce()))
		w.WhenIBuild("build")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{task("build"): ran})
		w.GivenTheBuildFileChanges(`owner = "1000"`, `owner = "01000"`)
		w.WhenIBuild("build")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{task("build"): reused})
	})
}

// theProbe declares the tool cache "tool" at /cache and the task "probe", which mounts it as mount gives it, lists
// /cache, as the observation "listing", and then writes a file there named by its parameter "file".
func theProbe(mount string) string {
	return fmt.Sprintf(`
		tool_cache(name = "tool", path = "/cache")
		task(
		    name = "probe",
		    base = "busybox",
		    params = {"file": "one"},
		    tool_caches = [%s],
		    steps = lambda ctx: [run(["sh", "-c", 'echo "observed listing=$(echo $(ls -A /cache))"; touch /cache/' +
		        ctx.file])],
		)
	`, mount)
}

// TestToolCaches_ATaskOrLayerMountsCachesForItsRunSteps specifies that a task's or layer's tool_caches mounts each
// cache, at the cache's path or the mount's, for every run() step of the target, and that Vamos sets no variable naming
// a mounted cache.
func TestToolCaches_ATaskOrLayerMountsCachesForItsRunSteps(t *testing.T) {
	parallelRule(t)
	t.Run("A later build sees what an earlier one left", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenTheBuildFileDeclares(theProbe(`mount("tool")`))
		w.WhenIBuild("probe", setting("file=one"))
		w.ThenItSucceeds()
		w.WhenIBuild("probe", setting("file=two"))
		w.ThenItSucceeds()
		w.ThenTheStepsObserved(observations{"listing": "one"})
	})

	t.Run("A mount overrides the cache's path", func(t *testing.T) {
		w := newToolCacheWorld(t)
		nonce := testenv.NewNonce()
		w.GivenTheBuildFileDeclares(`
			tool_cache(name = "tool", path = "/a")
			task(name = "writer", base = "busybox", tool_caches = [mount("tool")],
			     steps = [run(["sh", "-c", "echo ` + nonce + ` > /a/marker"])])
			task(name = "reader", base = "busybox", tool_caches = [mount("tool", path = "/b")],
			     steps = [run(["sh", "-c",
			     'echo "observed marker=$(cat /b/marker)"; echo "observed a=$([ -e /a ] && echo there || echo absent)"'
			])])
		`)
		w.WhenIBuild("writer")
		w.ThenItSucceeds()
		w.WhenIBuild("reader")
		w.ThenItSucceeds()
		w.ThenTheStepsObserved(observations{"marker": nonce, "a": "absent"})
	})

	t.Run("A step is given no variable for its cache", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenTheBuildFileDeclares(`
			tool_cache(name = "tool", path = "/cache")
			task(name = "probe", base = "busybox", tool_caches = [mount("tool")], steps = [run(["sh", "-c",
			     'echo ` + testenv.NewNonce() + `; echo "observed variables naming it=$(env | grep -c /cache)"'])])
		`)
		w.WhenIBuild("probe")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{task("probe"): ran})
		w.ThenTheStepsObserved(observations{"variables naming it": "0"})
	})

	t.Run("Two packages share one cache", func(t *testing.T) {
		w := newToolCacheWorld(t)
		nonce := testenv.NewNonce()
		w.GivenThePackageDeclares("tools", `tool_cache(name = "go-build", path = "/cache/go-build")`)
		w.GivenThePackageDeclares("api", testenv.PullBusybox)
		w.GivenThePackageDeclares("api", `
			task(name = "api", base = "busybox", tool_caches = [mount("//tools:go-build")],
			     steps = [run(["sh", "-c", "echo `+nonce+` > /cache/go-build/api"])])
		`)
		w.GivenThePackageDeclares("cli", testenv.PullBusybox)
		w.GivenThePackageDeclares("cli", `
			task(name = "cli", base = "busybox", tool_caches = [mount("//tools:go-build")],
			     steps = [run(["sh", "-c", 'echo "observed api=$(cat /cache/go-build/api)"'])])
		`)
		w.WhenIBuild("//api:api")
		w.ThenItSucceeds()
		w.WhenIBuild("//cli:cli")
		w.ThenItSucceeds()
		w.ThenTheStepsObserved(observations{"api": nonce})
	})
}

// TestToolCaches_ACachesContentsNeverEnterALayersImage specifies that what a run() step writes through a cache's
// mount stays in the cache and is no part of the layer's image, whether the cache was given empty or not, and that the
// image holds an empty directory at each cache path.
func TestToolCaches_ACachesContentsNeverEnterALayersImage(t *testing.T) {
	parallelRule(t)
	t.Run("A layer's image keeps no cache contents", func(t *testing.T) {
		w := newToolCacheWorld(t)
		nonce := testenv.NewNonce()
		w.GivenTheBuildFileDeclares(`
			tool_cache(name = "tool", path = "/cache")
			layer(name = "tools", base = "busybox", tool_caches = [mount("tool")], steps = [run(["sh", "-c",
			    'echo "observed marker=$(cat /cache/marker 2>/dev/null || echo none)"; ' +
			    'echo ` + nonce + ` > /cache/big; ' +
			    'echo ` + nonce + ` > /cache/marker; mkdir -p /opt && echo kept > /opt/kept'])])
		`)
		w.WhenISave("tools")
		w.ThenItSucceeds()
		w.ThenTheStepsObserved(observations{"marker": "none"})
		w.ThenTheImageHolds(under("/opt"), files{"/opt/kept": "kept\n"})
		w.ThenTheImageHasTheDirectory("/cache")
		w.ThenTheImageHoldsNothing(under("/cache"))
		w.WhenIBuild("tools", rebuilding())
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("tools"): rebuilt})
		w.ThenTheStepsObserved(observations{"marker": nonce})
		w.WhenISave("tools")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("tools"): reused})
		w.ThenTheImageHolds(under("/opt"), files{"/opt/kept": "kept\n"})
		w.ThenTheImageHasTheDirectory("/cache")
		w.ThenTheImageHoldsNothing(under("/cache"))
	})
}

// TestToolCaches_ACachesOwnerIsFixedByItsRuleWhenBuilt specifies that a cache's directory belongs to the rule's owner
// and group, whether the executor gives it empty or not.
func TestToolCaches_ACachesOwnerIsFixedByItsRuleWhenBuilt(t *testing.T) {
	parallelRule(t)
	for _, example := range []struct {
		name    string
		options []option
	}{
		{name: "A non-root step writes to its cache"},
		{name: "A non-root step writes to an empty cache", options: []option{withNoToolCaches()}},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newToolCacheWorld(t)
			w.GivenARegistry()
			w.WhenIPush("busybox", "<registry>/nonroot:v1")
			w.ThenItSucceeds()
			w.GivenTheRegistrysImageRunsAs("<registry>/nonroot:v1", "1000:1000")
			w.GivenTheBuildFileDeclares(`pull(name = "nonroot", image = "<registry>/nonroot", tag = "v1")`)
			w.GivenTheBuildFileDeclares(`
				tool_cache(name = "tool", path = "/cache", owner = "1000", group = "1000")
				task(name = "probe", base = "nonroot", tool_caches = [mount("tool")],
				     steps = [run(["sh", "-c", "echo ` + testenv.NewNonce() + ` > /cache/written"])])
			`)
			w.WhenIBuild("probe", example.options...)
			w.ThenItSucceeds()
		})
	}
}

// TestToolCaches_ACacheIsKeptApartForEachOwnerAndGroup specifies that an owner and group that haven't used a cache
// find it empty, and that the executor never changes the owner of what another owner and group left in it.
func TestToolCaches_ACacheIsKeptApartForEachOwnerAndGroup(t *testing.T) {
	parallelRule(t)
	t.Run("Changing a cache's owner gives it empty", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenARegistry()
		w.WhenIPush("busybox", "<registry>/nonroot:v1")
		w.ThenItSucceeds()
		w.GivenTheRegistrysImageRunsAs("<registry>/nonroot:v1", "1000:1000")
		w.GivenTheBuildFileDeclares(`pull(name = "nonroot", image = "<registry>/nonroot", tag = "v1")`)
		w.GivenTheBuildFileDeclares(`
			tool_cache(name = "tool", path = "/cache")
			task(name = "probe", base = "busybox", params = {"file": "one"}, tool_caches = [mount("tool")],
			     steps = lambda ctx: [run(["sh", "-c",
			         'echo "observed listing=$(echo $(ls -A /cache))"; mkdir /cache/' + ctx.file])])
		`)
		w.WhenIBuild("probe", setting("file=one"))
		w.ThenItSucceeds()
		w.GivenTheBuildFileChanges(`path = "/cache")`, `path = "/cache", owner = "1000", group = "1000")`)
		w.GivenTheBuildFileChanges(`base = "busybox", params`, `base = "nonroot", params`)
		w.WhenIBuild("probe", setting("file=two"))
		w.ThenItSucceeds()
		w.ThenTheStepsObserved(observations{"listing": ""})
	})

	t.Run("Changing a cache's owner back finds what it held", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenARegistry()
		w.WhenIPush("busybox", "<registry>/nonroot:v1")
		w.ThenItSucceeds()
		w.GivenTheRegistrysImageRunsAs("<registry>/nonroot:v1", "1000:1000")
		w.GivenTheBuildFileDeclares(`pull(name = "nonroot", image = "<registry>/nonroot", tag = "v1")`)
		w.GivenTheBuildFileDeclares(`
			tool_cache(name = "tool", path = "/cache")
			task(name = "probe", base = "busybox", params = {"file": "one"}, tool_caches = [mount("tool")],
			     steps = lambda ctx: [run(["sh", "-c",
			         'echo "observed listing=$(echo $(ls -A /cache))"; mkdir /cache/' + ctx.file])])
		`)
		w.WhenIBuild("probe", setting("file=one"))
		w.ThenItSucceeds()
		w.GivenTheBuildFileChanges(`path = "/cache")`, `path = "/cache", owner = "1000", group = "1000")`)
		w.GivenTheBuildFileChanges(`base = "busybox", params`, `base = "nonroot", params`)
		w.WhenIBuild("probe", setting("file=two"))
		w.ThenItSucceeds()
		w.ThenTheStepsObserved(observations{"listing": ""})
		w.GivenTheBuildFileChanges(`path = "/cache", owner = "1000", group = "1000")`, `path = "/cache")`)
		w.GivenTheBuildFileChanges(`base = "nonroot", params`, `base = "busybox", params`)
		w.WhenIBuild("probe", setting("file=three"))
		w.ThenItSucceeds()
		w.ThenTheStepsObserved(observations{"listing": "one"})
	})
}

// TestToolCaches_ACachesMountPointTakesItsOwner specifies that a cache's mount point belongs to the rule's owner and
// group, and each missing parent the executor makes to the user the image runs as, as a workdir() would make it.
func TestToolCaches_ACachesMountPointTakesItsOwner(t *testing.T) {
	parallelRule(t)
	t.Run("A cache's mount point belongs to its owner", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenARegistry()
		w.GivenTheBuildFileDeclares(`
			layer(name = "home", base = "busybox", steps = [run(["sh", "-c",
			      "mkdir -p /home/node && chown 1000:1000 /home/node"])])
		`)
		w.WhenIPush("home", "<registry>/node:v1")
		w.ThenItSucceeds()
		w.GivenTheRegistrysImageRunsAs("<registry>/node:v1", "1000:1000")
		w.GivenTheBuildFileDeclares(`
			pull(name = "node", image = "<registry>/node", tag = "v1")
			tool_cache(name = "go-build", path = "/home/node/.cache/go-build", owner = "2000", group = "2000")
			layer(name = "tools", base = "node", tool_caches = [mount("go-build")], steps = [run(["sh", "-c",
			      "mkdir -p /home/node/.cache/pip && echo ` + testenv.NewNonce() + ` > /home/node/.cache/pip/file"])])
		`)
		w.WhenISave("tools")
		w.ThenItSucceeds()
		w.ThenTheImageOwnersAre(owners{
			"/home/node/.cache":          "1000:1000",
			"/home/node/.cache/go-build": "2000:2000",
		})
		w.ThenTheImageHoldsNothing(under("/home/node/.cache/go-build"))
	})

	t.Run("A cache does not take a workdir() above it from its user", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenARegistry()
		w.WhenIPush("busybox", "<registry>/nonroot:v1")
		w.ThenItSucceeds()
		w.GivenTheRegistrysImageRunsAs("<registry>/nonroot:v1", "1000:1000")
		w.GivenTheBuildFileDeclares(`pull(name = "nonroot", image = "<registry>/nonroot", tag = "v1")`)
		w.GivenTheBuildFileDeclares(`
			tool_cache(name = "tool", path = "/app/.cache")
			task(name = "build", base = "nonroot", tool_caches = [mount("tool")], steps = [
			    run(["echo", "` + testenv.NewNonce() + `"]),
			    workdir("/app"),
			    run(["sh", "-c", "echo made > /app/file"]),
			])
		`)
		w.WhenIBuild("build")
		w.ThenItSucceeds()
	})
}

// anAliasedBase declares the layer "aliased", on busybox, whose step makes the image's links and directories that
// setup, a shell command, gives, such as "/alias" linked to "/real".
func anAliasedBase(setup string) string {
	return fmt.Sprintf(`
		layer(name = "aliased", base = "busybox", steps = [run(["sh", "-c", %q])])
	`, setup)
}

// aBaseWithTheLink declares the layer "aliased", on busybox, to which the task "links" adds the symbolic link link,
// to target, where a run() step can't make one, as in /dev or at /etc/hostname, which buildah mounts and binds while
// a step runs; and whose step then makes the links and directories that setup, a shell command, gives.
func aBaseWithTheLink(link, target, setup string) string {
	dir, name := path.Split(link)
	return fmt.Sprintf(`
		task(name = "links", base = "busybox", source = "/out", include = ["**"],
		     steps = [run(["sh", "-c", "mkdir -p /out%[1]s && ln -s %[3]s /out%[1]s%[2]s"])])
		layer(name = "aliased", base = "busybox", steps = [add("links", "/"), run(["sh", "-c", %[4]q])])
	`, dir, name, target, setup)
}

// TestToolCaches_AnExecutorResolvesEachCachePathBeforeTheStepsAndHoldsIt specifies that the executor resolves each
// cache path in the container's root filesystem before the steps, makes an empty directory there before the first
// run() step, and fails the attempt, naming the path, if a step moves the path or puts it through /dev, /proc or /sys,
// or if its location is no longer an empty directory.
func TestToolCaches_AnExecutorResolvesEachCachePathBeforeTheStepsAndHoldsIt(t *testing.T) {
	parallelRule(t)
	t.Run("A cache path through a symbolic link", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenTheBuildFileDeclares(anAliasedBase("mkdir /real && ln -s /real /alias"))
		w.GivenTheBuildFileDeclares(`
			tool_cache(name = "tool", path = "/alias/cache")
			layer(name = "tools", base = "aliased", tool_caches = [mount("tool")],
			      steps = [run(["sh", "-c", "echo ` + testenv.NewNonce() + ` > /alias/cache/file"])])
		`)
		w.WhenISave("tools")
		w.ThenItSucceeds()
		w.ThenTheImageHasTheDirectory("/real/cache")
		w.ThenTheImageHoldsNothing(under("/real/cache"))
	})

	for _, example := range []struct {
		name, setup, relink string
	}{
		{
			name:   "A step that moves a cache path",
			setup:  "mkdir /real /other && ln -s /real /alias",
			relink: "rm /alias && ln -s /other /alias",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newToolCacheWorld(t)
			w.GivenTheBuildFileDeclares(anAliasedBase(example.setup))
			w.GivenTheBuildFileDeclares(fmt.Sprintf(`
				tool_cache(name = "tool", path = "/alias/cache")
				task(name = "build", base = "aliased", tool_caches = [mount("tool")], steps = [
				    run(["sh", "-c", %q]),
				    run(["echo", "second ran"]),
				])
			`, example.relink))
			w.WhenIBuild("build")
			w.ThenItFailsWith(`task "build"`, "/alias/cache")
			w.ThenTheOutputDoesNotInclude("second ran")
		})
	}

	t.Run("A step that points a cache path through /dev", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenTheBuildFileDeclares(aBaseWithTheLink("/dev/hidden", "/real", "mkdir /real && ln -s /real /alias"))
		w.GivenTheBuildFileDeclares(`
			tool_cache(name = "tool", path = "/alias/cache")
			task(name = "build", base = "aliased", tool_caches = [mount("tool")], steps = [
			    run(["sh", "-c", "rm /alias && ln -s /dev/hidden /alias"]),
			    run(["echo", "second ran"]),
			])
		`)
		w.WhenIBuild("build")
		w.ThenItFailsWith(`task "build"`, "/alias/cache", "/dev")
		w.ThenTheOutputDoesNotInclude("second ran")
	})

	t.Run("A workdir() beneath a cache", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenTheBuildFileDeclares(anAliasedBase("ln -s /cache /alias"))
		w.GivenTheBuildFileDeclares(`
			tool_cache(name = "tool", path = "/cache")
			layer(name = "tools", base = "aliased", tool_caches = [mount("tool")],
			      steps = [run(["echo", "` + testenv.NewNonce() + `"]), workdir("/alias/sub")])
		`)
		w.WhenIBuild("tools")
		w.ThenItFailsWith(`layer "tools"`, "/cache", "working directory")
	})
}

// TestToolCaches_AnExecutorRefusesACachePathItCannotMountCleanly specifies that the executor fails the attempt,
// naming the paths, before its steps when a cache's location holds anything but an empty directory or overlaps
// another's, and just before its first run() step when a step put something there.
func TestToolCaches_AnExecutorRefusesACachePathItCannotMountCleanly(t *testing.T) {
	parallelRule(t)
	t.Run("The image already holds files at a cache path", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenTheBuildFileDeclares(anAliasedBase("mkdir /cache && touch /cache/old"))
		w.GivenTheBuildFileDeclares(`
			tool_cache(name = "tool", path = "/cache")
			task(name = "build", base = "aliased", tool_caches = [mount("tool")], steps = [run(["echo", "step ran"])])
		`)
		w.WhenIBuild("build")
		w.ThenItFailsWith(`task "build"`, "/cache")
		w.ThenTheOutputDoesNotInclude("step ran")
	})

	t.Run("Two cache paths that one symbolic link joins", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenTheBuildFileDeclares(anAliasedBase("mkdir /real && ln -s /real /alias"))
		w.GivenTheBuildFileDeclares(`
			tool_cache(name = "a", path = "/alias/cache")
			tool_cache(name = "b", path = "/real/cache")
			task(name = "build", base = "aliased", tool_caches = [mount("a"), mount("b")],
			     steps = [run(["echo", "step ran"])])
		`)
		w.WhenIBuild("build")
		w.ThenItFailsWith(`task "build"`, "/alias/cache", "/real/cache")
		w.ThenTheOutputDoesNotInclude("step ran")
	})

	t.Run("A step that makes a directory at a cache path", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenTheBuildFileDeclares(`
			layer(name = "aliased", base = "busybox", steps = [env(CACHE = "/src/.cache")])
			tool_cache(name = "tool", path = "/src/.cache")
			task(name = "build", base = "aliased", tool_caches = [mount("tool")],
			     steps = [workdir("$CACHE"), workdir("/src"), run(["echo", "step ran"])])
		`)
		w.WhenIBuild("build")
		w.ThenItFailsWith(`task "build"`, "/src/.cache")
		w.ThenTheOutputDoesNotInclude("step ran")
	})
}

// TestToolCaches_AnExecutorRefusesACachePathThatBuildahsOwnMountsOverlap specifies that the executor fails the attempt
// before its steps, naming the paths, when a cache's location is at or beneath /dev, /proc or /sys, or reached through
// one, or overlaps where a file buildah binds into a step, or a volume the image declares, resolves.
func TestToolCaches_AnExecutorRefusesACachePathThatBuildahsOwnMountsOverlap(t *testing.T) {
	parallelRule(t)
	for _, example := range []struct {
		name, setup, path string
		error             []string
	}{
		{
			name:  "A cache path that a symbolic link puts beneath /dev",
			setup: "ln -s /dev/shm /alias",
			path:  "/alias/cache",
			error: []string{"/alias/cache", "/dev"},
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newToolCacheWorld(t)
			w.GivenTheBuildFileDeclares(anAliasedBase(example.setup))
			w.GivenTheBuildFileDeclares(fmt.Sprintf(`
				tool_cache(name = "tool", path = %q)
				task(name = "build", base = "aliased", tool_caches = [mount("tool")],
				     steps = [run(["echo", "step ran"])])
			`, example.path))
			w.WhenIBuild("build")
			w.ThenItFailsWith(append([]string{`task "build"`}, example.error...)...)
			w.ThenTheOutputDoesNotInclude("step ran")
		})
	}

	t.Run("A file every step is given that a symbolic link puts in a cache", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenTheBuildFileDeclares(aBaseWithTheLink("/etc/hostname", "/cache/hostname", "true"))
		w.GivenTheBuildFileDeclares(`
			tool_cache(name = "tool", path = "/cache")
			task(name = "build", base = "aliased", tool_caches = [mount("tool")], steps = [run(["echo", "step ran"])])
		`)
		w.WhenIBuild("build")
		w.ThenItFailsWith(`task "build"`, "/etc/hostname", "/cache")
		w.ThenTheOutputDoesNotInclude("step ran")
	})

	t.Run("A volume the image declares beneath a cache", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenARegistry()
		w.WhenIPush("busybox", "<registry>/volume:v1")
		w.ThenItSucceeds()
		w.GivenTheRegistrysImageDeclaresTheVolume("<registry>/volume:v1", "/cache/data")
		w.GivenTheBuildFileDeclares(`
			pull(name = "volume", image = "<registry>/volume", tag = "v1")
			tool_cache(name = "tool", path = "/cache")
			task(name = "build", base = "volume", tool_caches = [mount("tool")], steps = [run(["echo", "step ran"])])
		`)
		w.WhenIBuild("build")
		w.ThenItFailsWith(`task "build"`, "/cache", "/cache/data")
		w.ThenTheOutputDoesNotInclude("step ran")
	})
}

// TestToolCaches_AnExecutorRefusesAPathThatPassesThroughACache specifies that the executor fails the attempt, naming
// the paths, when the resolution of a cache path passes through a cache's location before its end.
func TestToolCaches_AnExecutorRefusesAPathThatPassesThroughACache(t *testing.T) {
	parallelRule(t)
	t.Run("A cache path reached through another cache", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenTheBuildFileDeclares(anAliasedBase("ln -s first/entry/../../second /alias"))
		w.GivenTheBuildFileDeclares(`
			tool_cache(name = "a", path = "/first")
			tool_cache(name = "b", path = "/alias")
			task(name = "build", base = "aliased", tool_caches = [mount("a"), mount("b")],
			     steps = [run(["echo", "step ran"])])
		`)
		w.WhenIBuild("build")
		w.ThenItFailsWith(`task "build"`, "/alias", "/first")
		w.ThenTheOutputDoesNotInclude("step ran")
	})

	t.Run("A step that re-links a cache path through another cache", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenTheBuildFileDeclares(anAliasedBase("mkdir /second && ln -s /second /alias"))
		w.GivenTheBuildFileDeclares(`
			tool_cache(name = "a", path = "/first")
			tool_cache(name = "b", path = "/alias")
			task(name = "build", base = "aliased", tool_caches = [mount("a"), mount("b")], steps = [
			    run(["sh", "-c", "rm /alias && ln -s first/entry/../../second /alias"]),
			    run(["echo", "second ran"]),
			])
		`)
		w.WhenIBuild("build")
		w.ThenItFailsWith(`task "build"`, "/alias", "/first")
		w.ThenTheOutputDoesNotInclude("second ran")
	})
}

// TestToolCaches_AnExecutorRefusesASymbolicLinkAtDevProcOrSys specifies that the executor fails an attempt that mounts
// caches, naming the path, when /dev, /proc or /sys is a symbolic link before any run() step.
func TestToolCaches_AnExecutorRefusesASymbolicLinkAtDevProcOrSys(t *testing.T) {
	parallelRule(t)
	t.Run("An add() that makes /dev a symbolic link", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenTheBuildFileDeclares(`
			task(name = "devlink", base = "busybox", source = "/out", include = ["**"],
			     steps = [run(["sh", "-c", "mkdir /out && ln -s devices /out/dev"])])
			tool_cache(name = "tool", path = "/devices/cache")
			task(name = "build", base = "scratch", tool_caches = [mount("tool")], steps = [
			    add("devlink", "/"),
			    run(["echo", "step ran"]),
			])
		`)
		w.WhenIBuild("build")
		w.ThenItFailsWith(`task "build"`, "/dev")
		w.ThenTheOutputDoesNotInclude("step ran")
	})
}

// TestToolCaches_ToolCachesNeedChrootIsolation specifies that the executor fails an attempt that mounts caches before
// its steps, naming the isolation, when it runs steps with another isolation than chroot.
func TestToolCaches_ToolCachesNeedChrootIsolation(t *testing.T) {
	parallelRule(t)
	t.Run("A cache under OCI isolation", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenTheLocalExecutorRunsStepsWithOCIIsolation()
		w.GivenTheBuildFileDeclares(`
			tool_cache(name = "tool", path = "/cache")
			task(name = "build", base = "busybox", tool_caches = [mount("tool")], steps = [run(["echo", "step ran"])])
		`)
		w.WhenIBuild("build")
		w.ThenItFailsWith(`task "build"`, "oci")
		w.ThenTheOutputDoesNotInclude("step ran")
	})
}

// TestToolCaches_AnExecutorRefusesAWorkingDirectoryInACache specifies that the executor fails the attempt, naming the
// paths, when the working directory resolves into a cache before a run() step or after the last step.
func TestToolCaches_AnExecutorRefusesAWorkingDirectoryInACache(t *testing.T) {
	parallelRule(t)
	t.Run("A last workdir() that a variable puts at a cache", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenTheBuildFileDeclares(`
			layer(name = "aliased", base = "busybox", steps = [env(CACHE = "/cache")])
			tool_cache(name = "tool", path = "/cache")
			layer(name = "tools", base = "aliased", tool_caches = [mount("tool")],
			      steps = [run(["echo", "` + testenv.NewNonce() + `"]), workdir("$CACHE")])
		`)
		w.WhenISave("tools")
		w.ThenItFailsWith(`layer "tools"`, "/cache", "working directory")
		w.ThenNothingIsSaved()
	})

	t.Run("A workdir() that a variable puts at a cache", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenTheBuildFileDeclares(`
			layer(name = "aliased", base = "busybox", steps = [env(CACHE = "/cache")])
			tool_cache(name = "tool", path = "/cache")
			task(name = "build", base = "aliased", tool_caches = [mount("tool")],
			     steps = [workdir("$CACHE"), run(["sh", "-c", "echo step ran; echo > out"])])
		`)
		w.WhenIBuild("build")
		w.ThenItFailsWith(`task "build"`, "/cache", "working directory")
		w.ThenTheOutputDoesNotInclude("step ran")
	})
}

// TestToolCaches_AnExecutorRefusesASecretAddOrSourceThatResolvesIntoACache specifies that the executor fails the
// attempt, naming the paths, when a secret's file or a volume resolves at, beneath or above a cache before a run()
// step, or an add() destination resolves at or beneath one.
func TestToolCaches_AnExecutorRefusesASecretAddOrSourceThatResolvesIntoACache(t *testing.T) {
	parallelRule(t)
	t.Run("A secret's file inside a cache", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenTheBuildFileDeclares(anAliasedBase("ln -s /cache /alias"))
		w.GivenTheBuildFileDeclares(`
			tool_cache(name = "tool", path = "/cache")
			task(name = "build", base = "aliased", tool_caches = [mount("tool")],
			     steps = [run(["echo", "step ran"], secrets = [secret("token", file = "/alias/token")])])
		`)
		w.WhenIBuild("build", withSecrets("id=token,env=HOME"))
		w.ThenItFailsWith(`task "build"`, "/alias/token", "/cache")
		w.ThenTheOutputDoesNotInclude("step ran")
	})

	t.Run("A volume that an earlier step moves into a cache", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenARegistry()
		w.WhenIPush("busybox", "<registry>/volume:v1")
		w.ThenItSucceeds()
		w.GivenTheRegistrysImageDeclaresTheVolume("<registry>/volume:v1", "/alias/data")
		w.GivenTheBuildFileDeclares(`
			pull(name = "volume", image = "<registry>/volume", tag = "v1")
			tool_cache(name = "tool", path = "/cache")
			task(name = "build", base = "volume", tool_caches = [mount("tool")], steps = [
			    run(["sh", "-c", "rm -rf /alias && ln -s /cache /alias"]),
			    run(["echo", "second ran"]),
			])
		`)
		w.WhenIBuild("build")
		w.ThenItFailsWith(`task "build"`, "/alias/data", "/cache")
		w.ThenTheOutputDoesNotInclude("second ran")
	})

	t.Run("A symbolic link that an earlier step makes into a cache", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenTheBuildFileDeclares(`
			glob(name = "src", include = ["file"])
			tool_cache(name = "tool", path = "/cache")
			task(name = "build", base = "busybox", tool_caches = [mount("tool")],
			     steps = [run(["ln", "-s", "/cache", "/alias"]), add("src", "/alias/file")])
		`)
		w.GivenTheProjectHolds(files{"file": testenv.NewNonce()})
		w.WhenIBuild("build")
		w.ThenItFailsWith(`task "build"`, "/alias/file", "/cache")
	})

	t.Run("An add() destination that a variable puts in a cache", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenTheBuildFileDeclares(`
			glob(name = "src", include = ["file"])
			layer(name = "go", base = "busybox", steps = [env(GOPATH = "/go")])
			tool_cache(name = "tool", path = "/go/pkg/mod")
			task(name = "build", base = "go", tool_caches = [mount("tool")],
			     steps = [add("src", "$GOPATH/pkg/mod/extra"), run(["echo", "step ran"])])
		`)
		w.GivenTheProjectHolds(files{"file": testenv.NewNonce()})
		w.WhenIBuild("build")
		w.ThenItFailsWith(`task "build"`, "/go/pkg/mod", "/go/pkg/mod/extra")
		w.ThenTheOutputDoesNotInclude("step ran")
	})
}

// TestToolCaches_PruningDeletesCaches specifies that vamos prune on a standalone local executor deletes every
// workspace's caches with the rest of its cache and reports how many it removed, and never one that a build mounts.
func TestToolCaches_PruningDeletesCaches(t *testing.T) {
	parallelRule(t)
	t.Run("A build after a prune starts with empty caches", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenTheBuildFileDeclares(theProbe(`mount("tool")`))
		w.WhenIBuild("probe", setting("file=one"))
		w.ThenItSucceeds()
		w.WhenIPrune()
		w.ThenItSucceeds()
		w.ThenTheOutputIncludes(", 1 tool caches and")
		w.WhenIBuild("probe", setting("file=two"))
		w.ThenItSucceeds()
		w.ThenTheStepsObserved(observations{"listing": ""})
	})

	t.Run("A prune while another vamos uses the cache leaves it", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenTheBuildFileDeclares(theProbe(`mount("tool")`))
		w.WhenIBuild("probe", setting("file=one"))
		w.ThenItSucceeds()
		w.GivenAnotherVamosIsUsingTheBuildCache()
		w.WhenIPrune()
		w.ThenItFailsWith("another vamos is using the build cache")
		w.GivenTheOtherVamosIsDone()
		w.WhenIBuild("probe", setting("file=two"))
		w.ThenItSucceeds()
		w.ThenTheStepsObserved(observations{"listing": "one"})
	})
}

// TestToolCaches_ALocalCoordinatorsPruneLeavesToolCaches specifies that vamos prune on vamos coordinator --local
// neither deletes, moves nor counts the caches of standalone builds that share its store.
func TestToolCaches_ALocalCoordinatorsPruneLeavesToolCaches(t *testing.T) {
	parallelRule(t)
	t.Run("Pruning a local coordinator keeps a standalone build's cache", func(t *testing.T) {
		w := newToolCacheWorld(t)
		w.GivenTheBuildFileDeclares(theProbe(`mount("tool")`))
		w.WhenIBuild("probe", setting("file=one"))
		w.ThenItSucceeds()
		w.GivenALocalCoordinator()
		w.WhenIPrune(onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheOutputIncludes(", 0 tool caches and")
		w.WhenIBuild("probe", setting("file=two"))
		w.ThenItSucceeds()
		w.ThenTheStepsObserved(observations{"listing": "one"})
	})
}
