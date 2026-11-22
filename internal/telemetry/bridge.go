package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/vamos-build/vamos/internal/tracing"
)

// Bridge exports the spans that a coordinator's jobs and their attempts record, as OpenTelemetry spans,
// from the same snapshots that it stores.
// It converts them on a goroutine of its own, in the order it was given them, off the build's path,
// and hands them to an SDK that blocks it rather than drop a span, so that each snapshot is handed over whole;
// the SDK sends it in batches, of which one may still fail while another arrives.
// While a collector falls behind, more snapshots wait than the bridge holds, maxQueuedBytes of them,
// and it drops the next whole and every later one of the same job, whose trace is then cut short;
// it reports the drops once for each burst of them.
// Its methods may be called from several goroutines, and on a nil Bridge, which exports nothing.
type Bridge struct {
	tracer   trace.Tracer // nil if no trace is exported
	sampler  sampler
	instance string // the coordinator's, which a restart that keeps its state keeps, and every ID derives from

	produced *lru[string, trace.SpanContext] // the exported span that made each Produces token; the converter's

	mu       sync.Mutex
	attempts *lru[string, attemptRecord] // each queued attempt's span, by the attempt's ID

	queueMu   sync.Mutex
	nonEmpty  *sync.Cond // on queueMu: queue has a snapshot, or the bridge is closed
	queue     []queued   // the snapshots to convert, in order
	waiting   int        // the estimated bytes of queue's snapshots
	maxBytes  int        // the most waiting may be, maxQueuedBytes but in tests
	closed    bool
	abandoned bool          // close ran out of time: what still waits is dropped
	stopped   chan struct{} // closed once the converter has stopped

	drops    atomic.Int64  // snapshots dropped that the reporter hasn't reported
	report   chan struct{} // wakes the reporter
	reported chan struct{} // closed once the reporter has stopped
}

// queued is a snapshot waiting to be converted: the work that converts it and its estimated size.
type queued struct {
	convert func()
	bytes   int
}

// maxQueuedBytes bounds the estimated size of the snapshots that wait for conversion, which the bridge holds,
// so that a collector that falls behind costs a bounded memory: sixteen traces of tracing.MaxTraceSize.
// A snapshot that would pass it is dropped, unless none waits, so that one larger than it is still sent.
const maxQueuedBytes = 16 * tracing.MaxTraceSize

// maxQueuedSnapshots bounds how many snapshots wait, however small they are.
const maxQueuedSnapshots = 1 << 12

// errDropped is what the bridge reports of each burst of snapshots it drops, the same each time, so that the error
// handler reports a flood of them once a minute, with how many bursts it didn't report.
var errDropped = errors.New(
	"dropping a job's spans, and the rest of the job's, since the exporter is falling behind")

// attemptRecord is what the bridge remembers of an attempt, for the spans of jobs that join it to link to.
type attemptRecord struct {
	span trace.SpanContext
	job  string // the ID of the job that started it
}

// maxRemembered bounds how many Produces tokens and attempts a bridge remembers, so that its memory is bounded whatever
// the coordinator runs; a span whose token or attempt was forgotten gets no link.
const maxRemembered = 1 << 16

// newBridge returns a bridge that exports the spans of the coordinator instance through tracer, nil for none, and
// keeps the traces s samples. The caller stops it with close.
func newBridge(tracer trace.Tracer, s sampler, instance string) *Bridge {
	b := &Bridge{
		tracer:   tracer,
		sampler:  s,
		instance: instance,
		produced: newLRU[string, trace.SpanContext](maxRemembered),
		attempts: newLRU[string, attemptRecord](maxRemembered),
		maxBytes: maxQueuedBytes,
		stopped:  make(chan struct{}),
		report:   make(chan struct{}, 1),
		reported: make(chan struct{}),
	}
	b.nonEmpty = sync.NewCond(&b.queueMu)
	if tracer == nil {
		close(b.stopped)
		close(b.reported)
		return b
	}
	go b.convertQueue()
	go b.reportDrops()
	return b
}

// convertQueue converts the queued snapshots in order, until the bridge is closed and its queue empty.
// Within a conversion, the SDK may block it, for as long as an exporter's queue stays full.
func (b *Bridge) convertQueue() {
	defer close(b.stopped)
	for {
		b.queueMu.Lock()
		for len(b.queue) == 0 && !b.closed {
			b.nonEmpty.Wait()
		}
		if len(b.queue) == 0 || b.abandoned {
			b.queueMu.Unlock()
			return
		}
		q := b.queue[0]
		b.queue[0] = queued{} // so that the converted snapshot can be freed
		b.queue = b.queue[1:]
		b.waiting -= q.bytes
		b.queueMu.Unlock()
		q.convert()
	}
}

