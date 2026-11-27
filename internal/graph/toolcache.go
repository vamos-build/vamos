package graph

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ToolCache is a tool cache that a layer or task mounts for each of its run() steps: a directory that the executor
// keeps between builds, or gives empty, whose contents are never part of a key or an image.
// The key covers where it is mounted, which cache it is, and its owner and group, since those shape the container.
type ToolCache struct {
	Path  string `json:"path"`  // where it is mounted: absolute, clean and literal, as CheckCachePath requires
	Label string `json:"label"` // the tool_cache rule's canonical label, as "//tools:go-build"
	Owner uint32 `json:"owner"` // the numeric owner and group of its directory, as the rule gives them, or 0
	Group uint32 `json:"group"`
}

// MaxID is the largest owner or group a tool cache can have: 4294967295, one more, is what chown takes as
// "leave unchanged".
const MaxID = 4294967294

// systemRoots are the directories that buildah's chroot isolation mounts itself in every run() step,
// silently skipping any mount given at or beneath them, so a cache there would never be seen.
var systemRoots = []string{"/dev", "/proc", "/sys"}

// BoundFiles are the files that buildah's chroot isolation binds into every run() step, or, for the last two, into
// every run() step on a FIPS host. A cache at, beneath or above one would have the file bound over it, or a mount point
// made for it inside the cache.
var BoundFiles = []string{
	"/etc/hosts", "/etc/hostname", "/etc/resolv.conf", "/run/.containerenv", "/run/secrets", "/etc/crypto-policies",
}

// Within reports whether the clean absolute path p is dir or lies beneath it.
func Within(p, dir string) bool {
	return p == dir || dir == "/" || strings.HasPrefix(p, dir+"/")
}

// CheckCachePath checks p, the argument param that says where a tool cache is mounted, which is literal:
// a clean absolute path, not /, with no variable, not at or beneath /dev, /proc or /sys, and neither at, beneath nor
// above one of BoundFiles. Its error names p and the rule it breaks.
func CheckCachePath(param, p string) error {
	// "$" first, so that a variable's mistake isn't reported as such: a cache's path has none.
	if strings.Contains(p, "$") && !strings.Contains(p, "\x00") {
		return fmt.Errorf(`%s %q must not contain "$": a tool cache's path is literal, never expanded`, param, p)
	}
	if err := CheckPath(param, p); err != nil {
		return err
	}
	switch {
	case !utf8.ValidString(p):
		return fmt.Errorf("%s %q must be UTF-8, as the graph encodes it", param, p)
	case !path.IsAbs(p):
		return fmt.Errorf("%s %q must be an absolute path", param, p)
	case path.Clean(p) != p:
		return fmt.Errorf("%s %q must be in clean form, as %q", param, p, path.Clean(p))
	case p == "/":
		return fmt.Errorf("%s %q must not be the root directory", param, p)
	}
	for _, root := range systemRoots {
		if Within(p, root) {
			return fmt.Errorf("%s %q is within %s, which buildah mounts itself in every run() step", param, p, root)
		}
	}
	for _, bound := range BoundFiles {
		var relation string
		switch {
		case p == bound:
			relation = "is"
		case Within(p, bound):
			relation = "is beneath"
		case Within(bound, p):
			relation = "contains"
		default:
			continue
		}
		return fmt.Errorf("%s %q %s %s, which buildah binds into run() steps", param, p, relation, bound)
	}
	return nil
}

// ParseID parses s, the argument param that gives a tool cache's owner or group, as a numeric ID:
// decimal digits alone, read in base ten, from 0 to MaxID, so that "1000" and "01000" are one owner.
func ParseID(param, s string) (uint32, error) {
	if s == "" || strings.Trim(s, "0123456789") != "" {
		return 0, fmt.Errorf("%s %q must be a numeric ID, such as \"1000\", since a name can mean another ID in "+
			"each image", param, s)
	}
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil || n > MaxID {
		return 0, fmt.Errorf("%s %q must be from 0 to %d", param, s, MaxID)
	}
	return uint32(n), nil
}

// checkToolCaches checks the tool caches a layer or task with steps mounts: each path one that CheckCachePath accepts,
// sorted, none at or beneath another; each label canonical, as "//pkg:name", and mounted once; each owner and group in
// range; and, if any is mounted, a run() step to lend them to.
// Loading refuses all of these first, saying where, so an error here means a graph that Vamos didn't make.
func checkToolCaches(caches []ToolCache, steps []Step) error {
	// Checked in time that grows with the caches' size, however many or deep they are, as a client can send a
	// coordinator either: never in pairs, and never walking a path's ancestors, which is quadratic in its depth.
	// open holds the earlier paths that are prefixes, as strings, of the last one, shortest first. The paths being
	// sorted, any earlier path that is a prefix of c is a prefix of the path before c too, so once those that aren't
	// prefixes of c are dropped, open holds every earlier path that c could be within. Each is checked by the byte
	// after it in c: a "/" makes c within it. Being distinct prefixes of c, they are fewer than c's bytes.
	labels := make(map[string]string, len(caches))
	var open []ToolCache
	for i, c := range caches {
		if err := CheckCachePath("tool cache path", c.Path); err != nil {
			return err
		}
		if !canonicalLabel(c.Label) {
			return fmt.Errorf("tool cache %q at %s isn't a canonical label, such as //tools:go-build", c.Label, c.Path)
		}
		if c.Owner > MaxID || c.Group > MaxID {
			return fmt.Errorf("tool cache %s's owner and group, %d and %d, must be from 0 to %d",
				c.Label, c.Owner, c.Group, MaxID)
		}
		if other, ok := labels[c.Label]; ok {
			return fmt.Errorf("tool cache %s is mounted twice, at %s and %s", c.Label, other, c.Path)
		}
		if i > 0 && caches[i-1].Path >= c.Path {
			return fmt.Errorf("tool caches at %s and %s aren't sorted by path, or are at one path",
				caches[i-1].Path, c.Path)
		}
		for len(open) > 0 && !strings.HasPrefix(c.Path, open[len(open)-1].Path) {
			open = open[:len(open)-1]
		}
		for _, other := range open {
			if c.Path[len(other.Path)] == '/' {
				return fmt.Errorf("tool cache %s at %s is within %s at %s", c.Label, c.Path, other.Label, other.Path)
			}
		}
		labels[c.Label] = c.Path
		open = append(open, c)
	}
	if len(caches) > 0 && !slices.ContainsFunc(steps, func(in Step) bool { return in.Op == OpRun }) {
		return errors.New("it mounts tool caches but has no run() step to lend them to")
	}
	return nil
}

// canonicalLabel reports whether label is a target's label in canonical form, "//pkg:name", as loading makes it: a
// clean package within the workspace, "" for its root, which may hold ":" as a directory's name can, then the name,
// after the last ":", without "/" and not "scratch", which names the empty image, and neither holding a NUL byte.
func canonicalLabel(label string) bool {
	rest, ok := strings.CutPrefix(label, "//")
	i := strings.LastIndexByte(rest, ':')
	if !ok || i < 0 || strings.Contains(label, "\x00") || !utf8.ValidString(label) {
		return false
	}
	pkg, name := rest[:i], rest[i+1:]
	switch {
	case name == "", name == "scratch", strings.Contains(name, "/"):
		return false
	case pkg == "":
		return true
	}
	return path.Clean(pkg) == pkg && !path.IsAbs(pkg) && pkg != "." && pkg != ".." && !strings.HasPrefix(pkg, "../")
}
