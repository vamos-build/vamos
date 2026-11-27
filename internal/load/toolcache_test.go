package load

import (
	"context"
	"maps"
	"reflect"
	"strings"
	"testing"

	"github.com/vamos-build/vamos/internal/cache"
	"github.com/vamos-build/vamos/internal/fileset"
	"github.com/vamos-build/vamos/internal/graph"
	"github.com/vamos-build/vamos/internal/testenv"

	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/afero"
)

// loadNamed loads targets from the workspace com.acme.shop, whose root package's BUILD file is build and whose other
// files, by path relative to the root, are others.
func loadNamed(t *testing.T, build string, others map[string]string, targets ...string) (*graph.Graph, error) {
	t.Helper()
	fsys := afero.NewMemMapFs()
	files := map[string]string{"WORKSPACE": `workspace(name = "com.acme.shop")`, "BUILD": build}
	maps.Copy(files, others)
	for name, content := range files {
		testenv.Must(t, afero.WriteFile(fsys, "/project/"+name, []byte(content), 0o644))
	}
	return Load(t.Context(), fsys, Request{
		Dir:          "/project",
		Targets:      targets,
		Want:         WantBuild,
		ExecPlatform: imgspecv1.Platform{OS: "linux", Architecture: "amd64"},
		Store: func(context.Context, afero.Fs, string, *fileset.Selection) (cache.Blob, fileset.Stats, error) {
			t.Fatal("a glob was read, though the project has none")
			return cache.Blob{}, fileset.Stats{}, nil
		},
	})
}

// buildMounting declares a task "build" on scratch that mounts the tool cache "tool", declared by rule, with a run()
// step.
func buildMounting(rule string) string {
	return rule + `
task(name = "build", base = "scratch", tool_caches = [mount("tool")], steps = [run(["true"])])
`
}

// TestToolCacheOwner checks that a tool cache's owner and group are decimal digits, read in base ten, from 0 to
// 4294967294, so that one written with leading zeros is the same owner.
func TestToolCacheOwner(t *testing.T) {
	for _, tc := range []struct {
		owner string
		want  uint32
		err   string // in the error, or "" if it loads
	}{
		{owner: "0", want: 0},
		{owner: "4294967294", want: 4294967294},
		{owner: "01000", want: 1000},
		{owner: "4294967295", err: `owner "4294967295" must be from 0 to 4294967294`},
		{owner: strings.Repeat("9", 30), err: "must be from 0 to 4294967294"},
		{owner: "-1", err: `owner "-1" must be a numeric ID`},
		{owner: "+1000", err: `owner "+1000" must be a numeric ID`},
		{owner: "", err: `owner "" must be a numeric ID`},
	} {
		t.Run(tc.owner, func(t *testing.T) {
			g, err := loadNamed(t, buildMounting(`tool_cache(name = "tool", path = "/cache", owner = "`+tc.owner+
				`", group = "7")`), nil, "build")
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("Load = %v; want an error with %q", err, tc.err)
				}
				return
			}
			testenv.Must(t, err)
			got := g.Node(g.Roots[0]).Task.ToolCaches
			want := []graph.ToolCache{{Path: "/cache", Label: "//:tool", Owner: tc.want, Group: 7}}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("tool caches = %+v; want %+v", got, want)
			}
		})
	}
}

// TestToolCachePath checks the paths a tool_cache rule refuses: those buildah mounts itself or binds a file at,
// beneath or above, as it does in every run() step; and that a path that only starts like one is accepted.
func TestToolCachePath(t *testing.T) {
	for _, tc := range []struct {
		path string
		err  string // in the error, or "" if it loads
	}{
		{path: "/dev", err: `path "/dev" is within /dev`},
		{path: "/dev/shm/cache", err: `path "/dev/shm/cache" is within /dev`},
		{path: "/proc/cache", err: "is within /proc"},
		{path: "/sys", err: "is within /sys"},
		{path: "/etc", err: `path "/etc" contains /etc/hosts`},
		{path: "/run", err: `path "/run" contains /run/.containerenv`},
		{path: "/etc/hosts", err: `path "/etc/hosts" is /etc/hosts`},
		{path: "/run/.containerenv/cache", err: `path "/run/.containerenv/cache" is beneath /run/.containerenv`},
		{path: "/etc/crypto-policies/back-ends", err: "is beneath /etc/crypto-policies"},
		{path: "/", err: "must not be the root directory"},
		{path: "/cache/", err: `path "/cache/" must be in clean form, as "/cache"`},
		{path: "cache", err: "must be an absolute path"},
		{path: "$HOME/cache", err: `must not contain "$"`},
		{path: "/device/cache"},
		{path: "/run/cache"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			_, err := loadNamed(t, buildMounting(`tool_cache(name = "tool", path = "`+tc.path+`")`), nil, "build")
			if tc.err == "" {
				testenv.Must(t, err)
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("Load = %v; want an error with %q", err, tc.err)
			}
		})
	}
}