// reportDrops reports the snapshots that enqueue dropped, apart from the build that gave them, which a log that
// blocks would otherwise hold, until the converter stops: once for those dropped since it last woke, so that a burst
// is one report, whether or not the process's handler limits them.
func (b *Bridge) reportDrops() {
	defer close(b.reported)
	for {
		select {
		case <-b.report:
			if b.drops.Swap(0) > 0 {
				otel.Handle(errDropped) // once for all the drops since it last woke
			}
		case <-b.stopped:
			if b.drops.Swap(0) > 0 { // those that came as it stopped
				otel.Handle(errDropped)
			}
			return
		}
	}
}

// enqueue queues convert, which converts a snapshot of j, the attempt's if attempt isn't "", of an estimated bytes,
// unless the bridge is closed, or the snapshot is dropped: when so many bytes or snapshots wait already that it would
// pass the bridge's bounds, or when its job has lost one before. Drops are reported once for each burst.
func (b *Bridge) enqueue(j *job, attempt string, bytes int, convert func()) {
	b.queueMu.Lock()
	if b.closed {
		b.queueMu.Unlock()
		return
	}
	if j.cut || len(b.queue) > 0 && (b.waiting+bytes > b.maxBytes || len(b.queue) >= maxQueuedSnapshots) {
		j.cut = true
		b.drops.Add(1) // under the lock, so that close, which comes after, finds it counted
		b.queueMu.Unlock()
		select {
		case b.report <- struct{}{}:
		default: // the reporter is woken already
		}
		return
	}
	if attempt != "" {
		// Remembered for joiners to link to only once queued, as one not yet queued may still be dropped,
		// and before a joiner's snapshot, queued after, can be converted. Under queueMu, which mu nests in.
		b.mu.Lock()
		b.attempts.put(attempt, attemptRecord{span: j.spanContext(attemptSpanIDOf(attempt)), job: j.info.ID})
		b.mu.Unlock()
	}
	b.queue = append(b.queue, queued{convert: convert, bytes: bytes})
	b.waiting += bytes
	b.nonEmpty.Signal()
	b.queueMu.Unlock()
}

// close converts what is queued, stops the converter and reports the drops not reported yet, or returns as ctx ends;
// what it is given later, it drops without a word, as the coordinator has stopped.
func (b *Bridge) close(ctx context.Context) error {
	b.queueMu.Lock()
	b.closed = true
	b.nonEmpty.Broadcast()
	b.queueMu.Unlock()
	select {
	case <-b.stopped:
	case <-ctx.Done():
		return fmt.Errorf("converting the spans that wait: %w", ctx.Err())
	}
	select {
	case <-b.reported:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("reporting the spans dropped: %w", ctx.Err())
	}
}

// abandon drops what still waits, after close, reporting it, and makes the converter convert no more, so that the
// processors, which are stopped next, hand over only whole snapshots, but the one it is converting, which an
// exporter's full queue may block, as one whose collector never answers does: that one may be handed over in part,
// and its converter stays blocked, since the SDK gives it no way out, which matters only to a process that goes on
// after it gives up on its telemetry. It returns at once.
func (b *Bridge) abandon() {
	b.queueMu.Lock()
	n := len(b.queue)
	clear(b.queue) // so that the snapshots can be freed, though the converter stays blocked
	b.queue, b.waiting = nil, 0
	b.abandoned = true
	b.drops.Add(int64(n))
	b.queueMu.Unlock()
	if n > 0 {
		select {
		case b.report <- struct{}{}:
		default:
		}
	}
}

// estimatedSize estimates what spans take to hold, so that a bound on it bounds a queue's memory as a count of spans
// can't, since a span may hold an argument of up to tracing.MaxSpanSize: its strings, and a little for the rest.
func estimatedSize(spans []tracing.Span) int {
	n := 0
	for _, s := range spans {
		n += 256 + len(s.ID) + len(s.Parent) + len(s.Process) + len(s.Track) + len(s.Name) + len(s.Produces) +
			len(s.Uses)
		for k, v := range s.Args {
			n += 32 + len(k) + valueSize(v)
		}
	}
	return n
}

