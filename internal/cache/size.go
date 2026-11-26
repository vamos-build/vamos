package cache

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

// sizedDirectories are the directories whose files a Directory counts: its blobs first, then its records.
var sizedDirectories = []string{"cas/sha256", "ac", "tags", "history"}

// DirectorySize is how many bytes a Directory's files hold: its blobs' and its records', results, tags and histories.
type DirectorySize struct {
	Content, Records int64
}

// sizes keeps a Directory's running count of the bytes each of sizedDirectories holds, once asked for: a walk of what
// is there then, which runs in the background, and every file written or removed through the Directory since.
// While the walk runs, a file it hasn't reached is counted from its first change, as nothing before it, and the walk
// then leaves it, so that nothing is counted twice or left out. Until it is first asked for, nothing is counted, so
// that a Directory whose size nobody reads, as a local executor's, pays nothing for it.
type sizes struct {
	dir string
	// active is held shared by each change made before the count starts, through its write, and exclusively as the
	// count starts, so that no such change lands after the walk has read its directory, uncounted.
	active sync.RWMutex

	mu      sync.Mutex
	started bool    // the count has been asked for, and the walk started
	bytes   []int64 // by sizedDirectories' index
	walking bool
	cursor  struct { // the last file the walk counted
		dir  int
		name string
	}
	done   map[string]bool // files counted while the walk hadn't reached them, by path; nil once it has ended
	closed bool            // the Directory is closed: the walk stops, and none starts
	// incomplete says, by sizedDirectories' index, that a directory couldn't be read, so that its count isn't whole
	// until a clear counts it again; a directory that doesn't exist, as for a moment while a clear makes it again,
	// holds nothing.
	incomplete []bool
	cleared    []int // by sizedDirectories' index, how many clears have counted it, so that the walk's read older than
	// a clear's doesn't undo what the clear found
}

// newSizes returns the sizes of dir, uncounted until size is first called.
func newSizes(dir string) *sizes {
	return &sizes{dir: dir, bytes: make([]int64, len(sizedDirectories)),
		incomplete: make([]bool, len(sizedDirectories)), cleared: make([]int, len(sizedDirectories))}
}

// walk counts the files in dir's sizedDirectories, in the order of their names, which ReadDir gives, so that whether
// a file has been reached is known from the cursor alone, holding the names of a directory's files meanwhile; one it
// can't read leaves the count incomplete. It stops once the Directory is closed.
func (s *sizes) walk() {
	for i, sub := range sizedDirectories {
		s.mu.Lock()
		cleared := s.cleared[i]
		s.mu.Unlock()
		entries, err := os.ReadDir(filepath.Join(s.dir, sub))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			s.mu.Lock()
			s.incomplete[i] = s.incomplete[i] || s.cleared[i] == cleared // unless a clear has counted it since
			s.mu.Unlock()
		}
		for _, e := range entries {
			path := filepath.Join(s.dir, sub, e.Name())
			s.mu.Lock()
			if s.closed {
				s.mu.Unlock()
				return
			}
			if !s.done[path] {
				s.bytes[i] += fileSize(path)
			}
			s.cursor.dir, s.cursor.name = i, e.Name()
			s.mu.Unlock()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.walking, s.done = false, nil
}

// fileSize returns the size of the regular file at path, or 0 if there is none, as the walk and every change take it.
func fileSize(path string) int64 {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return 0
	}
	return info.Size()
}

// index returns which of sizedDirectories path is in, under dir, or -1 if none.
func index(dir, path string) int {
	parent, err := filepath.Rel(dir, filepath.Dir(path))
	if err != nil {
		return -1
	}
	for i, sub := range sizedDirectories {
		if filepath.ToSlash(parent) == sub {
			return i
		}
	}
	return -1
}

// change replaces or removes the file at path with do, and returns what do does, counting the difference it makes,
// if the count has started and path is in sizedDirectories. Changes are counted one at a time, so that two writes of
// one file are counted as one; what the count can't read of a file is taken as nothing, and never fails do.
func (s *sizes) change(path string, do func() error) error {
	i := index(s.dir, path)
	if i < 0 {
		return do()
	}
	s.active.RLock()
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		defer s.active.RUnlock() // through the write, which the count's start waits for
		return do()              // uncounted, and so held up by no other change
	}
	s.active.RUnlock()
	defer s.mu.Unlock()
	before := fileSize(path)
	err := do()
	after := fileSize(path)
	switch {
	case !s.walking || s.reached(i, filepath.Base(path)) || s.done[path]:
		s.bytes[i] += after - before
	default:
		s.done[path] = true // the walk leaves it, so what it held before isn't counted
		s.bytes[i] += after
	}
	return err
}

// reached reports whether the walk has counted the file name in sizedDirectories[i], or passed where it would be;
// s.mu must be held.
func (s *sizes) reached(i int, name string) bool {
	return i < s.cursor.dir || i == s.cursor.dir && name <= s.cursor.name
}

// clear empties sizedDirectories[i], which do does, without holding the count meanwhile, so that a long removal holds
// up no write or scrape, and then counts again what is there, what the clear left or what was written since, each file
// then left by the walk, if it hasn't reached it.
func (s *sizes) clear(i int, do func() error) error {
	err := do()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		return err
	}
	s.recount(i)
	return err
}

// recount counts sizedDirectories[i] again, whole, as it is now, each file then left by the walk, if it hasn't
// reached it; s.mu must be held.
func (s *sizes) recount(i int) {
	s.bytes[i] = 0
	s.cleared[i]++
	entries, rerr := os.ReadDir(filepath.Join(s.dir, sizedDirectories[i]))
	s.incomplete[i] = rerr != nil && !errors.Is(rerr, fs.ErrNotExist)
	for _, e := range entries {
		path := filepath.Join(s.dir, sizedDirectories[i], e.Name())
		s.bytes[i] += fileSize(path)
		if s.walking && !s.reached(i, e.Name()) {
			s.done[path] = true
		}
	}
}

// size returns the counts, starting them if this is the first call, and whether the walk has ended, before which
// they are partial.
func (s *sizes) size() (DirectorySize, bool) {
	s.mu.Lock()
	if !s.started && !s.closed {
		// The changes made before it, which hold active through their writes, land first.
		s.mu.Unlock()
		s.active.Lock()
		s.mu.Lock()
		if !s.started && !s.closed {
			s.started, s.walking, s.done = true, true, map[string]bool{}
			go s.walk()
		}
		s.active.Unlock()
	}
	defer s.mu.Unlock()
	if !s.walking {
		for i, failed := range s.incomplete {
			if failed { // tried again, as a read that failed once, as with too many open files, may not again
				s.recount(i)
			}
		}
	}
	var size DirectorySize
	for i, n := range s.bytes {
		if i == 0 {
			size.Content += n
		} else {
			size.Records += n
		}
	}
	return size, s.started && !s.walking && !slices.Contains(s.incomplete, true)
}

// close stops the walk, if it runs, and keeps any from starting.
func (s *sizes) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
}