// TestToolCacheLabels checks that a mount names its cache by a label relative to its own package, which the graph
// holds in canonical form, and that it mounts the cache at the cache's path unless it overrides it, the mounts sorted
// by path, in a task, and in a layer given by a function of ctx that a task is built on.
func TestToolCacheLabels(t *testing.T) {
	g, err := loadNamed(t, "", map[string]string{
		"tools/BUILD": `
tool_cache(name = "go-build", path = "/cache/go-build", owner = "1000", group = "1000")
tool_cache(name = "go-mod", path = "/cache/go-mod")
task(
    name = "local",
    base = "scratch",
    tool_caches = [mount(":go-build"), mount("go-mod", path = "/a/mod")],
    steps = [run(["true"])],
)
`,
		"app/BUILD": `
layer(
    name = "base",
    base = "scratch",
    tool_caches = [mount("//tools:go-mod")],
    steps = lambda ctx: [run(["true"])],
)
task(
    name = "app",
    base = "base",
    tool_caches = [mount("//tools:go-mod"), mount("//tools:go-build", path = "/root/.cache/go-build")],
    steps = [run(["true"])],
)
`,
	}, "//tools:local", "//app:app")
	testenv.Must(t, err)
	for i, want := range [][]graph.ToolCache{
		{
			{Path: "/a/mod", Label: "//tools:go-mod"},
			{Path: "/cache/go-build", Label: "//tools:go-build", Owner: 1000, Group: 1000},
		},
		{
			{Path: "/cache/go-mod", Label: "//tools:go-mod"},
			{Path: "/root/.cache/go-build", Label: "//tools:go-build", Owner: 1000, Group: 1000},
		},
	} {
		n := g.Node(g.Roots[i])
		if got := n.Task.ToolCaches; !reflect.DeepEqual(got, want) {
			t.Errorf("%s mounts %+v; want %+v", n.Desc, got, want)
		}
	}
	base := g.Node(g.Node(g.Roots[1]).Task.Base)
	want := []graph.ToolCache{{Path: "/cache/go-mod", Label: "//tools:go-mod"}}
	if got := base.Layer.ToolCaches; !reflect.DeepEqual(got, want) {
		t.Errorf("%s mounts %+v; want %+v", base.Desc, got, want)
	}
}

