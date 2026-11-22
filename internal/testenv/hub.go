package testenv

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/opencontainers/go-digest"
	"go.podman.io/image/v5/pkg/sysregistriesv2"
)

// HubCacheEnv names the variable that, if set, is the folder where the Docker Hub mirror keeps what it fetched,
// instead of vamos-test/docker.io in the user's cache folder. The folder is the user's: the mirror doesn't arrange
// for users to share it.
const HubCacheEnv = "VAMOS_TEST_HUB_CACHE"

// mirrorDockerHub starts a mirror of Docker Hub, which serves manifests and blobs by digest from a folder that
// outlasts the test binary, fetching from Docker Hub only what that folder lacks, and makes it docker.io's mirror for
// pulls by digest in a registries.conf of the binary's own, which InsecureRegistry and MirroredRegistry keep.
// So a suite whose builds pull busybox, pinned by digest, asks Docker Hub for it until the folder holds it rather than
// once per test binary, private store and fresh cache, which exhausted Docker Hub's pull allowance.
// containers/image asks Docker Hub itself for tags, and for what the mirror can't give, so tests see Docker Hub's
// answers, but for "(Mirrors also failed: ...)" before an error.
// The file is the one that CONTAINERS_REGISTRIES_CONF_OVERRIDE names, which containers/image reads after the
// environment's registries.conf and its drop-in files, keeping their settings, and replacing their docker.io entry,
// so there is no mirror where the environment configures docker.io itself, as CI with a mirror of its own may, or
// names an override file of its own.
// It returns a function that stops the mirror and removes its registries.conf and the variable that names it, for
// the end of the test binary.
func mirrorDockerHub() (stop func(), err error) {
	if conf := os.Getenv(overrideEnv); conf != "" {
		return nil, fmt.Errorf("%s names %s already", overrideEnv, conf)
	}
	if err := checkDockerHubUnconfigured(); err != nil {
		return nil, err
	}
	dir := os.Getenv(HubCacheEnv)
	if dir == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(cache, "vamos-test", "docker.io")
	}
	mirror, err := newHubMirror(dir, "index.docker.io")
	if err != nil {
		return nil, err
	}
	// Listened on here, rather than by httptest, which panics if it can't, so that the tests run without a mirror.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	server := &http.Server{Handler: mirror, ReadHeaderTimeout: time.Minute}
	go func() { _ = server.Serve(listener) }() // which ends when the server is closed
	confDir, err := os.MkdirTemp("", "vamos-test-registries-")
	if err != nil {
		server.Close()
		return nil, err
	}
	conf := filepath.Join(confDir, "registries.conf")
	text, err := registriesConfWith(nil, map[string]any{"location": "docker.io", "mirror": []map[string]any{{
		"location": listener.Addr().String(), "insecure": true, "pull-from-mirror": "digest-only",
	}}})
	if err == nil {
		err = os.WriteFile(conf, text, 0o644)
	}
	if err == nil {
		err = os.Setenv(overrideEnv, conf)
	}
	if err == nil { // read as the tests will, the environment's settings and all
		sysregistriesv2.InvalidateCache()
		_, err = sysregistriesv2.GetRegistries(nil)
	}
	if err != nil {
		_ = os.Unsetenv(overrideEnv)
		sysregistriesv2.InvalidateCache()
		server.Close()
		os.RemoveAll(confDir)
		return nil, err
	}
	return func() {
		server.Close()
		// No longer named, so that nothing that reads registry settings after finds the file gone.
		_ = os.Unsetenv(overrideEnv)
		sysregistriesv2.InvalidateCache()
		os.RemoveAll(confDir)
	}, nil
}

// checkDockerHubUnconfigured returns an error if the environment's registries.conf says anything of docker.io:
// an entry whose prefix or location is in it, or one, such as a wildcard's, that a Docker Hub image would use.
func checkDockerHubUnconfigured() error {
	in := func(s string) bool { return s == "docker.io" || strings.HasPrefix(s, "docker.io/") }
	registries, err := sysregistriesv2.GetRegistries(nil)
	if err != nil {
		return err
	}
	for _, reg := range registries {
		if in(reg.Prefix) || in(reg.Location) {
			return fmt.Errorf("%s configures %s, at %s, already", sysregistriesv2.ConfigurationSourceDescription(nil),
				reg.Prefix, reg.Location)
		}
	}
	if reg, err := sysregistriesv2.FindRegistry(nil, "docker.io/library/busybox"); err != nil {
		return err
	} else if reg != nil {
		return fmt.Errorf("%s configures docker.io already, as %s", sysregistriesv2.ConfigurationSourceDescription(nil),
			reg.Prefix)
	}
	return nil
}

