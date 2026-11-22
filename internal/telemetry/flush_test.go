package telemetry

import "context"

// flush waits until the snapshots queued before it are converted, or ctx ends, for tests to read what was exported.
// What it queues to know that is never dropped, and it waits for nothing on a bridge that converts nothing.
func (b *Bridge) flush(ctx context.Context) error {
	done := make(chan struct{})
	b.queueMu.Lock()
	if b.closed || b.tracer == nil {
		b.queueMu.Unlock()
		return nil
	}
	b.queue = append(b.queue, queued{convert: func() { close(done) }})
	b.nonEmpty.Signal()
	b.queueMu.Unlock()
	select {
	case <-done:
		return nil
	case <-b.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