// valueSize estimates what an argument's value takes: its strings, and a little for each value.
func valueSize(v any) int {
	switch v := v.(type) {
	case string:
		return len(v)
	case []string:
		n := 0
		for _, e := range v {
			n += 16 + len(e)
		}
		return n
	case []any:
		n := 0
		for _, e := range v {
			n += 16 + valueSize(e)
		}
		return n
	case map[string]any:
		n := 0
		for k, e := range v {
			n += 16 + len(k) + valueSize(e)
		}
		return n
	}
	return 16
}

// JobInfo is what the bridge needs to know of a job as it starts.
type JobInfo struct {
	ID        string
	Kind      string // build, push, publish or export
	Workspace string // the client's workspace's name, or "" for an unnamed one
	// Parent and State are the W3C traceparent and tracestate of the trace the job joins, or "" for none;
	// a Parent that isn't valid is taken as none.
	Parent, State string
	// Run is which run of the job this is: 0 for its first, and one more for each resumption after a restart, each
	// of which has a job span of its own in the job's one trace.
	Run int64
}

// job is the bridge's record of a job, which the job's context carries, and the attempts it starts inherit.
type job struct {
	info    JobInfo
	trace   trace.TraceID
	span    trace.SpanID      // the job's own
	parent  trace.SpanContext // the outer span, if the job joined one
	sampled bool
	cut     bool // it lost a snapshot, and so loses every later one; under the bridge's queueMu
}

type jobKey struct{}

// StartJob returns ctx carrying the job's record, and how it is exported, which tracing.ExportedOf then gives,
// so that the job's spans, and those of the attempts it starts, are exported into its trace.
// The trace's ID is the outer one's if the job joined one, and derived from the instance and the job otherwise,
// and whether it is kept is decided here, once, as a job resumed after a restart decides the same way.
func (b *Bridge) StartJob(ctx context.Context, info JobInfo) context.Context {
	if b == nil {
		return ctx
	}
	j := &job{info: info, span: jobSpanIDOf(b.instance, info.ID, info.Run)}
	if info.Parent != "" {
		j.parent = outerSpanContext(info.Parent, info.State)
	}
	j.trace = traceIDOf(b.instance, info.ID)
	if j.parent.IsValid() {
		j.trace = j.parent.TraceID()
	}
	j.sampled = b.tracer != nil && b.sampler.sampled(j.trace, j.parent)
	ctx = context.WithValue(ctx, jobKey{}, j)
	if b.tracer == nil {
		return ctx
	}
	return tracing.WithExported(ctx, tracing.Exported{TraceID: j.trace.String(), SpanID: j.span.String(),
		Sampled: j.sampled})
}

// Exports reports whether b exports traces, as one that Setup made for metrics alone doesn't.
func (b *Bridge) Exports() bool {
	return b != nil && b.tracer != nil
}

// jobOf returns the record that StartJob put in ctx, or nil.
func jobOf(ctx context.Context) *job {
	j, _ := ctx.Value(jobKey{}).(*job)
	return j
}

// Attempt exports the spans of the attempt id, whose span the target spans of jobs that join it link to once its
// snapshot is queued, which ran from start to end, into the trace of the job of ctx,
// later, on the bridge's goroutine, which reads spans then, so the caller doesn't change them;
// which started it, even if that job has since left it:
// under a span of the attempt's own, itself under the span that the attempt's work is part of, its starter's target.
func (b *Bridge) Attempt(ctx context.Context, id string, spans []tracing.Span, start, end time.Time) {
	j := jobOf(ctx)
	if b == nil || j == nil || b.tracer == nil || !j.sampled {
		return
	}
	parent := j.span
	if call := tracing.Call(ctx); call != nil && call.Parent != "" {
		parent = spanIDOf(b.instance, j.info.ID, call.Parent)
	}
	attempt := attemptSpanIDOf(id)
	b.enqueue(j, id, estimatedSize(spans), func() {
		b.start(j, parent, attempt, "attempt", start, end, nil,
			append(j.common(), attribute.String("vamos.attempt", id)), codes.Unset, "")
		b.export(j, attempt, spans)
	})
}

