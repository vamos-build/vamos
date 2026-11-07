package cli

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/testenv"
	"github.com/Topher-the-Geek/vamos/internal/worker"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/spf13/afero"
	"go.podman.io/storage"
)

// The constants of testenv, as this package's tests name them.
const (
	pullBusybox = testenv.PullBusybox
	projectDir  = testenv.ProjectDir
)

// The helpers of testenv, as this package's tests name them.
var (
	project        = testenv.Project
	requireBuildah = testenv.RequireBuildah
	must           = testenv.Must
	newNonce       = testenv.NewNonce
)

func TestMain(m *testing.M) {
	testenv.Main(m, worker.ChildFromEnv)
}

// privateStores is the stores of the test that asked for its own, while it runs; see usePrivateStores.
var privateStores *testenv.Stores

// stores returns the stores that vamos runs against: the test's own, if it asked for them, or else the suite's.
func stores(t *testing.T) *testenv.Stores {
	t.Helper()
	if privateStores != nil {
		return privateStores
	}
	return testenv.SuiteStores(t)
}

// usePrivateStores gives the test a container store and build cache of its own, for the rest of the test,
// for a test that destroys what they hold, as prune does.
// Tests run one at a time, so the stores are in a variable; a parallel test can't use it.
func usePrivateStores(t *testing.T) {
	t.Helper()
	t.Setenv("VAMOS_TEST_PRIVATE_STORES", "1") // which fails a parallel test, since the variable is shared
	previous := privateStores
	privateStores = testenv.PrivateStores(t)
	t.Cleanup(func() { privateStores = previous })
}

// insecureRegistry serves a registry over plain HTTP, through handler, which defaults to an empty registry,
// and marks it insecure in a registries.conf of the test's own, so that the executor,
// which takes its registry settings from its environment, reaches it; the client needs --tls-verify=false.
// It returns the server and the registry's host.
func insecureRegistry(t *testing.T, handler http.Handler) (*httptest.Server, string) {
	t.Helper()
	if handler == nil {
		handler = registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	testenv.InsecureRegistry(t, host)
	return server, host
}

// withStore calls use with the test's container store, open only until use returns.
func withStore(t *testing.T, use func(storage.Store)) {
	t.Helper()
	must(t, stores(t).WithStore(func(store storage.Store) error {
		use(store)
		return nil
	}))
}

// result is the outcome of one vamos invocation.
type result struct {
	stdout, stderr string
	err            error
}

// vamos runs the command line with args against fsys, as main does.
func vamos(t *testing.T, fsys afero.Fs, args ...string) result {
	t.Helper()
	return vamosWithOutput(t, fsys, t.Context(), nil, args...)
}

// vamosWithOutput is like vamos, but runs with ctx, as main does to handle Ctrl-C,
// and also copies stdout to watch, if it isn't nil.
func vamosWithOutput(t *testing.T, fsys afero.Fs, ctx context.Context, watch io.Writer, args ...string) result {
	t.Helper()
	cmd := NewRootCommand(Config{Fs: fsys, Store: &stores(t).Options})
	var stdout, stderr bytes.Buffer
	if watch != nil {
		cmd.SetOut(io.MultiWriter(&stdout, watch))
	} else {
		cmd.SetOut(&stdout)
	}
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(ctx)
	return result{stdout: stdout.String(), stderr: stderr.String(), err: err}
}

// succeeds fails the test unless r succeeded.
func (r result) succeeds(t *testing.T) result {
	t.Helper()
	if r.err != nil {
		t.Fatalf("vamos failed: %v\nstdout:\n%s\nstderr:\n%s", r.err, r.stdout, r.stderr)
	}
	if !strings.Contains(r.stdout, " built! ") {
		t.Errorf("stdout = %q, want it to report the image built", r.stdout)
	}
	return r
}

// ran fails the test unless r, a command that sends no image, such as build or prune, succeeded.
func (r result) ran(t *testing.T) result {
	t.Helper()
	if r.err != nil {
		t.Fatalf("vamos failed: %v\nstdout:\n%s\nstderr:\n%s", r.err, r.stdout, r.stderr)
	}
	return r
}
