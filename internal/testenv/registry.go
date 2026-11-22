package testenv

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/BurntSushi/toml"
	"go.podman.io/image/v5/pkg/sysregistriesv2"
	"go.podman.io/image/v5/types"
)

// InsecureRegistry marks the registry at host, "host:port", insecure in the registries.conf that containers/image
// reads after every other, so that an executor, which takes its registry settings from its environment, reaches it
// over plain HTTP, until the test ends. The client's own registry access needs --tls-verify=false still.
// In a test binary that Main runs, the entry goes in the binary's own registries.conf, which the tests share, so
// tests that call it may run in parallel; elsewhere it goes in one of the test's own, which the environment names,
// so they can't.
func InsecureRegistry(t *testing.T, host string) {
	t.Helper()
	addRegistry(t, map[string]any{"location": host, "insecure": true})
}

// MirroredRegistry marks the registries at host and at mirror insecure, as InsecureRegistry does, and makes mirror a
// mirror of host, which pulls of host's images try first.
func MirroredRegistry(t *testing.T, host, mirror string) {
	t.Helper()
	addRegistry(t, map[string]any{"location": host, "insecure": true,
		"mirror": []map[string]any{{"location": mirror, "insecure": true}}})
}

// overrideEnv names the registries.conf that containers/image reads after every other, whose entries replace theirs
// for the same prefix, so that a test's settings add to the environment's rather than replace them.
const overrideEnv = "CONTAINERS_REGISTRIES_CONF_OVERRIDE"

// addRegistry adds entry, a [[registry]] table, to the binary's shared registries.conf, if Main made one and the
// environment still names it, until the test ends, or else to one of the test's own.
func addRegistry(t *testing.T, entry map[string]any) {
	t.Helper()
	if !sharedConf.add(t, entry) {
		writeRegistriesConf(t, entry)
	}
}

// sharedConf is the registries.conf of a test binary that Main runs, which the environment names as overrideEnv
// for the whole run, so that tests running in parallel never change the environment, and which each test's
// registries are added to and removed from.
var sharedConf registriesConf

// registriesConf is a registries.conf whose [[registry]] tables tests add while they run.
type registriesConf struct {
	mu      sync.Mutex
	path    string                     // the file, or "" if there is none
	noDirs  string                     // an empty folder, as the drop-in folder of a staged file that write checks
	base    []byte                     // what it holds but for the tests' tables
	entries map[string][]registryEntry // the tests' tables, by prefix, the last of each in effect
	added   int                        // how many tables tests have added, which numbers each
	err     error                      // why Main couldn't share a file, which fails each test that adds a table
	named   string                     // what the environment named as overrideEnv as Main tried
	wasSet  bool                       // whether overrideEnv was set then, if to nothing
}

// registryEntry is a [[registry]] table that a test added, numbered in the order they were added.
type registryEntry struct {
	t     *testing.T
	table map[string]any
	n     int
}

// share makes the environment name, as overrideEnv, a registries.conf of the binary's own, holding what the one it
// named before holds, if it named one, and returns what names that one again and removes it, for the end of the run.
// If it can't, add fails each test that adds a table, saying why.
func (c *registriesConf) share() (stop func(), err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	previous, named := os.LookupEnv(overrideEnv)
	defer func() {
		if err != nil {
			c.path, c.err, c.named, c.wasSet = "", err, previous, named
		}
	}()
	var base []byte
	if previous != "" {
		if base, err = os.ReadFile(previous); err != nil {
			return nil, err
		}
	}
	dir, err := os.MkdirTemp("", "vamos-test-registries-")
	if err != nil {
		return nil, err
	}
	c.path, c.base, c.entries = filepath.Join(dir, "registries.conf"), base, map[string][]registryEntry{}
	c.noDirs = filepath.Join(dir, "registries.d")
	if err = os.Mkdir(c.noDirs, 0o755); err != nil {
		return nil, errors.Join(err, os.RemoveAll(dir))
	}
	if err = c.write(); err == nil {
		err = os.Setenv(overrideEnv, c.path)
	}
	if err == nil {
		sysregistriesv2.InvalidateCache()
		_, err = sysregistriesv2.GetRegistries(nil)
	}
	restore := func() {
		if named {
			_ = os.Setenv(overrideEnv, previous)
		} else {
			_ = os.Unsetenv(overrideEnv)
		}
		sysregistriesv2.InvalidateCache()
		os.RemoveAll(dir)
	}
	if err != nil {
		restore()
		return nil, err
	}
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.path = ""
		restore()
	}, nil
}

// add adds entry to the file until the test ends, in place of any that the test, or a test it is a subtest of, added
// for the same prefix before, which it restores then, and reports whether there is a file to add it to: there isn't
// once the environment names another, as a test that sets overrideEnv itself makes it. It fails the test if Main
// couldn't share a file, unless the test named one itself, if another test that is running has an entry for the
// prefix, or if containers/image can't read the file with the entry, which write checks before other tests can read
// it, and which add then removes again.
func (c *registriesConf) add(t *testing.T, entry map[string]any) bool {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if now, set := os.LookupEnv(overrideEnv); c.err != nil && now == c.named && set == c.wasSet {
		// A file of the test's own would need t.Setenv, which panics in a parallel test; one that set the
		// environment itself, as only a serial test can, still gets one.
		t.Fatalf("Main couldn't share a registries.conf with the tests: %v", c.err)
	}
	if c.path == "" || os.Getenv(overrideEnv) != c.path {
		return false
	}
	prefix := registryPrefix(entry)
	for _, e := range c.entries[prefix] {
		if e.t.Name() != t.Name() && !strings.HasPrefix(t.Name(), e.t.Name()+"/") {
			t.Fatalf("%s is in the registries.conf of %s, which is running", prefix, e.t.Name())
		}
	}
	c.added++
	added := registryEntry{t: t, table: entry, n: c.added}
	c.entries[prefix] = append(c.entries[prefix], added)
	err := c.write()
	if err == nil {
		_, err = sysregistriesv2.GetRegistries(nil)
	}
	if err != nil {
		c.drop(prefix, added.n)
		t.Fatalf("adding %v to %s: %v", entry, c.path, errors.Join(err, c.write()))
	}
	t.Cleanup(func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.drop(prefix, added.n)
		if c.path != "" {
			if err := c.write(); err != nil {
				t.Errorf("removing %v from %s: %v", entry, c.path, err)
			}
		}
	})
	return true
}

