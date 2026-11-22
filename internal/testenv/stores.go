package testenv

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/moby/sys/mountinfo"
	"go.podman.io/buildah"
	"go.podman.io/storage"
)

// RequireIntegrationEnv is the environment variable that, when set, makes tests which need buildah,
// root, ownership changes or a terminal fail, rather than skip, where those are unavailable:
// for a host that is meant to run the whole suite.
const RequireIntegrationEnv = "VAMOS_REQUIRE_INTEGRATION"

// Unavailable reports that what the test needs, what, isn't available here, as err says:
// the test fails if RequireIntegrationEnv is set, and is skipped otherwise.
func Unavailable(t *testing.T, what string, err error) {
	t.Helper()
	msg := fmt.Sprintf("%s isn't possible here: %v", what, err)
	if os.Getenv(RequireIntegrationEnv) != "" {
		t.Fatalf("%s, but %s is set", msg, RequireIntegrationEnv)
	}
	t.Skip(msg)
}

// RequireBuildah skips tests that build images with -short,
// and otherwise checks that they can be built here: as root, in a container store that opens,
// with a container that buildah can make and mount.
// Where they can't, the test is skipped, or fails if RequireIntegrationEnv is set.
// The test binary's TestMain must be Main, as for any test that builds.
func RequireBuildah(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("builds an image; skipped with -short")
	}
	if err := buildahAvailable(); err != nil {
		Unavailable(t, "building an image", err)
	}
}

// buildahAvailable probes, once, whether images can be built here: it needs root,
// and the suite's container store must open, which fails on a filesystem its driver can't use.
// It then makes and mounts a container from scratch, as every build does first, which needs nothing from a registry:
// that also needs buildah's network backend, whose lock is on /run, and the driver's mounts.
// Steps that run commands need more, such as an isolation that works here; their tests fail where it doesn't.
var buildahAvailable = sync.OnceValue(func() error {
	if os.Geteuid() != 0 {
		return errors.New("it needs root")
	}
	s, err := suiteStores()
	if err != nil {
		return err
	}
	store, err := storage.GetStore(s.Options)
	if err != nil {
		return fmt.Errorf("opening a container store at %s: %w", s.Options.GraphRoot, err)
	}
	err = probeBuilder(store)
	if _, shutdownErr := store.Shutdown(false); err == nil {
		err = shutdownErr
	}
	return err
})

// probeBuilder makes a container from scratch in store, mounts it, and deletes it.
func probeBuilder(store storage.Store) error {
	builder, err := buildah.NewBuilder(context.Background(), store, buildah.BuilderOptions{
		FromImage:  "scratch",
		PullPolicy: buildah.PullNever,
	})
	if err != nil {
		return fmt.Errorf("making a container from scratch: %w", err)
	}
	_, err = builder.Mount("")
	if err != nil {
		err = fmt.Errorf("mounting a container: %w", err)
	}
	return errors.Join(err, builder.Delete())
}

// RequireOwnership checks that tests which give files other owners can do so here,
// by trying it on a temporary file: the test is skipped if they can't, or fails if RequireIntegrationEnv is set.
func RequireOwnership(t *testing.T) {
	t.Helper()
	if err := ownershipAvailable(); err != nil {
		Unavailable(t, "changing a file's owner", err)
	}
}

// ownershipAvailable probes, once, whether files can be given other owners.
// Exports give files owners only as root, so it needs root, as well as a filesystem on which that works.
var ownershipAvailable = sync.OnceValue(func() error {
	if os.Geteuid() != 0 {
		return errors.New("it needs root")
	}
	f, err := os.CreateTemp("", "vamos-owner-")
	if err != nil {
		return err
	}
	f.Close()
	defer os.Remove(f.Name())
	return os.Lchown(f.Name(), 1234, 1235)
})

