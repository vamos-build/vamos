package apitest

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/vamos-build/vamos/internal/api"
	"github.com/vamos-build/vamos/internal/cache"

	"github.com/opencontainers/go-digest"
)

// traced is an executor whose one job has ended with an outcome naming its manifest and a prune's counts.
type traced struct{ api.Executor }

var tracedOutcome = api.Outcome{
	Trace:  &cache.Blob{Digest: digest.FromString("manifest"), Size: 7},
	Pruned: &api.Pruned{Results: 1, Histories: 2, Warnings: []string{"removing a target history: busy"}},
}

func (traced) Result(context.Context, api.JobID) (api.Outcome, error) {
	return tracedOutcome.Clone(), nil
}

func (traced) Trace(_ context.Context, id api.JobID) (cache.Blob, error) {
	if id != "job" {
		return cache.Blob{}, api.ErrNoTrace
	}
	return *tracedOutcome.Trace, nil
}

// TestJSONTrace checks that an outcome's trace and a prune's histories and warnings cross as JSON, and that Trace
// does, with its errors.
func TestJSONTrace(t *testing.T) {
	j := NewJSON(traced{})
	got, err := j.Result(t.Context(), "job")
	if err != nil || !reflect.DeepEqual(got, tracedOutcome) {
		t.Errorf("Result = %+v, %v; want %+v", got, err, tracedOutcome)
	}
	if b, err := j.Trace(t.Context(), "job"); err != nil || b != *tracedOutcome.Trace {
		t.Errorf("Trace = %v, %v; want %v", b, err, *tracedOutcome.Trace)
	}
	if _, err := j.Trace(t.Context(), "other"); !errors.Is(err, api.ErrNoTrace) {
		t.Errorf("Trace of a job without one = %v; want %v", err, api.ErrNoTrace)
	}
}