// drop removes the entry numbered n for prefix; c.mu is held.
func (c *registriesConf) drop(prefix string, n int) {
	c.entries[prefix] = slices.DeleteFunc(c.entries[prefix], func(e registryEntry) bool { return e.n == n })
	if len(c.entries[prefix]) == 0 {
		delete(c.entries, prefix)
	}
}

// write writes the file, its base with the tests' tables, replacing it whole, so that a reader that a running test
// starts never sees part of it, and makes containers/image read it again; c.mu is held. It has containers/image
// read the new file first, staged, alone, so that one it rejects never replaces the one that tests running at once
// read: as the main file, with an empty drop-in folder, which also leaves out the override.
func (c *registriesConf) write() error {
	text := c.base
	for _, prefix := range slices.Sorted(maps.Keys(c.entries)) {
		added := c.entries[prefix]
		var err error
		if text, err = registriesConfWith(text, added[len(added)-1].table); err != nil {
			return err
		}
	}
	tmp := c.path + ".new"
	if err := os.WriteFile(tmp, text, 0o644); err != nil {
		return err
	}
	sysregistriesv2.InvalidateCache() // which caches the staged file by its path, the same each time
	staged := &types.SystemContext{SystemRegistriesConfPath: tmp, SystemRegistriesConfDirPath: c.noDirs}
	if _, err := sysregistriesv2.GetRegistries(staged); err != nil {
		return errors.Join(err, os.Remove(tmp))
	}
	if err := os.Rename(tmp, c.path); err != nil {
		return err
	}
	// containers/image caches registries.conf without noticing that the file changed.
	sysregistriesv2.InvalidateCache()
	return nil
}

// writeRegistriesConf writes a registries.conf of the test's own, read as overrideEnv says, holding what the one it
// replaces holds, the Docker Hub mirror's entry or an override file of the environment's, with entry, a [[registry]]
// table, as registriesConfWith adds it, and makes containers/image read it.
func writeRegistriesConf(t *testing.T, entry map[string]any) {
	t.Helper()
	var base []byte
	existing := os.Getenv(overrideEnv)
	if existing != "" {
		var err error
		if base, err = os.ReadFile(existing); err != nil {
			t.Fatal(err)
		}
	}
	text, err := registriesConfWith(base, entry)
	if err != nil {
		t.Fatalf("adding %v to %s: %v", entry, existing, err)
	}
	conf := filepath.Join(t.TempDir(), "registries.conf")
	Must(t, os.WriteFile(conf, text, 0o644))
	t.Setenv(overrideEnv, conf)
	// containers/image caches registries.conf without noticing the variable change.
	sysregistriesv2.InvalidateCache()
	t.Cleanup(sysregistriesv2.InvalidateCache)
	if _, err := sysregistriesv2.GetRegistries(nil); err != nil {
		t.Fatalf("adding %v to %s, as %s: %v", entry, cmp.Or(existing, "no override"), conf, err)
	}
}

// registriesConfWith returns a registries.conf that holds what base, one, holds, with entry, a [[registry]] table,
// in place of any entry of base for the same prefix, as the entry's prefix, or else its location, says. It merges
// them as TOML, with the parser containers/image uses, so that base may be written in any layout TOML allows.
func registriesConfWith(base []byte, entry map[string]any) ([]byte, error) {
	conf := map[string]any{}
	if _, err := toml.Decode(string(base), &conf); err != nil {
		return nil, err
	}
	key := tomlKey(conf, "registry") // which containers/image matches regardless of case, as for an entry's keys
	var registries []map[string]any
	switch existing := conf[key].(type) {
	case nil:
	case []map[string]any: // tables
		registries = existing
	case []any: // an inline array
		for _, e := range existing {
			table, ok := e.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("an entry of registry is %T, not a table", e)
			}
			registries = append(registries, table)
		}
	default:
		return nil, fmt.Errorf("registry is %T, not an array of tables", existing)
	}
	var kept []map[string]any
	for _, e := range registries {
		if registryPrefix(e) != registryPrefix(entry) {
			kept = append(kept, e)
		}
	}
	conf[key] = append(kept, entry)
	var text bytes.Buffer
	text.WriteString("# written by testenv\n")
	if err := toml.NewEncoder(&text).Encode(conf); err != nil {
		return nil, err
	}
	return text.Bytes(), nil
}

// registryPrefix returns the prefix that a [[registry]] table applies to, as containers/image reads it: its prefix,
// or else its location, without a trailing "/".
func registryPrefix(entry map[string]any) string {
	prefix, _ := entry[tomlKey(entry, "prefix")].(string)
	if prefix == "" {
		prefix, _ = entry[tomlKey(entry, "location")].(string)
	}
	return strings.TrimRight(prefix, "/")
}

// tomlKey returns the key of table that is name but for case, as containers/image matches keys, or else name.
func tomlKey(table map[string]any, name string) string {
	for key := range table {
		if strings.EqualFold(key, name) {
			return key
		}
	}
	return name
}