// Stores is a container store and the build cache beside it, made for tests, in a directory of their own,
// so that tests never touch the developer's.
type Stores struct {
	Dir     string               // holds storage/, the store, run/, its run root, and vamos/, the build cache
	Options storage.StoreOptions // the store's, which a local executor opens it with
}

// UnnamedImages returns the IDs of the images in the container store that have no name,
// such as an image a failed target might leave behind; cached layers' images are named.
func (s *Stores) UnnamedImages() (map[string]bool, error) {
	return s.ImageIDs(func(img storage.Image) bool { return len(img.Names) == 0 })
}

// ImageIDs returns the IDs of the images in the container store for which keep is true.
func (s *Stores) ImageIDs(keep func(storage.Image) bool) (map[string]bool, error) {
	ids := map[string]bool{}
	err := s.WithStore(func(store storage.Store) error {
		images, err := store.Images()
		if err != nil {
			return err
		}
		for _, img := range images {
			if keep(img) {
				ids[img.ID] = true
			}
		}
		return nil
	})
	return ids, err
}

// ContainerIDs returns the IDs of the containers in the container store, such as one that a step runs in.
func (s *Stores) ContainerIDs() (map[string]bool, error) {
	ids := map[string]bool{}
	err := s.WithStore(func(store storage.Store) error {
		containers, err := store.Containers()
		for _, c := range containers {
			ids[c.ID] = true
		}
		return err
	})
	return ids, err
}

// WithStore calls use with the container store, open only until use returns,
// since each vamos run opens and shuts it too.
func (s *Stores) WithStore(use func(storage.Store) error) error {
	store, err := storage.GetStore(s.Options)
	if err != nil {
		return err
	}
	defer store.Shutdown(false)
	return use(store)
}

// suiteStores makes the stores that a test binary's tests share, once.
var suiteStores = sync.OnceValues(func() (*Stores, error) {
	suiteMade.Store(true)
	return newStores("suite")
})