// TestToolCacheConflicts checks what loading refuses of a target that mounts caches, and what it leaves to the
// executor: paths with a variable, which only the image's environment settles, and a source with "..", which the
// executor resolves through symbolic links before applying it.
func TestToolCacheConflicts(t *testing.T) {
	const rules = `
tool_cache(name = "a", path = "/cache")
tool_cache(name = "b", path = "/cache/go")
tool_cache(name = "c", path = "/build")
`
	for _, tc := range []struct {
		name   string
		mounts string // the task's tool_caches
		steps  string // its steps, which must run
		source string // its source, if any
		err    string // in the error, or "" if it loads
	}{
		{
			name:   "a cache beneath another",
			mounts: `mount("a"), mount("b")`,
			err:    "mounts b at /cache/go, within a at /cache; tool caches can't be nested",
		},
		{
			name:   "a cache above another",
			mounts: `mount("b"), mount("a")`,
			err:    "mounts b at /cache/go, within a at /cache",
		},
		{
			name:   "a cache moved beneath another",
			mounts: `mount("a"), mount("c", path = "/cache/build")`,
			err:    "mounts c at /cache/build, within a at /cache",
		},
		{
			name:   "a secret's file in a cache",
			mounts: `mount("a")`,
			steps:  `run(["true"], secrets = [secret("token", file = "/cache/token")])`,
			err:    `gives secret "token" as the file /cache/token, which overlaps the tool cache a`,
		},
		{
			name:   "a secret's file at a cache",
			mounts: `mount("a")`,
			steps:  `run(["true"], secrets = [secret("token", file = "/cache")])`,
			err:    "as the file /cache, which overlaps",
		},
		{
			name:   "a secret's file above a cache",
			mounts: `mount("b")`,
			steps:  `run(["true"], secrets = [secret("token", file = "/cache")])`,
			err:    "as the file /cache, which overlaps the tool cache b, mounted at /cache/go",
		},
		{
			name:   "an add() into a cache",
			mounts: `mount("a")`,
			steps:  `add("src", "/cache/src"), run(["true"])`,
			err:    `add("src", "/cache/src") puts files within the tool cache a, mounted at /cache`,
		},
		{
			name:   "an add() into a cache, spelled otherwise",
			mounts: `mount("a")`,
			steps:  `add("src", "/x/../cache/f"), run(["true"])`,
			err:    "puts files within the tool cache a",
		},
		{
			name:   "an add() above a cache",
			mounts: `mount("b")`,
			steps:  `add("src", "/cache"), run(["true"])`,
		},
		{
			name:   "an add() whose variable may put it in a cache",
			mounts: `mount("a")`,
			steps:  `add("src", "$DIR/../cache/f"), run(["true"])`,
		},
		{
			name:   "a workdir() in a cache",
			mounts: `mount("a")`,
			steps:  `workdir("/cache//sub"), run(["true"])`,
			err:    `workdir("/cache//sub") is within the tool cache a, mounted at /cache`,
		},
		{
			name:   "a workdir() at a cache once cleaned",
			mounts: `mount("a")`,
			steps:  `workdir("/x/../cache"), run(["true"])`,
			err:    "the working directory can't be in a tool cache",
		},
		{
			name:   "a workdir() in a cache, written with an escaped dollar",
			mounts: `mount("a")`,
			steps:  `workdir("/cache/$$tmp"), run(["true"])`,
			err:    `workdir("/cache/$$tmp") is within the tool cache a`,
		},
		{
			name:   "an add() into a cache, written with an escaped dollar",
			mounts: `mount("a")`,
			steps:  `add("src", "/cache/$$f"), run(["true"])`,
			err:    "puts files within the tool cache a",
		},
		{
			name:   "a workdir() above a cache",
			mounts: `mount("b")`,
			steps:  `workdir("/cache"), run(["true"])`,
		},
		{
			name:   "a workdir() whose variable may put it in a cache",
			mounts: `mount("a")`,
			steps:  `workdir("$CACHE"), run(["true"])`,
		},
		{
			name:   "a source in a cache",
			mounts: `mount("c")`,
			source: "/build/out",
			err:    `source "/build/out" is within the tool cache c, mounted at /build`,
		},
		{
			name:   "a source in a cache, spelled otherwise",
			mounts: `mount("c")`,
			source: "/build/./out",
			err:    "is within the tool cache c",
		},
		{
			name:   "a source above a cache",
			mounts: `mount("a", path = "/out/.cache")`,
			source: "/out",
		},
		{
			name:   "a source whose .. the executor resolves",
			mounts: `mount("c")`,
			source: "/work/../build",
		},
		{
			name:   "no run() step",
			mounts: `mount("a")`,
			steps:  `add("src", "/src")`,
			err:    "mounts tool caches but has no run() step",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps := tc.steps
			if steps == "" {
				steps = `run(["true"])`
			}
			output := ""
			if tc.source != "" {
				output = `source = "` + tc.source + `", include = ["*"],`
			}
			build := rules + `
task(name = "src", base = "scratch", steps = [run(["true"])], source = "/src", include = ["*"])
task(name = "build", base = "scratch", tool_caches = [` + tc.mounts + `], steps = [` + steps + `], ` + output + `)
`
			_, err := loadNamed(t, build, nil, "build")
			if tc.err == "" {
				testenv.Must(t, err)
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("Load = %v; want an error with %q", err, tc.err)
			}
		})
	}
}

