package testenv

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"go.podman.io/image/v5/pkg/sysregistriesv2"
	"go.podman.io/image/v5/types"
)

// TestHubMirror checks that the mirror serves an image's manifest and blobs by digest, asking its registry for each
// only once, and that it answers a tag, or what the registry doesn't have, with the registry API's error code for it,
// so that containers/image asks the registry itself, for a blob too.
func TestHubMirror(t *testing.T) {
	var requests atomic.Int64
	reg := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/" {
			requests.Add(1)
		}
		reg.ServeHTTP(w, r)
	}))
	t.Cleanup(upstream.Close)
	host := strings.TrimPrefix(upstream.URL, "http://")
	img, err := random.Image(256, 2)
	Must(t, err)
	ref, err := name.ParseReference(host+"/library/base:v1", name.Insecure)
	Must(t, err)
	Must(t, remote.Write(ref, img))
	requests.Store(0) // the push's own
	manifestDigest, err := img.Digest()
	Must(t, err)
	layers, err := img.Layers()
	Must(t, err)
	blobDigest, err := layers[1].Digest()
	Must(t, err)
	manifest, err := img.RawManifest()
	Must(t, err)
	mediaType, err := img.MediaType()
	Must(t, err)

	dir := t.TempDir()
	// get asks a mirror of its own, as each test binary has, sharing dir, for path.
	get := func(t *testing.T, path string) (status int, contentType, body string) {
		t.Helper()
		mirror, err := newHubMirror(dir, host, name.Insecure)
		Must(t, err)
		server := httptest.NewServer(mirror)
		defer server.Close()
		resp, err := http.Get(server.URL + path)
		Must(t, err)
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		Must(t, err)
		return resp.StatusCode, resp.Header.Get("Content-Type"), string(data)
	}
	rc, err := layers[1].Compressed()
	Must(t, err)
	blob, err := io.ReadAll(rc)
	Must(t, err)

	for round := range 2 {
		status, contentType, body := get(t, "/v2/library/base/manifests/"+manifestDigest.String())
		if status != http.StatusOK || body != string(manifest) {
			t.Fatalf("round %d: the manifest by digest gave %d, want it as pushed", round, status)
		}
		if contentType != string(mediaType) {
			t.Errorf("round %d: the manifest's Content-Type is %q, want %q", round, contentType, mediaType)
		}
		if status, _, body := get(t, "/v2/library/base/blobs/"+blobDigest.String()); status != http.StatusOK ||
			body != string(blob) {
			t.Fatalf("round %d: the blob by digest gave %d, want it as pushed", round, status)
		}
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("the registry was asked %d times, want 2: the manifest and the blob, once each", got)
	}
	// A manifest whose media type was lost is fetched again, rather than served without one.
	entries, err := filepath.Glob(filepath.Join(dir, "manifests", "*.type"))
	Must(t, err)
	for _, e := range entries {
		Must(t, os.Remove(e))
	}
	status, contentType, _ := get(t, "/v2/library/base/manifests/"+manifestDigest.String())
	if status != http.StatusOK || contentType != string(mediaType) {
		t.Errorf("the manifest without its media type gave %d, %q; want it fetched again, as %q", status, contentType,
			mediaType)
	}
	if got := requests.Load(); got != 3 {
		t.Errorf("the registry was asked %d times in all, want 3: once more for the manifest", got)
	}

	for path, code := range map[string]string{
		"/v2/library/base/manifests/v1":                                "MANIFEST_UNKNOWN",
		"/v2/library/base/manifests/sha256:" + strings.Repeat("0", 64): "MANIFEST_UNKNOWN",
		"/v2/library/base/blobs/sha256:" + strings.Repeat("0", 64):     "BLOB_UNKNOWN",
	} {
		status, _, body := get(t, path)
		var answer struct{ Errors []struct{ Code string } }
		if err := json.Unmarshal([]byte(body), &answer); status != http.StatusNotFound || err != nil ||
			len(answer.Errors) != 1 || answer.Errors[0].Code != code {
			t.Errorf("GET %s gave %d, %s; want 404 Not Found with %s", path, status, body, code)
		}
	}
}

