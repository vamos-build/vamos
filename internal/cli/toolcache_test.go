package cli

import (
	"strings"
	"testing"
)

// TestCommandsRefuseAToolCache checks that every command that takes a target refuses a tool cache's label, saying
// that a tool cache is mounted with mount(): build, which builds a publication's images rather than refusing one,
// among them, and publish, which takes a publication.
func TestCommandsRefuseAToolCache(t *testing.T) {
	const want = `"//tools:go-build" is a tool_cache(), which is no target: ` +
		"a tool cache is mounted with mount(), in a task's or layer's tool_caches"
	fsys := project(t, map[string]string{
		"WORKSPACE":   `workspace(name = "com.acme.shop")`,
		"tools/BUILD": `tool_cache(name = "go-build", path = "/cache/go-build")`,
	})
	dir := t.TempDir()
	for _, args := range [][]string{
		{"build", "//tools:go-build"},
		{"save", "//tools:go-build", dir + "/app.tar"},
		{"write", "//tools:go-build", dir + "/app.tar"},
		{"export", "//tools:go-build", dir + "/out"},
		{"push", "//tools:go-build", "registry.example/app:v1"},
		{"load", "//tools:go-build", "app:v1"},
		{"graph", "//tools:go-build"},
		{"publish", "//tools:go-build", "--tag", "v1"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			r := vamos(t, fsys, append(args, "-C", projectDir)...)
			if r.err == nil || !strings.Contains(r.err.Error(), want) {
				t.Errorf("vamos %s = %v; want an error with %q", strings.Join(args, " "), r.err, want)
			}
		})
	}
}
