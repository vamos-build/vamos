// Package cachetest tests implementations of cache.ContentStore and cache.RecordStore:
// Content and Records check the stores' contracts on any implementation, local or remote.
// It is for tests alone.
package cachetest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vamos-build/vamos/internal/cache"

	"github.com/opencontainers/go-digest"
	"github.com/spf13/afero"
)

// Fixture makes the stores that the suite tests.
type Fixture struct {
	// New returns fresh stores, which t's cleanup closes.
	New func(t *testing.T) Stores
}

// Stores is one implementation of the store interfaces, under test.
type Stores struct {
	Content cache.ContentStore
	Records cache.RecordStore // nil if the implementation has no record store; Records then skips
	// Unclearable says that the content store refuses ClearContent, as a remote one does, which a prune clears.
	Unclearable bool
	// Corrupt makes the content store hold data under b's digest, whatever data is, however the implementation keeps
	// blobs, so that the suite can corrupt a blob as a bad disk might.
	Corrupt func(t *testing.T, b cache.Blob, data []byte)
}

// must fails the test at once if err isn't nil.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// Content checks the contract of a content store: blobs are found by digest, verified as they are read, put only
// within limits, imported only as advertised, dropped once found corrupt so that a put repairs them,
// and refused when malformed; and that trees are read back as stored and refused when invalid.
func Content(t *testing.T, f Fixture) {
	t.Run("blobs", func(t *testing.T) { testContentStore(t, f.New(t)) })
	t.Run("import", func(t *testing.T) { testImport(t, f.New(t)) })
	t.Run("corrupt", func(t *testing.T) { testCorrupt(t, f.New(t)) })
	t.Run("put file repairs", func(t *testing.T) { testPutFileRepairs(t, f.New(t)) })
	t.Run("malformed digests", func(t *testing.T) { testMalformedDigests(t, f.New(t)) })
	t.Run("trees", func(t *testing.T) { testReadTree(t, f.New(t)) })
	t.Run("checkout corrupt file", func(t *testing.T) { testCheckoutCorruptFile(t, f.New(t)) })
	t.Run("stat", func(t *testing.T) { testStat(t, f.New(t)) })
}

// Records checks the contract of a record store: the latest result for a key is kept, a key with none has no result,
// malformed records are refused, tag records are kept until replaced, and clearing results leaves tags;
// and that target history keeps what its adds say, within its cap, whatever adds run at once,
// survives clearing results, and is pruned by when it was last used.
func Records(t *testing.T, f Fixture) {
	if f.New(t).Records == nil {
		t.Skip("the implementation has no record store")
	}
	t.Run("results and tags", func(t *testing.T) { testRecordStore(t, f.New(t)) })
	t.Run("history", func(t *testing.T) { testHistory(t, f.New(t)) })
	t.Run("concurrent history adds", func(t *testing.T) { testConcurrentHistory(t, f.New(t)) })
	t.Run("history cap", func(t *testing.T) { testHistoryCap(t, f.New(t)) })
	t.Run("history prune", func(t *testing.T) { testPruneHistory(t, f.New(t)) })
}