// hubMirror serves a registry's manifests and blobs by digest from dir, under manifests/ and blobs/, each in a file
// named by its digest, a manifest's media type beside it in one with ".type" added, with a lock file for each digest
// fetched in locks/. What dir lacks it fetches from the registry at upstream, checks against its digest, and keeps,
// renamed into place whole, so that test binaries that run at once can share dir. A digest it holds is served under
// any repository's name, so it shows that the content exists, not that the repository has it. It fetches with
// go-containerregistry, which reads no registries.conf, so that the mirror is never its own upstream, and
// anonymously, so that the mirror, which any local user can reach, gives none of them what only the test runner's
// credentials may read; filling dir takes a request for each manifest.
type hubMirror struct {
	dir      string
	upstream string
	options  []name.Option // for references to upstream
}

// newHubMirror returns a mirror of upstream, a registry host, that keeps what it fetches in dir.
func newHubMirror(dir, upstream string, options ...name.Option) (*hubMirror, error) {
	for _, kind := range []string{"manifests", "blobs", "locks"} {
		if err := os.MkdirAll(filepath.Join(dir, kind), 0o755); err != nil {
			return nil, err
		}
	}
	return &hubMirror{dir: dir, upstream: upstream, options: options}, nil
}

// ServeHTTP answers the registry API's version check, and GET and HEAD of /v2/<name>/manifests/<digest> and
// /v2/<name>/blobs/<digest>; anything else, a tag included, isn't found, so that containers/image asks Docker Hub.
func (h *hubMirror) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "the mirror is read-only", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path == "/v2/" {
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/v2/")
	parts := strings.Split(rest, "/")
	if !ok || len(parts) < 3 {
		registryError(w, "NAME_UNKNOWN", "the mirror serves only manifests and blobs by digest")
		return
	}
	repo, kind := strings.Join(parts[:len(parts)-2], "/"), parts[len(parts)-2]
	unknown := map[string]string{"manifests": "MANIFEST_UNKNOWN", "blobs": "BLOB_UNKNOWN"}[kind]
	d, err := digest.Parse(parts[len(parts)-1])
	if unknown == "" || err != nil {
		registryError(w, cmp.Or(unknown, "NAME_UNKNOWN"), "the mirror serves only manifests and blobs by digest")
		return
	}
	path := filepath.Join(h.dir, kind, d.Algorithm().String()+"-"+d.Encoded())
	f, mediaType, err := h.open(path, kind)
	if err != nil { // missing, or unreadable, as a lost media type leaves a manifest, which fetching makes good
		if f, mediaType, err = h.fill(r, repo, kind, d, path); err != nil {
			if r.Context().Err() != nil { // the client gave up, and asks no one else
				return
			}
			// Unknown, as the registry API says, so that containers/image asks the registry itself, for a blob too,
			// and reports its answer, or else refused for too many requests, as containers/image falls back on too, so
			// that a pull doesn't take the mirror's refusal for one that may not happen again; said here too, since a
			// test that passes that way spends what the mirror saves. Not found either way, so that containers/image
			// doesn't wait to ask the mirror again.
			msg := fmt.Sprintf("fetching %s@%s from %s: %v", repo, d, h.upstream, err)
			fmt.Fprintf(os.Stderr, "testenv: the registry mirror is passing a request to its registry: %s\n", msg)
			var terr *transport.Error
			if errors.As(err, &terr) && terr.StatusCode == http.StatusTooManyRequests {
				registryError(w, "TOOMANYREQUESTS", msg)
			} else {
				registryError(w, unknown, msg)
			}
			return
		}
	}
	defer f.Close()
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Docker-Content-Digest", d.String())
	http.ServeContent(w, r, "", time.Time{}, f)
}

