package cache

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
)

// TestDirectorySizeCountsWhatWasThere checks that a directory opened again counts the blobs and records it already
// held, once its walk ends, and what is written and removed while it walks, each once.
func TestDirectorySizeCountsWhatWasThere(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	d, err := OpenDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	var blobs int64
	for i := range 50 {
		b, err := d.PutBytes(ctx, []byte(fmt.Sprintf("blob %d", i)))
		if err != nil {
			t.Fatal(err)
		}
		blobs += b.Size
	}
	if err := d.SetTag(ctx, "example.com/app:v1", &TagRecord{Digest: digest.FromString("i")}); err != nil {
		t.Fatal(err)
	}
	before := waitForSize(t, d)
	if before.Content != blobs || before.Records == 0 {
		t.Fatalf("the directory counts %+v; want %d bytes of content and the tag's record", before, blobs)
	}
	d.Close()

	again, err := OpenDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	// Written and removed while the walk may run: a new blob, one there before put again, and a corrupt one dropped.
	added, err := again.PutBytes(ctx, []byte("added while walking"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := again.PutBytes(ctx, []byte("blob 0")); err != nil {
		t.Fatal(err)
	}
	if got := waitForSize(t, again); got.Content != blobs+added.Size || got.Records != before.Records {
		t.Errorf("opened again, the directory counts %+v; want %d bytes of content and %d of records", got,
			blobs+added.Size, before.Records)
	}
}

// waitForSize returns d's size once its walk has ended.
func waitForSize(t *testing.T, d *Directory) DirectorySize {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if size, walked := d.Size(); walked {
			return size
		}
	}
	t.Fatal("the directory's walk never ended")
	return DirectorySize{}
}

// TestSizesAheadOfTheWalk checks, driving the walk by hand, that a file changed twice before the walk reaches it is
// counted once, at its last size, and that one removed before the walk reaches it isn't counted.
func TestSizesAheadOfTheWalk(t *testing.T) {
	dir := t.TempDir()
	if err := makeSubdirectories(dir); err != nil {
		t.Fatal(err)
	}
	write := func(name, data string) func() error {
		return func() error { return os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600) }
	}
	for name, data := range map[string]string{"ac/removed": "12345", "ac/rewritten": "123", "cas/sha256/passed": "1"} {
		if err := write(name, data)(); err != nil {
			t.Fatal(err)
		}
	}
	s := newSizes(dir)
	s.started, s.walking, s.done = true, true, map[string]bool{} // the walk has reached nothing yet

	if err := s.change(filepath.Join(dir, "ac", "rewritten"), write("ac/rewritten", "1234567")); err != nil {
		t.Fatal(err)
	}
	if err := s.change(filepath.Join(dir, "ac", "rewritten"), write("ac/rewritten", "12")); err != nil {
		t.Fatal(err)
	}
	if err := s.change(filepath.Join(dir, "ac", "removed"), func() error {
		return os.Remove(filepath.Join(dir, "ac", "removed"))
	}); err != nil {
		t.Fatal(err)
	}
	s.walk()
	if size, walked := s.size(); !walked || size != (DirectorySize{Content: 1, Records: 2}) {
		t.Errorf("the count = %+v, walked %t; want 1 byte of content and 2 of records, walked", size, walked)
	}
}

// TestSizesAfterAFailedClear checks that a clear that fails counts what it left.
func TestSizesAfterAFailedClear(t *testing.T) {
	dir := t.TempDir()
	if err := makeSubdirectories(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ac", "left"), []byte("123"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newSizes(dir)
	s.started, s.bytes[1] = true, 10 // as if the walk had ended, counting more than is left
	failed := errors.New("removing failed")
	if err := s.clear(1, func() error { return failed }); !errors.Is(err, failed) {
		t.Fatalf("the clear = %v; want %v", err, failed)
	}
	if size, _ := s.size(); size.Records != 3 {
		t.Errorf("after a failed clear, the records hold %d bytes; want the 3 left", size.Records)
	}
}

// TestSizeUncountedUntilAsked checks that a directory counts nothing, and walks nothing, until its size is asked for.
func TestSizeUncountedUntilAsked(t *testing.T) {
	d, err := OpenDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.PutBytes(t.Context(), []byte("before")); err != nil {
		t.Fatal(err)
	}
	d.sizes.mu.Lock()
	started := d.sizes.started
	d.sizes.mu.Unlock()
	if started {
		t.Error("the count started before anything asked for it")
	}
	if got := waitForSize(t, d); got.Content != 6 {
		t.Errorf("once asked for, the content is %d bytes; want the 6 put before", got.Content)
	}
}

// TestSizesOfAnUnreadableDirectory checks that a walk that can't read a directory never reports its count as whole.
func TestSizesOfAnUnreadableDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := makeSubdirectories(dir); err != nil {
		t.Fatal(err)
	}
	tags := filepath.Join(dir, "tags")
	if err := os.Remove(tags); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tags, nil, 0o600); err != nil { // which can't be read as a directory
		t.Fatal(err)
	}
	s := newSizes(dir)
	s.started, s.walking, s.done = true, true, map[string]bool{}
	s.walk()
	if _, whole := s.size(); whole {
		t.Error("a count missing a directory is reported whole")
	}
}

// TestSizesOfADirectoryAClearMakesAgain checks that a walk that finds a directory gone, as a clear removes and makes
// it again, counts it as empty, and that a clear's recount makes a directory the walk couldn't read whole again.
func TestSizesOfADirectoryAClearMakesAgain(t *testing.T) {
	dir := t.TempDir()
	if err := makeSubdirectories(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "ac")); err != nil { // as the clear has it for a moment
		t.Fatal(err)
	}
	s := newSizes(dir)
	s.started, s.walking, s.done = true, true, map[string]bool{}
	s.walk()
	if _, whole := s.size(); !whole {
		t.Error("a count of a directory gone for a moment isn't reported whole")
	}
	s.incomplete[1] = true // as if the walk couldn't read it
	if err := s.clear(1, func() error { return makeSubdirectories(dir) }); err != nil {
		t.Fatal(err)
	}
	if _, whole := s.size(); !whole {
		t.Error("a clear's recount didn't make the count whole again")
	}
}

// TestSizesStartAfterTheWritesInFlight checks that a write in flight as the count starts, uncounted as it began, lands
// before the walk reads its directory, so that the walk counts it.
func TestSizesStartAfterTheWritesInFlight(t *testing.T) {
	dir := t.TempDir()
	if err := makeSubdirectories(dir); err != nil {
		t.Fatal(err)
	}
	s := newSizes(dir)
	path := filepath.Join(dir, "ac", "written")
	writing, write := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.change(path, func() error {
			close(writing)
			<-write
			return os.WriteFile(path, []byte("123456"), 0o600)
		})
	}()
	<-writing
	started := make(chan struct{})
	go func() {
		s.size() // which waits for the write
		close(started)
	}()
	select {
	case <-started:
		t.Fatal("the count started while a write uncounted was in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(write)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	<-started
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if size, whole := s.size(); whole {
			if size.Records != 6 {
				t.Errorf("the records hold %d bytes; want the 6 written as the count started", size.Records)
			}
			return
		}
	}
	t.Fatal("the walk never ended")
}