// TestHubMirrorKeepsTheEnvironmentsSettings checks that the mirror's registries.conf adds to the environment's, as a
// test's own does after it, and that there is no mirror where the environment says anything of Docker Hub, or names
// an override file of its own.
func TestHubMirrorKeepsTheEnvironmentsSettings(t *testing.T) {
	t.Setenv(HubCacheEnv, t.TempDir())
	t.Setenv(overrideEnv, "") // restored, with what the mirror sets, once the test ends
	t.Cleanup(sysregistriesv2.InvalidateCache)
	environment := filepath.Join(t.TempDir(), "registries.conf")
	withEnvironment := func(t *testing.T, conf string) {
		t.Helper()
		Must(t, os.WriteFile(environment, []byte(conf), 0o644))
		t.Setenv("CONTAINERS_REGISTRIES_CONF", environment)
		t.Setenv(overrideEnv, "")
		sysregistriesv2.InvalidateCache()
	}

	t.Run("settings kept", func(t *testing.T) {
		withEnvironment(t, `short-name-mode = "permissive"
registry = [{location = "registry:5000", insecure = true}]
`)
		stop, err := mirrorDockerHub()
		Must(t, err)
		t.Cleanup(stop)
		InsecureRegistry(t, "other:5000") // a test's own file, after the mirror's
		for _, prefix := range []string{"registry:5000", "other:5000"} {
			if reg, err := sysregistriesv2.FindRegistry(nil, prefix+"/app"); err != nil || reg == nil || !reg.Insecure {
				t.Errorf("%s is configured as %+v, %v; want it insecure", prefix, reg, err)
			}
		}
		hub, err := sysregistriesv2.FindRegistry(nil, "docker.io/library/busybox")
		if err != nil || hub == nil || len(hub.Mirrors) != 1 || hub.Mirrors[0].PullFromMirror != "digest-only" {
			t.Errorf("docker.io is configured as %+v, %v; want the mirror, for digests", hub, err)
		}
		if mode, err := sysregistriesv2.GetShortNameMode(nil); err != nil || mode != types.ShortNameModePermissive {
			t.Errorf("the short-name mode is %v, %v; want the environment's, permissive", mode, err)
		}
	})

	const entry = "[[registry]]\n"
	for name, conf := range map[string]string{
		"docker.io":             entry + "location = \"docker.io\"\n[[registry.mirror]]\nlocation = \"hub:5000\"\n",
		"a Docker Hub prefix":   entry + "prefix = \"docker.io/team\"\nlocation = \"docker.io/team\"\nblocked = true\n",
		"a Docker Hub location": entry + "prefix = \"hub.example\"\nlocation = \"docker.io\"\nblocked = true\n",
		"a wildcard":            entry + "prefix = \"*.io\"\nblocked = true\n",
	} {
		t.Run(name+" left as it is", func(t *testing.T) {
			withEnvironment(t, conf)
			if stop, err := mirrorDockerHub(); err == nil {
				stop()
				t.Fatal("a mirror started where the environment configures Docker Hub, want none")
			}
			if got := os.Getenv(overrideEnv); got != "" {
				t.Errorf("%s = %q, want it unset", overrideEnv, got)
			}
		})
	}

	t.Run("an override of the environment's own left as it is", func(t *testing.T) {
		withEnvironment(t, "")
		own := filepath.Join(t.TempDir(), "override.conf")
		Must(t, os.WriteFile(own, []byte("[[registry]]\nlocation = \"registry:5000\"\ninsecure = true\n"), 0o644))
		t.Setenv(overrideEnv, own)
		if stop, err := mirrorDockerHub(); err == nil {
			stop()
			t.Fatal("a mirror started over the environment's override, want none")
		}
		if got := os.Getenv(overrideEnv); got != own {
			t.Errorf("%s = %q, want it as it was, %q", overrideEnv, got, own)
		}
		// A test's own file keeps what the environment's override says.
		InsecureRegistry(t, "other:5000")
		for _, prefix := range []string{"registry:5000", "other:5000"} {
			if reg, err := sysregistriesv2.FindRegistry(nil, prefix+"/app"); err != nil || reg == nil || !reg.Insecure {
				t.Errorf("%s is configured as %+v, %v; want it insecure", prefix, reg, err)
			}
		}
	})
}