// SuiteStores returns the stores that the test binary's tests share.
// Tests that destroy what a store holds, such as by pruning, must use PrivateStores instead.
func SuiteStores(t *testing.T) *Stores {
	t.Helper()
	s, err := suiteStores()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// PrivateStores returns stores for the test alone, removed once it ends, whose run root starts with the overlay
// driver's checks of the filesystem that the suite's store recorded, as seedDriverChecks says, where the suite's store
// is overlay; in -short runs, which mount no layer, without those made at a first mount, as driverChecks says.
func PrivateStores(t *testing.T) *Stores {
	t.Helper()
	s, err := newStores(storesName(t.Name()))
	if err != nil {
		t.Fatal(err)
	}
	if overlayOrDefault(s.Options.GraphDriverName) {
		if err := seedDriverChecks(s.Options.RunRoot); err != nil {
			os.RemoveAll(s.Dir)
			t.Fatalf("copying the overlay driver's checks of the filesystem: %v", err)
		}
	}
	t.Cleanup(func() {
		if err := s.remove(); err != nil {
			t.Errorf("removing the test's stores: %v", err)
		}
	})
	return s
}

// storesName returns the test name as part of the name of its stores' directory: "/" as "-", and any other character
// but a letter, a digit, "_", "." and "-" as "_", since overlay mount options, which name the directory, are separated
// by commas and can't hold "=" or ":" in a path. A long name is cut short, and a hash of it kept, since every layer
// of a mount names the directory, and mount options are limited to a page.
func storesName(test string) string {
	name := strings.Map(func(r rune) rune {
		switch {
		case r == '/':
			return '-'
		case strings.ContainsRune("_.-", r), 'a' <= r && r <= 'z', 'A' <= r && r <= 'Z', '0' <= r && r <= '9':
			return r
		}
		return '_'
	}, test)
	if len(name) > 64 {
		h := fnv.New32a()
		h.Write([]byte(test))
		name = fmt.Sprintf("%s-%08x", name[:48], h.Sum32())
	}
	return name
}

// RemoveSuiteStores removes the shared stores, if they were made.
// A TestMain that may have used them calls it after running the tests.
func RemoveSuiteStores() error {
	if !suiteMade.Load() {
		return nil
	}
	if s, err := suiteStores(); err == nil {
		return s.remove()
	}
	return nil // making them failed, and newStores fails only before it makes anything
}

// suiteMade reports whether suiteStores has been called.
var suiteMade atomic.Bool

// newStores makes stores in a new directory named after name.
// The directory is beside the default container store, whose filesystem its driver is known to work on:
// a container's own overlay filesystem, as /tmp often is, can't hold an overlay store.
// Where that can't be written, as without root, it is in the temporary directory, where non-building tests
// can still use it.
// The store takes the default driver and its options, so that it works as the developer's does,
// but with overlay's metacopy off, which Fedora's default storage.conf turns on, unless NaiveDiffEnv is set:
// explicitly where the kernel takes the option and no mount program is given, so that a kernel whose overlay module
// turns it on by default doesn't.
// With metacopy, the overlay driver can't diff a layer natively, and diffs it naively instead,
// which waits for the next whole second after every commit, since it compares the times of files only to the second:
// that wait was most of the time the suite took.
func newStores(name string) (*Stores, error) {
	options, err := storage.DefaultStoreOptions()
	if err != nil {
		return nil, fmt.Errorf("reading the default container storage options: %w", err)
	}
	pattern := "vamos-test-" + name + "-"
	dir, err := os.MkdirTemp(filepath.Dir(options.GraphRoot), pattern)
	if err != nil {
		if dir, err = os.MkdirTemp("", pattern); err != nil {
			return nil, err
		}
	}
	options.GraphRoot = filepath.Join(dir, "storage")
	options.RunRoot = filepath.Join(dir, "run")
	options.ImageStore = ""
	options.TransientStore = false
	options.GraphDriverOptions = withoutSharedStores(options.GraphDriverOptions)
	if overlayOrDefault(options.GraphDriverName) {
		sayOff := overlayTakesMetacopy() && !hasMountProgram(options.GraphDriverOptions)
		options.GraphDriverOptions = withMetacopy(options.GraphDriverOptions, os.Getenv(NaiveDiffEnv) != "", sayOff)
	}
	return &Stores{Dir: dir, Options: options}, nil
}

// overlayOrDefault reports whether a store whose driver is named driver may use the overlay driver: one that names
// none takes the first that works, as storage.conf's driver_priority, or containers/storage, orders them, which is
// overlay where it works.
func overlayOrDefault(driver string) bool {
	return driver == "overlay" || driver == ""
}

// seedDriverChecks copies into runRoot the overlay driver's checks of the filesystem, which it records in its run root
// and reads before checking again: whether overlay works there, with metacopy, with native diffs, and with idmapped
// lower directories. A store's checks are the same for every test's, since each is on the same filesystem with the
// same options, so the suite's store makes them once, and each test's store starts with them, as a store that has
// been opened before does, rather than making them again. A check makes mounts, and the check of idmapped lower
// directories forks this process, without starting a program, into a user namespace until it ends: the fork holds
// every file this process has open, and so every lock on one, such as a build cache's, that another test, running in
// parallel, would then find held after it closed it. Where the suite's store can't be opened, as without root,
// nothing is copied, and a store checks the filesystem itself.
func seedDriverChecks(runRoot string) error {
	from := driverChecks()
	if from == "" {
		return nil
	}
	entries, err := os.ReadDir(from)
	if err != nil {
		return err
	}
	to := filepath.Join(runRoot, "overlay")
	if err := os.MkdirAll(to, 0o700); err != nil {
		return err
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(from, e.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(to, e.Name()), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// driverChecks returns the folder of the overlay driver's checks of the filesystem in the run root of the suite's
// store, having opened the store, and mounted a container in it where buildah can, outside -short runs, so that it
// made them all, since the driver makes some only as it first mounts a layer; or "" if the store can't be opened, or
// uses another driver, as one that names none may.
var driverChecks = sync.OnceValue(func() string {
	if !testing.Short() {
		_ = buildahAvailable() // which mounts a container in the suite's store; RequireBuildah reports why it can't
	}
	s, err := suiteStores()
	if err != nil {
		return ""
	}
	overlay := false
	if err := s.WithStore(func(store storage.Store) error {
		overlay = store.GraphDriverName() == "overlay"
		return nil
	}); err != nil || !overlay {
		return ""
	}
	return filepath.Join(s.Options.RunRoot, "overlay")
})

// NaiveDiffEnv is the environment variable that, when set, gives the overlay driver of the stores that tests make
// metacopy, as Fedora's default storage.conf does, so that it diffs layers naively, comparing their files with their
// parent layers', rather than natively, as it does without metacopy; another driver always diffs naively.
// containers/storage settles which diff its overlay driver uses once per process, for the first overlay store it opens,
// so the variable applies to a whole test binary, such as one that a test runs to build with naive diffs.
const NaiveDiffEnv = "VAMOS_TEST_NAIVE_DIFF"

// withMetacopy returns the driver options with overlay's metacopy turned on, in its mount options, or off: with
// metacopy=off where sayOff says the kernel takes it, or else with metacopy=on left out, and an explicit metacopy=off
// kept, which leaves it as the kernel's overlay module has it, off unless that turns it on. The overlay driver takes
// the last mount option meant for it, as overlay.mountopt=nodev,metacopy=on or mountopt=nodev, which containers/storage
// emits for storage.conf's overlay and general options, and skips those of another driver, as zfs.mountopt, so only
// that last one is changed.
// Emptied, it stays, unless none came before it, since dropping it would make an earlier one the last.
func withMetacopy(options []string, on, sayOff bool) []string {
	setting := ""
	switch {
	case on:
		setting = "metacopy=on"
	case sayOff:
		setting = "metacopy=off"
	}
	kept := slices.Clone(options)
	last, earlier := -1, false
	for i, o := range kept {
		key, _, _ := strings.Cut(o, "=")
		driver, name, ok := strings.Cut(strings.ToLower(strings.TrimSpace(key)), ".")
		if !ok {
			driver, name = "", driver
		}
		if name == "mountopt" && (driver == "" || driver == "overlay" || driver == "overlay2") {
			earlier = earlier || last >= 0
			last = i
		}
	}
	if last < 0 {
		if setting != "" {
			kept = append(kept, "overlay.mountopt="+setting)
		}
		return kept
	}
	key, value, _ := strings.Cut(kept[last], "=")
	var opts []string
	for opt := range strings.SplitSeq(value, ",") {
		optName, optValue, _ := strings.Cut(opt, "=")
		metacopy := strings.TrimSpace(optName) == "metacopy"
		if opt != "" && (!metacopy || setting == "" && strings.TrimSpace(optValue) == "off") {
			opts = append(opts, opt)
		}
	}
	if setting != "" {
		opts = append(opts, setting)
	}
	if len(opts) == 0 && !earlier {
		return slices.Delete(kept, last, last+1)
	}
	kept[last] = key + "=" + strings.Join(opts, ",")
	return kept
}

// overlayTakesMetacopy reports, once for the process, so that every store a test binary makes has the same mount
// options, which key the driver's checks that seedDriverChecks copies, whether the kernel's overlay module takes the
// metacopy mount option, as it does from Linux 4.19, with a parameter for its default: an older one refuses to mount
// with it, even as metacopy=off. A module that isn't loaded yet has no parameters to look at, so the kernel's version
// decides then.
var overlayTakesMetacopy = sync.OnceValue(func() bool {
	if _, err := os.Stat("/sys/module/overlay"); err == nil {
		_, err := os.Stat("/sys/module/overlay/parameters/metacopy")
		return err == nil
	}
	release, err := os.ReadFile("/proc/sys/kernel/osrelease")
	return err == nil && kernelAtLeast(string(release), 4, 19)
})

// kernelAtLeast reports whether release, a kernel release such as 6.8.0-45-generic, is at least major.minor.
func kernelAtLeast(release string, major, minor int) bool {
	var gotMajor, gotMinor int
	if _, err := fmt.Sscanf(strings.TrimSpace(release), "%d.%d", &gotMajor, &gotMinor); err != nil {
		return false
	}
	return gotMajor > major || gotMajor == major && gotMinor >= minor
}

// hasMountProgram reports whether the driver options give overlay a program to mount with, such as fuse-overlayfs,
// which is handed the mount options, and with which the driver diffs naively whatever metacopy says.
func hasMountProgram(options []string) bool {
	for _, o := range options {
		key, _, _ := strings.Cut(o, "=")
		driver, name, ok := strings.Cut(strings.ToLower(strings.TrimSpace(key)), ".")
		if !ok {
			driver, name = "", driver
		}
		if name == "mount_program" && (driver == "" || driver == "overlay" || driver == "overlay2") {
			return true
		}
	}
	return false
}

// withoutSharedStores returns the driver options without stores that the store would share with others:
// additional image stores, which would show the developer's images to tests, hiding redundant pulls,
// and additional layer stores, which would serve layers to tests and count their use.
// containers/storage emits them as imagestore=<dir> and additionallayerstore=<dir>,
// and the overlay driver also takes additionalimagestore=<dir>.
// Keys are matched as the driver parses them: trimmed, in any case, and with or without a driver's prefix,
// as in overlay.imagestore=<dir>.
func withoutSharedStores(options []string) []string {
	var kept []string
	for _, o := range options {
		key, _, _ := strings.Cut(o, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		if _, name, ok := strings.Cut(key, "."); ok {
			key = name
		}
		switch key {
		case "imagestore", "additionalimagestore", "additionallayerstore":
		default:
			kept = append(kept, o)
		}
	}
	return kept
}

// remove unmounts and removes the stores.
// A store that never opened, as when the probe found its driver can't use the filesystem, has nothing mounted,
// so its directory is removed all the same.
// Where something under the directory is still mounted, it is left as it is, since removing it would remove
// the store's records while the mounts that they describe remain.
func (s *Stores) remove() error {
	if _, err := os.Stat(s.Options.GraphRoot); err == nil {
		if store, err := storage.GetStore(s.Options); err == nil {
			if err := shutdown(store); err != nil {
				return fmt.Errorf("shutting down the container store at %s, so leaving %s: %w",
					s.Options.GraphRoot, s.Dir, err)
			}
		}
	}
	mounts, err := mountsUnder(s.Dir)
	if err != nil {
		return fmt.Errorf("listing mounts, so leaving %s: %w", s.Dir, err)
	}
	if len(mounts) > 0 {
		return fmt.Errorf("leaving %s, since these are still mounted: %s", s.Dir, strings.Join(mounts, ", "))
	}
	return os.RemoveAll(s.Dir)
}

// shutdown unmounts every layer of store, and then the driver's own mounts.
// Shutdown reports that a layer is used if any was mounted when it started, even once it has unmounted them,
// and then leaves the driver mounted; called again, with none mounted, it unmounts the driver too.
func shutdown(store storage.Store) error {
	_, err := store.Shutdown(true)
	if errors.Is(err, storage.ErrLayerUsedByContainer) {
		_, err = store.Shutdown(true)
	}
	return err
}

// mountsUnder returns the mount points at dir or under it.
func mountsUnder(dir string) ([]string, error) {
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved // mount points are listed by their real paths
	}
	infos, err := mountinfo.GetMounts(mountinfo.PrefixFilter(dir))
	if err != nil {
		return nil, err
	}
	var mounts []string
	for _, info := range infos {
		mounts = append(mounts, info.Mountpoint)
	}
	return mounts, nil
}
