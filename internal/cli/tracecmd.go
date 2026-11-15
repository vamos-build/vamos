package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"strings"

	"github.com/vamos-build/vamos/internal/api"
	"github.com/vamos-build/vamos/internal/cache"
	"github.com/vamos-build/vamos/internal/finish"
	"github.com/vamos-build/vamos/internal/tracing"

	"github.com/opencontainers/go-digest"
	"github.com/spf13/cobra"
)

// traceCommand returns `vamos trace <digest|job> <file.json>`, which writes a stored trace, run through handle.
func traceCommand(
	handle func(func(inv *invocation, args []string) error) func(*cobra.Command, []string) error,
) *cobra.Command {
	return &cobra.Command{
		Use:   "trace <digest|job> <file.json>",
		Short: "Write a stored trace, which Perfetto (ui.perfetto.dev) opens, to a file",
		Long: "Write the trace that a digest names, as a command printed it, or a job's, by its ID, from the\n" +
			"coordinator that ran it, to file.json, which Perfetto opens: a command's trace as --trace would\n" +
			"have written it, a job's, or an attempt's. A digest reads its trace until the build cache is pruned;\n" +
			"a job ID, with --coordinator, for as long as the coordinator remembers the job.",
		Args: traceArgs,
		RunE: handle(func(inv *invocation, args []string) error { return inv.writeStoredTrace(args[0], args[1]) }),
	}
}

// traceArgs requires a digest or job and a file.
func traceArgs(cmd *cobra.Command, args []string) error {
	switch {
	case len(args) == 0:
		return errors.New("missing the trace's digest or job, and the file to write, e.g. vamos trace " +
			"sha256:4f2a... trace.json")
	case len(args) == 1:
		return fmt.Errorf("missing the file to write after %q, e.g. vamos trace %s trace.json", args[0], args[0])
	case len(args) > 2:
		return fmt.Errorf("expected a digest or job and a file, got %d arguments: %s", len(args),
			strings.Join(args, " "))
	}
	return nil
}

// writeStoredTrace writes the stored trace that name, a digest or a job's ID, names to path, as --trace writes one:
// a command's, a job's or an attempt's. An argument holding a ':' is a digest, since a job ID is hex alone, read
// from the executor's content store; anything else is a job ID, which only a coordinator remembers.
func (inv *invocation) writeStoredTrace(name, path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if err := finish.CheckSaveDest(abs); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	var d digest.Digest
	if strings.Contains(name, ":") {
		if d, err = digest.Parse(name); err != nil {
			return fmt.Errorf("%q is not a valid digest: %w", name, err)
		}
	} else if inv.opts.coordinator == "" {
		return fmt.Errorf("%q is a job, which is read from the coordinator that holds it: give --coordinator, or "+
			"the trace's digest, which the command printed", name)
	}
	ex, _, err := inv.executor()
	if err != nil {
		return err
	}
	defer inv.closeExecutor(ex)
	ctx := inv.ctx
	content, err := ex.ContentStore()
	if err != nil {
		return err
	}
	var b cache.Blob
	if d == "" {
		if b, err = jobTrace(ctx, ex, api.JobID(name)); err != nil {
			return err
		}
	} else if b, err = content.Stat(ctx, d); errors.Is(err, cache.ErrMissing) {
		return fmt.Errorf("%s: the executor no longer holds this trace; a prune of the build cache removes traces", d)
	} else if err != nil {
		return fmt.Errorf("%s: %w", d, err)
	}
	if b.Size > tracing.MaxTraceSize {
		return fmt.Errorf("%s is not a trace this vamos can read: it is larger than any stored trace", b.Digest)
	}
	data, err := content.Read(ctx, b, tracing.MaxTraceSize)
	if errors.Is(err, cache.ErrMissing) {
		return fmt.Errorf("%s: the executor no longer holds this trace; a prune of the build cache removes traces",
			b.Digest)
	} else if err != nil {
		return fmt.Errorf("%s: %w", b.Digest, err)
	}
	spans, err := storedSpans(ctx, content, data)
	if err != nil {
		return fmt.Errorf("%s is not a trace this vamos can read: %w", b.Digest, err)
	}
	return finish.WriteFile(abs, func(w io.Writer) error {
		return tracing.WriteChrome(w, spans)
	})
}

// jobTrace returns the blob of the manifest of the job id, which ex, a coordinator, remembers.
func jobTrace(ctx context.Context, ex api.Executor, id api.JobID) (cache.Blob, error) {
	b, err := ex.Trace(ctx, id)
	switch {
	case errors.Is(err, api.ErrRunning):
		return cache.Blob{}, fmt.Errorf("the job %s is still running: its trace is stored once it ends", id)
	case errors.Is(err, api.ErrNoTrace):
		return cache.Blob{}, fmt.Errorf("the job %s stored no trace", id)
	case errors.Is(err, api.ErrExpired), errors.Is(err, api.ErrNoJob):
		return cache.Blob{}, fmt.Errorf("the coordinator doesn't know the job %s any more; the trace digest that its "+
			"command printed still reads its trace", id)
	case err != nil:
		return cache.Blob{}, err
	}
	return b, nil
}

// storedSpans returns the spans of data, a stored trace of a kind it names, as a trace file shows them: a command's
// assembled with its jobs' and attempts', a job's with its attempts', and an attempt's alone.
func storedSpans(ctx context.Context, content cache.ContentStore, data []byte) ([]tracing.Span, error) {
	h, err := tracing.ReadHeader(data)
	if err != nil {
		return nil, err
	}
	switch h.Kind {
	case api.KindCommand:
		doc, err := api.DecodeCommandTrace(data)
		if err != nil {
			return nil, err
		}
		return assemble(ctx, content, doc, tracing.Lost{}), nil
	case api.KindJob:
		if _, err := api.DecodeJobTrace(data); err != nil { // so that a malformed one fails, as it would as a whole
			return nil, err
		}
		b := cache.Blob{Digest: digest.FromBytes(data), Size: int64(len(data))}
		return assemble(ctx, content, api.CommandTrace{Jobs: []api.CommandJob{{Manifest: &b}}}, tracing.Lost{}), nil
	case tracing.KindAttempt:
		a, err := tracing.DecodeAttempt(data)
		if err != nil {
			return nil, err
		}
		return withoutDanglingArrows(tracing.Trim(a.Spans, tracing.Lost{}, math.MaxInt)), nil
	}
	return nil, fmt.Errorf("%w: its kind %q isn't one this vamos knows", tracing.ErrNotTrace, h.Kind)
}
