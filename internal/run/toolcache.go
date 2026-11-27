package run

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/vamos-build/vamos/internal/graph"
	"github.com/vamos-build/vamos/internal/sched"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"go.podman.io/buildah/define"
	"go.podman.io/storage"
)

// toolCachesDir is the directory of the build cache that holds the workspaces' tool caches:
// <workspace>/<label>/<owner>-<group>-<mapping>/data, each of the first two named by the digest of a name, and the
// third by the rule's numeric IDs and the digest of the builder's ID mappings, or "host" without any.
const toolCachesDir = "tool-caches"

// toolCacheMount is a tool cache as an attempt mounts it for each of its run() steps.
type toolCacheMount struct {
	graph.ToolCache
	dir   string // on the host: the directory mounted
	empty bool   // dir is the attempt's own, made empty for it, rather than the workspace's cache
	// hostUID and hostGID are the cache's owner and group as the host sees them, through the builder's ID mappings.
	hostUID, hostGID uint32
	// location is where Path resolved in the container before its steps, which every check holds it to,
	// and existed says that an empty directory was there then, which dev and ino identify.
	location string
	existed  bool
	dev, ino uint64
}

// toolCaches is what a target's tool caches are, once its builder exists.
type toolCaches struct {
	mounts   []*toolCacheMount // sorted by path, as the action gives them
	temp     string            // the directory of the attempt's own empty caches, if any, which it deletes
	prepared bool              // the mount points were made, just before the first run() step
	// secrets are those of every run() step, whose files no resolution may pass through a cache on the way to,
	// at any check; each step's alone must not overlap a cache, as that step runs.
	secrets []graph.Secret
}

// systemRoots are the directories that buildah's chroot isolation mounts itself in every run() step, from the host's
// side, following a symbolic link there as the host sees it.
var systemRoots = []string{"/dev", "/proc", "/sys"}

// boundFiles are the files that buildah's chroot isolation binds into every run() step, the last three on a FIPS host
// alone, each resolved in the image before each run() step, the two crypto-policies leaves apart.
var boundFiles = []string{
	"/etc/hosts", "/etc/hostname", "/etc/resolv.conf", "/run/.containerenv", "/run/secrets",
	"/etc/crypto-policies/back-ends", "/etc/crypto-policies/config",
}

// resolution is where a path leads in a container's root filesystem, followed as the container would follow it,
// never leaving it.
type resolution struct {
	location string // clean and absolute, as the container sees it
	// through is every directory it passed through on the way, in order, its end and the root, which no cache can be,
	// left out.
	through []string
}