// TestToolCacheDeclarations checks what declaring tool caches and the targets that mount them refuses or keeps:
// task()'s arguments given by position, which tool_caches follows; params, even empty, given to tool_cache(); a dep()
// that names a tool cache; and a literal tool_caches of a target given by a function of ctx, resolved as it is
// declared, so that it names only caches declared before it.
func TestToolCacheDeclarations(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build string // declaring the target "build"
		err   string // in the error, or "" if it loads
	}{
		{
			name:  "a task's arguments by position",
			build: `task("build", "scratch", [run(["mkdir", "/out"])], "/out", ["**"])`,
		},
		{
			name:  "a task's arguments by position, with a function of ctx",
			build: `task("build", "scratch", lambda ctx: [run(["mkdir", "/out"])], "/out", ["**"])`,
		},
		{
			name: "empty params given to a tool cache",
			build: `tool_cache(name = "tool", path = "/cache", params = {})
task(name = "build", base = "scratch", steps = [run(["true"])])`,
			err: "tool_cache: a tool cache takes no parameters",
		},
		{
			name: "params that aren't a dict given to a tool cache",
			build: `tool_cache(name = "tool", path = "/cache", params = 1)
task(name = "build", base = "scratch", steps = [run(["true"])])`,
			err: "tool_cache: a tool cache takes no parameters",
		},
		{
			name: "empty params given to a pull",
			build: `pull(name = "base", image = "busybox", tag = "1.36", params = {})
task(name = "build", base = "scratch", steps = [run(["true"])])`,
			err: "pull: a pull takes no parameters",
		},
		{
			name: "an owner and group of None",
			build: `tool_cache(name = "tool", path = "/cache", owner = None, group = None)
task(name = "build", base = "scratch", tool_caches = [mount("tool")], steps = [run(["true"])])`,
		},
		{
			name: "an owner of None and a group",
			build: `tool_cache(name = "tool", path = "/cache", owner = None, group = "1000")
task(name = "build", base = "scratch", tool_caches = [mount("tool")], steps = [run(["true"])])`,
			err: "tool_cache: owner and group are given together, or neither",
		},
		{
			name: "a mount with a path of None",
			build: `tool_cache(name = "tool", path = "/cache")
task(name = "build", base = "scratch", tool_caches = [mount("tool", path = None)], steps = [run(["true"])])`,
		},
		{
			name: "a mount of a publication",
			build: `layer(name = "image", base = "scratch")
publication(name = "release", images = {"registry.example/app": "image"})
task(name = "build", base = "scratch", tool_caches = [mount("release")], steps = [run(["true"])])`,
			err: `tool_caches[0] "release" is a publication(), not a tool_cache(); mount() names a tool cache`,
		},
		{
			name: "a tool cache's name that isn't UTF-8",
			build: `tool_cache(name = "é"[0], path = "/cache")
task(name = "build", base = "scratch", steps = [run(["true"])])`,
			err: "must be UTF-8, as the graph encodes it",
		},
		{
			name: "a tool cache's path that isn't UTF-8",
			build: `tool_cache(name = "tool", path = "/cache" + "é"[0])
task(name = "build", base = "scratch", tool_caches = [mount("tool")], steps = [run(["true"])])`,
			err: "must be UTF-8",
		},
		{
			name: "a dep() of a tool cache",
			build: `tool_cache(name = "tool", path = "/cache")
d = dep("tool")
task(name = "build", base = "scratch", steps = [run(["true"])])`,
			err: `dep: "tool" is a tool_cache(), which is no target`,
		},
		{
			name: "a cache declared later, with a function of ctx",
			build: `task(name = "build", base = "scratch", tool_caches = [mount("tool")],
     steps = lambda ctx: [run(["true"])])
tool_cache(name = "tool", path = "/cache")`,
			err: `task "build": tool_caches[0] "tool" is not an earlier tool_cache(); there are none before this`,
		},
		{
			name: "a cache path with a function of ctx",
			build: `tool_cache(name = "tool", path = "/cache")
task(name = "build", base = "scratch", tool_caches = [mount("tool", path = "/dev/cache")],
     steps = lambda ctx: [run(["true"])])`,
			err: `task "build": mount("tool", path = "/dev/cache"): path "/dev/cache" is within /dev`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadNamed(t, tc.build, nil, "build")
			if tc.err == "" {
				testenv.Must(t, err)
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("Load = %v; want an error with %q", err, tc.err)
			}
		})
	}
}