// TestSharedRegistriesConf checks that the binary's own registries.conf keeps what the override file it replaces says,
// that tests' registries are in it while the tests run, and removed as each ends, a subtest's entry replacing its
// test's for the same prefix until then, without the environment changing, so that tests running in parallel can add
// theirs, and that stopping names the replaced file again.
func TestSharedRegistriesConf(t *testing.T) {
	own := filepath.Join(t.TempDir(), "override.conf")
	Must(t, os.WriteFile(own, []byte("[[registry]]\nlocation = \"registry:5000\"\ninsecure = true\n"), 0o644))
	t.Setenv(overrideEnv, own)
	t.Cleanup(sysregistriesv2.InvalidateCache)
	insecure := func(t *testing.T, host string) bool {
		t.Helper()
		reg, err := sysregistriesv2.FindRegistry(nil, host+"/app")
		Must(t, err)
		return reg != nil && reg.Insecure
	}

	var c registriesConf
	stop, err := c.share()
	Must(t, err)
	shared := os.Getenv(overrideEnv)
	if shared == own || shared == "" {
		t.Fatalf("%s = %q, want a file of the binary's own", overrideEnv, shared)
	}
	if !insecure(t, "registry:5000") {
		t.Error("registry:5000, which the replaced file marks insecure, isn't")
	}
	t.Run("one", func(t *testing.T) {
		if !c.add(t, map[string]any{"location": "one:5000", "insecure": true}) {
			t.Fatal("there was no file to add to")
		}
		t.Run("two", func(t *testing.T) {
			c.add(t, map[string]any{"location": "two:5000", "insecure": true})
			for _, host := range []string{"registry:5000", "one:5000", "two:5000"} {
				if !insecure(t, host) {
					t.Errorf("%s isn't insecure while both tests run", host)
				}
			}
			// A subtest's entry replaces its test's for the same prefix while it runs.
			c.add(t, map[string]any{"location": "one:5000", "insecure": false})
			if insecure(t, "one:5000") {
				t.Error("one:5000 is insecure still, once a subtest's entry replaced it")
			}
		})
		if insecure(t, "two:5000") {
			t.Error("two:5000 is insecure still once its test ended")
		}
		if !insecure(t, "one:5000") {
			t.Error("one:5000 isn't insecure while its test runs, once its subtest's entry is gone")
		}
	})
	if insecure(t, "one:5000") {
		t.Error("one:5000 is insecure still once its test ended")
	}
	if got := os.Getenv(overrideEnv); got != shared {
		t.Errorf("%s = %q after the tests, want it unchanged, %q", overrideEnv, got, shared)
	}

	stop()
	if got := os.Getenv(overrideEnv); got != own {
		t.Errorf("%s = %q once stopped, want the replaced file again, %q", overrideEnv, got, own)
	}
	if _, err := os.Stat(shared); !os.IsNotExist(err) {
		t.Errorf("the binary's file is left once stopped: %v", err)
	}
	if c.add(t, map[string]any{"location": "three:5000", "insecure": true}) {
		t.Error("an entry was added once stopped, want the test's own file used")
	}
}