// resolveIn resolves p, an absolute path, in the root filesystem at root, following symbolic links within it as
// securejoin does, a ".." at its top staying there and a missing component taken as it is, and returns where it
// leads and what it passed through.
func resolveIn(root, p string) (resolution, error) {
	var res resolution
	current := "/"
	pending := strings.Split(p, "/")
	links := 0
	for len(pending) > 0 {
		component := pending[0]
		pending = pending[1:]
		switch component {
		case "", ".":
			continue
		case "..":
			res.pass(current)
			current = path.Dir(current)
			continue
		}
		res.pass(current)
		next := path.Join(current, component)
		info, err := os.Lstat(filepath.Join(root, next))
		switch {
		case errors.Is(err, os.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
			current = next
		case err != nil:
			return resolution{}, err
		case info.Mode()&os.ModeSymlink != 0:
			links++
			if links > 255 {
				return resolution{}, fmt.Errorf("resolving %s: too many levels of symbolic links", p)
			}
			target, err := os.Readlink(filepath.Join(root, next))
			if err != nil {
				return resolution{}, err
			}
			if path.IsAbs(target) {
				current = "/"
			}
			pending = append(strings.Split(target, "/"), pending...)
		default:
			current = next
		}
	}
	res.location = current
	return res, nil
}

// pass records that the resolution passed through dir, unless it is the root.
func (res *resolution) pass(dir string) {
	if dir != "/" {
		res.through = append(res.through, dir)
	}
}

// within reports whether the clean absolute path p is dir, or beneath it.
func within(p, dir string) bool { return graph.Within(p, dir) }

// overlap reports whether one of the clean absolute paths a and b is, or holds, the other.
func overlap(a, b string) bool { return within(a, b) || within(b, a) }

// idMappingName names the builder's ID mappings in a cache's directory: the digest of the UID and GID mappings, or
// "host" without any, so that a builder that maps IDs otherwise finds another directory.
func idMappingName(options define.IDMappingOptions) string {
	if len(options.UIDMap) == 0 && len(options.GIDMap) == 0 {
		return "host"
	}
	data, err := json.Marshal([][]specs.LinuxIDMapping{options.UIDMap, options.GIDMap})
	if err != nil {
		panic(err) // mappings always encode
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// hostID returns the host's ID of id in a container whose mappings are maps, or false if they map it to none.
// Without mappings, an ID is the host's.
func hostID(id uint32, maps []specs.LinuxIDMapping) (uint32, bool) {
	if len(maps) == 0 {
		return id, true
	}
	for _, m := range maps {
		if id >= m.ContainerID && uint64(id-m.ContainerID) < uint64(m.Size) {
			host := uint64(m.HostID) + uint64(id-m.ContainerID)
			if host > graph.MaxID {
				return 0, false
			}
			return uint32(host), true
		}
	}
	return 0, false
}

// digestName returns the hex digest of name, which names its directory so that neither "/" nor the name itself,
// such as a workspace's, appears in a path a step can read in its mount table.
func digestName(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:])
}

// openToolCaches gives the target st the tool caches its action mounts: the workspace's own, in the build cache, or
// directories of the attempt's own, made empty, if the runner keeps none, as a worker's doesn't, or the request asks
// for them, as --no-tool-caches does. Each belongs to the rule's owner and group, as the builder's ID mappings put
// them on the host, which also choose the workspace's directory.
func (r *localRunner) openToolCaches(st *targetState, req sched.Request) error {
	caches := req.Action.ToolCaches
	if len(caches) == 0 {
		return nil
	}
	if st.runner.isolation != define.IsolationChroot {
		return fmt.Errorf("tool caches need buildah's chroot isolation, whose mounts their paths are checked against, "+
			"and steps run here with %s isolation", st.runner.isolation)
	}
	mappings := st.builder.IDMappingOptions
	persistent := r.toolCacheRoot != "" && !req.NoToolCaches
	if persistent {
		// The name chooses a cache shared by every build of the workspace, so only one load would accept does.
		if err := graph.CheckWorkspaceName(req.Workspace); err != nil {
			return fmt.Errorf("choosing the workspace's tool caches: workspace %w", err)
		}
	}
	tc := &toolCaches{}
	st.caches = tc
	for i, c := range caches {
		m := &toolCacheMount{ToolCache: c}
		var ok bool
		if m.hostUID, ok = hostID(c.Owner, mappings.UIDMap); !ok {
			return fmt.Errorf("tool cache %s: its owner, %d, is no ID that this executor's containers map", c.Label,
				c.Owner)
		}
		if m.hostGID, ok = hostID(c.Group, mappings.GIDMap); !ok {
			return fmt.Errorf("tool cache %s: its group, %d, is no ID that this executor's containers map", c.Label,
				c.Group)
		}
		var err error
		if persistent {
			m.dir, err = persistentToolCache(r.toolCacheRoot, req.Workspace, m, idMappingName(mappings))
		} else {
			m.empty = true
			m.dir, err = tc.tempToolCache(r.store, st.builder.ContainerID, i, m)
		}
		if err != nil {
			return fmt.Errorf("tool cache %s: %w", c.Label, err)
		}
		tc.mounts = append(tc.mounts, m)
	}
	return nil
}

// persistentToolCache returns the directory of the workspace's cache m in root, for the builder's ID mappings,
// mapping, making it on first use: the label's directory, with a file that names the label, then the owner's, with
// data/ in it, each made under a temporary name beside it and renamed into place, so that no attempt sees one half
// made. A rename that finds another attempt's in place lost a race, and uses the winner's.
func persistentToolCache(root, workspace string, m *toolCacheMount, mapping string) (string, error) {
	workspaceDir := filepath.Join(root, digestName(workspace))
	if err := os.MkdirAll(workspaceDir, 0o700); err != nil {
		return "", err
	}
	labelDir := filepath.Join(workspaceDir, digestName(m.Label))
	err := publishDir(labelDir, func(tmp string) error {
		return os.WriteFile(filepath.Join(tmp, "label"), []byte(m.Label+"\n"), 0o600)
	})
	if err != nil {
		return "", err
	}
	ownerDir := filepath.Join(labelDir, fmt.Sprintf("%d-%d-%s", m.Owner, m.Group, mapping))
	err = publishDir(ownerDir, func(tmp string) error {
		return makeCacheDir(filepath.Join(tmp, "data"), m)
	})
	if err != nil {
		return "", err
	}
	return filepath.Join(ownerDir, "data"), nil
}

// publishDir makes the directory dir, if it isn't there, by making a temporary one beside it, named .tmp-*, filling
// it with fill and renaming it into place. A rename that finds dir there, which is never empty, lost a race to
// another attempt, and removes its own; any other failure is returned, naming the path.
func publishDir(dir string, fill func(tmp string) error) error {
	if _, err := os.Stat(dir); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), ".tmp-")
	if err != nil {
		return err
	}
	if err := fill(tmp); err != nil {
		return errors.Join(err, os.RemoveAll(tmp))
	}
	err = os.Rename(tmp, dir)
	if errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) {
		return os.RemoveAll(tmp)
	}
	if err != nil {
		return errors.Join(fmt.Errorf("making %s: %w", dir, err), os.RemoveAll(tmp))
	}
	return nil
}