// Job exports the job's span, which holds the others, with how it ended, outcome, which is an error if it is
// "failed", and then the job's own spans, which it recorded from start to end,
// later, on the bridge's goroutine, which reads spans then, so the caller doesn't change them.
func (b *Bridge) Job(ctx context.Context, spans []tracing.Span, start, end time.Time, outcome string) {
	j := jobOf(ctx)
	if b == nil || j == nil || b.tracer == nil || !j.sampled {
		return
	}
	attrs := append(j.common(),
		attribute.String("vamos.job", j.info.ID),
		attribute.String("vamos.job.kind", j.info.Kind),
		attribute.String("vamos.job.outcome", outcome),
		attribute.Int64("vamos.job.run", j.info.Run))
	status := codes.Unset
	if outcome == "failed" {
		status = codes.Error
	}
	var parent trace.SpanID
	if j.parent.IsValid() {
		parent = j.parent.SpanID()
	}
	b.enqueue(j, "", estimatedSize(spans), func() {
		b.start(j, parent, j.span, j.info.Kind, start, end, nil, attrs, status, "")
		b.export(j, j.span, spans)
	})
}

// export exports spans, one snapshot of a job's or an attempt's, each under the span of the vamos span it is part of,
// or under top, the job's or the attempt's span, if that isn't in the snapshot.
// What the snapshot's own spans produced, a span using it links to first, wherever it lies in the snapshot,
// and the bridge then remembers it for the snapshots exported later.
func (b *Bridge) export(j *job, top trace.SpanID, spans []tracing.Span) {
	// Copies of spans an attempt's export holds, such as its cache write, are neither indexed nor exported, so that
	// nothing points at a span this snapshot doesn't make, unless the attempt wasn't exported, as one whose starter's
	// trace isn't kept isn't, which leaves the copies to every job that keeps its own.
	skip := func(s tracing.Span) bool {
		if s.Args[tracing.Shared] != true {
			return false
		}
		id, _ := s.Args["attempt"].(string)
		b.mu.Lock()
		_, exported := b.attempts.get(id)
		b.mu.Unlock()
		return exported
	}
	// The spans skipped, by place, as two targets' copies of one attempt's cache write share an ID.
	skipped := map[int]bool{}
	ids := make(map[string]trace.SpanID, len(spans)) // the derived ID of each span in the snapshot
	produced := map[string]trace.SpanContext{}
	for i, s := range spans {
		if _, seen := ids[s.ID]; skip(s) || seen && s.Args[tracing.Shared] == true {
			skipped[i] = true
			continue
		}
		ids[s.ID] = spanIDOf(b.instance, j.info.ID, s.ID)
		if s.Produces != "" {
			produced[s.Produces] = j.spanContext(ids[s.ID])
			b.produced.put(s.Produces, produced[s.Produces]) // in the snapshot's order, which the lru's follows
		}
	}
	for i, s := range spans {
		if skipped[i] {
			continue
		}
		parent := top
		if id, ok := ids[s.Parent]; ok && s.Parent != "" {
			parent = id
		}
		end := s.End
		if s.Instant || end.Before(s.Start) {
			end = s.Start
		}
		status, description := codes.Unset, ""
		if v, ok := s.Args["error"]; ok && v != nil { // a string, or anything else as its JSON
			status, description = codes.Error, attributeOf("error", v).Value.String()
		}
		b.start(j, parent, ids[s.ID], s.Name, s.Start, end, b.links(j, s, produced),
			append(j.common(), attributesOf(s)...), status, description)
	}
}

// links returns what s links to: the span that made what it uses, in its own snapshot, produced, or one exported
// earlier, and the span of an attempt that another job started and its target joined.
func (b *Bridge) links(j *job, s tracing.Span, produced map[string]trace.SpanContext) []trace.Link {
	var links []trace.Link
	if s.Uses != "" {
		sc, ok := produced[s.Uses]
		if !ok {
			sc, ok = b.produced.get(s.Uses)
		}
		if ok {
			links = append(links, trace.Link{SpanContext: sc})
		}
	}
	// Only a target's span joins an attempt; others, such as a cache write's, name the attempt they are part of.
	if id, ok := s.Args["attempt"].(string); ok && s.Args[tracing.Role] == tracing.RoleTarget {
		b.mu.Lock()
		a, ok := b.attempts.get(id)
		b.mu.Unlock()
		if ok && a.job != j.info.ID {
			links = append(links, trace.Link{SpanContext: a.span,
				Attributes: []attribute.KeyValue{attribute.String("vamos.link", "joined")}})
		}
	}
	return links
}

