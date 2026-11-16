package cli

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vamos-build/vamos/internal/api"
	"github.com/vamos-build/vamos/internal/cache"
	"github.com/vamos-build/vamos/internal/remote"
	"github.com/vamos-build/vamos/internal/tracing"
)

// storedTraces is a coordinator that runs nothing, whose jobs are known by the manifests it stored for them.
type storedTraces struct {
	api.Executor
	traces  map[api.JobID]cache.Blob
	running map[api.JobID]bool
}

func (s storedTraces) Capabilities(context.Context) (api.Capabilities, error) {
	return api.Capabilities{Instance: "i-1", Clock: time.Now()}, nil
}

func (s storedTraces) Trace(_ context.Context, id api.JobID) (cache.Blob, error) {
	if s.running[id] {
		return cache.Blob{}, api.ErrRunning
	}
	if b, ok := s.traces[id]; ok {
		return b, nil
	}
	return cache.Blob{}, api.ErrNoJob
}

func (storedTraces) Release(context.Context, api.JobID) error { return nil }

// TestTraceCommand checks what vamos trace writes for each kind of stored trace, by digest and by job, and how it
// fails, naming what it was given and writing nothing.
func TestTraceCommand(t *testing.T) {
	m := cache.NewMemory()
	ctx := t.Context()
	put := func(data []byte) cache.Blob {
		b, err := m.PutBytes(ctx, data)
		must(t, err)
		return b
	}
	now := time.Now()
	step := tracing.Span{ID: "s1", Process: "executor", Track: `layer "app"`, Name: `run(["make"])`, Start: now,
		End: now.Add(time.Second)}
	attemptData, err := tracing.EncodeAttempt("a1", []tracing.Span{step}, tracing.Lost{})
	must(t, err)
	attempt := put(attemptData)
	target := tracing.Span{ID: "t1", Process: "executor", Track: `layer "app"`, Name: `layer "app"`, Start: now,
		End: now.Add(2 * time.Second)}
	manifestData, err := api.JobTrace{Spans: []tracing.Span{target}, Entries: []api.TraceEntry{{Node: 1,
		Desc: `layer "app"`, Outcome: api.NodeMiss, Started: true, Attempt: "a1",
		Trace: &attempt}}}.Encode(tracing.Lost{})
	must(t, err)
	manifest := put(manifestData)
	log := put([]byte("step output\n"))
	malformed := put([]byte(`{"version":1,"kind":`))
	malformedJob := put([]byte(`{"version":1,"kind":"job","spans":"invalid"}`))
	unknownKind := put([]byte(`{"version":1,"kind":"party"}`))
	unknownVersion := put([]byte(`{"version":9,"kind":"attempt"}`))
	big := make([]byte, tracing.MaxTraceSize+1)
	oversized := put(big)

	coordinator := storedTraces{traces: map[api.JobID]cache.Blob{"0123456789abcdef": manifest},
		running: map[api.JobID]bool{"fedcba9876543210": true}}
	server := httptest.NewServer((&remote.Server{Executor: coordinator, Content: m}).Handler())
	t.Cleanup(server.Close)
	fsys := project(t, nil)

	for _, tc := range []struct {
		name  string
		args  []string
		want  string   // in the error, or "" for success
		spans []string // that the file written holds
	}{
		{name: "a command's job by its ID", args: []string{"0123456789abcdef", "--coordinator", server.URL},
			spans: []string{`layer "app"`, `run(["make"])`}},
		{name: "a job's trace by its digest", args: []string{manifest.Digest.String(), "--coordinator", server.URL},
			spans: []string{`layer "app"`, `run(["make"])`}},
		{name: "an attempt's trace by its digest", args: []string{attempt.Digest.String(), "--coordinator", server.URL},
			spans: []string{`run(["make"])`}},
		{name: "a digest cut short", args: []string{"sha256:abc123"},
			want: `"sha256:abc123" is not a valid digest`},
		{name: "a job without a coordinator", args: []string{"0123456789abcdef"},
			want: `"0123456789abcdef" is a job, which is read from the coordinator that holds it`},
		{name: "a running job", args: []string{"fedcba9876543210", "--coordinator", server.URL},
			want: "the job fedcba9876543210 is still running"},
		{name: "a job the coordinator doesn't know", args: []string{"00000000000000aa", "--coordinator", server.URL},
			want: "the coordinator doesn't know the job 00000000000000aa any more; the trace digest that its " +
				"command printed still reads its trace"},
		{name: "a log", args: []string{log.Digest.String(), "--coordinator", server.URL},
			want: log.Digest.String() + " is not a trace this vamos can read"},
		{name: "malformed JSON", args: []string{malformed.Digest.String(), "--coordinator", server.URL},
			want: malformed.Digest.String() + " is not a trace this vamos can read"},
		{name: "a job's trace that doesn't decode", args: []string{malformedJob.Digest.String(), "--coordinator",
			server.URL}, want: malformedJob.Digest.String() + " is not a trace this vamos can read"},
		{name: "an unknown kind", args: []string{unknownKind.Digest.String(), "--coordinator", server.URL},
			want: unknownKind.Digest.String() +
				` is not a trace this vamos can read: not a stored trace: its kind "party"`},
		{name: "an unknown version", args: []string{unknownVersion.Digest.String(), "--coordinator", server.URL},
			want: unknownVersion.Digest.String() +
				" is not a trace this vamos can read: not a stored trace: its version 9"},
		{name: "a blob larger than any trace", args: []string{oversized.Digest.String(), "--coordinator", server.URL},
			want: oversized.Digest.String() +
				" is not a trace this vamos can read: it is larger than any stored trace"},
		{name: "a trace the executor no longer holds",
			args: []string{"sha256:" + strings.Repeat("0", 64), "--coordinator", server.URL},
			want: "sha256:" + strings.Repeat("0", 64) + ": the executor no longer holds this trace"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "trace.json")
			args := append([]string{"trace", tc.args[0], out}, tc.args[1:]...)
			r := vamos(t, fsys, args...)
			if tc.want != "" {
				if r.err == nil || !strings.Contains(r.err.Error(), tc.want) {
					t.Errorf("vamos trace = %v; want an error with %q", r.err, tc.want)
				}
				if _, err := os.Stat(out); !os.IsNotExist(err) {
					t.Errorf("a failed vamos trace left %s: %v", out, err)
				}
				return
			}
			r.ran(t)
			data, err := os.ReadFile(out)
			must(t, err)
			var file struct {
				TraceEvents []struct{ Name string } `json:"traceEvents"`
			}
			must(t, json.Unmarshal(data, &file))
			for _, want := range tc.spans {
				found := false
				for _, e := range file.TraceEvents {
					found = found || e.Name == want
				}
				if !found {
					t.Errorf("the trace written has no %q", want)
				}
			}
		})
	}
}