// TestRegistriesConfWith checks that a test's registry entry is merged into a registries.conf of any layout TOML
// allows, replacing only the entry for its own prefix, and keeping everything else the file says.
func TestRegistriesConfWith(t *testing.T) {
	entry := map[string]any{"location": "registry:5000", "insecure": true}
	for name, base := range map[string]string{
		"tables": "short-name-mode = \"permissive\"\n[[registry]]\nlocation = \"registry:5000\"\n" +
			"[[registry]]\nprefix = \"registry:5000/team\"\nlocation = \"registry:5000/team\"\nblocked = true\n" +
			"[aliases]\napp = \"registry:5000/app\"\n",
		"an inline array": "short-name-mode = \"permissive\"\nregistry = [{location = \"registry:5000\"}, " +
			"{prefix = \"registry:5000/team\", location = \"registry:5000/team\", blocked = true}]\n" +
			"[aliases]\napp = \"registry:5000/app\"\n",
		"a trailing slash and other cases": "short-name-mode = \"permissive\"\n" +
			"[[Registry]]\nLocation = \"registry:5000/\"\n" +
			"[[Registry]]\nPrefix = \"registry:5000/team/\"\nLocation = \"registry:5000/team\"\nBlocked = true\n" +
			"[aliases]\napp = \"registry:5000/app\"\n",
		"CRLF": "short-name-mode = \"permissive\"\r\n[[registry]]\r\nlocation=\"registry:5000\"\r\n" +
			"[[registry]]\r\nprefix = 'registry:5000/team'\r\nlocation = \"registry:5000/team\"\r\nblocked = true\r\n" +
			"[aliases]\r\napp = \"registry:5000/app\"\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			text, err := registriesConfWith([]byte(base), entry)
			Must(t, err)
			conf := filepath.Join(t.TempDir(), "registries.conf")
			Must(t, os.WriteFile(conf, text, 0o644))
			sys := &types.SystemContext{SystemRegistriesConfPath: conf, SystemRegistriesConfDirPath: t.TempDir(),
				UserShortNameAliasConfPath: filepath.Join(t.TempDir(), "none.conf")} // not the host's aliases
			t.Cleanup(sysregistriesv2.InvalidateCache)
			if reg, err := sysregistriesv2.FindRegistry(sys, "registry:5000/app"); err != nil || reg == nil ||
				!reg.Insecure {
				t.Errorf("registry:5000 is configured as %+v, %v; want the entry, insecure\n%s", reg, err, text)
			}
			if reg, err := sysregistriesv2.FindRegistry(sys, "registry:5000/team/app"); err != nil || reg == nil ||
				!reg.Blocked {
				t.Errorf("registry:5000/team is configured as %+v, %v; want it blocked still\n%s", reg, err, text)
			}
			if mode, err := sysregistriesv2.GetShortNameMode(sys); err != nil || mode != types.ShortNameModePermissive {
				t.Errorf("the short-name mode is %v, %v; want it permissive still\n%s", mode, err, text)
			}
			if alias, _, err := sysregistriesv2.ResolveShortNameAlias(sys, "app"); err != nil || alias == nil ||
				alias.String() != "registry:5000/app" {
				t.Errorf("the alias app is %v, %v; want registry:5000/app still\n%s", alias, err, text)
			}
		})
	}
}

