package cli

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vamos-build/vamos/internal/api"
	"github.com/vamos-build/vamos/internal/remote"
)

// warningPrune is an executor whose prune removes a target history and warns that it couldn't remove another.
type warningPrune struct{ api.Executor }

var warnedPrune = api.Outcome{Pruned: &api.Pruned{Results: 2, Blobs: 3, Histories: 1,
	Warnings: []string{"pruning target histories: removing a target history: device busy"}}}

func (warningPrune) Capabilities(context.Context) (api.Capabilities, error) {
	return api.Capabilities{Instance: "i-1"}, nil
}
func (warningPrune) Prune(context.Context) (api.JobID, error) { return "prune", nil }
func (warningPrune) Watch(_ context.Context, _ api.JobID, from int64, fn func(api.Event) error) error {
	if from > 0 {
		return nil
	}
	o := warnedPrune.Clone()
	return fn(api.Event{Kind: api.EventDone, Outcome: &o})
}
func (warningPrune) Result(context.Context, api.JobID) (api.Outcome, error) {
	return warnedPrune.Clone(), nil
}
func (warningPrune) Release(context.Context, api.JobID) error { return nil }

// TestPruneReportsHistories checks that a prune on a coordinator reports the target histories it removed with the
// rest, and each of its warnings.
func TestPruneReportsHistories(t *testing.T) {
	server := httptest.NewServer((&remote.Server{Executor: warningPrune{}}).Handler())
	t.Cleanup(server.Close)
	var stdout, stderr bytes.Buffer
	inv := &invocation{
		app:    &app{fs: project(t, nil)},
		ctx:    t.Context(),
		stdout: &stdout,
		stderr: &stderr,
		opts:   defaultOptions(),
	}
	inv.opts.coordinator = server.URL
	must(t, inv.prune())
	if want := "Removed 2 results, 3 files, 0 tool caches and 1 target histories from the build cache, and 0 image " +
		"tags from the cache repository\n"; stdout.String() != want {
		t.Errorf("prune printed %q; want %q", stdout.String(), want)
	}
	if want := "vamos: warning: pruning target histories: removing a target history: device busy\n"; !strings.Contains(
		stderr.String(), want) {
		t.Errorf("prune warned %q; want %q", stderr.String(), want)
	}
}