// start makes and ends the span with ID id, in j's trace under parent, or under none if parent is zero.
func (b *Bridge) start(j *job, parent, id trace.SpanID, name string, start, end time.Time, links []trace.Link,
	attrs []attribute.KeyValue, status codes.Code, description string) {
	ctx := context.Background()
	if parent.IsValid() {
		ctx = trace.ContextWithSpanContext(ctx, j.spanContext(parent))
	}
	_, span := b.tracer.Start(withIDs(ctx, j.trace, id), cut(name, maxNameSize),
		trace.WithTimestamp(start), trace.WithLinks(links...), trace.WithAttributes(bounded(attrs)...))
	if status != codes.Unset {
		span.SetStatus(status, cut(description, maxValueSize))
	}
	span.End(trace.WithTimestamp(end))
}

// spanContext returns the context of the span id in j's trace, which the SDK takes as a parent or link without a span
// of its own, sampled as j is, whose flag the provider's parent-based sampler follows.
// It is remote only if it is the outer span the job joined; the others are this process's, as OTLP's flags then say.
func (j *job) spanContext(id trace.SpanID) trace.SpanContext {
	var flags trace.TraceFlags
	if j.sampled {
		flags = trace.FlagsSampled
	}
	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    j.trace,
		SpanID:     id,
		TraceFlags: flags,
		TraceState: j.parent.TraceState(),
		Remote:     j.parent.IsValid() && id == j.parent.SpanID(),
	})
}

// common returns the attributes every span of j's carries.
func (j *job) common() []attribute.KeyValue {
	if j.info.Workspace == "" {
		return nil
	}
	return []attribute.KeyValue{attribute.String("vamos.workspace", j.info.Workspace)}
}

// renamed maps the arguments that the attributes table in design.md names to their attributes, in the order a span
// keeps them in, a target's and its miss's cause before its lists of files; any other argument becomes
// vamos.arg.<key>.
var renamed = []struct{ arg, attribute string }{
	{"target", "vamos.target"},
	{"miss", "vamos.miss.cause"},
	{"attempt", "vamos.attempt"},
	{"added", "vamos.miss.added"},
	{"removed", "vamos.miss.removed"},
	{"changed", "vamos.miss.changed"},
}

// attributesOf returns the attributes of s: its process and track, and its arguments but its error, which is its
// status.
// The arguments the attributes table names come first, in renamed's order, so that a span over its budget keeps them,
// and the others in the order of their names, so that a cut keeps the same ones every time.
func attributesOf(s tracing.Span) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, len(s.Args)+2)
	attrs = append(attrs, attribute.String("vamos.process", s.Process), attribute.String("vamos.track", s.Track))
	for _, r := range renamed {
		if v, ok := s.Args[r.arg]; ok {
			attrs = append(attrs, attributeOf(r.attribute, v))
		}
	}
	for _, k := range slices.Sorted(maps.Keys(s.Args)) {
		isRenamed := slices.ContainsFunc(renamed, func(r struct{ arg, attribute string }) bool { return r.arg == k })
		if k == "error" || k == tracing.Shared || isRenamed {
			continue
		}
		attrs = append(attrs, attributeOf("vamos.arg."+strings.ReplaceAll(k, " ", "_"), s.Args[k]))
	}
	return attrs
}

