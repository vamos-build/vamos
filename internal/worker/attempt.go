package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/vamos-build/vamos/internal/remote"
	"github.com/vamos-build/vamos/internal/run"
	"github.com/vamos-build/vamos/internal/sched"
	"github.com/vamos-build/vamos/internal/tracing"
)

// The pace of an attempt's reporting.
const (
	// eventBatch is how long output is gathered before it is sent, so that a chatty step doesn't make a request
	// per line.
	eventBatch = 200 * time.Millisecond
	// fenceFraction is the part of the lease after which an attempt whose lease couldn't be renewed is killed,
	// so that it has stopped before the lease expires at the coordinator, measured on this machine's monotonic clock.
	fenceFraction = 0.8
	// killWait is how long a killed attempt's processes have to exit before the worker gives up waiting.
	killWait = 30 * time.Second
	// waitDelay bounds how long an attempt's end waits for its stderr's pipe to close once its subprocess has gone, as
	// a process it left may hold it.
	waitDelay = 10 * time.Second
)

// attempt runs the assignment a: a subprocess in a cgroup of its own, whose output and spans are sent as events,
// whose lease is renewed by heartbeat, and whose ending is reported.
// Once ctx ends, the coordinator asks for a kill, or the lease can't be renewed in time, everything in the cgroup is
// killed and the attempt's container removed, and only what the subprocess reported before the kill is reported, as
// execute says.
func (w *Worker) attempt(ctx context.Context, reg remote.Registration, a *remote.Assignment, assigned time.Time) {
	id := a.Attempt
	kill := w.killer(id, killWait)
	outcome, err := w.execute(ctx, reg, a, assigned, func() error { return kill(false) })
	if err != nil {
		w.logf("attempt %s: %v", id, err)
	}
	if err := kill(true); err != nil {
		w.logf("attempt %s: %v", id, err)
	}
	if err := run.RemoveAttempt(context.WithoutCancel(ctx), w.cfg.Store, id); err != nil {
		w.logf("attempt %s: removing its container: %v", id, err)
	}
	if outcome == nil {
		return // killed, or lost: there is nothing to report, and no one to report it to
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reg.Lease)
	defer cancel()
	if err := w.client.Report(rctx, id, *outcome); err != nil && !errors.Is(err, remote.ErrStaleAttempt) {
		w.logf("attempt %s: reporting: %v", id, err)
	}
}

// killer returns what kills the attempt id's processes, waiting for its cgroup to empty for at most wait, and removes
// the cgroup if remove says so, counting it stuck if it won't empty, once for the attempt, whether the kill that found
// it so was the cancellation's or the cleanup's after it.
func (w *Worker) killer(id string, wait time.Duration) func(remove bool) error {
	var stuck atomic.Bool
	return func(remove bool) error {
		// The wait is copied, as the kills can come at once: from the context's cancellation and from the execution.
		d := wait
		if stuck.Load() {
			d = 0 // found stuck already: not waited for again
		}
		err := w.contain.stop(id, d)
		if err == nil && remove {
			err = w.contain.remove(id)
		}
		if errors.Is(err, errStuck) && stuck.CompareAndSwap(false, true) {
			w.metrics.stick()
		}
		return err
	}
}

