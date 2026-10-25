package testenv

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/afero"
)

// ProjectDir is where in-memory test projects live.
const ProjectDir = "/project"

// PullBusybox declares the base image for tests, as the target "busybox", in one line.
// Tests that build pull it once into their suite's store, and later builds find it there.
const PullBusybox = `pull(name="busybox", image="docker.io/library/busybox", tag="1.36")` + "\n"

// Project returns an in-memory filesystem holding files, keyed by their path within ProjectDir.
func Project(t *testing.T, files map[string]string) afero.Fs {
	t.Helper()
	fsys, err := MemProject(files)
	if err != nil {
		t.Fatal(err)
	}
	return fsys
}

// MemProject is Project for a runner without a *testing.T, such as godog's steps.
func MemProject(files map[string]string) (afero.Fs, error) {
	fsys := afero.NewMemMapFs()
	if err := fsys.MkdirAll(ProjectDir, 0o755); err != nil {
		return nil, err
	}
	for name, content := range files {
		path := filepath.Join(ProjectDir, name)
		// MemMapFs would make missing folders with no permissions.
		if err := fsys.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		if err := afero.WriteFile(fsys, path, []byte(content), 0o644); err != nil {
			return nil, err
		}
	}
	return fsys, nil
}

// DiskProject is like Project, but keeps the files on disk, under root/project,
// for tests that need symbolic links or that write into the project.
// Links maps a link's path within ProjectDir to its target.
func DiskProject(t *testing.T, files, links map[string]string) (fsys afero.Fs, root string) {
	t.Helper()
	root = t.TempDir()
	fsys = afero.NewBasePathFs(afero.NewOsFs(), root)
	for name, content := range files {
		path := filepath.Join(ProjectDir, name)
		if err := fsys.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := afero.WriteFile(fsys, path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, target := range links {
		path := filepath.Join(root, ProjectDir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
	}
	return fsys, root
}

// Must fails the test at once if err isn't nil.
func Must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// EditedFs is a filesystem in which the file File is changed to hold Edit when it is opened for the At'th time,
// or the second if At is zero, as if it were edited while a build reads it.
type EditedFs struct {
	afero.Fs
	File, Edit string
	At         int
	opened     int
}

// Open opens the file name, first editing it if it is File, opened At-1 times before.
func (f *EditedFs) Open(name string) (afero.File, error) {
	if name == f.File {
		f.opened++
		if f.opened == max(f.At, 2) {
			if err := afero.WriteFile(f.Fs, name, []byte(f.Edit), 0o644); err != nil {
				return nil, err
			}
		}
	}
	return f.Fs.Open(name)
}