// TestToolCachePullsInAnUnnamedWorkspace checks that vamos update finds the pulls of a target that mounts caches in an
// unnamed workspace, which can't be built, rather than being unsure of them, so that it removes the pins no pull uses.
func TestToolCachePullsInAnUnnamedWorkspace(t *testing.T) {
	fsys := afero.NewMemMapFs()
	testenv.Must(t, afero.WriteFile(fsys, "/project/WORKSPACE", nil, 0o644))
	testenv.Must(t, afero.WriteFile(fsys, "/project/BUILD", []byte(`
pull(name = "base", image = "busybox", tag = "1.36")
tool_cache(name = "tool", path = "/cache")
task(name = "app", base = "base", tool_caches = [mount("tool")], steps = [run(["true"])])
`), 0o644))
	pulls, _, unsure, err := Pulls(t.Context(), fsys, Request{
		Dir:          "/project",
		ExecPlatform: imgspecv1.Platform{OS: "linux", Architecture: "amd64"},
	})
	testenv.Must(t, err)
	if unsure != "" || len(pulls) != 1 || pulls[0].Label != "base" {
		t.Errorf("Pulls = %+v, unsure %q; want base alone, and sure", pulls, unsure)
	}
}

// TestToolCachesOfAConfiguredTarget checks that a target given by a function of ctx keeps the lists it was declared
// with, which a later append doesn't change, even one list given for two arguments, and that tool_caches given by a
// function resolve as the target is configured, once every BUILD file has run.
func TestToolCachesOfAConfiguredTarget(t *testing.T) {
	g, err := loadNamed(t, `
caches = []
include = ["a"]
task(name = "appended", base = "scratch", tool_caches = caches, steps = lambda ctx: [run(["true"])],
     source = "/out", include = include)
task(name = "given", base = "scratch", tool_caches = lambda ctx: [mount("tool")], steps = [run(["true"])])
shared = []
layer(name = "aliased", base = "scratch", command = shared, tool_caches = shared, steps = lambda ctx: [])
tool_cache(name = "tool", path = "/cache")
caches.append(mount("tool"))
include.append("b")
shared.append("echo")
`, nil, "appended", "given", "aliased")
	testenv.Must(t, err)
	appended := g.Node(g.Roots[0]).Task
	if appended.ToolCaches != nil {
		t.Errorf("appended mounts %+v; want none, as declared", appended.ToolCaches)
	}
	if got := appended.Output.Selection.Include; !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("appended includes %q; want [a], as declared", got)
	}
	if got := g.Node(g.Roots[2]).Layer.Command; len(got) != 0 {
		t.Errorf("aliased's command = %q; want none, as declared", got)
	}
	want := []graph.ToolCache{{Path: "/cache", Label: "//:tool"}}
	if got := g.Node(g.Roots[1]).Task.ToolCaches; !reflect.DeepEqual(got, want) {
		t.Errorf("given mounts %+v; want %+v", got, want)
	}
}

// TestToolCacheNone checks that None for a tool cache's owner and group, or a mount's path, is taken as not given:
// root's owner and group, and the cache's own path.
func TestToolCacheNone(t *testing.T) {
	g, err := loadNamed(t, `
tool_cache(name = "tool", path = "/cache", owner = None, group = None)
task(name = "build", base = "scratch", tool_caches = [mount("tool", path = None)], steps = [run(["true"])])
`, nil, "build")
	testenv.Must(t, err)
	want := []graph.ToolCache{{Path: "/cache", Label: "//:tool"}}
	if got := g.Node(g.Roots[0]).Task.ToolCaches; !reflect.DeepEqual(got, want) {
		t.Errorf("build mounts %+v; want %+v", got, want)
	}
}

// TestToolCacheLabelOfAColonPackage checks that a mount in a package whose directory's name holds a colon, which
// only -C can reach, makes a graph that graph.Validate accepts.
func TestToolCacheLabelOfAColonPackage(t *testing.T) {
	fsys := afero.NewMemMapFs()
	testenv.Must(t, afero.WriteFile(fsys, "/project/WORKSPACE", []byte(`workspace(name = "com.acme.shop")`), 0o644))
	testenv.Must(t, afero.WriteFile(fsys, "/project/v2:tools/BUILD", []byte(`
tool_cache(name = "tool", path = "/cache")
task(name = "build", base = "scratch", tool_caches = [mount("tool")], steps = [run(["true"])])
`), 0o644))
	g, err := Load(t.Context(), fsys, Request{
		Dir:          "/project/v2:tools",
		Targets:      []string{"build"},
		Want:         WantBuild,
		ExecPlatform: imgspecv1.Platform{OS: "linux", Architecture: "amd64"},
	})
	testenv.Must(t, err)
	if got := g.Node(g.Roots[0]).Task.ToolCaches[0].Label; got != "//v2:tools:tool" {
		t.Errorf("the mount's label = %q; want //v2:tools:tool", got)
	}
}