// execute runs the subprocess of a and returns what it reported, or nil if it was killed or never started because
// its lease couldn't be renewed; assigned is when the assignment arrived, and kill kills its processes, leaving their
// cgroup to be read.
func (w *Worker) execute(ctx context.Context, reg remote.Registration, a *remote.Assignment, assigned time.Time,
	kill func() error) (*remote.AttemptReport, error) {
	cgroup, err := w.contain.create(a.Attempt)
	if err != nil {
		return failed(err), nil
	}
	defer cgroup.Close()
	input, err := json.Marshal(run.AttemptInput{
		Attempt:         a.Attempt,
		Request:         a.Request,
		Push:            a.Push,
		Coordinator:     w.cfg.Coordinator,
		CacheRepository: reg.CacheRepository,
		PullThrough:     reg.PullThrough,
		Trace:           a.Trace,
	})
	if err != nil {
		return failed(err), nil
	}
	// The coordinator started the lease before sending the assignment, which may have been delayed for any part
	// of it: nothing starts until a renewal has been answered, whose send the fence then counts from.
	renewed, clock, err := w.renewFirst(ctx, reg, a.Attempt, assigned)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := w.cfg.Child(cctx)
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stderr = w.cfg.Log
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(cgroup.Fd())}
	cmd.Cancel = kill
	// A process the subprocess left that holds its stderr open, as a step's daemon may, doesn't hold up Wait for
	// longer than this once the subprocess has exited or been killed. Nothing bounds one that holds its stdout open,
	// which is read to its end before Wait, while the lease is renewed.
	cmd.WaitDelay = waitDelay
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return failed(err), nil
	}
	if err := cmd.Start(); err != nil {
		return failed(fmt.Errorf("starting the attempt: %w", err)), nil
	}
	pid := cmd.Process.Pid

	// The lease is renewed until the subprocess ends, and what it used has been read; a kill from the coordinator, or
	// a lease that can't be renewed in time, ends it, stopping its processes until it has exited, as one that closed
	// its output and hangs must still be. The first cause of the attempt's end is recorded as it comes, with whether
	// the subprocess was still running then, as its cgroup holds it while it hasn't exited, and a kill decides the
	// end only if it was, and came before the subprocess's output ended, or the subprocess reported nothing, so that no
	// report a kill stands in for keeps the coordinator from trying the attempt again.
	var e ending
	killed := func(cause error) {
		e.kill(cause, func() bool { return w.contain.holds(a.Attempt, pid) })
		cancel() // which ends the heartbeat and the events, as the coordinator stops counting the attempt
	}
	beat, stopBeat := context.WithCancel(cctx)
	defer stopBeat()
	go w.heartbeat(beat, reg, a.Attempt, renewed, clock, killed)

	// What the subprocess prints is sent in batches, and its last line is its report; its output has ended once it
	// has been read, though its last events may still be sending.
	// Nothing reads its output once a line can't be read, such as one too long, so it would block writing the rest
	// while its lease is renewed: it is stopped as reading stops, before the last events are sent, and the attempt
	// fails, as that decided, whatever kill comes as they are sent.
	// A read's failure stops the subprocess through its cgroup, rather than by ending the attempt's context, so that
	// the lease is still renewed, and what it printed before is still sent, until its failure is reported; a kill ends
	// the context, as it renews nothing more, nor sends what a coordinator that stopped counting the attempt refuses.
	report, readErr := w.relay(cctx, a.Attempt, clock, stdout, func(err error) {
		e.outputEnd(err)
		if err != nil {
			if kerr := kill(); kerr != nil {
				w.logf("attempt %s: %v", a.Attempt, kerr)
			}
		}
	})
	waitErr := cmd.Wait()
	liveAtKill, early, cause := e.exited()
	// The kill stopped the subprocess only if its cgroup held it as the kill came and a signal ended it, rather than
	// its own exit, which may come between the two.
	decided, fenced := killDecides(cause, liveAtKill && signalled(cmd.ProcessState), early, report != nil)
	if fenced {
		w.metrics.fence()
	}
	if decided {
		return nil, cause
	}
	// Whatever the subprocess left running, as a daemon of a step, stops before what it used is read, while the lease
	// is still renewed, however long the stop takes.
	if err := kill(); err != nil {
		w.logf("attempt %s: %v", a.Attempt, err)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	switch {
	case report != nil: // the last line it prints, which stands, whatever it printed after
	case readErr != nil:
		report = failed(fmt.Errorf("reading the attempt's output: %w", readErr))
	case waitErr != nil:
		report = failed(fmt.Errorf("the attempt's process ended without a report: %w", waitErr))
	default:
		report = failed(errors.New("the attempt's process ended without a report"))
	}
	// Read once the subprocess has exited, whether or not it reported, as one that ran out of memory doesn't, from
	// its cgroup, which a kill stops but leaves for the cleanup to remove.
	report.Usage = w.contain.usage(a.Attempt)
	return report, nil
}

// signalled reports whether the process that state describes was ended by SIGKILL, as cgroup.kill ends it, rather than
// by a crash's signal of its own.
func signalled(state *os.ProcessState) bool {
	if state == nil {
		return false
	}
	ws, ok := state.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled() && ws.Signal() == syscall.SIGKILL
}

