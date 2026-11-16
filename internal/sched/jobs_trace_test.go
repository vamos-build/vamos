package sched

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/vamos-build/vamos/internal/api"
	"github.com/vamos-build/vamos/internal/cache"

	"github.com/opencontainers/go-digest"
)

// TestJobsTrace checks that a job's manifest is found by its ID while the job runs, once it has ended, and once it
// is evicted, and that a job without one, an evicted one remembered from an earlier run, and an unknown one each say
// so.
func TestJobsTrace(t *testing.T) {
	now := time.Unix(1000, 0)
	var mu sync.Mutex
	jobs := &Jobs{now: func() time.Time { mu.Lock(); defer mu.Unlock(); return now }}
	jobs.Expire(time.Minute, time.Minute)
	evicted := map[api.JobID]bool{}
	jobs.OnEvict(func(id api.JobID) {
		mu.Lock()
		defer mu.Unlock()
		evicted[id] = true
	})
	trace := &cache.Blob{Digest: digest.FromString("manifest"), Size: 7}

	release := make(chan struct{})
	traced, err := jobs.Start(false, func(ctx context.Context, ev Events) api.Outcome {
		<-release
		return api.Outcome{Trace: trace}
	})
	if err != nil {
		t.Fatal(err)
	}
	untraced, err := jobs.Start(false, func(ctx context.Context, ev Events) api.Outcome {
		return api.Outcome{}
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.Trace(traced); !errors.Is(err, api.ErrRunning) {
		t.Errorf("Trace of a running job = %v; want %v", err, api.ErrRunning)
	}
	close(release)
	for _, id := range []api.JobID{traced, untraced} {
		j, err := jobs.Lookup(id)
		if err != nil {
			t.Fatal(err)
		}
		if err := j.Watch(t.Context(), 0, func(api.Event) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	check := func(when string) {
		t.Helper()
		if got, err := jobs.Trace(traced); err != nil || got != *trace {
			t.Errorf("Trace of a job with a manifest, %s = %v, %v; want %v", when, got, err, *trace)
		}
		if _, err := jobs.Trace(untraced); !errors.Is(err, api.ErrNoTrace) {
			t.Errorf("Trace of a job without one, %s = %v; want %v", when, err, api.ErrNoTrace)
		}
	}
	check("once it has ended")

	for _, id := range []api.JobID{traced, untraced} {
		j, _ := jobs.Lookup(id)
		j.Release()
	}
	jobs.Sweep()
	if _, err := jobs.Lookup(traced); !errors.Is(err, api.ErrExpired) {
		t.Fatalf("Lookup after the sweep = %v; want %v", err, api.ErrExpired)
	}
	check("once it is evicted")
	mu.Lock()
	if !evicted[traced] || !evicted[untraced] {
		t.Errorf("OnEvict was told of %v; want both jobs", evicted)
	}
	mu.Unlock()

	jobs.RememberEvicted("earlier", trace)
	jobs.RememberEvicted("earlier without", nil)
	if got, err := jobs.Trace("earlier"); err != nil || got != *trace {
		t.Errorf("Trace of a job remembered with a manifest = %v, %v; want %v", got, err, *trace)
	}
	if _, err := jobs.Trace("earlier without"); !errors.Is(err, api.ErrNoTrace) {
		t.Errorf("Trace of a job remembered without one = %v; want %v", err, api.ErrNoTrace)
	}
	if _, err := jobs.Trace("never"); !errors.Is(err, api.ErrNoJob) {
		t.Errorf("Trace of an unknown job = %v; want %v", err, api.ErrNoJob)
	}
	// Once evictedRemembered more are evicted, the first is forgotten.
	for i := range evictedRemembered {
		jobs.RememberEvicted(api.JobID(fmt.Sprint("later ", i)), nil)
	}
	if _, err := jobs.Trace("earlier"); !errors.Is(err, api.ErrNoJob) {
		t.Errorf("Trace of a job no longer remembered = %v; want %v", err, api.ErrNoJob)
	}
	jobs.Stop()
}