// fill fetches the manifest or blob, as kind says, of the repository repo with digest d into the cache, at path, and
// opens it, as open does. One test binary fetches it while others that run at once wait under a lock for its digest,
// and then find it, rather than each fetching it from Docker Hub; the lock is released before the content is sent,
// and one that can't be taken leaves the mirror to fetch without it.
func (h *hubMirror) fill(r *http.Request, repo, kind string, d digest.Digest, path string) (*os.File, string, error) {
	unlock, err := lockFile(r.Context(), filepath.Join(h.dir, "locks", d.Algorithm().String()+"-"+d.Encoded()))
	if err == nil {
		defer unlock()
	} else if r.Context().Err() != nil {
		return nil, "", err
	}
	if f, mediaType, err := h.open(path, kind); err == nil { // which another fetched meanwhile
		return f, mediaType, nil
	}
	if err := h.fetch(r, repo, kind, d, path); err != nil {
		return nil, "", err
	}
	f, mediaType, err := h.open(path, kind)
	if err != nil {
		return nil, "", fmt.Errorf("reading what was fetched: %w", err)
	}
	return f, mediaType, nil
}

// lockFile takes an exclusive lock on the file at path, made if need be, waiting for any other's until ctx ends, and
// returns a function that releases it.
func lockFile(ctx context.Context, path string) (unlock func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { f.Close() }, nil // which releases the lock
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// open opens the manifest or blob, as kind says, at path, and returns it and its media type.
func (h *hubMirror) open(path, kind string) (*os.File, string, error) {
	mediaType := []byte("application/octet-stream")
	if kind == "manifests" {
		var err error
		if mediaType, err = os.ReadFile(path + ".type"); err != nil { // which is written first, but may be lost
			return nil, "", err
		}
	}
	f, err := os.Open(path)
	return f, string(mediaType), err
}

// fetch copies the manifest or blob, as kind says, of the repository repo with digest d from upstream to path.
func (h *hubMirror) fetch(r *http.Request, repo, kind string, d digest.Digest, path string) error {
	ref, err := name.NewDigest(h.upstream+"/"+repo+"@"+d.String(), h.options...)
	if err != nil {
		return err
	}
	// Not after a refusal for too many requests, which containers/image meets again, and waits on, when the mirror
	// passes the request to Docker Hub.
	options := []remote.Option{remote.WithContext(r.Context()), remote.WithRetryStatusCodes(
		http.StatusRequestTimeout, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable,
		http.StatusGatewayTimeout, 499, 522, // go-containerregistry's own, but for 429
	)}
	if kind == "manifests" {
		desc, err := remote.Get(ref, options...)
		if err != nil {
			return err
		}
		// The type first, so that a manifest found always has one.
		if err := writeWhole(path+".type", strings.NewReader(string(desc.MediaType)), ""); err != nil {
			return err
		}
		return writeWhole(path, bytes.NewReader(desc.Manifest), d)
	}
	layer, err := remote.Layer(ref, options...)
	if err != nil {
		return err
	}
	blob, err := layer.Compressed()
	if err != nil {
		return err
	}
	defer blob.Close()
	return writeWhole(path, blob, d)
}

// registryError answers with 404 Not Found and the registry API's error code, which containers/image needs to fall
// back from a mirror that lacks a blob to the registry itself, and classifies its error by.
func registryError(w http.ResponseWriter, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"code": code, "message": message}}})
}

// writeWhole writes what rd gives to path, through a temporary file renamed into place,
// checking first that it has digest d, unless d is "".
func writeWhole(path string, rd io.Reader, d digest.Digest) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".fetching-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // which fails once it is renamed
	var verifier digest.Verifier
	if d != "" {
		verifier = d.Verifier()
		rd = io.TeeReader(rd, verifier)
	}
	_, err = io.Copy(f, rd)
	// As a file written plainly would be, rather than CreateTemp's 0600.
	if err := errors.Join(err, f.Chmod(0o644), f.Close()); err != nil {
		return err
	}
	if verifier != nil && !verifier.Verified() {
		return fmt.Errorf("the registry gave content whose digest isn't %s", d)
	}
	return os.Rename(f.Name(), path)
}