// makeCacheDir makes dir, a cache's data, owned by m's owner and group on the host, with mode 0755, whatever the umask.
func makeCacheDir(dir string, m *toolCacheMount) error {
	if err := os.Mkdir(dir, 0o755); err != nil {
		return err
	}
	if err := os.Lchown(dir, int(m.hostUID), int(m.hostGID)); err != nil {
		return err
	}
	return os.Chmod(dir, 0o755)
}

// tempToolCache returns an empty directory of the attempt's own for its i'th cache m, in the tool-caches/ directory of
// its container's own directory, on the store's volume, which the runner deletes as the attempt ends, and which goes
// with the container otherwise.
func (tc *toolCaches) tempToolCache(store storage.Store, container string, i int, m *toolCacheMount) (string, error) {
	if tc.temp == "" {
		dir, err := store.ContainerDirectory(container)
		if err != nil {
			return "", err
		}
		tc.temp = filepath.Join(dir, toolCachesDir)
		if err := os.MkdirAll(tc.temp, 0o700); err != nil {
			return "", err
		}
	}
	dir := filepath.Join(tc.temp, strconv.Itoa(i))
	return dir, makeCacheDir(dir, m)
}

// remove deletes the attempt's own empty caches, if it made any.
func (tc *toolCaches) remove() error {
	if tc == nil || tc.temp == "" {
		return nil
	}
	if err := removeAll(tc.temp); err != nil {
		return fmt.Errorf("removing its temporary tool caches: %w", err)
	}
	tc.temp = ""
	return nil
}

// active reports whether the target mounts tool caches.
func (tc *toolCaches) active() bool { return tc != nil && len(tc.mounts) > 0 }

// resolve resolves p in the target's root filesystem.
func (st *targetState) resolve(p string) (resolution, error) {
	return resolveIn(st.mountPoint, p)
}

// hostPath returns where location, as the container sees it, is on the host.
func (st *targetState) hostPath(location string) string {
	return filepath.Join(st.mountPoint, location)
}