// testContentStore checks that blobs are found by digest, verified as they are read, and put only within limits.
func testContentStore(t *testing.T, s Stores) {
	ctx := t.Context()
	data := []byte("some content")
	b, err := s.Content.PutBytes(ctx, data)
	must(t, err)
	if want := (cache.Blob{Digest: digest.FromBytes(data), Size: int64(len(data))}); b != want {
		t.Errorf("PutBytes = %+v, want %+v", b, want)
	}
	if err := s.Content.Has(ctx, b); err != nil {
		t.Errorf("Has = %v, want it held", err)
	}
	got, err := s.Content.Read(ctx, b, int64(len(data)))
	if err != nil || !bytes.Equal(got, data) {
		t.Errorf("Read = %q, %v; want %q", got, err, data)
	}
	if _, err := s.Content.Read(ctx, b, int64(len(data))-1); !errors.Is(err, cache.ErrOverLimit) {
		t.Errorf("Read with too small a limit = %v, want %v", err, cache.ErrOverLimit)
	}
	other := cache.Blob{Digest: digest.FromString("other"), Size: 5}
	if err := s.Content.Has(ctx, other); !errors.Is(err, cache.ErrMissing) {
		t.Errorf("Has of a blob never put = %v, want %v", err, cache.ErrMissing)
	}
	if _, err := s.Content.Get(ctx, other); !errors.Is(err, cache.ErrMissing) {
		t.Errorf("Get of a blob never put = %v, want %v", err, cache.ErrMissing)
	}
	missing, err := s.Content.Missing(ctx, []cache.Blob{b, other})
	if err != nil || len(missing) != 1 || missing[0] != other {
		t.Errorf("Missing = %+v, %v; want the blob never put", missing, err)
	}
	if missing, err := s.Content.Missing(ctx, nil); err != nil || len(missing) != 0 {
		t.Errorf("Missing of nothing = %+v, %v; want nothing", missing, err)
	}
	if _, err := s.Content.Put(ctx, bytes.NewReader(data), int64(len(data))-1); !errors.Is(err, cache.ErrOverLimit) {
		t.Errorf("Put over the limit = %v, want %v", err, cache.ErrOverLimit)
	}
	contents, err := s.Content.ReadMany(ctx, []cache.Blob{b, b}, int64(len(data)))
	if err != nil || len(contents) != 2 || !bytes.Equal(contents[1], data) {
		t.Errorf("ReadMany = %q, %v; want the content twice", contents, err)
	}
	if _, err := s.Content.ReadMany(ctx, []cache.Blob{b, other}, int64(len(data))); !errors.Is(err, cache.ErrMissing) {
		t.Errorf("ReadMany with a blob never put = %v, want %v", err, cache.ErrMissing)
	}
	if _, err := s.Content.ReadMany(ctx, []cache.Blob{b}, int64(len(data))-1); !errors.Is(err, cache.ErrOverLimit) {
		t.Errorf("ReadMany with too small a limit = %v, want %v", err, cache.ErrOverLimit)
	}
	empty, err := s.Content.PutBytes(ctx, nil)
	if err != nil || empty.Size != 0 {
		t.Errorf("PutBytes of nothing = %+v, %v; want the empty blob", empty, err)
	}
	if got, err := s.Content.Read(ctx, empty, 0); err != nil || len(got) != 0 {
		t.Errorf("Read of the empty blob = %q, %v; want nothing", got, err)
	}
	if s.Unclearable {
		if _, err := s.Content.ClearContent(ctx); err == nil {
			t.Error("ClearContent of a store that refuses it succeeded")
		}
		if err := s.Content.Has(ctx, b); err != nil {
			t.Errorf("Has after a refused clear = %v, want it held", err)
		}
		return
	}
	if n, err := s.Content.ClearContent(ctx); err != nil || n != 2 {
		t.Errorf("ClearContent = %d, %v; want 2 blobs removed", n, err)
	}
	if err := s.Content.Has(ctx, b); !errors.Is(err, cache.ErrMissing) {
		t.Errorf("Has after clearing = %v, want %v", err, cache.ErrMissing)
	}
	if _, err := s.Content.PutBytes(ctx, data); err != nil {
		t.Errorf("PutBytes after clearing: %v", err)
	}
}

// testImport checks that imported bytes become visible only if they are what was advertised.
func testImport(t *testing.T, s Stores) {
	ctx := t.Context()
	data := []byte("imported")
	b := cache.Blob{Digest: digest.FromBytes(data), Size: int64(len(data))}
	for _, tc := range []struct {
		name string
		b    cache.Blob
		data []byte
	}{
		{name: "wrong content", b: b, data: []byte("imposter")},
		{name: "truncated", b: b, data: data[:4]},
		{name: "too long", b: b, data: append(append([]byte{}, data...), '!')},
		{name: "wrong size", b: cache.Blob{Digest: b.Digest, Size: b.Size + 1}, data: data},
	} {
		if err := s.Content.Import(ctx, tc.b, bytes.NewReader(tc.data)); !errors.Is(err, cache.ErrCorrupt) {
			t.Errorf("Import of %s = %v, want %v", tc.name, err, cache.ErrCorrupt)
		}
		if err := s.Content.Has(ctx, tc.b); !errors.Is(err, cache.ErrMissing) {
			t.Errorf("after importing %s, Has = %v, want %v", tc.name, err, cache.ErrMissing)
		}
	}
	must(t, s.Content.Import(ctx, b, bytes.NewReader(data)))
	if got, err := s.Content.Read(ctx, b, b.Size); err != nil || !bytes.Equal(got, data) {
		t.Errorf("Read after importing = %q, %v", got, err)
	}
}

