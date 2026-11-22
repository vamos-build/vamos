package run

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/vamos-build/vamos/internal/cache"
	"github.com/vamos-build/vamos/internal/fileset"
	"github.com/vamos-build/vamos/internal/graph"
	"github.com/vamos-build/vamos/internal/sched"
	"github.com/vamos-build/vamos/internal/testenv"

	"github.com/spf13/afero"
	"go.podman.io/buildah"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

// memFile returns a filesystem holding the file /f with content.
func memFile(content string) afero.Fs {
	fsys := afero.NewMemMapFs()
	if err := afero.WriteFile(fsys, "/f", []byte(content), 0o644); err != nil {
		panic(err) // a filesystem in memory can't refuse it
	}
	return fsys
}

// brokenContent is a content store that can't be reached: nothing in it can be checked or read.
type brokenContent struct {
	cache.ContentStore
	err error
}

// Has fails.
func (b *brokenContent) Has(context.Context, cache.Blob) error {
	return b.err
}

// Get fails.
func (b *brokenContent) Get(context.Context, cache.Blob) (io.ReadCloser, error) {
	return nil, b.err
}

// Read fails.
func (b *brokenContent) Read(context.Context, cache.Blob, int64) ([]byte, error) {
	return nil, b.err
}

// TestRunnerCheckContent checks that the local runner's check tells a task's files that are corrupt,
// which make its result unusable, from a content store that can't be reached, which is an error.
func TestRunnerCheckContent(t *testing.T) {
	m := cache.NewMemory()
	a := &sched.Action{Version: sched.ActionVersion, Kind: sched.KindTask, Output: fileset.DefaultSelection()}
	file, _, err := cache.PutFile(t.Context(), m, memFile("data"), "/f", 100)
	testenv.Must(t, err)
	tree, err := m.PutBytes(t.Context(), (&cache.Tree{Entries: []cache.TreeEntry{
		{Path: "f", Type: cache.EntryFile, Mode: 0o644, Blob: &file},
	}}).Encode())
	testenv.Must(t, err)
	result := &cache.ActionResult{Output: &tree}

	r := &localRunner{content: m}
	if problem, err := r.Check(t.Context(), a, result); problem != nil || err != nil {
		t.Errorf("check = %v, %v; want the result usable", problem, err)
	}
	fire := errors.New("disk on fire")
	r.content = &brokenContent{ContentStore: m, err: fire}
	if problem, err := r.Check(t.Context(), a, result); problem != nil || !errors.Is(err, fire) {
		t.Errorf("check with the store unreachable = %v, %v; want only the store's error", problem, err)
	}
	m.Replace(file, nil)
	r.content = m
	if problem, err := r.Check(t.Context(), a, result); err != nil || problem == nil ||
		!strings.HasPrefix(problem.Error(), "its files can't be used from the cache: f: ") {
		t.Errorf("check with a file corrupt = %v, %v; want the result unusable", problem, err)
	}
}

// TestGiveSecrets checks that a run() step's secrets become buildah's secret mounts, each value in a file only this
// process can read, and that a secret without a value, as on a worker, which is never sent one, fails the step.
func TestGiveSecrets(t *testing.T) {
	st := &targetState{secrets: map[string][]byte{"token": []byte("hunter2")}}
	var options buildah.RunOptions
	dir, err := st.giveSecrets([]graph.Secret{{ID: "token", Env: "TOKEN"}, {ID: "token", File: "/run/t"}}, &options)
	defer os.RemoveAll(dir)
	testenv.Must(t, err)
	if want := []string{"type=secret,id=token,env=TOKEN", "type=secret,id=token,target=/run/t"}; !slices.Equal(
		options.RunMounts, want) {
		t.Errorf("mounts = %q, want %q", options.RunMounts, want)
	}
	source := options.Secrets["token"].Source
	if info, err := os.Stat(source); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the value's file %s: %v, %v; want it readable by its owner alone", source, info, err)
	}

	st.secrets = nil
	dir, err = st.giveSecrets([]graph.Secret{{ID: "token", Env: "TOKEN"}}, &buildah.RunOptions{})
	defer os.RemoveAll(dir)
	if err == nil || !strings.Contains(err.Error(), `secret "token" isn't given here`) {
		t.Errorf("a secret without a value = %v, want it refused", err)
	}
}

// TestBuilderOptionsNoHostMounts checks that a target's builder takes its mounts from an empty file,
// rather than the host's mounts.conf, whose mounts would make a target depend on the host.
func TestBuilderOptionsNoHostMounts(t *testing.T) {
	if got := builderOptions("scratch", nil, nil).DefaultMountsFilePath; got != os.DevNull {
		t.Errorf("the builder's mounts file = %q, want %s", got, os.DevNull)
	}
}

// TestStartCancelled checks that a target cancelled before buildah makes its container makes none, and that one
// cancelled just after buildah made it, as an interrupt that comes then is, returns the container, unmounted, so that
// the caller deletes it, rather than leaving it in the store, as buildah's NewBuilder does with a context that has
// ended by then, or mounting it, whose failure would hide the cancellation.
func TestStartCancelled(t *testing.T) {
	testenv.RequireBuildah(t)
	for _, tt := range []struct {
		name    string
		before  bool // cancelled before start, rather than once the container is made
		created int
	}{
		{name: "before the container is made", before: true, created: 0},
		{name: "as the container is made", created: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store, err := storage.GetStore(testenv.PrivateStores(t).Options)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = store.Shutdown(true) })
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tt.before {
				cancel()
			}
			counting := &cancelOnCreate{Store: store, cancel: cancel}
			r := &localRunner{store: counting, systemContext: &types.SystemContext{}}
			req := sched.Request{Action: &sched.Action{Kind: sched.KindTask, Base: sched.Scratch}}
			st, err := r.start(ctx, req, nil, nil)
			if st != nil {
				if err := st.delete(); err != nil {
					t.Errorf("deleting the target's container: %v", err)
				}
			}
			if !errors.Is(err, context.Canceled) {
				t.Errorf("start returned %v, want it cancelled", err)
			}
			if counting.created != tt.created {
				t.Errorf("start made %d containers, want %d", counting.created, tt.created)
			}
			if counting.mounted != 0 {
				t.Errorf("start mounted a container %d times, want none", counting.mounted)
			}
			containers, err := store.Containers()
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range containers {
				t.Errorf("the store holds the container %s, which start left", c.ID)
			}
		})
	}
}

// cancelOnCreate is a container store that cancels a build once it has made a container, counting those it made,
// and the mounts of containers.
type cancelOnCreate struct {
	storage.Store
	cancel           context.CancelFunc
	created, mounted int
}

func (s *cancelOnCreate) Mount(id, mountLabel string) (string, error) {
	s.mounted++
	return s.Store.Mount(id, mountLabel)
}

func (s *cancelOnCreate) CreateContainer(
	id string, names []string, image, layer, metadata string, options *storage.ContainerOptions,
) (*storage.Container, error) {
	c, err := s.Store.CreateContainer(id, names, image, layer, metadata, options)
	if err == nil {
		s.created++
	}
	s.cancel()
	return c, err
}
