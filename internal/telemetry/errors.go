package telemetry

import (
	"crypto/sha256"
	"fmt"
	"sync"
	"time"
)

// errorInterval is how often the error handler reports each distinct error at most.
const errorInterval = time.Minute

// errorHandler reports the errors OpenTelemetry's exporters meet, which happen apart from any build and so are
// reported in the process's log alone: each distinct one at most once an errorInterval, with how many like it it
// didn't report since, so that a collector that is down doesn't flood the log.
type errorHandler struct {
	warn func(string)
	now  func() time.Time

	mu   sync.Mutex
	seen map[[sha256.Size]byte]*errorSeen // by a digest of the error's text as reported
}

// errorSeen is when the handler last reported an error, and how many like it it has suppressed since.
type errorSeen struct {
	reported   time.Time
	suppressed int
}

// maxErrorsSeen bounds how many distinct errors the handler remembers; past that, it forgets them all and starts over,
// so that errors that each name something new can't grow its memory without end.
const maxErrorsSeen = 1 << 10

// maxErrorSize bounds the text of an error the handler reports.
const maxErrorSize = 4 << 10

// newErrorHandler returns a handler that reports through warn, with the time now gives.
func newErrorHandler(warn func(string), now func() time.Time) *errorHandler {
	return &errorHandler{warn: warn, now: now, seen: map[[sha256.Size]byte]*errorSeen{}}
}

// Handle reports err, unless one like it was reported within the last errorInterval.
func (h *errorHandler) Handle(err error) {
	if err == nil {
		return
	}
	msg := err.Error()
	if len(msg) > maxErrorSize { // as a collector's or proxy's response, which an exporter's error may hold, can be
		msg = cut(msg, maxErrorSize)
	}
	// By what is reported, so that errors that differ only past the cut, as by a request's ID, count as one.
	key := sha256.Sum256([]byte(msg))
	now := h.now()
	h.mu.Lock()
	seen := h.seen[key]
	if seen != nil && now.Sub(seen.reported) < errorInterval {
		seen.suppressed++
		h.mu.Unlock()
		return
	}
	suppressed := 0
	if seen != nil {
		suppressed = seen.suppressed
	}
	if len(h.seen) >= maxErrorsSeen && seen == nil {
		clear(h.seen)
	}
	h.seen[key] = &errorSeen{reported: now}
	h.mu.Unlock()
	if suppressed > 0 {
		msg = fmt.Sprintf("%s (and %d more like it since it was last reported)", msg, suppressed)
	}
	h.warn("exporting telemetry: " + msg)
}