// checkSystemRoots refuses a /dev, /proc or /sys that is a symbolic link, which buildah would follow from the host's
// side as it mounts its own there, where no check of a cache's path, resolved within the root filesystem, can follow.
func (st *targetState) checkSystemRoots() error {
	for _, root := range systemRoots {
		info, err := os.Lstat(st.hostPath(root))
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symbolic link, which buildah would follow from the host's side as it "+
				"mounts %s; a target that mounts tool caches needs a real %s", root, root, root)
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// systemRootOf returns the system root that location, or one of through, is at or beneath, or "".
func systemRootOf(res resolution) string {
	for _, p := range append(append([]string{}, res.through...), res.location) {
		for _, root := range systemRoots {
			if within(p, root) {
				return root
			}
		}
	}
	return ""
}

// watched is a path that a cache must not overlap, or be passed through by: a bound file, a declared volume, a
// secret's file or the working directory, with what to call it.
type watched struct {
	what string // as messages name it, such as "the file /etc/hosts, which buildah binds into every run() step"
	path string // as the image or step gives it
}

// watchedPaths returns the bound files, the volumes the image declares, each cleaned before it is resolved as buildah
// cleans it, and the files of secrets.
func (st *targetState) watchedPaths(secrets []graph.Secret) []watched {
	var paths []watched
	for _, f := range boundFiles {
		paths = append(paths, watched{what: f + ", which buildah binds into run() steps", path: f})
	}
	for _, v := range st.builder.Volumes() {
		paths = append(paths, watched{what: "the volume " + v + " that the image declares", path: path.Clean(v)})
	}
	for _, s := range secrets {
		if s.File != "" {
			paths = append(paths, watched{what: fmt.Sprintf("secret %q's file %s", s.ID, s.File), path: s.File})
		}
	}
	return paths
}

// workdir returns the working directory that the next run() step gets: the last workdir()'s, or the image's.
func (st *targetState) workdir() string {
	if dir := st.builder.WorkDir(); dir != "" {
		return dir
	}
	return "/"
}

// checkCaches checks the target's caches against what buildah mounts and binds and the image declares, and each
// other, refusing a cache whose resolved location overlaps another's or one of overlapping, or which a cache path, the
// working directory or one of passing passes through on the way to its end, and a /dev, /proc or /sys that is a link.
// locate, if set, records each cache's location rather than requiring the one recorded, as before the steps.
func (st *targetState) checkCaches(overlapping, passing []watched, locate bool) error {
	tc := st.caches
	if err := st.checkSystemRoots(); err != nil {
		return err
	}
	for _, m := range tc.mounts {
		res, err := st.resolve(m.Path)
		if err != nil {
			return err
		}
		if root := systemRootOf(res); root != "" {
			return fmt.Errorf("tool cache path %s resolves through %s, to %s, where buildah mounts the host's own",
				m.Path, root, res.location)
		}
		if locate {
			m.location = res.location
		} else if res.location != m.location {
			return fmt.Errorf("tool cache path %s now resolves to %s, not %s, since a step moved it; "+
				"mount the cache at a path that the target's steps leave alone", m.Path, res.location, m.location)
		}
	}
	for i, m := range tc.mounts {
		for _, other := range tc.mounts[:i] {
			if overlap(m.location, other.location) {
				return fmt.Errorf("tool cache paths %s and %s resolve to %s and %s, one within the other",
					other.Path, m.Path, other.location, m.location)
			}
		}
	}
	for _, w := range overlapping {
		res, err := st.resolve(w.path)
		if err != nil {
			return err
		}
		for _, m := range tc.mounts {
			if overlap(res.location, m.location) {
				return fmt.Errorf("%s resolves to %s, which overlaps the tool cache at %s, mounted at %s",
					w.what, res.location, m.Path, m.location)
			}
		}
	}
	return st.checkPassesThrough(passing)
}

// checkPassesThrough refuses a cache path, the working directory or one of watched whose resolution passes through
// a cache's location before its end, its own included: what a warm cache holds could lead it elsewhere once mounted.
func (st *targetState) checkPassesThrough(watchedPaths []watched) error {
	tc := st.caches
	// The working directory as it is, and as buildah cleans it before it enters it.
	paths := append([]watched{{what: "the working directory " + st.workdir(), path: st.workdir()}}, watchedPaths...)
	if cleaned := path.Clean(st.workdir()); cleaned != st.workdir() {
		paths = append(paths, watched{
			what: fmt.Sprintf("the working directory %s, as buildah cleans it to %s,", st.workdir(), cleaned),
			path: cleaned,
		})
	}
	for _, m := range tc.mounts {
		paths = append(paths, watched{what: "tool cache path " + m.Path, path: m.Path})
	}
	for _, w := range paths {
		res, err := st.resolve(w.path)
		if err != nil {
			return err
		}
		for _, p := range res.through {
			for _, m := range tc.mounts {
				if within(p, m.location) {
					return fmt.Errorf("%s resolves through %s, within the tool cache at %s, whose contents could "+
						"lead it elsewhere", w.what, p, m.Path)
				}
			}
		}
	}
	return nil
}

// checkWorkdir refuses a working directory that resolves into a cache, where relative writes would go into the cache,
// as it is or as buildah cleans it before making it, which settles a ".." before following the link it follows.
func (st *targetState) checkWorkdir() error {
	for _, dir := range []string{st.workdir(), path.Clean(st.workdir())} {
		res, err := st.resolve(dir)
		if err != nil {
			return err
		}
		what := st.workdir()
		if dir != st.workdir() {
			what = fmt.Sprintf("%s, as buildah cleans it to %s,", st.workdir(), dir)
		}
		for _, m := range st.caches.mounts {
			if within(res.location, m.location) {
				return fmt.Errorf("the working directory %s is in the tool cache at %s, mounted at %s",
					what, m.Path, m.location)
			}
		}
	}
	return nil
}

// emptyDirAt describes what is at location: whether it is absent, or an empty directory, with its device, inode and
// owner on the host.
func (st *targetState) emptyDirAt(location string) (absent, empty bool, stat *syscall.Stat_t, err error) {
	host := st.hostPath(location)
	info, err := os.Lstat(host)
	// Beneath a file there is nothing, and making the mount point there fails, naming it.
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return true, false, nil, nil
	} else if err != nil {
		return false, false, nil, err
	}
	stat, _ = info.Sys().(*syscall.Stat_t)
	if !info.IsDir() {
		return false, false, stat, nil
	}
	dir, err := os.Open(host)
	if err != nil {
		return false, false, stat, err
	}
	defer dir.Close()
	if _, err := dir.Readdirnames(1); errors.Is(err, io.EOF) {
		return false, true, stat, nil
	} else if err != nil {
		return false, false, stat, err
	}
	return false, false, stat, nil
}