// ending records the first cause of an attempt's end, as it comes: a kill, from the coordinator or the lease's
// fence, the failure of a read of the subprocess's output, or the subprocess's exit; only a kill is kept, as only a
// kill can decide the end, with whether the subprocess was still running then, and whether its output had ended.
type ending struct {
	mu          sync.Mutex
	settled     bool  // the first cause has come
	why         error // the kill that came first, if one did
	liveAtKill  bool  // the subprocess's cgroup held it as that kill came
	beforeEnd   bool  // that kill came before the output ended
	outputEnded bool
}

// kill records cause, a kill, as the end, if nothing ended the attempt before it; holds reports whether the
// subprocess's cgroup holds it now.
func (e *ending) kill(cause error, holds func() bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.settled {
		e.settled = true
		e.why, e.liveAtKill, e.beforeEnd = cause, holds(), !e.outputEnded
	}
}

// outputEnd records that the output has ended, and, if err says a read of it failed, that failure as the end, if
// nothing ended the attempt before it. Both are recorded at once, so that a kill, as the lease's fence, can't come
// between the two: one that came before the output ended would decide the end, and drop the failure.
func (e *ending) outputEnd(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err != nil {
		e.settled = true
	}
	e.outputEnded = true
}

// exited records that the subprocess has exited, after which a kill decides nothing, and returns whether the
// subprocess's cgroup held it as the kill that came first came, if one did, whether it came before the output ended,
// and that kill.
func (e *ending) exited() (liveAtKill, beforeEnd bool, why error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.settled = true
	return e.liveAtKill, e.beforeEnd, e.why
}

// killDecides returns whether why, a kill, decides the attempt's end, rather than its process, and whether it is a
// fence to count. Only a kill that stopped the subprocess, stopped, decides or counts, and it decides only if it came
// before the subprocess's output ended, beforeEnd, or the subprocess reported nothing, reported, since a report the
// kill made would keep the coordinator from trying the attempt again. A kill that found the subprocess exited, as one
// that came while its last events were sent, leaves its own end, its report or its failure, to stand.
func killDecides(why error, stopped, beforeEnd, reported bool) (decided, fenced bool) {
	if why == nil || !stopped {
		return false, false
	}
	return beforeEnd || !reported, errors.Is(why, errFenced)
}

// errFenced says that an attempt was killed as its lease couldn't be renewed in time.
var errFenced = errors.New("the lease couldn't be renewed")

// failed returns a report of a failure that is the worker's, not the action's.
func failed(err error) *remote.AttemptReport {
	return &remote.AttemptReport{Failure: sched.FailureOf(err)}
}

// renewFirst renews the lease of the attempt id before its subprocess starts, and returns when the renewing request
// was sent, from which the fence counts, and the estimate of the clock's offset its answer gives.
// The coordinator started the first lease on assigning the attempt, which may have reached this worker late, so
// assigned, when it arrived, bounds nothing that runs; only a renewal's send, which the coordinator received later,
// does. A request that gets no answer is sent again every heartbeat interval until a lease has passed since assigned,
// by when the coordinator has lost the attempt; an answer that the attempt is stale, or asks for the kill, ends it.
func (w *Worker) renewFirst(ctx context.Context, reg remote.Registration, id string, assigned time.Time) (
	time.Time, *tracing.Offset, error) {
	for {
		hctx, cancel := context.WithTimeout(ctx, reg.Heartbeat)
		sent := time.Now()
		a, err := w.client.Heartbeat(hctx, id, remote.Heartbeat{})
		received := time.Now()
		cancel()
		switch {
		case err == nil && a.Kill:
			return time.Time{}, nil, errors.New("the coordinator asked for the attempt to be killed before it started")
		case err == nil:
			return sent, estimateOffset(sent, received, a.Clock, nil), nil
		case ctx.Err() != nil:
			return time.Time{}, nil, ctx.Err()
		case !remote.IsTransport(err):
			return time.Time{}, nil, fmt.Errorf("the coordinator no longer counts the attempt: %w", err)
		case time.Since(assigned) >= reg.Lease:
			return time.Time{}, nil, fmt.Errorf("the lease couldn't be renewed before the attempt started: %w", err)
		}
		select {
		case <-time.After(time.Until(sent.Add(reg.Heartbeat))):
		case <-ctx.Done():
			return time.Time{}, nil, ctx.Err()
		}
	}
}