// testCorrupt checks that content that doesn't match its digest is reported as corrupt when read,
// however it differs, and told apart from content that is missing;
// that a blob found corrupt is dropped, so that putting its content again repairs it, though its size was right,
// as cache.PutFile does for sources captured again; and that a reader that finds corrupt a blob repaired since
// leaves the repair.
func testCorrupt(t *testing.T, s Stores) {
	ctx := t.Context()
	data := []byte("the real content")
	b, err := s.Content.PutBytes(ctx, data)
	must(t, err)
	for _, tc := range []struct {
		name      string
		data      []byte
		hasDetect bool // Has can tell by the size alone
	}{
		{name: "same size", data: []byte("the fake content"), hasDetect: false},
		{name: "truncated", data: data[:5], hasDetect: true},
		{name: "longer", data: append(append([]byte{}, data...), "..."...), hasDetect: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s.Corrupt(t, b, tc.data)
			if err := s.Content.Has(ctx, b); tc.hasDetect && !errors.Is(err, cache.ErrCorrupt) {
				t.Errorf("Has = %v, want %v, since the size differs", err, cache.ErrCorrupt)
			} else if !tc.hasDetect && err != nil {
				t.Errorf("Has = %v, want nil, since the size is right", err)
			}
			if tc.hasDetect {
				if missing, err := s.Content.Missing(ctx, []cache.Blob{b}); err != nil || len(missing) != 1 {
					t.Errorf("Missing = %v, %v; want the corrupt blob", missing, err)
				}
			}
			if _, err := s.Content.Read(ctx, b, b.Size); !errors.Is(err, cache.ErrCorrupt) {
				t.Errorf("Read = %v, want %v", err, cache.ErrCorrupt)
			} else {
				var ce *cache.CorruptError
				if !errors.As(err, &ce) || ce.Blob != b {
					t.Errorf("Read's error %v doesn't name the blob", err)
				}
			}
			if errors.Is(err, cache.ErrMissing) {
				t.Errorf("Read reports corrupt content as missing: %v", err)
			}
			// Found corrupt, it is dropped, so that a client that has its content puts it again.
			if err := s.Content.Has(ctx, b); !errors.Is(err, cache.ErrMissing) {
				t.Errorf("Has after a read found it corrupt = %v, want %v", err, cache.ErrMissing)
			}
			if missing, err := s.Content.Missing(ctx, []cache.Blob{b}); err != nil || len(missing) != 1 {
				t.Errorf("Missing after a read found it corrupt = %v, %v; want the blob", missing, err)
			}

			s.Corrupt(t, b, tc.data)
			r, err := s.Content.Get(ctx, b)
			must(t, err)
			_, err = io.Copy(io.Discard, r)
			r.Close()
			if !errors.Is(err, cache.ErrCorrupt) {
				t.Errorf("reading Get to its end = %v, want %v", err, cache.ErrCorrupt)
			}

			// A reader that opened the corrupt blob before it was repaired leaves the repair.
			s.Corrupt(t, b, tc.data)
			r, err = s.Content.Get(ctx, b)
			must(t, err)
			if _, err := s.Content.PutBytes(ctx, data); err != nil {
				t.Fatal(err)
			}
			_, err = io.Copy(io.Discard, r)
			r.Close()
			if !errors.Is(err, cache.ErrCorrupt) {
				t.Errorf("reading to its end a Get opened before the repair = %v, want %v", err, cache.ErrCorrupt)
			}
			if got, err := s.Content.Read(ctx, b, b.Size); err != nil || !bytes.Equal(got, data) {
				t.Errorf("Read after putting again = %q, %v", got, err)
			}
		})
	}
}

// testPutFileRepairs checks that capturing a file again repairs its blob once a read has found it corrupt,
// though the corrupt content has the right size, so that Has alone can't tell.
func testPutFileRepairs(t *testing.T, s Stores) {
	ctx := t.Context()
	fsys := afero.NewMemMapFs()
	data := []byte("the real content")
	must(t, afero.WriteFile(fsys, "/src.txt", data, 0o644))
	b, _, err := cache.PutFile(ctx, s.Content, fsys, "/src.txt", int64(len(data)))
	must(t, err)
	s.Corrupt(t, b, []byte("the fake content"))
	if _, err := s.Content.Read(ctx, b, b.Size); !errors.Is(err, cache.ErrCorrupt) {
		t.Fatalf("Read = %v, want %v", err, cache.ErrCorrupt)
	}
	if _, stored, err := cache.PutFile(ctx, s.Content, fsys, "/src.txt", int64(len(data))); err != nil || !stored {
		t.Errorf("PutFile again = stored %v, %v; want the content stored again", stored, err)
	}
	if got, err := s.Content.Read(ctx, b, b.Size); err != nil || !bytes.Equal(got, data) {
		t.Errorf("Read after capturing again = %q, %v; want the real content", got, err)
	}
}

// testMalformedDigests checks that a digest that isn't one is refused by every method, before it could name a path.
func testMalformedDigests(t *testing.T, s Stores) {
	ctx := t.Context()
	for _, d := range []digest.Digest{"", "sha256:../../etc/passwd", "sha256:abc",
		digest.Digest("md5:" + strings.Repeat("0", 32)), digest.Digest("sha512:" + strings.Repeat("0", 128)),
		digest.Digest(strings.Repeat("0", 64))} {
		b := cache.Blob{Digest: d, Size: 1}
		if err := s.Content.Has(ctx, b); err == nil || errors.Is(err, cache.ErrMissing) {
			t.Errorf("Has(%q) = %v, want it refused as malformed", d, err)
		}
		if _, err := s.Content.Get(ctx, b); err == nil || errors.Is(err, cache.ErrMissing) {
			t.Errorf("Get(%q) = %v, want it refused as malformed", d, err)
		}
		if _, err := s.Content.Read(ctx, b, 1); err == nil || errors.Is(err, cache.ErrMissing) {
			t.Errorf("Read(%q) = %v, want it refused as malformed", d, err)
		}
		if err := s.Content.Import(ctx, b, strings.NewReader("x")); err == nil {
			t.Errorf("Import(%q) succeeded, want it refused as malformed", d)
		}
		if _, err := s.Content.Missing(ctx, []cache.Blob{b}); err == nil {
			t.Errorf("Missing(%q) succeeded, want it refused as malformed", d)
		}
	}
	negative := cache.Blob{Digest: digest.FromString("x"), Size: -1}
	if err := s.Content.Has(ctx, negative); err == nil || errors.Is(err, cache.ErrMissing) {
		t.Errorf("Has of a blob with a negative size = %v, want it refused", err)
	}
}

