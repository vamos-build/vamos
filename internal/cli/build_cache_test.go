package cli

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/spf13/afero"
	"github.com/vamos-build/vamos/internal/run"
	"github.com/vamos-build/vamos/internal/testenv"
	"go.podman.io/storage"
)

// cachedBuild is a BUILD file with a layer and a task whose results are cached, and a final layer that uses both.
// Its targets print a value unique to the test, so that results cached by other tests or earlier runs are never reused.
func cachedBuild(nonce string) string {
	return pullBusybox + fmt.Sprintf(`
glob(name="input", include=["input.txt"])
layer(
    name="deps",
    base="busybox",
    steps=[
        add("input", "/deps"),
        build_env(LANG="C", TZ="UTC"),
        env(GREETING="hello", NAME="vamos"),
        run(["sh", "-c", "echo deps %[1]s; echo deps warning %[1]s >&2; echo %[1]s > /deps/nonce.txt"]),
    ],
)
task(
    name="size",
    base="busybox",
    steps=[
        add("input", "/work"),
        run(["sh", "-c", "echo size %[1]s; wc -c < /work/input.txt > /work/size.txt; echo %[1]s > /work/nonce.txt"]),
    ],
    source="/work",
    include=["size.txt", "nonce.txt"],
)
layer(name="app", base="deps", steps=[add("size", "/app")])
`, nonce)
}

// recordsNaming returns the paths of the test's action results that have the field, such as "epoch".
func recordsNaming(t *testing.T, field string) []string {
	t.Helper()
	dir := run.CacheDir(stores(t).Options)
	entries, err := os.ReadDir(filepath.Join(dir, "ac"))
	must(t, err)
	var paths []string
	for _, e := range entries {
		path := filepath.Join(dir, "ac", e.Name())
		data, err := os.ReadFile(path)
		must(t, err)
		var record map[string]json.RawMessage
		must(t, json.Unmarshal(data, &record))
		if _, ok := record[field]; ok {
			paths = append(paths, path)
		}
	}
	return paths
}

// assertTargets fails the test unless r reports each target in want, such as `layer "app"`, as "building" or "cached".
func assertTargets(t *testing.T, r result, want map[string]string) {
	t.Helper()
	for target, how := range want {
		if line := target + ": " + how + "\n"; !strings.Contains(r.stdout, line) {
			t.Errorf("stdout = %q, want %q", r.stdout, line)
		}
	}
}

// builtID returns the image ID that r reports after "Image built!".
func builtID(t *testing.T, r result) string {
	t.Helper()
	_, line, ok := strings.Cut(r.stdout, "Image built! ")
	if !ok {
		t.Fatalf("stdout = %q, want it to report the image built", r.stdout)
	}
	fields := strings.Fields(line)
	return fields[len(fields)-1]
}

// TestExplicitStoreRoots checks that a container store given in part is refused, rather than opened,
// since containers/storage would fill in the developer's roots, and the build cache would be put in the working folder.
func TestExplicitStoreRoots(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options storage.StoreOptions
	}{
		{
			name:    "no graph root",
			options: storage.StoreOptions{RunRoot: "/run/vamos-test"},
		},
		{
			name:    "no run root",
			options: storage.StoreOptions{GraphRoot: "/var/lib/vamos-test/storage"},
		},
		{
			name:    "relative graph root",
			options: storage.StoreOptions{GraphRoot: "storage", RunRoot: "/run/vamos-test"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := NewRootCommand(Config{Fs: afero.NewMemMapFs(), Store: &tc.options})
			cmd.SetArgs([]string{"prune"})
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			err := cmd.Execute()
			want := fmt.Sprintf("the container store's graph root %q and run root %q must both be absolute paths",
				tc.options.GraphRoot, tc.options.RunRoot)
			if err == nil || err.Error() != want {
				t.Errorf("prune: %v, want %q", err, want)
			}
		})
	}
}