// heartbeat renews the attempt's lease every heartbeat interval until ctx ends, and calls kill, once, when the
// coordinator asks for a kill or counts the attempt stale, or when fenceFraction of the lease has passed since the
// last renewal, whatever a request still in flight might yet answer, so that the attempt has stopped before its lease
// expires at the coordinator. A request that gets no answer within a heartbeat interval is given up; the next
// interval sends another while the fence hasn't passed.
// The coordinator renews on receiving a request, at some point after it was sent, so the fence counts from the send,
// not from the answer, which a slow reply would push past the lease; renewed is the send of the renewal renewFirst
// made, and clock its estimate of the clock's offset.
// Each answer's clock refines the estimate of how far this clock is ahead of the coordinator's, which the next
// heartbeat carries, so that the coordinator shifts the attempt's spans onto its own clock.
func (w *Worker) heartbeat(ctx context.Context, reg remote.Registration, id string, renewed time.Time,
	clock *tracing.Offset, kill func(error)) {
	ticker := time.NewTicker(reg.Heartbeat)
	defer ticker.Stop()
	fence := time.Duration(float64(reg.Lease) * fenceFraction)
	fencer := time.NewTimer(time.Until(renewed.Add(fence))) // on this machine's monotonic clock
	defer fencer.Stop()
	type answer struct {
		sent, received time.Time
		answer         remote.HeartbeatAnswer
		err            error
	}
	var inFlight chan answer
	var lastErr error
	for {
		select {
		case <-ctx.Done():
			return
		case <-fencer.C:
			why := "no answer came"
			if lastErr != nil {
				why = lastErr.Error()
			}
			kill(fmt.Errorf("%w for %s: %s", errFenced, fence.Round(time.Second), why))
			return
		case <-ticker.C:
			if inFlight != nil {
				continue // the last request hasn't answered: the fence decides
			}
			inFlight = make(chan answer, 1)
			go func(ch chan<- answer, clock *tracing.Offset) {
				hctx, cancel := context.WithTimeout(ctx, reg.Heartbeat)
				defer cancel()
				sent := time.Now()
				a, err := w.client.Heartbeat(hctx, id, remote.Heartbeat{Clock: clock})
				ch <- answer{sent: sent, received: time.Now(), answer: a, err: err}
			}(inFlight, clock)
		case ans := <-inFlight:
			inFlight = nil
			switch {
			case ans.err == nil && ans.answer.Kill:
				kill(errors.New("the coordinator asked for the attempt to be killed"))
				return
			case ans.err == nil:
				// A timer that has fired is drained first, so that an answer that came after the fence doesn't
				// leave its expiry behind; a fence already passed fires at once.
				if !fencer.Stop() {
					select {
					case <-fencer.C:
					default:
					}
				}
				fencer.Reset(max(time.Until(ans.sent.Add(fence)), 0))
				clock = estimateOffset(ans.sent, ans.received, ans.answer.Clock, clock)
			case ctx.Err() != nil:
				return
			case !remote.IsTransport(ans.err):
				kill(fmt.Errorf("the coordinator no longer counts the attempt: %w", ans.err))
				return
			default:
				lastErr = ans.err
			}
		}
	}
}

// estimateOffset returns the better of best and the estimate one heartbeat gives of how far this clock is ahead of
// the coordinator's: a request sent at sent, answered with the coordinator's clock reading remote, whose answer came
// at received, puts the coordinator's clock at remote − (sent + received)/2 ahead of this one's, give or take half
// the round trip, and the estimate with the smaller uncertainty is kept.
func estimateOffset(sent, received, remote time.Time, best *tracing.Offset) *tracing.Offset {
	if remote.IsZero() {
		return best
	}
	roundTrip := received.Sub(sent)
	estimate := &tracing.Offset{Offset: -remote.Sub(sent.Add(roundTrip / 2)), Uncertainty: roundTrip / 2}
	if best != nil && best.Uncertainty <= estimate.Uncertainty {
		return best
	}
	return estimate
}

// The bounds of an attempt's events as they cross in JSON, where output is base64.
const (
	// maxBatch is the most one batch of events may hold, encoded, well within what the transport takes.
	maxBatch = 4 << 20
	// maxChunk is the most bytes of output one event carries, so that a long write splits across batches.
	maxChunk = 1 << 20
	// maxPending is the most that waits to be sent, encoded: beyond it the subprocess's output isn't read, so that
	// its writes block while the coordinator is slow, rather than the worker's memory growing.
	maxPending = 4 * maxBatch
)

