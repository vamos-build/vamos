package load

import (
	"context"
	"strings"
	"testing"

	"github.com/vamos-build/vamos/internal/cache"
	"github.com/vamos-build/vamos/internal/fileset"
	"github.com/vamos-build/vamos/internal/testenv"

	"github.com/opencontainers/go-digest"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/afero"
)

// TestNodeTarget checks that a target's node is identified by its label, platform and every parameter's value,
// the same whether the platform and parameters are given or defaulted, and whatever order the parameters are
// declared in, and differently in another configuration; and that its base is identified as configured.
func TestNodeTarget(t *testing.T) {
	const declared = `
layer(name="base", base="scratch")
layer(name="app", base="base", params={%s})
`
	modeFirst := `"mode": "debug", "level": 1`
	levelFirst := `"level": 1, "mode": "debug"`
	const want = `//:app platform=linux/amd64 level=1 mode="debug"`
	for _, tc := range []struct {
		name     string
		params   string
		platform string
		sets     []string
		want     string
	}{
		{name: "defaults", params: modeFirst, want: want},
		{name: "parameters declared in another order", params: levelFirst, want: want},
		{name: "the platform given", params: modeFirst, platform: "linux/amd64", want: want},
		{name: "defaults given", params: modeFirst, sets: []string{"mode=debug", "level=1"}, want: want},
		{
			name:   "another mode",
			params: modeFirst,
			sets:   []string{"mode=release"},
			want:   `//:app platform=linux/amd64 level=1 mode="release"`,
		},
		{
			name:     "another platform",
			params:   modeFirst,
			platform: "linux/arm64",
			want:     `//:app platform=linux/arm64 level=1 mode="debug"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fsys := afero.NewMemMapFs()
			build := []byte(fmtBuild(declared, tc.params))
			testenv.Must(t, afero.WriteFile(fsys, "/project/BUILD", build, 0o644))
			g, err := Load(t.Context(), fsys, Request{
				Dir:          "/project",
				Targets:      []string{"app"},
				Want:         WantImage,
				Sets:         tc.sets,
				Platform:     tc.platform,
				ExecPlatform: imgspecv1.Platform{OS: "linux", Architecture: "amd64"},
				Store: func(context.Context, afero.Fs, string, *fileset.Selection) (cache.Blob, fileset.Stats, error) {
					t.Fatal("a glob was read, though the project has none")
					return cache.Blob{}, fileset.Stats{}, nil
				},
			})
			testenv.Must(t, err)
			app := g.Node(g.Roots[0])
			if app.Target != tc.want {
				t.Errorf("app's target = %q; want %q", app.Target, tc.want)
			}
			platform := tc.platform
			if platform == "" {
				platform = "linux/amd64"
			}
			if base := g.Node(app.Layer.Base); base.Target != "//:base platform="+platform {
				t.Errorf("its base's target = %q; want //:base platform=%s", base.Target, platform)
			}
		})
	}
}

// TestNodeTargetKinds checks that pulls, layers and tasks are identified, and that indexes and globs, which keep no
// history, aren't.
func TestNodeTargetKinds(t *testing.T) {
	fsys := afero.NewMemMapFs()
	testenv.Must(t, afero.WriteFile(fsys, "/project/BUILD", []byte(`
pull(name="busybox", image="busybox", tag="1.36")
glob(name="src", include=["*.txt"])
layer(name="app", base="busybox", steps=[add("src", "/src")])
task(name="check", base="app")
index(name="multi", layer="app", platforms=["linux/amd64", "linux/arm64"])
`), 0o644))
	testenv.Must(t, afero.WriteFile(fsys, "/project/a.txt", nil, 0o644))
	g, err := Load(t.Context(), fsys, Request{
		Dir:          "/project",
		Targets:      []string{"check", "multi"},
		Want:         WantBuild,
		ExecPlatform: imgspecv1.Platform{OS: "linux", Architecture: "amd64"},
		Store: func(context.Context, afero.Fs, string, *fileset.Selection) (cache.Blob, fileset.Stats, error) {
			return cache.Blob{Digest: digest.FromString("tree"), Size: 1}, fileset.Stats{}, nil
		},
	})
	testenv.Must(t, err)
	want := map[string]string{
		`pull "busybox"`:                        "//:busybox platform=linux/amd64",
		`pull "busybox" (platform=linux/arm64)`: "//:busybox platform=linux/arm64",
		`layer "app"`:                           "//:app platform=linux/amd64",
		`layer "app" (platform=linux/arm64)`:    "//:app platform=linux/arm64",
		`task "check"`:                          "//:check platform=linux/amd64",
		`index "multi"`:                         "",
		`glob "src"`:                            "",
	}
	for _, n := range g.Nodes {
		w, ok := want[n.Desc]
		if !ok {
			t.Errorf("unexpected node %s", n.Desc)
			continue
		}
		delete(want, n.Desc)
		if n.Target != w {
			t.Errorf("%s's target = %q; want %q", n.Desc, n.Target, w)
		}
	}
	for desc := range want {
		t.Errorf("no node %s", desc)
	}
}

// fmtBuild returns build with params in place of its %s.
func fmtBuild(build, params string) string {
	return strings.Replace(build, "%s", params, 1)
}