// attributeOf returns v as the attribute key: a number, boolean, string or list of strings as itself,
// whether it was recorded in this process or decoded from JSON, as a worker's relayed spans are,
// and any other value as its JSON.
func attributeOf(key string, v any) attribute.KeyValue {
	switch v := v.(type) {
	case string:
		return attribute.String(key, v)
	case bool:
		return attribute.Bool(key, v)
	case float64: // what JSON decodes every number to, so a whole one is taken as the int it was recorded as
		if v == math.Trunc(v) && math.Abs(v) < 1<<53 {
			return attribute.Int64(key, int64(v))
		}
		return attribute.Float64(key, v)
	case []string:
		return attribute.StringSlice(key, v)
	case []any:
		strs := make([]string, 0, len(v))
		for _, e := range v {
			s, ok := e.(string)
			if !ok {
				break
			}
			strs = append(strs, s)
		}
		if len(strs) == len(v) {
			return attribute.StringSlice(key, strs)
		}
	}
	// Any other number, a time.Duration among them, as what its JSON decodes to, so that it is the same attribute
	// whether it was recorded here or relayed from a worker.
	switch r := reflect.ValueOf(v); r.Kind() {
	case reflect.String: // a named string, such as a kind of target, as JSON writes it
		return attribute.String(key, r.String())
	case reflect.Bool:
		return attribute.Bool(key, r.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return attribute.Int64(key, r.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if r.Uint() <= math.MaxInt64 {
			return attribute.Int64(key, int64(r.Uint()))
		}
		return attribute.Float64(key, float64(r.Uint()))
	case reflect.Float32: // as its shortest decimal, as JSON writes it, rather than its exact binary value
		f, _ := strconv.ParseFloat(strconv.FormatFloat(r.Float(), 'g', -1, 32), 64)
		return attributeOf(key, f)
	case reflect.Float64:
		return attributeOf(key, r.Float())
	case reflect.Slice:
		if r.Type().Elem().Kind() == reflect.String { // a named []string, or a slice of named strings
			strs := make([]string, r.Len())
			for i := range strs {
				strs[i] = r.Index(i).String()
			}
			return attribute.StringSlice(key, strs)
		}
	}
	data, err := json.Marshal(v)
	if err != nil {
		return attribute.String(key, "unencodable")
	}
	return attribute.String(key, string(data))
}

// The bridge's budget of what an exported span holds, which the exporters' queues hold until they send it:
// its name, its status's description and its attributes, each key, each string and each list,
// copied when cut, so that what is kept doesn't keep the rest alive,
// and the SDK's own limits, which count runes rather than bytes and keep what they cut alive, needn't bound it.
const (
	maxNameSize   = 1 << 10 // of the name, which may hold all of a step's command, and the status's description
	maxKeySize    = 1 << 7
	maxValueSize  = 1 << 10 // of each string, alone or in a list
	maxListLength = 1 << 6
	// maxAttributes is fewer than the SDK's 128 attributes a span, so that vamos.truncated, after them, is kept.
	maxAttributes = 120
	// maxAttributesSize bounds a span's attributes in all, their keys and values, past which it keeps no more
	// but vamos.truncated; with the name and status, a span holds some 20 KiB, a full queue some 40 MiB.
	maxAttributesSize = 16 << 10
)

// cut returns s as valid UTF-8, since OTLP's protobuf encoding refuses a request with any string that isn't, as a
// file's name may not be, and if that is longer than n bytes, a copy of as many whole runes of it as fit with "…".
func cut(s string, n int) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "\uFFFD")
	}
	if len(s) <= n {
		return s
	}
	i := max(n-len("…"), 0)
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return strings.Clone(s[:i]) + "…"
}

// bounded returns attrs within the bridge's budget, in their order: each key and string cut, each list cut short,
// to what is left of maxAttributesSize if it would pass it, at most maxAttributes, and none once the budget is spent,
// with vamos.truncated after them if the budget shortened a string or list, or dropped an attribute, so that a backend
// shows that some are missing; a string cut only to maxValueSize, as any span's may be, isn't marked.
func bounded(attrs []attribute.KeyValue) []attribute.KeyValue {
	kept := make([]attribute.KeyValue, 0, min(len(attrs), maxAttributes+1))
	size := 0
	truncated := false
	for _, kv := range attrs {
		key := attribute.Key(cut(string(kv.Key), maxKeySize))
		left := maxAttributesSize - size - len(key)
		if len(kept) == maxAttributes || left <= len("…") {
			truncated = true
			break
		}
		v := kv.Value
		switch v.Type() {
		case attribute.STRING:
			s := cut(v.AsString(), maxValueSize)
			if len(s) > left {
				s, truncated = cut(s, left), true
			}
			v = attribute.StringValue(s)
		case attribute.STRINGSLICE:
			all := v.AsStringSlice()
			var list []string
			for _, s := range all {
				s = cut(s, maxValueSize)
				if len(list) == maxListLength || 16+len(s) > left {
					break
				}
				left -= 16 + len(s)
				list = append(list, s)
			}
			if len(list) < len(all) {
				truncated = true
				if len(list) == 0 { // as none of it fit
					continue
				}
			}
			v = attribute.StringSliceValue(list)
		}
		if n := len(key) + valueBytes(v); size+n > maxAttributesSize { // as a number may, with a few bytes left
			truncated = true
			break
		}
		size += len(key) + valueBytes(v)
		kept = append(kept, attribute.KeyValue{Key: key, Value: v})
	}
	if truncated {
		kept = append(kept, attribute.Bool("vamos.truncated", true))
	}
	return kept
}

// valueBytes returns what v takes, roughly.
func valueBytes(v attribute.Value) int {
	switch v.Type() {
	case attribute.STRING:
		return len(v.AsString())
	case attribute.STRINGSLICE:
		n := 0
		for _, s := range v.AsStringSlice() {
			n += 16 + len(s)
		}
		return n
	}
	return 8
}