// beforeSteps checks the target's caches before any step, recording where each resolves, and whether an empty
// directory is there, which it may be, as one an earlier layer mounted the cache at leaves, but nothing else may.
// secrets are those of every run() step, whose files are checked for overlap only as their step runs, since an earlier
// step may move them.
func (st *targetState) beforeSteps(secrets []graph.Secret) error {
	if !st.caches.active() {
		return nil
	}
	st.caches.secrets = secrets
	if err := st.checkCaches(st.watchedPaths(nil), st.watchedPaths(secrets), true); err != nil {
		return err
	}
	for _, m := range st.caches.mounts {
		absent, empty, stat, err := st.emptyDirAt(m.location)
		if err != nil {
			return err
		}
		switch {
		case absent:
		case !empty:
			return fmt.Errorf("the image already holds something at tool cache path %s, at %s, which the cache "+
				"would hide; mount the cache at another path", m.Path, m.location)
		default:
			m.existed, m.dev, m.ino = true, stat.Dev, stat.Ino
		}
	}
	return nil
}

// prepare makes the mount points just before the first run() step: once the working directory is checked and each
// cache path resolves where it did and holds nothing but the empty directory it held, it makes the missing parents as
// the image's user, as workdir() does, and the mount point itself, and gives it, made or kept, the cache's owner and
// group and mode 0755.
func (st *targetState) prepare() error {
	if err := st.checkWorkdir(); err != nil {
		return err
	}
	for _, m := range st.caches.mounts {
		res, err := st.resolve(m.Path)
		if err != nil {
			return err
		}
		if root := systemRootOf(res); root != "" {
			return fmt.Errorf("tool cache path %s now resolves through %s, to %s, where buildah mounts the host's "+
				"own, since a step moved it", m.Path, root, res.location)
		}
		if res.location != m.location {
			return fmt.Errorf("tool cache path %s now resolves to %s, not %s, since a step moved it; "+
				"mount the cache at a path that the target's steps leave alone", m.Path, res.location, m.location)
		}
		absent, empty, stat, err := st.emptyDirAt(m.location)
		if err != nil {
			return err
		}
		switch {
		case absent && !m.existed:
		case empty && m.existed && stat.Dev == m.dev && stat.Ino == m.ino:
		case absent:
			return fmt.Errorf("a step removed the empty directory at tool cache path %s, at %s, before the first "+
				"run() step; leave the cache's path alone", m.Path, m.location)
		default:
			return fmt.Errorf("a step put something at tool cache path %s, at %s, before the first run() step; "+
				"make the cache's parent, not the cache's path, before it", m.Path, m.location)
		}
		if absent {
			mode := os.FileMode(0o755)
			if parent := path.Dir(m.location); parent != "/" {
				if err := st.builder.EnsureContainerPathAs(parent, st.builder.User(), &mode); err != nil {
					return fmt.Errorf("making tool cache path %s: %w", m.Path, err)
				}
			}
			if err := os.Mkdir(st.hostPath(m.location), 0o755); err != nil {
				return fmt.Errorf("making tool cache path %s: %w", m.Path, err)
			}
		}
		if err := os.Lchown(st.hostPath(m.location), int(m.hostUID), int(m.hostGID)); err != nil {
			return fmt.Errorf("tool cache %s: giving its mount point its owner and group: %w", m.Label, err)
		}
		if err := os.Chmod(st.hostPath(m.location), 0o755); err != nil {
			return fmt.Errorf("tool cache %s: %w", m.Label, err)
		}
	}
	st.caches.prepared = true
	return nil
}