// diffedBuild is a BUILD file whose layers change files in every way that a layer's diff records: a file's content
// and mode, a file and a directory removed, a directory replaced, files added, a symbolic link and a hard link.
// The nonce goes into the first layer, so that its results are never reused from another test's.
func diffedBuild(nonce string) string {
	return pullBusybox + fmt.Sprintf(`
glob(name="input", include=["input.txt"])
layer(
    name="base",
    base="busybox",
    steps=[
        run(["sh", "-c", "echo %s > /nonce; mkdir -p /d/sub /e /g"]),
        run(["sh", "-c", "echo a > /d/a; echo b > /d/sub/b; echo c > /c; echo e > /e/e; echo g > /g/g"]),
    ],
)
layer(
    name="app",
    base="base",
    steps=[
        add("input", "/d"),
        run(["sh", "-c", "echo A > /d/a; chmod 600 /d/sub/b; rm /c; rm -r /g"]),
        run(["sh", "-c", "rm -r /e; mkdir /e; echo f > /e/f; ln -s /d/a /l; ln /d/a /h"]),
    ],
)
`, nonce)
}

// naiveDiffHelperEnv marks the test binary run by TestNaiveAndNativeDiffsMakeTheSameImage, and holds the nonce
// that TestNaiveDiffHelper builds diffedBuild with; naiveDiffOutEnv names the file that it writes what it saved to.
const (
	naiveDiffHelperEnv = "VAMOS_TEST_NAIVE_DIFF_HELPER"
	naiveDiffOutEnv    = "VAMOS_TEST_NAIVE_DIFF_OUT"
)

// TestNaiveDiffHelper saves diffedBuild's app with stores of its own, and writes whether the store diffed natively
// and what the image holds, when TestNaiveAndNativeDiffsMakeTheSameImage runs it; otherwise it does nothing.
func TestNaiveDiffHelper(t *testing.T) {
	nonce := os.Getenv(naiveDiffHelperEnv)
	if nonce == "" {
		return
	}
	native, image := saveDiffedBuild(t, nonce)
	must(t, os.WriteFile(os.Getenv(naiveDiffOutEnv), fmt.Appendf(nil, "native diff %t\n%s", native, image), 0o644))
}

// saveDiffedBuild saves diffedBuild's app with stores of the test's own, and returns whether the store diffed
// natively, and what the image holds: its configuration, but for the digests of its layers, and its flattened
// filesystem, each entry's header and the digest of its content.
// Its layers' digests are left out since the diffs differ in how they record a directory replaced, which a native
// diff marks opaque and a naive diff removes the files of, though the filesystems are the same.
func saveDiffedBuild(t *testing.T, nonce string) (native bool, image string) {
	t.Helper()
	usePrivateStores(t)
	fsys := project(t, map[string]string{"BUILD": diffedBuild(nonce), "input.txt": "input\n"})
	path := filepath.Join(t.TempDir(), "app.tar")
	vamos(t, fsys, "save", "app", path, "-C", projectDir).succeeds(t)
	withStore(t, func(store storage.Store) {
		status, err := store.Status()
		must(t, err)
		for _, pair := range status {
			if pair[0] == "Native Overlay Diff" {
				native = pair[1] == "true"
			}
		}
	})

	img, err := tarball.ImageFromPath(path, nil)
	must(t, err)
	config, err := img.ConfigFile()
	must(t, err)
	config.RootFS.DiffIDs = nil
	text, err := json.Marshal(config)
	must(t, err)
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", text)
	_, err = testenv.WalkImage(img, func(hdr *tar.Header, r io.Reader) bool {
		content := sha256.New()
		_, err := io.Copy(content, r)
		must(t, err)
		fmt.Fprintf(&b, "%s %c %o %d:%d %q %s %x\n", testenv.ImagePath(hdr.Name), hdr.Typeflag, hdr.Mode, hdr.Uid,
			hdr.Gid, hdr.Linkname, hdr.ModTime.UTC().Format(time.RFC3339Nano), content.Sum(nil))
		return false
	})
	must(t, err)
	return native, b.String()
}

