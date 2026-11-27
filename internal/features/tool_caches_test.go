package features

import (
	"fmt"
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
// tool cache is mounted with mount(); and that vamos update passes over it.
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
