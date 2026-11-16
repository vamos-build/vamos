package cli

import (
	"fmt"
	"runtime"
	"runtime/metrics"
	"sync"
	"testing"
	"time"

	"github.com/vamos-build/vamos/internal/api"
	"github.com/vamos-build/vamos/internal/cache"
	"github.com/vamos-build/vamos/internal/graph"
	"github.com/vamos-build/vamos/internal/tracing"
)

// BenchmarkAssembleNearTheCap assembles a command trace whose jobs name many attempt traces, each near
// tracing.MaxTraceSize, as --trace and vamos trace do, reporting its allocations and its peak heap, sampled while it
// runs, above the fixture's, so that what a command trace of many large attempts costs the client shows.
func BenchmarkAssembleNearTheCap(b *testing.B) {
	const attempts = 16
	ctx := b.Context()
	m := cache.NewMemory()
	start := time.Now()
	var entries []api.TraceEntry
	var targets []tracing.Span
	for i := range attempts {
		id := fmt.Sprint("a", i)
		desc := fmt.Sprintf(`task "t%d"`, i)
		var spans []tracing.Span
		for size := 0; size < tracing.MaxTraceSize*11/10; size += 200 { // a little past the cap, which trims it
			at := start.Add(time.Duration(len(spans)) * time.Microsecond)
			spans = append(spans, tracing.Span{ID: fmt.Sprint(id, ".", len(spans)), Process: "executor", Track: desc,
				Name: `run(["make", "--jobs", "8", "all"])`, Start: at, End: at.Add(time.Microsecond),
				Args: map[string]any{"step": len(spans)}})
		}
		data, err := tracing.EncodeAttempt(id, spans, tracing.Lost{})
		if err != nil {
			b.Fatal(err)
		}
		if len(data) < tracing.MaxTraceSize*9/10 {
			b.Fatalf("an attempt trace of %d bytes; want one near the cap, %d", len(data), tracing.MaxTraceSize)
		}
		blob, err := m.PutBytes(ctx, data)
		if err != nil {
			b.Fatal(err)
		}
		entries = append(entries, api.TraceEntry{Node: graph.Ref(i), Desc: desc, Outcome: api.NodeMiss, Started: true,
			Attempt: id, Trace: &blob})
		targets = append(targets, tracing.Span{ID: fmt.Sprint("t", i), Process: "executor", Track: desc, Name: desc,
			Start: start, End: start.Add(time.Hour)})
	}
	data, err := api.JobTrace{Spans: targets, Entries: entries}.Encode(tracing.Lost{})
	if err != nil {
		b.Fatal(err)
	}
	manifest, err := m.PutBytes(ctx, data)
	if err != nil {
		b.Fatal(err)
	}
	doc := api.CommandTrace{Jobs: []api.CommandJob{{ID: "j1", Manifest: &manifest}}}

	var most uint64
	for b.Loop() {
		b.StopTimer()
		runtime.GC()
		base := heapObjects() // the fixture's blobs, which assemble doesn't allocate
		var peak uint64
		stop := sampleHeap(&peak)
		b.StartTimer()
		spans := assemble(ctx, m, doc, tracing.Lost{})
		b.StopTimer()
		stop()
		most = max(most, peak-min(base, peak))
		for _, s := range spans {
			if s.Name == "trace incomplete" {
				b.Fatalf("an attempt's trace wasn't assembled: %v", s.Args)
			}
		}
		if len(spans) < attempts*100 {
			b.Fatalf("%d spans assembled; want the attempts'", len(spans))
		}
		b.StartTimer()
	}
	b.ReportMetric(float64(most), "peak-heap-B")
}

// heapObjects returns the bytes of the heap's live objects, and of those not yet collected.
func heapObjects() uint64 {
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(sample)
	return sample[0].Value.Uint64()
}

// sampleHeap samples the heap's live objects every millisecond, keeping the most seen in peak, until the function it
// returns is called.
func sampleHeap(peak *uint64) (stop func()) {
	done, started := make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}} // reused, allocating nothing
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		read := func() {
			metrics.Read(sample)
			*peak = max(*peak, sample[0].Value.Uint64())
		}
		read()
		close(started)
		for {
			select {
			case <-done:
				read() // the last, as the work ended
				return
			case <-tick.C:
				read()
			}
		}
	})
	<-started
	return func() { close(done); wg.Wait() }
}