// relay reads the subprocess's lines from r: output and spans, which it sends to the coordinator in batches of at
// most maxBatch, each carrying clock, the estimate of the clock's offset made before the attempt started, and the
// report, which it returns, once the stream ends. It stops reading while maxPending waits.
func (w *Worker) relay(ctx context.Context, id string, clock *tracing.Offset, r io.Reader, ended func(error)) (
	*remote.AttemptReport, error) {
	q := newEventQueue()
	send := func() {
		for {
			batch, ok := q.take()
			if !ok {
				return
			}
			batch.Clock = clock
			sctx, cancel := context.WithTimeout(ctx, w.lease())
			err := w.client.Events(sctx, id, batch)
			cancel()
			// A batch that isn't taken isn't sent again, but for one of an attempt the coordinator no longer counts,
			// or that a kill or the worker's shutdown stopped, which is no loss of the coordinator's.
			if err != nil && ctx.Err() == nil {
				if !errors.Is(err, remote.ErrStaleAttempt) {
					w.metrics.drop()
				}
				w.logf("attempt %s: sending events: %v", id, err)
			}
		}
	}
	ticker := time.NewTicker(eventBatch)
	defer ticker.Stop()
	stop := make(chan struct{})
	flushed := make(chan struct{})
	go func() {
		defer close(flushed)
		for {
			select {
			case <-ticker.C:
				send()
			case <-stop:
				send()
				return
			}
		}
	}()
	var report *remote.AttemptReport
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), run.MaxLineSize)
	for scanner.Scan() {
		var line run.AttemptLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			w.logf("attempt %s printed a line that isn't one of its own: %q", id, scanner.Bytes())
			continue
		}
		switch {
		case line.Output != nil:
			q.addOutput(line.Output.Stream, line.Output.Data)
		case len(line.Spans) > 0:
			q.addSpans(line.Spans)
		case line.Report != nil:
			report = line.Report
		}
	}
	if ended != nil {
		ended(scanner.Err()) // before the last events are sent, which may wait for a slow coordinator
	}
	close(stop)
	<-flushed
	return report, scanner.Err()
}

// eventQueue holds an attempt's events until they are sent, in the order they came.
type eventQueue struct {
	mu     sync.Mutex
	taken  *sync.Cond // signalled when a batch is taken, making room
	events []queued
	size   int // the encoded size of events
}

// queued is one event waiting, output or a span, and its encoded size.
type queued struct {
	output *remote.OutputEvent
	span   *tracing.Span
	size   int
}

// newEventQueue returns an empty queue.
func newEventQueue() *eventQueue {
	q := &eventQueue{}
	q.taken = sync.NewCond(&q.mu)
	return q
}

// addOutput queues a write of data to stream, in events of at most maxChunk bytes.
func (q *eventQueue) addOutput(stream byte, data []byte) {
	var events []queued
	for len(data) > 0 {
		chunk := data[:min(len(data), maxChunk)]
		data = data[len(chunk):]
		events = append(events, queued{output: &remote.OutputEvent{Stream: stream, Data: chunk},
			size: base64.StdEncoding.EncodedLen(len(chunk)) + 32})
	}
	q.add(events)
}

// addSpans queues spans.
func (q *eventQueue) addSpans(spans []tracing.Span) {
	events := make([]queued, len(spans))
	for i := range spans {
		data, _ := json.Marshal(spans[i]) // decoded from JSON, so it encodes
		events[i] = queued{span: &spans[i], size: len(data) + 1}
	}
	q.add(events)
}

// add queues events once fewer than maxPending bytes wait.
func (q *eventQueue) add(events []queued) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for q.size >= maxPending {
		q.taken.Wait()
	}
	for _, e := range events {
		q.events = append(q.events, e)
		q.size += e.size
	}
}

// take removes the oldest events, up to maxBatch encoded, or one if it alone is larger, and reports whether there
// were any.
func (q *eventQueue) take() (remote.AttemptEvents, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var batch remote.AttemptEvents
	n, size := 0, 0
	for n < len(q.events) && (n == 0 || size+q.events[n].size <= maxBatch) {
		e := q.events[n]
		if e.output != nil {
			batch.Output = append(batch.Output, *e.output)
		} else {
			batch.Spans = append(batch.Spans, *e.span)
		}
		size += e.size
		n++
	}
	clear(q.events[:n]) // so that what was sent can be collected
	q.events = q.events[n:]
	q.size -= size
	q.taken.Broadcast()
	return batch, n > 0
}