// checkMountPoints refuses a cache's location that is no longer an empty directory with the cache's owner and group,
// as a step that made something there while nothing was mounted, or an add() above it, would leave it.
func (st *targetState) checkMountPoints() error {
	for _, m := range st.caches.mounts {
		_, empty, stat, err := st.emptyDirAt(m.location)
		if err != nil {
			return err
		}
		if !empty || stat == nil || stat.Uid != m.hostUID || stat.Gid != m.hostGID {
			return fmt.Errorf("tool cache path %s, at %s, is no longer an empty directory owned by the cache's owner "+
				"and group, %d:%d, as a step left it while nothing was mounted there", m.Path, m.location, m.Owner,
				m.Group)
		}
	}
	return nil
}

// beforeRun checks the target's caches before a run() step given secrets, whose files must not overlap a cache,
// preparing their mount points before the first, and returns the step's mounts: a read-write bind of each cache at its
// location, which buildah follows no link to, rather than a --mount in RunOptions.RunMounts, which buildah would make
// an overlay it discards.
func (st *targetState) beforeRun(secrets []graph.Secret) ([]specs.Mount, error) {
	if !st.caches.active() {
		return nil, nil
	}
	if !st.caches.prepared {
		if err := st.prepare(); err != nil {
			return nil, err
		}
	}
	// The working directory first: a workdir() in a cache makes something at its mount point too.
	if err := st.checkWorkdir(); err != nil {
		return nil, err
	}
	if err := st.checkCaches(st.watchedPaths(secrets), st.watchedPaths(st.caches.secrets), false); err != nil {
		return nil, err
	}
	if err := st.checkMountPoints(); err != nil {
		return nil, err
	}
	var mounts []specs.Mount
	for _, m := range st.caches.mounts {
		mounts = append(mounts, specs.Mount{
			Destination: m.location, Type: define.TypeBind, Source: m.dir, Options: []string{"rbind", "rw"},
		})
	}
	return mounts, nil
}

// afterSteps checks the target's caches once its steps have run, before a layer's image is committed or a task's
// output collected: each cache path still resolves to its location, an empty directory with the cache's owner and
// group, the working directory isn't in one, and nothing passes through one.
func (st *targetState) afterSteps() error {
	if !st.caches.active() {
		return nil
	}
	if err := st.checkWorkdir(); err != nil {
		return err
	}
	if st.caches.prepared {
		if err := st.checkMountPoints(); err != nil {
			return err
		}
	}
	return st.checkCaches(st.watchedPaths(nil), st.watchedPaths(st.caches.secrets), false)
}

// checkInto refuses what, a path a step writes to or output is collected from, that resolves at or beneath a cache.
func (st *targetState) checkInto(what, p string) error {
	if !st.caches.active() {
		return nil
	}
	res, err := st.resolve(p)
	if err != nil {
		return err
	}
	for _, m := range st.caches.mounts {
		if within(res.location, m.location) {
			return fmt.Errorf("%s %s resolves to %s, in the tool cache at %s; what is there stays in the cache",
				what, p, res.location, m.Path)
		}
	}
	return nil
}