// TestNaiveAndNativeDiffsMakeTheSameImage checks that an overlay store that diffs layers naively, as with Fedora's
// default storage.conf, and one that diffs them natively, as the stores of tests do, make the same image, but for
// how its layers record a directory replaced: so the images that tests check are those that users of either get.
// containers/storage settles which diff it uses once per process, so the naive diff is in a new one.
func TestNaiveAndNativeDiffsMakeTheSameImage(t *testing.T) {
	requireBuildah(t)
	nonce := newNonce()
	native, nativeImage := saveDiffedBuild(t, nonce)
	if !native {
		testenv.Unavailable(t, "diffing a layer natively", errors.New("the store's overlay driver diffs naively here"))
	}

	out := filepath.Join(t.TempDir(), "naive.txt")
	cmd := exec.Command(os.Args[0], "-test.run", "^TestNaiveDiffHelper$")
	cmd.Env = append(os.Environ(), naiveDiffHelperEnv+"="+nonce, naiveDiffOutEnv+"="+out, testenv.NaiveDiffEnv+"=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building with naive diffs failed: %v; output:\n%s", err, output)
	}
	got, err := os.ReadFile(out)
	must(t, err)
	// A kernel without metacopy, before Linux 4.19, refuses it, and the driver then drops metacopy=on and diffs
	// natively, so naive diffs can't be had there.
	if strings.HasPrefix(string(got), "native diff true\n") {
		testenv.Unavailable(t, "diffing a layer naively", errors.New("the overlay driver doesn't take metacopy here"))
	}
	if want := "native diff false\n" + nativeImage; string(got) != want {
		t.Errorf("building with naive diffs saved\n%s\nwant, as with native diffs,\n%s", got, want)
	}
}

// defaultStoreHelperEnv marks the test binary run by TestDefaultStore,
// in which TestDefaultStoreHelper prunes the default store.
const defaultStoreHelperEnv = "VAMOS_TEST_DEFAULT_STORE_HELPER"

// TestDefaultStoreHelper prunes, without a store in its configuration, when TestDefaultStore runs it;
// otherwise it does nothing.
func TestDefaultStoreHelper(t *testing.T) {
	if os.Getenv(defaultStoreHelperEnv) == "" {
		return
	}
	cmd := NewRootCommand(Config{Fs: afero.NewMemMapFs()})
	cmd.SetArgs([]string{"prune"})
	must(t, cmd.Execute())
}

// TestDefaultStore checks that Vamos, given no store, works on the one that containers/storage finds,
// with the build cache beside it, against a default store of its own.
// containers/storage reads its defaults once per process, so Vamos runs in a new one.
func TestDefaultStore(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("configures the default container store, which needs root")
	}
	dir := t.TempDir()
	conf := filepath.Join(dir, "storage.conf")
	must(t, os.WriteFile(conf, fmt.Appendf(nil, `[storage]
driver = "vfs"
graphroot = "%[1]s/default/storage"
runroot = "%[1]s/default/run"
`, dir), 0o644))
	cache := filepath.Join(dir, "default", "vamos")
	must(t, os.MkdirAll(filepath.Join(cache, "cas", "sha256"), 0o755))
	must(t, os.WriteFile(filepath.Join(cache, "cas", "sha256", "stale"), nil, 0o644))

	cmd := exec.Command(os.Args[0], "-test.run", "^TestDefaultStoreHelper$")
	cmd.Env = slices.DeleteFunc(os.Environ(), func(v string) bool { return strings.HasPrefix(v, "STORAGE_OPTS=") })
	cmd.Env = append(cmd.Env, defaultStoreHelperEnv+"=1", "CONTAINERS_STORAGE_CONF="+conf)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pruning the default store failed: %v; output:\n%s", err, out)
	}
	if want := "and 0 results, 1 files and 0 target histories"; !strings.Contains(string(out), want) {
		t.Errorf("output lacks %q, for the file in the default store's build cache:\n%s", want, out)
	}
	for _, path := range []string{filepath.Join(dir, "default", "storage"), filepath.Join(dir, "default", "run")} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("pruning didn't open the default store: %v", err)
		}
	}
}