// TestHubMirrorRateLimited checks that the mirror asks a registry that refuses it for too many requests once, and
// passes the refusal on, with a status that containers/image doesn't ask the mirror again after, and the code it
// falls back to the registry itself on, and takes for a refusal rather than for a miss that may not happen again.
func TestHubMirrorRateLimited(t *testing.T) {
	var requests atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			return
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"errors":[{"code":"TOOMANYREQUESTS","message":"pull rate limit"}]}`))
	}))
	t.Cleanup(upstream.Close)
	mirror, err := newHubMirror(t.TempDir(), strings.TrimPrefix(upstream.URL, "http://"), name.Insecure)
	Must(t, err)
	server := httptest.NewServer(mirror)
	t.Cleanup(server.Close)
	resp, err := http.Get(server.URL + "/v2/library/base/manifests/sha256:" + strings.Repeat("0", 64))
	Must(t, err)
	var answer struct{ Errors []struct{ Code string } }
	err = json.NewDecoder(resp.Body).Decode(&answer)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || err != nil || len(answer.Errors) != 1 ||
		answer.Errors[0].Code != "TOOMANYREQUESTS" {
		t.Errorf("a manifest the registry refused for too many requests gave %s, %+v, %v; want 404 Not Found with "+
			"TOOMANYREQUESTS", resp.Status, answer, err)
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("the registry was asked %d times, want once", n)
	}
}

// TestHubMirrorFillsOnce checks that mirrors sharing an empty folder, as test binaries that run at once do, ask their
// registry for what they are all asked for once between them.
func TestHubMirrorFillsOnce(t *testing.T) {
	var requests atomic.Int64
	reg := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/manifests/") {
			requests.Add(1)
			time.Sleep(100 * time.Millisecond) // so that the other mirror is asked meanwhile
		}
		reg.ServeHTTP(w, r)
	}))
	t.Cleanup(upstream.Close)
	host := strings.TrimPrefix(upstream.URL, "http://")
	img, err := random.Image(256, 1)
	Must(t, err)
	ref, err := name.ParseReference(host+"/library/base:v1", name.Insecure)
	Must(t, err)
	Must(t, remote.Write(ref, img))
	manifestDigest, err := img.Digest()
	Must(t, err)
	requests.Store(0) // the push's own
	dir := t.TempDir()
	var wg sync.WaitGroup
	for range 3 {
		mirror, err := newHubMirror(dir, host, name.Insecure)
		Must(t, err)
		server := httptest.NewServer(mirror)
		t.Cleanup(server.Close)
		wg.Go(func() {
			resp, err := http.Get(server.URL + "/v2/library/base/manifests/" + manifestDigest.String())
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("the manifest gave %s, want it", resp.Status)
			}
		})
	}
	wg.Wait()
	if n := requests.Load(); n != 1 {
		t.Errorf("mirrors sharing a folder asked their registry for the manifest %d times, want once", n)
	}
}

// TestHubMirrorWaiterGivesUp checks that a request waits for another test binary to fill the cache, rather than
// fetching itself, until its client gives up, and then gives up too.
func TestHubMirrorWaiterGivesUp(t *testing.T) {
	var requests atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/" {
			requests.Add(1)
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(upstream.Close)
	dir := t.TempDir()
	mirror, err := newHubMirror(dir, strings.TrimPrefix(upstream.URL, "http://"), name.Insecure)
	Must(t, err)
	unlock, err := lockFile(t.Context(), filepath.Join(dir, "locks", "sha256-"+strings.Repeat("0", 64)))
	Must(t, err) // held, as by a binary that is fetching
	const patience = 100 * time.Millisecond
	start := time.Now() // before the timeout starts, which the request must outlast
	ctx, cancel := context.WithTimeout(t.Context(), patience)
	defer cancel()
	served := make(chan struct{})
	go func() {
		defer close(served)
		mirror.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(ctx, http.MethodGet,
			"/v2/library/base/manifests/sha256:"+strings.Repeat("0", 64), nil))
	}()
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		unlock() // so that the request can end, whatever it waits for
		<-served
		t.Fatal("a request whose client gave up still waited for the lock after 5s; want it to give up too")
	}
	unlock()
	if took := time.Since(start); took < patience {
		t.Errorf("the request ended after %v, before its client gave up after %v; want it to wait for the lock", took,
			patience)
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("the registry was asked %d times while another held the lock; want none", n)
	}
}

// TestHubMirrorStopLeavesNoOverride checks that once the mirror stops, its registries.conf, which it removes, is no
// longer named, so that what reads registry settings after still can.
func TestHubMirrorStopLeavesNoOverride(t *testing.T) {
	t.Setenv(HubCacheEnv, t.TempDir())
	t.Setenv(overrideEnv, "") // restored once the test ends
	environment := filepath.Join(t.TempDir(), "registries.conf")
	Must(t, os.WriteFile(environment, nil, 0o644))
	t.Setenv("CONTAINERS_REGISTRIES_CONF", environment)
	sysregistriesv2.InvalidateCache()
	t.Cleanup(sysregistriesv2.InvalidateCache)
	stop, err := mirrorDockerHub()
	Must(t, err)
	stop()
	if got := os.Getenv(overrideEnv); got != "" {
		t.Errorf("%s = %q once the mirror stopped, want it unset", overrideEnv, got)
	}
	if reg, err := sysregistriesv2.FindRegistry(nil, "docker.io/library/busybox"); err != nil || reg != nil {
		t.Errorf("docker.io is configured as %+v, %v, once the mirror stopped; want it as the environment says, "+
			"not at all", reg, err)
	}
}