// testReadTree checks that a tree is read back as stored, and that one that isn't a valid tree,
// or is too large, is refused as corrupt.
func testReadTree(t *testing.T, s Stores) {
	ctx := t.Context()
	a, err := s.Content.PutBytes(ctx, []byte("a"))
	must(t, err)
	good := &cache.Tree{Entries: []cache.TreeEntry{
		{Path: "dir", Type: cache.EntryDir, Mode: 0o755},
		{Path: "dir/a.txt", Type: cache.EntryFile, Mode: 0o644, Blob: &a},
		{Path: "link", Type: cache.EntryLink, Target: "dir/a.txt"},
	}}
	b, err := s.Content.PutBytes(ctx, good.Encode())
	must(t, err)
	got, err := cache.ReadTree(ctx, s.Content, b)
	if err != nil || len(got.Entries) != 3 {
		t.Errorf("ReadTree = %+v, %v; want the tree", got, err)
	}
	must(t, cache.CheckTree(ctx, s.Content, b))

	for _, tc := range []struct {
		name    string
		entries []cache.TreeEntry
		want    string
	}{
		{
			name:    "parent path",
			entries: []cache.TreeEntry{{Path: "../escaped", Type: cache.EntryLink, Target: "x"}},
			want:    `"../escaped" isn't a clean path`,
		},
		{
			name:    "absolute path",
			entries: []cache.TreeEntry{{Path: "/escaped", Type: cache.EntryLink, Target: "x"}},
			want:    `"/escaped" isn't a clean path`,
		},
		{
			name: "beneath a link",
			entries: []cache.TreeEntry{
				{Path: "out", Type: cache.EntryLink, Target: "/tmp"},
				{Path: "out/escaped", Type: cache.EntryLink, Target: "x"},
			},
			want: `"out/escaped" is beneath the symbolic link "out"`,
		},
		{
			name: "beneath a link named by an unclean path",
			entries: []cache.TreeEntry{
				{Path: "a/../out", Type: cache.EntryLink, Target: "/tmp"},
				{Path: "out/escaped", Type: cache.EntryLink, Target: "x"},
			},
			want: `"a/../out" isn't a clean path`,
		},
		{
			name:    "file without content",
			entries: []cache.TreeEntry{{Path: "empty", Type: cache.EntryFile, Mode: 0o644}},
			want:    `file "empty" has no content`,
		},
		{
			name:    "unknown type",
			entries: []cache.TreeEntry{{Path: "dev", Type: "device", Mode: 0o644}},
			want:    `"dev" has unknown type "device"`,
		},
		{
			name:    "duplicate path",
			entries: []cache.TreeEntry{{Path: "a", Type: cache.EntryDir}, {Path: "a", Type: cache.EntryDir}},
			want:    `"a" isn't after "a"`,
		},
		{
			name:    "unsorted",
			entries: []cache.TreeEntry{{Path: "b", Type: cache.EntryDir}, {Path: "a", Type: cache.EntryDir}},
			want:    `"a" isn't after "b"`,
		},
		{
			name:    "mode beyond chmod bits",
			entries: []cache.TreeEntry{{Path: "a", Type: cache.EntryDir, Mode: 0o10000}},
			want:    `"a" has mode 10000`,
		},
		{
			name:    "link with content",
			entries: []cache.TreeEntry{{Path: "l", Type: cache.EntryLink, Target: "x", Blob: &a}},
			want:    `link "l" has a mode or content`,
		},
		{
			name: "file with malformed digest",
			entries: []cache.TreeEntry{{Path: "f", Type: cache.EntryFile,
				Blob: &cache.Blob{Digest: "sha256:../x", Size: 1}}},
			want: `file "f": blob "sha256:../x"`,
		},
		{
			name:    "empty path",
			entries: []cache.TreeEntry{{Path: "", Type: cache.EntryDir}},
			want:    `entry 0 has no path`,
		},
		{
			name: "beneath a file",
			entries: []cache.TreeEntry{
				{Path: "f", Type: cache.EntryFile, Mode: 0o644, Blob: &a},
				{Path: "f/x", Type: cache.EntryFile, Mode: 0o644, Blob: &a},
			},
			want: `"f/x" is beneath the file "f"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := s.Content.PutBytes(ctx, (&cache.Tree{Entries: tc.entries}).Encode())
			must(t, err)
			_, err = cache.ReadTree(ctx, s.Content, b)
			if !errors.Is(err, cache.ErrCorrupt) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("ReadTree = %v, want %v containing %q", err, cache.ErrCorrupt, tc.want)
			}
			if _, err := cache.Checkout(ctx, s.Content, b, t.TempDir()); !errors.Is(err, cache.ErrCorrupt) {
				t.Errorf("Checkout = %v, want %v", err, cache.ErrCorrupt)
			}
		})
	}
	t.Run("not JSON", func(t *testing.T) {
		b, err := s.Content.PutBytes(ctx, []byte("{"))
		must(t, err)
		if _, err := cache.ReadTree(ctx, s.Content, b); !errors.Is(err, cache.ErrCorrupt) {
			t.Errorf("ReadTree = %v, want %v", err, cache.ErrCorrupt)
		}
	})
	t.Run("too large", func(t *testing.T) {
		huge := cache.Blob{Digest: digest.FromString("huge"), Size: cache.MaxTreeSize + 1}
		if _, err := cache.ReadTree(ctx, s.Content, huge); !errors.Is(err, cache.ErrOverLimit) {
			t.Errorf("ReadTree = %v, want %v", err, cache.ErrOverLimit)
		}
	})
	t.Run("missing file", func(t *testing.T) {
		gone := cache.Blob{Digest: digest.FromString("gone"), Size: 4}
		b, err := s.Content.PutBytes(ctx, (&cache.Tree{Entries: []cache.TreeEntry{
			{Path: "gone", Type: cache.EntryFile, Mode: 0o644, Blob: &gone},
		}}).Encode())
		must(t, err)
		if err := cache.CheckTree(ctx, s.Content, b); !errors.Is(err, cache.ErrMissing) {
			t.Errorf("CheckTree = %v, want %v", err, cache.ErrMissing)
		}
		if _, err := cache.Checkout(ctx, s.Content, b, t.TempDir()); !errors.Is(err, cache.ErrMissing) {
			t.Errorf("Checkout = %v, want %v", err, cache.ErrMissing)
		}
	})
}

// testCheckoutCorruptFile checks that a checkout fails as corrupt when a file's content doesn't match its blob.
func testCheckoutCorruptFile(t *testing.T, s Stores) {
	ctx := t.Context()
	a, err := s.Content.PutBytes(ctx, []byte("real"))
	must(t, err)
	b, err := s.Content.PutBytes(ctx, (&cache.Tree{Entries: []cache.TreeEntry{
		{Path: "a.txt", Type: cache.EntryFile, Mode: 0o644, Blob: &a},
	}}).Encode())
	must(t, err)
	s.Corrupt(t, a, []byte("fake"))
	must(t, cache.CheckTree(ctx, s.Content, b)) // the size is right, so only reading tells
	dir := t.TempDir()
	_, err = cache.Checkout(ctx, s.Content, b, dir)
	var ce *cache.CorruptError
	if !errors.As(err, &ce) || ce.Blob != a {
		t.Errorf("Checkout = %v, want %v naming %s", err, cache.ErrCorrupt, a.Digest)
	}
}

// testRecordStore checks the contract of a record store.
func testRecordStore(t *testing.T, s Stores) {
	ctx := t.Context()
	key := digest.FromString("an action")
	if r, err := s.Records.Result(ctx, key); err != nil || r != nil {
		t.Fatalf("result before any = %v, %v; want none", r, err)
	}
	must(t, s.Records.SetResult(ctx, key, &cache.ActionResult{Epoch: 1}))
	must(t, s.Records.SetResult(ctx, key, &cache.ActionResult{Epoch: 2}))
	if r, err := s.Records.Result(ctx, key); err != nil || r == nil || r.Epoch != 2 {
		t.Errorf("result = %+v, %v; want the latest, with epoch 2", r, err)
	}
	bad := &cache.ActionResult{Image: "sha256:../../escape", Epoch: 3}
	if err := s.Records.SetResult(ctx, key, bad); err == nil {
		t.Error("SetResult accepted a result naming a malformed image digest")
	}
	if _, err := s.Records.Result(ctx, "not a digest"); err == nil {
		t.Error("Result accepted a malformed key")
	}
	if err := s.Records.SetResult(ctx, digest.Digest("sha512:"+strings.Repeat("0", 128)),
		&cache.ActionResult{Epoch: 1}); err == nil {
		t.Error("SetResult accepted a key that isn't a sha256 digest")
	}

	if r, err := s.Records.Tag(ctx, "example.com/app:1"); err != nil || r != nil {
		t.Fatalf("tag before any = %v, %v; want none", r, err)
	}
	record := &cache.TagRecord{Digest: digest.FromString("index"), MIMEType: "application/json", Manifest: []byte("{}"),
		Instances: map[string]cache.TagInstance{"linux/arm64": {
			Digest:   digest.FromString("manifest"),
			Image:    digest.FromString("image"),
			Platform: "linux/arm64/v8",
			Epoch:    5,
			Locations: []cache.Location{{
				Kind:      cache.LocationRegistry,
				Reference: "example.com/app@" + digest.FromString("m").String(),
			}},
		}}}
	must(t, s.Records.SetTag(ctx, "example.com/app:1", record))
	if err := s.Records.SetTag(ctx, "example.com/app:1", &cache.TagRecord{Digest: "nope"}); err == nil {
		t.Error("SetTag accepted a record with a malformed digest")
	}
	bad = &cache.ActionResult{
		Image:     digest.FromString("i"),
		Locations: []cache.Location{{Kind: "cloud", Reference: "x"}},
	}
	if err := s.Records.SetResult(ctx, key, bad); err == nil {
		t.Error("SetResult accepted a result with a location of an unknown kind")
	}
	for _, instance := range []cache.TagInstance{
		{Digest: "nope"},
		{Digest: digest.FromString("m"), Image: "nope"},
		{Digest: digest.FromString("m"), Locations: []cache.Location{{Kind: cache.LocationStore}}},
	} {
		malformed := &cache.TagRecord{
			Digest:    digest.FromString("index"),
			Instances: map[string]cache.TagInstance{"linux/amd64": instance},
		}
		if err := s.Records.SetTag(ctx, "example.com/app:1", malformed); err == nil {
			t.Errorf("SetTag accepted a record with the malformed instance %+v", instance)
		}
	}
	if g, err := s.Records.Generation(ctx); err != nil || g != 0 {
		t.Errorf("generation before any clearing = %d, %v; want 0", g, err)
	}
	if n, err := s.Records.ClearResults(ctx); err != nil || n != 1 {
		t.Errorf("ClearResults = %d, %v; want 1 result removed", n, err)
	}
	if g, err := s.Records.Generation(ctx); err != nil || g != 1 {
		t.Errorf("generation after clearing = %d, %v; want 1", g, err)
	}
	if r, err := s.Records.Result(ctx, key); err != nil || r != nil {
		t.Errorf("result after clearing = %v, %v; want none", r, err)
	}
	r, err := s.Records.Tag(ctx, "example.com/app:1")
	if err != nil || r == nil || r.Digest != record.Digest || string(r.Manifest) != string(record.Manifest) ||
		len(r.Instances) != 1 || r.Instances["linux/arm64"].Image != record.Instances["linux/arm64"].Image ||
		len(r.Instances["linux/arm64"].Locations) != 1 {
		t.Errorf("tag after clearing = %+v, %v; want the record kept, %+v", r, err, record)
	}
	must(t, s.Records.SetResult(ctx, key, &cache.ActionResult{Epoch: 4}))
	if r, err := s.Records.Result(ctx, key); err != nil || r == nil || r.Epoch != 4 {
		t.Errorf("result set after clearing = %+v, %v; want epoch 4", r, err)
	}
}

// testStat checks that Stat gives a held blob's size by its digest, ErrMissing for one not held,
// and never drops a blob, even one that a read would find corrupt.
func testStat(t *testing.T, s Stores) {
	ctx := t.Context()
	data := []byte("some content")
	b, err := s.Content.PutBytes(ctx, data)
	must(t, err)
	if got, err := s.Content.Stat(ctx, b.Digest); err != nil || got != b {
		t.Errorf("Stat = %+v, %v; want %+v", got, err, b)
	}
	if _, err := s.Content.Stat(ctx, digest.FromString("never put")); !errors.Is(err, cache.ErrMissing) {
		t.Errorf("Stat of a blob never put = %v, want %v", err, cache.ErrMissing)
	}
	if _, err := s.Content.Stat(ctx, "sha256:../../escape"); err == nil {
		t.Error("Stat accepted a malformed digest")
	}
	if s.Corrupt == nil {
		return
	}
	s.Corrupt(t, b, []byte("other bytes"))
	if got, err := s.Content.Stat(ctx, b.Digest); err != nil || got.Size != int64(len("other bytes")) {
		t.Errorf("Stat of a corrupt blob = %+v, %v; want the size of what is held", got, err)
	}
	if got, err := s.Content.Stat(ctx, b.Digest); err != nil || got.Digest != b.Digest {
		t.Errorf("Stat again = %+v, %v; want the blob still held", got, err)
	}
}

// historyName returns the name of a history, as a digest of name.
func historyName(name string) digest.Digest {
	return digest.FromString("history " + name)
}

// historyEntry returns an entry of the key named key whose sources' trees are trees.
func historyEntry(key string, trees ...digest.Digest) cache.HistoryEntry {
	action := json.RawMessage(`{"key":"` + key + `"}`)
	e := cache.HistoryEntry{Key: digest.FromString(key), ActionVersion: 1, Action: action}
	for _, tree := range trees {
		e.Adds = append(e.Adds, cache.Supplier{Tree: tree})
	}
	return e
}

// historyFiles returns n files whose paths take about size bytes each.
func historyFiles(n, size int) []cache.HistoryFile {
	files := make([]cache.HistoryFile, n)
	for i := range files {
		files[i] = cache.HistoryFile{Path: fmt.Sprintf("%d/%s", i, strings.Repeat("p", size)), Type: "file"}
	}
	return files
}

// historyKeys returns which of keys h's entries have, in h's order, by the names historyEntry took.
func historyKeys(h *cache.History, keys ...string) []string {
	if h == nil {
		return nil
	}
	byKey := map[digest.Digest]string{}
	for _, k := range keys {
		byKey[digest.FromString(k)] = k
	}
	var out []string
	for _, e := range h.Entries {
		out = append(out, byKey[e.Key])
	}
	return out
}

// testHistory checks that a history keeps at most keep entries, most recent first and each key once,
// with each tree once, that Newest follows adds, and that clearing results leaves it.
func testHistory(t *testing.T, s Stores) {
	ctx := t.Context()
	name := historyName("app")
	if h, err := s.Records.History(ctx, name); err != nil || h != nil {
		t.Fatalf("history before any = %v, %v; want none", h, err)
	}
	if head, err := s.Records.Newest(ctx, name); err != nil || head != (cache.HistoryHead{}) {
		t.Fatalf("newest before any = %+v, %v; want none", head, err)
	}
	src := digest.FromString("src")
	trees := map[digest.Digest][]cache.HistoryFile{src: historyFiles(2, 1)}
	before := time.Now().Add(-time.Second)
	for _, key := range []string{"a", "b", "a", "c"} {
		must(t, s.Records.AddHistory(ctx, name, historyEntry(key, src), trees, 2))
		head, err := s.Records.Newest(ctx, name)
		if err != nil || head.Newest != historyEntry(key, src).Fingerprint() || head.Used.Before(before) {
			t.Errorf("newest after adding %s = %+v, %v; want its fingerprint, used now", key, head, err)
		}
	}
	h, err := s.Records.History(ctx, name)
	if err != nil || !slices.Equal(historyKeys(h, "a", "b", "c"), []string{"c", "a"}) {
		t.Fatalf("history = %v, %v; want [c a], keeping 2", historyKeys(h, "a", "b", "c"), err)
	}
	if len(h.Trees) != 1 || len(h.Trees[src]) != 2 {
		t.Errorf("trees %v; want src's alone, once", h.Trees)
	}
	if string(h.Entries[0].Action) != `{"key":"c"}` {
		t.Errorf("the newest entry's action is %s; want it as added", h.Entries[0].Action)
	}
	if err := s.Records.AddHistory(ctx, name, cache.HistoryEntry{Key: "nope"}, nil, 2); err == nil {
		t.Error("AddHistory accepted an entry with a malformed key")
	}
	for what, bad := range map[string]map[digest.Digest][]cache.HistoryFile{
		"a malformed tree digest": {"sha256:nope": historyFiles(1, 1)},
		"a file without a path":   {src: {{Type: "file"}}},
		"a malformed file digest": {src: {{Path: "a", Type: "file", Digest: "sha256:nope"}}},
	} {
		if err := s.Records.AddHistory(ctx, name, historyEntry("d", src), bad, 2); err == nil {
			t.Errorf("AddHistory accepted trees with %s", what)
		}
	}
	// An action nested as deeply as JSON allows alone can't be nested inside a record, which would then not read.
	deep := historyEntry("deep")
	deep.Action = json.RawMessage(strings.Repeat("[", 9_999) + strings.Repeat("]", 9_999))
	headBefore, err := s.Records.Newest(ctx, name)
	must(t, err)
	if err := s.Records.AddHistory(ctx, name, deep, nil, 2); err == nil {
		t.Error("AddHistory accepted an action too deeply nested for its record to read back")
	}
	future := historyEntry("future")
	future.Recorded = time.Date(10_000, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := s.Records.AddHistory(ctx, name, future, nil, 2); err == nil {
		t.Error("AddHistory accepted an entry recorded at a time that can't be encoded")
	}
	if head, err := s.Records.Newest(ctx, name); err != nil || head.Newest != headBefore.Newest ||
		!head.Used.Equal(headBefore.Used) {
		t.Errorf("newest after refused adds = %+v, %v; want it as it was, %+v", head, err, headBefore)
	}
	h, err = s.Records.History(ctx, name)
	if got := historyKeys(h, "a", "c"); err != nil || !slices.Equal(got, []string{"c", "a"}) {
		t.Errorf("history after refused adds = %v, %v; want it as it was, [c a]", got, err)
	}
	if _, err := s.Records.History(ctx, "not a digest"); err == nil {
		t.Error("History accepted a malformed name")
	}
	_, err = s.Records.ClearResults(ctx)
	must(t, err)
	if h, err := s.Records.History(ctx, name); err != nil || len(historyKeys(h, "a", "c")) != 2 {
		t.Errorf("history after clearing results = %v, %v; want it kept", historyKeys(h, "a", "c"), err)
	}
}

// testConcurrentHistory checks that adds to one history at once all survive.
func testConcurrentHistory(t *testing.T, s Stores) {
	ctx := t.Context()
	name := historyName("app")
	const adds = 16
	var wg sync.WaitGroup
	errs := make([]error, adds)
	for i := range adds {
		wg.Go(func() {
			errs[i] = s.Records.AddHistory(ctx, name, historyEntry(fmt.Sprint(i)), nil, adds)
		})
	}
	wg.Wait()
	must(t, errors.Join(errs...))
	h, err := s.Records.History(ctx, name)
	must(t, err)
	var keys []string
	for i := range adds {
		keys = append(keys, fmt.Sprint(i))
	}
	got := historyKeys(h, keys...)
	slices.Sort(got)
	slices.Sort(keys)
	if !slices.Equal(got, keys) {
		t.Errorf("after %d adds at once, the history holds %v; want each of them once", adds, got)
	}
}

// testHistoryCap checks that adds keep a history within MaxHistorySize: the oldest entries go, a newest entry with
// two large trees keeps its key with its trees by digest, and one too large even then adds no entry but becomes the
// newest and moves Used.
func testHistoryCap(t *testing.T, s Stores) {
	ctx := t.Context()
	name := historyName("app")
	// Each of these trees takes about 4 MiB, so that the record holds three or four.
	for i := range 6 {
		tree := digest.FromString(fmt.Sprint("tree ", i))
		trees := map[digest.Digest][]cache.HistoryFile{tree: historyFiles(4000, 1000)}
		must(t, s.Records.AddHistory(ctx, name, historyEntry(fmt.Sprint(i), tree), trees, 8))
	}
	h, err := s.Records.History(ctx, name)
	must(t, err)
	got := historyKeys(h, "0", "1", "2", "3", "4", "5")
	if len(got) < 2 || len(got) > 4 || got[0] != "5" || got[1] != "4" {
		t.Errorf("history %v; want the newest few, most recent first, within the cap", got)
	}
	if data, _ := json.Marshal(h); len(data) > cache.MaxHistorySize {
		t.Errorf("the history takes %d bytes; want at most %d", len(data), cache.MaxHistorySize)
	}

	// Two trees of about 10 MiB each: the newest entry keeps its key, and one of them by digest alone.
	large, larger := digest.FromString("large"), digest.FromString("larger")
	must(t, s.Records.AddHistory(ctx, name, historyEntry("wide", large, larger), map[digest.Digest][]cache.HistoryFile{
		large:  historyFiles(10_000, 1000),
		larger: historyFiles(10_000, 1100),
	}, 8))
	h, err = s.Records.History(ctx, name)
	must(t, err)
	if len(h.Entries) == 0 || h.Entries[0].Key != digest.FromString("wide") {
		t.Fatalf("history %v; want the wide entry first", historyKeys(h, "wide"))
	}
	if _, ok := h.Trees[larger]; ok {
		t.Error("the larger tree is kept whole beside the large one, past the cap")
	}
	if _, ok := h.Trees[large]; !ok {
		t.Error("the large tree, which fits alone, isn't kept")
	}

	// An entry whose action alone passes the cap.
	huge := historyEntry("huge")
	huge.Action = json.RawMessage(`"` + strings.Repeat("x", cache.MaxHistorySize) + `"`)
	before, err := s.Records.Newest(ctx, name)
	must(t, err)
	must(t, s.Records.AddHistory(ctx, name, huge, nil, 8))
	h, err = s.Records.History(ctx, name)
	must(t, err)
	if h.Entries[0].Key != digest.FromString("wide") {
		t.Errorf("after an entry too large to keep, the first entry is %s; want the wide one", h.Entries[0].Key)
	}
	head, err := s.Records.Newest(ctx, name)
	if err != nil || head.Newest != huge.Fingerprint() || head.Used.Before(before.Used) {
		t.Errorf("newest after an entry too large to keep = %+v, %v; want its fingerprint, used again", head, err)
	}
}

// testPruneHistory checks that PruneHistory removes the histories used before its cutoff alone, and counts them.
func testPruneHistory(t *testing.T, s Stores) {
	ctx := t.Context()
	must(t, s.Records.AddHistory(ctx, historyName("old"), historyEntry("a"), nil, 8))
	cutoff := time.Now().Add(time.Second)
	for cutoff.After(time.Now()) {
		time.Sleep(10 * time.Millisecond)
	}
	must(t, s.Records.AddHistory(ctx, historyName("new"), historyEntry("b"), nil, 8))
	removed, err := s.Records.PruneHistory(ctx, cutoff)
	if err != nil || removed != 1 {
		t.Errorf("PruneHistory = %d, %v; want 1 removed", removed, err)
	}
	if h, err := s.Records.History(ctx, historyName("old")); err != nil || h != nil {
		t.Errorf("the history used before the cutoff = %v, %v; want it gone", h, err)
	}
	if h, err := s.Records.History(ctx, historyName("new")); err != nil || h == nil {
		t.Errorf("the history used after the cutoff = %v, %v; want it kept", h, err)
	}
	if removed, err := s.Records.PruneHistory(ctx, cutoff); err != nil || removed != 0 {
		t.Errorf("PruneHistory again = %d, %v; want none removed", removed, err)
	}
}
