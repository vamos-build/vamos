package testenv

import (
	"bytes"
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"go.podman.io/image/v5/pkg/sysregistriesv2"
)

// InsecureRegistry marks the registry at host, "host:port", insecure in a registries.conf of the test's own,
// so that an executor, which takes its registry settings from its environment, reaches it over plain HTTP.
// The client's own registry access needs --tls-verify=false still.
// Tests that call it can't run in parallel, since the environment is shared.
func InsecureRegistry(t *testing.T, host string) {
	t.Helper()
	writeRegistriesConf(t, map[string]any{"location": host, "insecure": true})
}

// MirroredRegistry marks the registries at host and at mirror insecure, as InsecureRegistry does, and makes mirror a
// mirror of host, which pulls of host's images try first.
func MirroredRegistry(t *testing.T, host, mirror string) {
	t.Helper()
	writeRegistriesConf(t, map[string]any{"location": host, "insecure": true,
		"mirror": []map[string]any{{"location": mirror, "insecure": true}}})
}

// overrideEnv names the registries.conf that containers/image reads after every other, whose entries replace theirs
// for the same prefix, so that a test's settings add to the environment's rather than replace them.
const overrideEnv = "CONTAINERS_REGISTRIES_CONF_OVERRIDE"

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
