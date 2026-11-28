package load

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/vamos-build/vamos/internal/cache"
	"github.com/vamos-build/vamos/internal/fileset"
	"github.com/vamos-build/vamos/internal/graph"
	"github.com/vamos-build/vamos/internal/tracing"

	"github.com/containerd/platforms"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/afero"
	"go.podman.io/image/v5/docker/reference"
	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

// workspace is a project's BUILD files, each in its own directory, or package, loaded as targets name them.
// A package is named by its directory relative to the workspace's root, "" for the root itself,
// and a target by its label, "//dir:name".
type workspace struct {
	ctx          context.Context // of the load, which ends Starlark evaluation when it ends
	fs           afero.Fs
	root         string                // absolute: the directory holding the WORKSPACE file, or else the only package
	name         string                // that the WORKSPACE file gives with workspace(), or "" for an unnamed workspace
	current      string                // the package of -C, whose targets are named without a label
	packages     map[string]*buildFile // those loaded, by package
	modules      map[string]*module    // .star files loaded, by label
	execPlatform *platformValue        // the executor's, which run() steps run on without emulation, as EXEC_PLATFORM
	platform     *platformValue        // the command's, given with --platform: the named target's, unless an index
	configs      map[string]*config    // every configuration made, by key, so that equal ones are the same
	builtins     starlark.StringDict   // of BUILD and .star files, made once
	lock         *Lock                 // the root's lockfile, whose pins each pull's node takes
}

// buildFile is the targets declared by the BUILD file of one package.
type buildFile struct {
	pkg   string
	dir   string                  // absolute: the package's directory, which globs are relative to
	path  string                  // of the BUILD file, for messages
	names []string                // of every target, globs included, in the order declared
	decls map[string]*declaration // each target, by name
	refs  []targetRef             // the labels given to add() and dep() as the file ran, for vamos update to follow
}

// declaration records a target of a BUILD file: where it was declared, by which builtin, and the call's arguments,
// from which template makes its template, the target as the call describes it.
// In a template, targets that it uses are named by targetRefs, resolved as it is made;
// configuring the target then makes an instance of it, in which they are targets in configurations.
//
// Running the BUILD file only declares the target, checking the call's shape and keeping its arguments, frozen.
// Its template is made as the target is configured: once, if no argument is a function of ctx,
// and otherwise in each configuration the target is needed in.
// A tool cache, which is no target and names none, is made as the BUILD file runs.
type declaration struct {
	kind      string // "glob", "pull", "layer", "task", "index", "publication" or "tool_cache"
	pos       syntax.Position
	file      *buildFile
	params    []*param // that params declares, in order, which are known before any configuration
	hasOutput bool     // a glob, or a task given source, whose files add() can name
	usesCtx   bool     // some argument is a function of ctx, so that the template depends on the configuration
	// the template, once made, if it doesn't depend on the configuration: a *fileSet, *pullSpec, *targetSpec,
	// *indexSpec, *publicationSpec or *toolCacheSpec, whose attributes are never functions
	static any

	// The call's arguments, frozen, of which some may be functions of ctx, and how to make the template;
	// none for a tool cache, made as it is declared.
	fn     *starlark.Builtin
	args   starlark.Tuple
	kwargs []starlark.Tuple
	make   makeFunc
	given  map[string]starlark.Value // the arguments, by parameter, for givenRefs
}

// makeFunc makes the template of a target from the arguments of the call to fn that declares it, checking them and
// resolving the targets they name, and returns the target's name too.
// It runs in c: as the target is configured, with what the functions of ctx among the arguments return in their
// place, or, for a tool cache, as the BUILD file declares it.
// The arguments don't include params, which the declaration holds.
type makeFunc func(c *evalContext, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (
	string, any, error)

// module is a .star file, run once as load() first names it.
type module struct {
	globals starlark.StringDict
	err     error
	loading bool        // while it runs, to report a cycle of loads
	refs    []targetRef // the labels given to dep() as it ran, or a .star file it loaded did
}

// evalMode says what Starlark code is running, which decides what builtins may do.
type evalMode int

const (
	declaring     evalMode = iota // a BUILD file, declaring its targets
	configuring                   // a target being configured: its rule, and the functions of ctx it was given
	loadingModule                 // the top level of a .star file, as load() runs it
)

// evalContext is what builtins know of the thread calling them, kept in its locals under contextKey.
// Builtins are shared across the workspace, so caller scope must come from the thread.
// A function defined in a .star file resolves labels against the BUILD file or target it runs for,
// not the module defining it.
type evalContext struct {
	mode     evalMode
	file     *buildFile         // the BUILD file running, or declaring the target being configured; nil in a module
	made     []madeStep         // while declaring: every step made, in order
	listed   map[step]bool      // while declaring: steps in some target's list
	deferred bool               // while declaring: some target's steps are given by a function of ctx
	target   *declaration       // while configuring: the target being made
	refs     []targetRef        // while declaring or loading a module: the labels given to add() and dep()
	hasRef   map[[2]string]bool // the package and label of each of refs
	clean    map[any]bool       // while declaring: the values that rule calls froze, which hold no function
}

// addRef records ref among the labels given to add() and dep(), unless it is there already.
func (c *evalContext) addRef(ref targetRef) {
	key := [2]string{ref.pkg, ref.ref}
	if c.hasRef[key] {
		return
	}
	if c.hasRef == nil {
		c.hasRef = map[[2]string]bool{}
	}
	c.hasRef[key] = true
	c.refs = append(c.refs, ref)
}

// contextKey is the thread local holding a thread's evalContext.
const contextKey = "vamos.context"

// contextOf returns the evalContext of thread.
func contextOf(thread *starlark.Thread) *evalContext {
	return thread.Local(contextKey).(*evalContext)
}

// scratch is the base name for an empty image, as in a Dockerfile's "FROM scratch".
const scratch = "scratch"

// pullSpec is an image to fetch from a registry, declared by a call to pull().
type pullSpec struct {
	label string
	id    confLabel // of an instance: the pull in its configuration
	image reference.NamedTagged
}

// fileSet is a set of files that add() can name in its source:
// files in a package, declared by glob(), or files a task makes.
type fileSet struct {
	label       string             // of the glob or task
	task        *targetSpec        // the task that makes the files, or nil for a glob
	dir         string             // a glob's package directory
	sel         *fileset.Selection // the files, within dir or the task's source
	configuring string             // a glob given by a function of ctx: the target and configuration, for errors
}

// targetSpec runs on another target's image, declared by a call to layer() or task().
// A layer's image becomes the base of later targets; a task's image is discarded once its output, if any, is collected.
// A task without output, such as one that runs tests, is run only for its steps to succeed.
type targetSpec struct {
	label   string
	id      confLabel // of an instance: the target in its configuration
	task    bool      // declared by task(), not layer()
	baseRef targetRef // of a template: a pull or layer, or scratch
	base    confLabel // of an instance: the base in its configuration
	steps   []step    // applied in order
	command []string  // a layer's default command
	// entrypoint is a layer's, if given, even empty, which graph.Layer.Entrypoint describes; nil if not given
	entrypoint *[]string
	source     string            // a task's directory of output
	output     *fileSet          // a task's output, found in source; nil for a layer, or a task without output
	mounts     []graph.ToolCache // the tool caches it mounts for its run() steps, sorted by path
}

// indexSpec is an index of images for several platforms, declared by a call to index():
// its manifests, each a pull or layer, named with the platform it is for.
type indexSpec struct {
	manifests []targetRef // in the order given, which the index keeps
}

// publicationSpec is a release, declared by a call to publication(): the tasks that must succeed before it publishes,
// and the images it publishes, each a pull, layer or index, to its repository.
// No target can use a publication, so it is never configured as a target, but only expanded into its roots.
type publicationSpec struct {
	checks []targetRef         // in the order given
	images []publishedImageRef // in the dictionary's order
}

// publishedImageRef is an image that a publication publishes, and the repository it publishes it to.
type publishedImageRef struct {
	repository reference.Named // with no tag or digest
	key        string          // the repository as the publication gives it, for messages
	target     targetRef
}

// madeStep is a step made by a call to the builtin kind at pos.
type madeStep struct {
	step step
	kind string
	pos  syntax.Position
}

// loadError is an error in a BUILD file, which already says where.
// When loading a package's BUILD file fails as another names one of its targets, the error is reported as it is,
// rather than as an error of the call that named the target.
type loadError struct{ err error }

func (e *loadError) Error() string { return e.err.Error() }
func (e *loadError) Unwrap() error { return e.err }

// openWorkspace returns the workspace that holds dir, the directory given with -C:
// the nearest directory at or above dir with a WORKSPACE file, or else dir alone.
// It evaluates that WORKSPACE file, if it is a regular file, which may name the workspace, but no other,
// as evalWorkspaceFile describes, returning the workspace even if that fails, and loads no BUILD file yet.
// exec is the executor's platform, which targets are built for unless --platform gives another.
func openWorkspace(ctx context.Context, fsys afero.Fs, dir string, exec imgspecv1.Platform) (*workspace, error) {
	info, err := fsys.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("directory %s does not exist", dir)
	} else if err != nil {
		return nil, err
	} else if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	root, evaluate := dir, false
	for d := dir; ; d = filepath.Dir(d) {
		// Any error but a missing file stops the search, rather than choosing another root.
		if info, err := fsys.Stat(filepath.Join(d, "WORKSPACE")); err == nil {
			// Only a regular file is evaluated: anything else, such as a directory, only marks the root.
			root, evaluate = d, info.Mode().IsRegular()
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("looking for a WORKSPACE file: %w", err)
		}
		if filepath.Dir(d) == d {
			break
		}
	}
	current, err := filepath.Rel(root, dir)
	if err != nil {
		return nil, err
	}
	if current == "." {
		current = ""
	}
	// The graph's platforms must be normalized, and an executor's may not be.
	exec = platforms.Normalize(exec)
	w := &workspace{
		fs:           fsys,
		root:         root,
		current:      filepath.ToSlash(current),
		packages:     map[string]*buildFile{},
		modules:      map[string]*module{},
		ctx:          ctx,
		execPlatform: &platformValue{exec},
		platform:     &platformValue{exec},
		configs:      map[string]*config{},
	}
	if evaluate {
		if err := w.evalWorkspaceFile(); err != nil {
			return w, err
		}
	}
	return w, nil
}

// label returns the label of the target name in package pkg.
func label(pkg, name string) string {
	return "//" + pkg + ":" + name
}

// parseLabel returns the package and name of the target that ref names from package pkg:
// "name" or ":name" for a target of pkg itself, or "//dir:name" for one in the package at dir.
// The name of a target in pkg isn't checked, since it is only looked up there.
func parseLabel(ref, pkg string) (string, string, error) {
	rest, ok := strings.CutPrefix(ref, "//")
	if !ok {
		return pkg, strings.TrimPrefix(ref, ":"), nil
	}
	dir, name, ok := strings.Cut(rest, ":")
	if !ok {
		return "", "", fmt.Errorf(`label %q must end with ":" and a target's name`, ref)
	}
	if dir != "" && (dir == "." || path.Clean(dir) != dir || !filepath.IsLocal(dir)) {
		return "", "", fmt.Errorf("label %q: %q is not a clean path to a directory within the workspace", ref, dir)
	}
	if err := checkName(name); err != nil {
		return "", "", fmt.Errorf("label %q: %w", ref, err)
	}
	return dir, name, nil
}

// checkName reports whether name can name a target.
func checkName(name string) error {
	switch {
	case name == "":
		return errors.New("name must not be empty")
	case name == scratch:
		return fmt.Errorf("name %q is reserved for the empty base image", name)
	case strings.ContainsAny(name, ":/"):
		return fmt.Errorf(`name %q must not contain ":" or "/", which labels use`, name)
	case strings.Contains(name, "\x00"):
		return fmt.Errorf("name %q must not contain a NUL byte", name)
	}
	return nil
}

// display names the target with label l for messages: by its name alone if it is in the package of -C.
func (w *workspace) display(l string) string {
	rest := strings.TrimPrefix(l, "//")
	i := strings.LastIndexByte(rest, ':')
	if rest[:i] == w.current {
		return rest[i+1:]
	}
	return l
}

// load returns the BUILD file of package pkg, loading it if it hasn't been.
// Loading it runs it, which declares its targets and names no other package's,
// so that a package is loaded whole, never while another is part way through, and is complete once returned.
// The packages its targets name are loaded as those targets are configured.
func (w *workspace) load(pkg string) (*buildFile, error) {
	if f, ok := w.packages[pkg]; ok {
		return f, nil
	}
	if err := w.checkPackageDir(pkg); err != nil {
		return nil, err
	}
	dir := filepath.Join(w.root, filepath.FromSlash(pkg))
	buildPath, err := w.findBuildFile(dir)
	if err != nil {
		return nil, err
	}
	src, err := afero.ReadFile(w.fs, buildPath)
	if err != nil {
		return nil, err
	}

	f := &buildFile{pkg: pkg, dir: dir, path: buildPath, decls: map[string]*declaration{}}
	if err := w.exec(f, src); err != nil {
		var nested *loadError
		if !errors.As(err, &nested) {
			nested = &loadError{err}
		}
		return nil, nested
	}
	w.packages[pkg] = f
	return f, nil
}

// buildFileNames are the names a package's BUILD file may have.
// BUILD.vms is for a directory that also holds a directory named build, as a project's build output often is:
// where names are compared without case, as on macOS or with git's core.ignorecase, BUILD can't sit beside it,
// and a .gitignore of build ignores BUILD too.
var buildFileNames = []string{"BUILD", "BUILD.vms"}

// findBuildFile returns the path of the BUILD file in dir, by one of buildFileNames.
// A directory by one of the names, as build is found as BUILD where names are compared without case, isn't one.
// Two are an error rather than one hiding the other, since a change to the hidden one would be silently ignored.
func (w *workspace) findBuildFile(dir string) (string, error) {
	var found []string
	for _, name := range buildFileNames {
		p := filepath.Join(dir, name)
		info, err := w.fs.Stat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return "", err
		}
		if !info.IsDir() {
			found = append(found, p)
		}
	}
	switch len(found) {
	case 0:
		return "", fmt.Errorf("no BUILD or BUILD.vms file in %s", dir)
	case 1:
		return found[0], nil
	}
	return "", fmt.Errorf("%s has both BUILD and BUILD.vms; a package has one BUILD file, so remove one of them", dir)
}

// checkPackageDir reports a symbolic link on the way from the workspace's root to package pkg,
// which could lead outside the workspace, as checkInclude does for a glob's files.
// The root itself may be reached through a link, as from a linked $PWD.
func (w *workspace) checkPackageDir(pkg string) error {
	if pkg == "" {
		return nil
	}
	current := w.root
	for component := range strings.SplitSeq(pkg, "/") {
		current = filepath.Join(current, component)
		info, _, err := lstat(w.fs, current)
		if errors.Is(err, fs.ErrNotExist) {
			return nil // reported as a missing BUILD file
		} else if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("package %q: %s is a symbolic link; a package must be a directory within the workspace",
				pkg, current)
		}
	}
	return nil
}

// lookup returns the BUILD file that declares the target ref names from package from, and the target's name there,
// which it may lack: f, the BUILD file of the target naming it, if ref names a target of its own package.
// Another package is loaded if it hasn't been, whole, so the file returned has every target it declares.
func (w *workspace) lookup(f *buildFile, ref, from string) (*buildFile, string, error) {
	pkg, name, err := parseLabel(ref, from)
	if err != nil {
		return nil, "", err
	}
	if pkg == f.pkg {
		return f, name, nil
	}
	other, err := w.load(pkg)
	return other, name, err
}

// labelHint suggests the label meant by name,
// which names no target since it has a ":", as a label missing its leading "//".
// A name can't contain ":", so this is likely.
func labelHint(name string) string {
	return fmt.Sprintf(`; a label starts with "//", as in "//%s"`, name)
}

// missing reports that f has no target name, or no tool cache if cache is set, as mount() names one, suggesting the
// one meant from those alone: a tool cache is no target, and only a tool cache can be mounted.
func (f *buildFile) missing(name string, cache bool) error {
	if cache {
		isCache := func(d *declaration) bool { return d.kind == "tool_cache" }
		return f.missingOf(name, "tool cache", "tool caches", "none", isCache)
	}
	return f.missingOf(name, "target", "targets", "none", func(d *declaration) bool { return d.kind != "tool_cache" })
}

// missingOf reports that f has no what name, suggesting the one meant, or else listing them, from those that usable
// accepts, which are plural, as what a reference may name; or saying that it declares none of them, as none puts it.
func (f *buildFile) missingOf(name, what, plural, none string, usable func(*declaration) bool) error {
	names := slices.DeleteFunc(slices.Clone(f.names), func(n string) bool { return !usable(f.decls[n]) })
	hint := fmt.Sprintf("; its %s are %s", plural, fileset.QuoteNames(names))
	if strings.Contains(name, ":") {
		hint = labelHint(name)
	} else if len(names) == 0 {
		hint = "; it declares " + none
	} else if closest := closestName(name, names); closest != "" {
		hint = fmt.Sprintf("; did you mean %q?", closest)
	}
	return fmt.Errorf("%s has no %s %q%s", f.path, what, name, hint)
}

// exec executes the BUILD file f, whose source is src, declaring its targets in f.
// The files named by its globs are read only once a build needs them, by readGlobs.
func (w *workspace) exec(f *buildFile, src []byte) error {
	c := &evalContext{mode: declaring, file: f, listed: map[step]bool{}, clean: map[any]bool{}}
	thread := &starlark.Thread{Name: f.path, Load: w.loadModule}
	thread.SetLocal(contextKey, c)
	defer cancelWith(w.ctx, thread)()
	if _, err := starlark.ExecFileOptions(&syntax.FileOptions{}, thread, f.path, src, w.predeclared()); err != nil {
		return w.interrupted(describeExecError(err))
	}

	f.refs = c.refs
	// A step made as the file runs may be listed only by a function of ctx giving steps, which runs later.
	if c.deferred {
		return nil
	}
	for _, m := range c.made {
		if !c.listed[m.step] {
			return fmt.Errorf("%s: %s: %s is not in the steps of any layer() or task()", m.pos, m.kind, m.step)
		}
	}
	return nil
}

// describeExecError returns err, from running a BUILD or .star file, as it is reported: as one line saying where.
// An error in another package's BUILD file already says where it is.
func describeExecError(err error) error {
	if nested, ok := errors.AsType[*loadError](err); ok {
		return nested
	}
	if evalErr, ok := errors.AsType[*starlark.EvalError](err); ok {
		return errors.New(describeEvalError(evalErr))
	}
	return err
}

// loadModule implements load(): it returns the globals of the .star file that label names, such as "//:defs.star",
// running it the first time it is named.
func (w *workspace) loadModule(caller *starlark.Thread, label string) (starlark.StringDict, error) {
	if !strings.HasPrefix(label, "//") {
		return nil, fmt.Errorf(`a .star file is named by its label, which starts with "//" and ends with ".star", ` +
			`such as "//:defs.star"`)
	}
	if !strings.HasSuffix(label, ".star") {
		return nil, fmt.Errorf(`only a .star file can be loaded, such as "//:defs.star"`)
	}
	// A target's label checks the rest, but what it would say of a target's name would mislead.
	if _, file, ok := strings.Cut(label, ":"); !ok || strings.ContainsAny(file, ":/") {
		return nil, fmt.Errorf(`a .star file's label names its directory, then ":" and the file, ` +
			`such as "//:defs.star" or "//tools:defs.star"`)
	}
	pkg, name, err := parseLabel(label, "")
	if err != nil {
		return nil, err
	}
	if m, ok := w.modules[label]; ok {
		if m.loading {
			return nil, fmt.Errorf("%s loads itself, through the files it loads", label)
		}
		w.passRefs(caller, m)
		return m.globals, m.err
	}
	if err := w.checkPackageDir(pkg); err != nil {
		return nil, err
	}
	path := filepath.Join(w.root, filepath.FromSlash(pkg), name)
	src, err := afero.ReadFile(w.fs, path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no file %s", path)
	} else if err != nil {
		return nil, err
	}

	m := &module{loading: true}
	w.modules[label] = m
	thread := &starlark.Thread{Name: path, Load: w.loadModule}
	mc := &evalContext{mode: loadingModule}
	thread.SetLocal(contextKey, mc)
	defer cancelWith(w.ctx, thread)()
	m.globals, m.err = starlark.ExecFileOptions(&syntax.FileOptions{}, thread, path, src, w.predeclared())
	if m.err != nil {
		m.err = w.interrupted(describeExecError(m.err))
	}
	m.loading = false
	m.refs = mc.refs
	w.passRefs(caller, m)
	return m.globals, m.err
}

// passRefs gives the BUILD or .star file that caller runs, which loads m, the labels given to dep() as m ran,
// since what m defines may use them there.
// Each is passed once, so that modules loaded along many paths pass on no more than they hold.
func (w *workspace) passRefs(caller *starlark.Thread, m *module) {
	if c := contextOf(caller); c.mode == declaring || c.mode == loadingModule {
		for _, ref := range m.refs {
			c.addRef(ref)
		}
	}
}

// predeclared returns the builtins of BUILD and .star files,
// which take what they need to know of the code calling them from the thread's evalContext.
func (w *workspace) predeclared() starlark.StringDict {
	if w.builtins != nil {
		return w.builtins
	}
	selectionNames := func(required bool) []string {
		params := selectionParams(fileset.DefaultSelection(), new(*starlark.List), new(*starlark.List), required)
		var names []string
		for i := 0; i < len(params); i += 2 {
			names = append(names, params[i].(string))
		}
		return names
	}
	globParams := append([]string{"name"}, selectionNames(true)...)
	layerParams := []string{"name", "base", "steps?", "command?", "entrypoint?", "tool_caches?"}
	// tool_caches comes last, after the selection's parameters, so that calls giving those by position keep working.
	taskParams := append([]string{"name", "base", "steps?", "source?"}, selectionNames(false)...)
	taskParams = append(taskParams, "tool_caches?")
	w.builtins = starlark.StringDict{
		"glob":          w.declarer("glob", globParams, w.makeGlob),
		"pull":          w.declarer("pull", []string{"name", "image", "tag?"}, w.makePull),
		"layer":         w.declarer("layer", layerParams, w.makeLayer),
		"task":          w.declarer("task", taskParams, w.makeTask),
		"index":         w.declarer("index", []string{"name", "layer", "platforms?"}, w.makeIndex),
		"publication":   w.declarer("publication", []string{"name", "images", "checks?"}, w.makePublication),
		"tool_cache":    w.declarer("tool_cache", []string{"name", "path", "owner?", "group?"}, w.makeToolCache),
		"mount":         starlark.NewBuiltin("mount", w.newMount),
		"add":           track(starlark.NewBuiltin("add", w.newAdd)),
		"dep":           starlark.NewBuiltin("dep", w.newDep),
		"arg":           starlark.NewBuiltin("arg", newArg),
		"param":         starlark.NewBuiltin("param", newParamBuiltin),
		"EXEC_PLATFORM": execPlatformValue{},
	}
	for name, b := range stepBuiltins {
		w.builtins[name] = track(b.(*starlark.Builtin))
	}
	return w.builtins
}

// track wraps a builtin that makes steps, recording each one it makes as a BUILD file runs, with where,
// so that one left out of every target's steps is reported rather than silently ignored.
func track(b *starlark.Builtin) *starlark.Builtin {
	return starlark.NewBuiltin(b.Name(), func(
		thread *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
	) (starlark.Value, error) {
		v, err := b.CallInternal(thread, args, kwargs)
		if in, ok := v.(step); ok {
			if c := contextOf(thread); c.mode == declaring {
				c.made = append(c.made, madeStep{step: in, kind: b.Name(), pos: thread.CallFrame(1).Pos})
			}
		}
		return v, err
	})
}

// declarer returns the builtin kind, which declares a target, taking params, as starlark.UnpackArgs names them,
// of which the first is the target's name, and the target's own params, unless it is a pull.
// A call checks only its shape: its keywords, its required arguments, a string name, unique in the package, and
// params. It keeps its arguments, frozen, from which makeTemplate makes the target's template as the target is
// configured: once, or, if an argument is a function of ctx, in each configuration the target is needed in.
// A tool cache, which is no target and names none, is made, and checked in full, as it is called.
func (w *workspace) declarer(kind string, params []string, makeTemplate makeFunc) *starlark.Builtin {
	return starlark.NewBuiltin(kind, func(
		thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
	) (starlark.Value, error) {
		c := contextOf(thread)
		switch c.mode {
		case configuring:
			return nil, fmt.Errorf("%s: a function of ctx can't declare a target; declare it as the BUILD file runs",
				fn.Name())
		case loadingModule:
			return nil, fmt.Errorf("%s: a .star file can't declare a target as it is loaded; "+
				"declare it in a function that a BUILD file calls", fn.Name())
		}
		d := &declaration{kind: kind, pos: thread.CallFrame(1).Pos, file: c.file}
		if kind != "tool_cache" {
			freeze(args, kwargs, c.clean)
		}
		// A pull or tool cache refuses params as given, before its value is parsed, even an empty dict.
		isParams := func(kv starlark.Tuple) bool { return kv[0] == starlark.String("params") }
		givesParams := slices.ContainsFunc(kwargs, isParams)
		switch {
		case kind == "pull" && givesParams:
			return nil, fmt.Errorf("%s: a pull takes no parameters; only its platform configures it", fn.Name())
		case kind == "tool_cache" && givesParams:
			return nil, fmt.Errorf("%s: a tool cache takes no parameters, since it is mounted, never configured",
				fn.Name())
		}
		var err error
		if kwargs, d.params, err = takeParams(fn.Name(), kwargs); err != nil {
			return nil, err
		}
		var name string
		if kind == "tool_cache" {
			// A tool cache's arguments are checked as the BUILD file runs, so a function among them is refused there.
			if name, d.static, err = makeTemplate(c, fn, args, kwargs); err != nil {
				return nil, err
			}
		} else {
			if name, err = w.record(c, d, params, fn, args, kwargs); err != nil {
				return nil, err
			}
			d.fn, d.args, d.kwargs, d.make = fn, args, kwargs, makeTemplate
		}
		if err := w.declare(c.file, name, d); err != nil {
			return nil, fmt.Errorf("%s: %w", fn.Name(), err)
		}
		return starlark.None, nil
	})
}

// record checks the shape of a call to fn declaring a target, whose parameters are params, as d records it: that it
// gives only fn's keywords, every argument fn requires, and a string name, which it returns.
// It reads no argument's value but to say what it can before the target is made: whether one is a function of ctx,
// whether a task's files can be added, and which steps are listed, for the BUILD file to check that every step it
// makes is.
func (w *workspace) record(
	c *evalContext, d *declaration, params []string, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
) (string, error) {
	values := make([]starlark.Value, len(params))
	pairs := make([]any, 0, 2*len(params))
	for i, p := range params {
		pairs = append(pairs, p, &values[i])
	}
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs, pairs...); err != nil {
		return "", err
	}
	name, ok := values[0].(starlark.String)
	if !ok {
		hint := ""
		if _, ok := values[0].(starlark.Callable); ok {
			hint = "; a target's name can't depend on ctx"
		}
		return "", fmt.Errorf("%s: name is %s, want string%s", fn.Name(), values[0].Type(), hint)
	}
	d.usesCtx = hasFunction(args, kwargs)
	d.given = map[string]starlark.Value{}
	for i, p := range params {
		if values[i] == nil {
			continue
		}
		param := strings.TrimSuffix(p, "?")
		d.given[param] = values[i]
		// A function gives a whole argument, or none of it, so that the rest is data, which the call freezes.
		if _, isFunction := values[i].(starlark.Callable); !isFunction {
			if where, ok := functionWithin(values[i], c.clean, map[any]bool{}); ok {
				slices.Reverse(where)
				return "", fmt.Errorf("%s: %s%s is a function; a function of ctx must give the whole %s, "+
					"not a value within it", fn.Name(), param, strings.Join(where, ""), param)
			}
		}
	}
	d.hasOutput = d.kind == "glob"
	// A task has output given both source and include, as functions or not:
	// makeTask refuses one without the other, and include can't be None.
	if i := slices.Index(params, "source?"); i >= 0 {
		d.hasOutput = values[i] != nil && values[slices.Index(params, "include?")] != nil
	}
	// Steps listed by a function are known only once it runs; those given as they are, now,
	// even in a value that isn't a list, which making the target then refuses.
	if i := slices.Index(params, "steps?"); i >= 0 {
		if _, ok := values[i].(starlark.Callable); ok {
			c.deferred = true
		} else {
			listSteps(c, values[i], map[any]bool{})
		}
	}
	return string(name), nil
}

// identity returns what tells v apart from other values as a map key: v itself, but for a tuple, a slice, which its
// first element and its length name, as a value of no other type.
func identity(v starlark.Value) any {
	type slice struct {
		first *starlark.Value
		n     int
	}
	if t, ok := v.(starlark.Tuple); ok {
		if len(t) == 0 {
			return slice{} // every empty tuple is alike
		}
		return slice{&t[0], len(t)}
	}
	return v
}

// functionWithin reports whether v holds a function, through lists, tuples, dicts, their keys too, dep()s and add()s,
// skipping the values in clean, which hold none, and in seen, which it has walked; and where, as the parts of a path
// within v, innermost first, such as "[1]", which the caller puts after the argument's name.
func functionWithin(v starlark.Value, clean, seen map[any]bool) ([]string, bool) {
	if _, ok := v.(starlark.Callable); ok {
		return nil, true
	}
	key := identity(v)
	switch v.(type) {
	case *starlark.List, *starlark.Dict, *starlark.Set, starlark.Tuple, *depValue, *addStep:
		if clean[key] || seen[key] {
			return nil, false
		}
		seen[key] = true
	}
	var held []starlark.Value
	var part func(i int) string
	switch v := v.(type) {
	case *starlark.List, starlark.Tuple:
		held = slices.Collect(starlark.Elements(v.(starlark.Iterable)))
		part = func(i int) string { return fmt.Sprintf("[%d]", i) }
	case *starlark.Set:
		held = slices.Collect(starlark.Elements(v))
		part = func(i int) string { return fmt.Sprintf("'s element %s", held[i]) }
	case *starlark.Dict:
		items := v.Items()
		for _, item := range items {
			held = append(held, item[0], item[1])
		}
		part = func(i int) string {
			if i%2 == 0 {
				return fmt.Sprintf("'s key %s", items[i/2][0])
			}
			return fmt.Sprintf("[%s]", items[i/2][0])
		}
	case *depValue:
		held, part = v.target.values(), func(i int) string { return "'s dep() argument " + v.target.args[i].name }
	case *addStep:
		held, part = v.target.values(), func(i int) string { return "'s dep() argument " + v.target.args[i].name }
	}
	for i, h := range held {
		if where, ok := functionWithin(h, clean, seen); ok {
			return append(where, part(i)), true
		}
	}
	return nil, false
}

// listSteps records the steps that v, a target's steps as given, holds as listed in c, even within a list, tuple or
// dict within it, or v itself, which making the target refuses, so that it is refused for what it is, not as unlisted.
func listSteps(c *evalContext, v starlark.Value, seen map[any]bool) {
	switch v := v.(type) {
	case step:
		c.listed[v] = true
	case *starlark.List, starlark.Tuple:
		if key := identity(v); !seen[key] {
			seen[key] = true
			for e := range starlark.Elements(v.(starlark.Iterable)) {
				listSteps(c, e, seen)
			}
		}
	case *starlark.Dict:
		if !seen[v] {
			seen[v] = true
			for _, item := range v.Items() {
				listSteps(c, item[1], seen)
			}
		}
	}
}

// freeze freezes the arguments of a call, and every list and dict they hold, as freezeValueIn does,
// so that the target they declare is made from the values the call was given:
// a BUILD file that changes one afterwards fails where it does.
// clean holds the values that earlier calls froze, which hold no function, so that they aren't walked again.
func freeze(args starlark.Tuple, kwargs []starlark.Tuple, clean map[any]bool) {
	for _, v := range args {
		freezeValueIn(v, clean)
	}
	for _, kv := range kwargs {
		freezeValueIn(kv[1], clean)
	}
}

// freezeValueIn freezes the lists, dicts and sets that v is or holds, through lists, dicts, sets, tuples, dep()s and
// add()s, unless one holds a function, directly or through others: a function, and the variables it captures, are left
// as they are, as are the values that hold one.
// Starlark's Freeze of a function freezes what it captures without noting what it has frozen, so it would never end
// where a function captures itself, and would take time exponential in functions that share what they capture.
// A function can only be a whole argument, so a value holding one is refused as its target is made.
// Each value is walked once and frozen after what it holds, so that freezing takes time linear in what v holds, but
// for Starlark's own Freeze of a list or dict, which walks a tuple it holds anew along every path to it.
// It skips the values in clean, which are frozen and hold no function, and adds to it those it freezes.
func freezeValueIn(v starlark.Value, clean map[any]bool) {
	fz := &freezer{index: map[any]int{}, clean: clean}
	fz.walk(v)
	// A value holds a function if one it holds does, which a value it holds back may not have known as it was walked.
	var spread []int
	for i, n := range fz.nodes {
		if n.function {
			spread = append(spread, i)
		}
	}
	for len(spread) > 0 {
		i := spread[len(spread)-1]
		spread = spread[:len(spread)-1]
		for _, h := range fz.nodes[i].holders {
			if !fz.nodes[h].function {
				fz.nodes[h].function = true
				spread = append(spread, h)
			}
		}
	}
	for _, i := range fz.order {
		if n := fz.nodes[i]; !n.function {
			if _, isTuple := n.value.(starlark.Tuple); !isTuple { // immutable, and frozen with what holds it
				n.value.Freeze()
			}
			clean[identity(n.value)] = true
		}
	}
}

// freezer walks the values that a rule's argument holds, for freezeValueIn.
type freezer struct {
	nodes []freezeNode // each value walked
	index map[any]int  // of each value in nodes, by identity
	order []int        // the nodes, each after those it holds but itself, as they are frozen
	clean map[any]bool // values frozen already that hold no function, which aren't walked
}

// freezeNode is a value that holds others, which freezeValueIn freezes unless it holds a function.
type freezeNode struct {
	value    starlark.Value
	holders  []int // the nodes holding it
	function bool  // it holds a function
}

// walk adds v, if it holds other values, and those it holds, to fz.nodes, returning its index there, or -1 for a value
// that holds none, and whether v is a function.
func (fz *freezer) walk(v starlark.Value) (int, bool) {
	switch v := v.(type) {
	case starlark.Callable:
		return -1, true
	case *starlark.List, *starlark.Dict, *starlark.Set:
	case starlark.Tuple:
		if len(v) == 0 {
			return -1, false
		}
	case *depValue:
		if v.target.frozen {
			return -1, false
		}
	case *addStep:
		if v.target.frozen {
			return -1, false
		}
	default:
		return -1, false
	}
	key := identity(v)
	if fz.clean[key] {
		return -1, false
	}
	if i, ok := fz.index[key]; ok {
		return i, false
	}
	var held []starlark.Value
	switch v := v.(type) {
	case *starlark.List:
		held = slices.Collect(v.Elements())
	case *starlark.Dict:
		for _, item := range v.Items() {
			held = append(held, item[0], item[1])
		}
	case *starlark.Set:
		held = slices.Collect(starlark.Elements(v))
	case starlark.Tuple:
		held = v
	case *depValue:
		held = v.target.values()
	case *addStep:
		held = v.target.values()
	}
	// Indexed before what it holds is walked, so that a value holding itself is walked once.
	i := len(fz.nodes)
	fz.index[key] = i
	fz.nodes = append(fz.nodes, freezeNode{value: v})
	var holds []int
	function := false
	for _, h := range held {
		j, isFunction := fz.walk(h)
		function = function || isFunction
		if j >= 0 {
			holds = append(holds, j)
		}
	}
	fz.nodes[i].function = function
	for _, j := range holds {
		fz.nodes[j].holders = append(fz.nodes[j].holders, i)
	}
	fz.order = append(fz.order, i)
	return i, false
}

// takeParams returns kwargs, the keyword arguments of a call to fnName,
// without params, and the parameters that params declares, if given.
func takeParams(fnName string, kwargs []starlark.Tuple) ([]starlark.Tuple, []*param, error) {
	i := slices.IndexFunc(kwargs, func(kv starlark.Tuple) bool { return kv[0] == starlark.String("params") })
	if i < 0 {
		return kwargs, nil, nil
	}
	params, err := parseParams(fnName, kwargs[i][1])
	return slices.Delete(slices.Clone(kwargs), i, i+1), params, err
}

// hasFunction reports whether one of a call's arguments is a function, as of ctx.
func hasFunction(args starlark.Tuple, kwargs []starlark.Tuple) bool {
	for _, v := range args {
		if _, ok := v.(starlark.Callable); ok {
			return true
		}
	}
	for _, kv := range kwargs {
		if _, ok := kv[1].(starlark.Callable); ok {
			return true
		}
	}
	return false
}

// declare records d, the target name, in f.
func (w *workspace) declare(f *buildFile, name string, d *declaration) error {
	if err := checkName(name); err != nil {
		return err
	}
	if earlier, ok := f.decls[name]; ok {
		return fmt.Errorf("name %q is already used at %s", name, earlier.pos)
	}
	f.decls[name] = d
	f.names = append(f.names, name)
	return nil
}

// makeGlob makes the template of a glob(): its files.
func (w *workspace) makeGlob(
	c *evalContext, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
) (string, any, error) {
	var name string
	var include, exclude *starlark.List
	sel := fileset.DefaultSelection()
	params := append([]any{"name", &name}, selectionParams(sel, &include, &exclude, true)...)
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs, params...); err != nil {
		return "", nil, err
	}
	if err := selectFiles(sel, fn.Name(), include, exclude, c.file.dir); err != nil {
		return "", nil, err
	}
	return name, &fileSet{label: label(c.file.pkg, name), dir: c.file.dir, sel: sel}, nil
}

// makePull makes the template of a pull(): the image to pull.
func (w *workspace) makePull(
	c *evalContext, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
) (string, any, error) {
	var name, image string
	tag := "latest"
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs,
		"name", &name,
		"image", &image,
		"tag?", &tag,
	); err != nil {
		return "", nil, err
	}
	ref, err := parsePullImage(image, tag)
	if err != nil {
		return "", nil, fmt.Errorf("%s: %w", fn.Name(), err)
	}
	return name, &pullSpec{label: label(c.file.pkg, name), image: ref}, nil
}

// refUse is what a target that another names is used for, which decides the kinds it may be.
type refUse int

const (
	useFiles     refUse = iota // an add()'s source: a glob, or a task with output
	useImage                   // a base, or an index's manifest: a pull or layer
	useCheck                   // a publication's check: a task
	usePublished               // an image a publication publishes: a pull, layer or index
	useCache                   // a tool cache that mount() names: a tool_cache
)

// resolve finds the target that ref, the argument param of a call to fn, names, setting ref.label:
// one of the kinds that use allows, returning its declaration.
// It checks the arguments ref gives the target too.
// It runs as the target naming ref is made, as it is configured, once every BUILD file it names has run whole,
// so ref may name a target declared anywhere: above or below it, in its own package or another.
// Configuration reports targets that name each other in a cycle.
// No target can name a publication, which only the command line names, and only mount() names a tool cache.
func (w *workspace) resolve(
	c *evalContext, fn *starlark.Builtin, param string, ref *targetRef, use refUse,
) (*declaration, error) {
	return w.resolveAs(c, fn.Name(), param, ref, use)
}

// resolveAs is resolve, with errors saying they are of who, such as a builtin's name, rather than fn.
func (w *workspace) resolveAs(c *evalContext, who, param string, ref *targetRef, use refUse) (*declaration, error) {
	var kinds []string
	// plural lists the targets of kinds, and none says there are none, for a reference naming none.
	var want, rule, plural, none string
	switch use {
	case useFiles:
		kinds, want = []string{"glob", "task"}, "glob() or task()"
		plural, none = "globs and tasks with output", "no globs or tasks with output"
	case useImage:
		kinds, want = []string{"pull", "layer"}, "pull() or layer()"
		plural, none = "pulls and layers", "no pulls or layers"
	case useCheck:
		kinds, want, rule, plural, none = []string{"task"}, "task()", "; a check must be a task", "tasks", "no tasks"
	case usePublished:
		kinds = []string{"pull", "layer", "index"}
		plural, none = "pulls, layers and indexes", "no pulls, layers or indexes"
		want, rule = "pull(), layer() or index()", "; a published target must be a pull, layer or index"
	case useCache:
		kinds, want, plural, none = []string{"tool_cache"}, "tool_cache()", "tool caches", "none"
		rule = "; mount() names a tool cache"
	}
	// A target being made can't use itself, even in another configuration, which would only be a cycle.
	usable := func(d *declaration) bool {
		return slices.Contains(kinds, d.kind) && (use != useFiles || d.hasOutput) && d != c.target
	}
	from, name, err := w.lookup(c.file, ref.ref, ref.pkg)
	if err != nil {
		return nil, fmt.Errorf("%s: %s %q: %w", who, param, ref.ref, err)
	}
	d, ok := from.decls[name]
	// The names the reference could have meant, which a hint chooses from.
	var usableNames []string
	if !ok {
		for _, n := range from.names {
			if usable(from.decls[n]) {
				usableNames = append(usableNames, n)
			}
		}
	}
	switch {
	case !ok && param == "base" && !ref.viaDep && !strings.HasPrefix(ref.ref, "//") &&
		closestName(name, append(slices.Clone(usableNames), scratch)) == scratch:
		// Only a base given as a string can be scratch.
		return nil, fmt.Errorf("%s: %s %q: %s has no target %q; did you mean %q, the empty image?",
			who, param, ref.ref, from.path, name, scratch)
	case !ok && use == useImage && !strings.HasPrefix(ref.ref, "//") && strings.ContainsAny(name, "/:") &&
		closestName(name, usableNames) == "":
		// No name holds either, so this is likely an image, such as busybox:1.36, which only a pull() names,
		// or a label missing its leading "//".
		return nil, fmt.Errorf("%s: %s %q: %s has no target %q; to build on an image, declare a pull() of it, "+
			`or name a target of another package by a label starting with "//"`, who, param, ref.ref, from.path, name)
	case !ok && use == useCache:
		return nil, fmt.Errorf("%s: %s %q: %w", who, param, ref.ref, from.missing(name, true))
	case !ok:
		// What the reference can't use isn't suggested, since naming it would only fail again.
		err := from.missingOf(name, "target", plural, none, usable)
		return nil, fmt.Errorf("%s: %s %q: %w", who, param, ref.ref, err)
	case d.kind == "tool_cache" && use != useCache && ref.pos.IsValid():
		return nil, &posError{pos: ref.pos, err: fmt.Errorf("dep: %w", noTarget(ref.ref))}
	case d.kind == "tool_cache" && use != useCache:
		return nil, fmt.Errorf("%s: %s: %w", who, param, noTarget(ref.ref))
	case d.kind == "publication" && use != useCache:
		return nil, fmt.Errorf("%s: %s %q is a publication(), which can't be depended on; "+
			"%q publishes it, and %q builds what it publishes",
			who, param, ref.ref, "vamos publish", "vamos build")
	case !slices.Contains(kinds, d.kind):
		return nil, fmt.Errorf("%s: %s %q is %s %s(), not %s %s%s",
			who, param, ref.ref, article(d.kind), d.kind, article(kinds[0]), want, rule)
	case use == useFiles && !d.hasOutput:
		return nil, fmt.Errorf("%s: %s %q is a task() without output; give it source and include",
			who, param, ref.ref)
	}
	if err := d.checkArgs(ref); err != nil && ref.pos.IsValid() {
		return nil, &posError{pos: ref.pos, err: fmt.Errorf("dep: %w", err)}
	} else if err != nil {
		return nil, fmt.Errorf("%s: %s: %w", who, param, err)
	}
	ref.label = label(from.pkg, name)
	return d, nil
}

// newAdd implements add(source, dest, owner, group).
// The target that adds it resolves its source as it is made, reporting a mistake where add() is called.
func (w *workspace) newAdd(
	thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
) (starlark.Value, error) {
	var source starlark.Value
	var dest, owner, group string
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs,
		"source", &source,
		"dest", &dest,
		"owner?", &owner,
		"group?", &group,
	); err != nil {
		return nil, err
	}
	c := contextOf(thread)
	if c.file == nil {
		return nil, fmt.Errorf("%s: a .star file can't name a target as it is loaded; "+
			"call add() in a function that a BUILD file calls", fn.Name())
	}

	ref, err := targetRefOf(c, fn.Name(), "source", source)
	if err != nil {
		return nil, err
	}
	if _, _, err := parseLabel(ref.ref, ref.pkg); err != nil {
		return nil, fmt.Errorf("%s: source %q: %w", fn.Name(), ref.ref, err)
	}
	if c.mode == declaring {
		c.addRef(ref)
	}
	if err := graph.CheckPath("dest", dest); err != nil {
		return nil, fmt.Errorf("%s: %w", fn.Name(), err)
	}
	for _, id := range [][2]string{{"owner", owner}, {"group", group}} {
		if strings.Contains(id[1], ":") {
			return nil, fmt.Errorf(`%s: %s %q must not contain ":"; give owner and group apart`,
				fn.Name(), id[0], id[1])
		}
		if strings.Contains(id[1], "\x00") {
			return nil, fmt.Errorf("%s: %s %q must not contain a NUL byte", fn.Name(), id[0], id[1])
		}
	}
	return &addStep{
		target: ref, given: source, dest: dest, owner: owner, group: group, pos: callerPos(thread),
	}, nil
}

// callerPos returns the position of the call to the builtin that thread is running, in the innermost BUILD or .star
// file calling it, as describeEvalError reports an error of the builtin.
func callerPos(thread *starlark.Thread) syntax.Position {
	for depth := 1; depth < thread.CallStackDepth(); depth++ {
		if pos := thread.CallFrame(depth).Pos; pos.Filename() != "<builtin>" && pos.IsValid() {
			return pos
		}
	}
	return thread.CallFrame(1).Pos
}

// makeBase returns the base of a target made by a call to fn, as base, the argument, gives it.
func (w *workspace) makeBase(c *evalContext, fn *starlark.Builtin, base starlark.Value) (targetRef, error) {
	if base == starlark.String(scratch) {
		return targetRef{ref: scratch, label: scratch}, nil
	}
	ref, err := targetRefOf(c, fn.Name(), "base", base)
	if err != nil {
		return targetRef{}, err
	}
	_, err = w.resolve(c, fn, "base", &ref, useImage)
	return ref, err
}

// makeLayer makes the template of a layer().
func (w *workspace) makeLayer(
	c *evalContext, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
) (string, any, error) {
	var name string
	var base starlark.Value
	var steps, command, entrypoint, toolCaches *starlark.List
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs,
		"name", &name,
		"base", &base,
		"steps?", &steps,
		"command?", &command,
		"entrypoint?", &entrypoint,
		"tool_caches?", &toolCaches,
	); err != nil {
		return "", nil, err
	}

	s := &targetSpec{label: label(c.file.pkg, name)}
	var err error
	if s.steps, err = w.stepList(c, fn.Name(), steps); err != nil {
		return "", nil, err
	}
	if s.command, err = stringList(fn.Name(), "command", command); err != nil {
		return "", nil, err
	}
	if entrypoint != nil {
		list, err := stringList(fn.Name(), "entrypoint", entrypoint)
		if err != nil {
			return "", nil, err
		}
		s.entrypoint = &list
	}
	if s.baseRef, err = w.makeBase(c, fn, base); err != nil {
		return "", nil, err
	}
	if s.mounts, err = w.mounts(c, fn, name, toolCaches, s); err != nil {
		return "", nil, err
	}
	return name, s, nil
}

// makeTask makes the template of a task().
func (w *workspace) makeTask(
	c *evalContext, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
) (string, any, error) {
	var name, source string
	var base starlark.Value
	var steps, toolCaches, include, exclude *starlark.List
	sel := fileset.DefaultSelection()
	params := append([]any{
		"name", &name,
		"base", &base,
		"steps?", &steps,
		"source?", &source,
	}, append(selectionParams(sel, &include, &exclude, false), "tool_caches?", &toolCaches)...)
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs, params...); err != nil {
		return "", nil, err
	}

	s := &targetSpec{label: label(c.file.pkg, name), task: true, source: source}
	var err error
	if s.steps, err = w.stepList(c, fn.Name(), steps); err != nil {
		return "", nil, err
	}
	// A task makes files for add() only if it says where they are.
	switch {
	case source == "" && include != nil:
		return "", nil, fmt.Errorf("%s: include needs source, the directory it is relative to", fn.Name())
	case source != "" && include == nil:
		return "", nil, fmt.Errorf("%s: source needs include, the files to collect from it", fn.Name())
	case source != "":
		if !path.IsAbs(source) {
			return "", nil, fmt.Errorf("%s: source %q must be an absolute path", fn.Name(), source)
		}
		if strings.Contains(source, "\x00") {
			return "", nil, fmt.Errorf("%s: source %q must not contain a NUL byte", fn.Name(), source)
		}
		if err := selectFiles(sel, fn.Name(), include, exclude, source); err != nil {
			return "", nil, err
		}
		s.output = &fileSet{label: s.label, sel: sel}
	default:
		for _, kv := range kwargs {
			if param := string(kv[0].(starlark.String)); isSelectionParam(param) {
				return "", nil, fmt.Errorf("%s: %s needs source and include, the files to collect", fn.Name(), param)
			}
		}
	}
	if s.baseRef, err = w.makeBase(c, fn, base); err != nil {
		return "", nil, err
	}
	if s.mounts, err = w.mounts(c, fn, name, toolCaches, s); err != nil {
		return "", nil, err
	}
	return name, s, nil
}

// makeIndex makes the template of an index(): the pulls and layers whose images it lists, each named for its platform.
// layer is one target, for each of platforms, or a dict from each platform to its own target.
func (w *workspace) makeIndex(
	c *evalContext, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
) (string, any, error) {
	var name string
	var layer starlark.Value
	var platformList *starlark.List
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs,
		"name", &name,
		"layer", &layer,
		"platforms?", &platformList,
	); err != nil {
		return "", nil, err
	}
	s := &indexSpec{}
	var where []string // says where each manifest's platform is given, for errors
	add := func(param string, v starlark.Value, platform, at string) error {
		ref, err := targetRefOf(c, fn.Name(), param, v)
		if err != nil {
			return err
		}
		if ref.exec || ref.platform != nil {
			return fmt.Errorf("%s: %s: an index's dep() can't give platform, since %s says what each "+
				"manifest is for", fn.Name(), param, at)
		}
		if _, err := w.resolve(c, fn, param, &ref, useImage); err != nil {
			return err
		}
		if ref.platform, err = parsePlatform(platform); err != nil {
			return fmt.Errorf("%s: %s: %w", fn.Name(), at, err)
		}
		for i, other := range s.manifests {
			if other.platform.String() == ref.platform.String() {
				return fmt.Errorf("%s: %s and %s are both %s; an index has one image for each platform",
					fn.Name(), where[i], at, ref.platform)
			}
		}
		s.manifests, where = append(s.manifests, ref), append(where, at)
		return nil
	}

	if dict, ok := layer.(*starlark.Dict); ok {
		if platformList != nil {
			return "", nil, fmt.Errorf("%s: give platforms, or a dict for layer, whose keys are the platforms, "+
				"not both", fn.Name())
		}
		if dict.Len() == 0 {
			return "", nil, fmt.Errorf("%s: layer must not be empty", fn.Name())
		}
		for _, item := range dict.Items() {
			platform, ok := starlark.AsString(item[0])
			if !ok {
				return "", nil, fmt.Errorf("%s: layer has a key of type %s, want a platform, such as \"linux/arm64\"",
					fn.Name(), item[0].Type())
			}
			param := fmt.Sprintf("layer[%s]", item[0])
			if err := add(param, item[1], platform, param+"'s key"); err != nil {
				return "", nil, err
			}
		}
		return name, s, nil
	}

	if platformList == nil {
		return "", nil, fmt.Errorf("%s: platforms is missing: the platforms to build layer for, such as "+
			"[\"linux/amd64\", \"linux/arm64\"]; or give layer as a dict from each platform to its target", fn.Name())
	}
	platforms, err := stringList(fn.Name(), "platforms", platformList)
	if err != nil {
		return "", nil, err
	}
	if len(platforms) == 0 {
		return "", nil, fmt.Errorf("%s: platforms must not be empty", fn.Name())
	}
	for i, platform := range platforms {
		if err := add("layer", layer, platform, fmt.Sprintf("platforms[%d]", i)); err != nil {
			return "", nil, err
		}
	}
	return name, s, nil
}

// makePublication makes the template of a publication(): the tasks it checks, and the images it publishes,
// by repository, in the dictionary's order.
func (w *workspace) makePublication(
	c *evalContext, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
) (string, any, error) {
	var name string
	var images *starlark.Dict
	var checks *starlark.List
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs,
		"name", &name,
		"images", &images,
		"checks?", &checks,
	); err != nil {
		return "", nil, err
	}
	s := &publicationSpec{}
	if checks != nil {
		for i := range checks.Len() {
			param := fmt.Sprintf("checks[%d]", i)
			ref, err := targetRefOf(c, fn.Name(), param, checks.Index(i))
			if err != nil {
				return "", nil, err
			}
			if _, err := w.resolve(c, fn, param, &ref, useCheck); err != nil {
				return "", nil, err
			}
			s.checks = append(s.checks, ref)
		}
	}
	if images.Len() == 0 {
		return "", nil, fmt.Errorf("%s: images is empty, so it publishes no image; "+
			"give a dict from each repository to the pull, layer or index published there", fn.Name())
	}
	for _, item := range images.Items() {
		repo, ok := starlark.AsString(item[0])
		if !ok {
			return "", nil, fmt.Errorf("%s: images has a key of type %s, want a repository, such as %q",
				fn.Name(), item[0].Type(), "ghcr.io/team/app")
		}
		repository, err := parseRepository(repo)
		if err != nil {
			return "", nil, fmt.Errorf("%s: images: %w", fn.Name(), err)
		}
		for _, other := range s.images {
			if other.repository.Name() == repository.Name() {
				return "", nil, fmt.Errorf("%s: images has %q twice, as %q and %q; "+
					"a repository gets one image of a publication",
					fn.Name(), repository.Name(), other.key, repo)
			}
		}
		param := fmt.Sprintf("images[%s]", item[0])
		ref, err := targetRefOf(c, fn.Name(), param, item[1])
		if err != nil {
			return "", nil, err
		}
		if _, err := w.resolve(c, fn, param, &ref, usePublished); err != nil {
			return "", nil, err
		}
		s.images = append(s.images, publishedImageRef{repository: repository, key: repo, target: ref})
	}
	return name, s, nil
}

// parseRepository returns the repository that repo, a key of a publication's images, names:
// a lowercase registry repository name, with no tag or digest, which come from publish's --tag.
func parseRepository(repo string) (reference.Named, error) {
	if repo == "" {
		return nil, errors.New("a repository must not be empty")
	}
	// The reference's own parsing allows an uppercase registry host, which names the same registry as lowercase.
	if repo != strings.ToLower(repo) {
		return nil, fmt.Errorf("repository %q must be lowercase", repo)
	}
	named, err := reference.ParseNormalizedNamed(repo)
	if err != nil {
		return nil, fmt.Errorf("repository %q: %w", repo, err)
	}
	if _, ok := named.(reference.Tagged); ok {
		return nil, fmt.Errorf("repository %q must not include a tag; the tag comes from --tag", repo)
	}
	if _, ok := named.(reference.Digested); ok {
		return nil, fmt.Errorf("repository %q must not include a digest; publish writes the tags --tag gives", repo)
	}
	return named, nil
}

// target returns the label and kind of the target that ref, given on the command line,
// names from the package of -C, loading the BUILD file that declares it, and making its template unless that needs a
// configuration.
// A tool cache is refused, since it is no target, whatever the command does with what it names.
func (w *workspace) target(ref string) (string, string, error) {
	pkg, name, err := parseLabel(ref, w.current)
	if err != nil {
		return "", "", err
	}
	f, err := w.load(pkg)
	if err != nil {
		return "", "", err
	}
	d, ok := f.decls[name]
	if !ok {
		return "", "", f.missing(name, false)
	}
	if d.kind == "tool_cache" {
		return "", "", noTarget(ref)
	}
	// A mistake in the target's rule is reported before what the command makes of its kind,
	// as it is for a target whose rule needs a configuration, made as the command line configures it.
	if !d.usesCtx {
		if _, err := w.template(d, confLabel{label: label(pkg, name)}); err != nil {
			return "", "", err
		}
	}
	return label(pkg, name), d.kind, nil
}

// buildTargets returns the labels and kinds of the targets to build that names, given on the command line, name,
// without repeats.
func (w *workspace) buildTargets(names []string) ([]string, []string, error) {
	var labels, kinds []string
	for _, name := range names {
		l, kind, err := w.target(name)
		if err != nil {
			return nil, nil, err
		}
		if kind == "glob" {
			return nil, nil, fmt.Errorf("%q is a glob(), whose files are read by the targets that add them; "+
				"name a pull(), layer(), task(), index() or publication() to build", name)
		}
		if !slices.Contains(labels, l) {
			labels, kinds = append(labels, l), append(kinds, kind)
		}
	}
	return labels, kinds, nil
}

// imageTarget returns the label and kind of the target whose image,
// or index of images, to send that name, given on the command line, names.
func (w *workspace) imageTarget(name string) (string, string, error) {
	l, kind, err := w.target(name)
	if err != nil {
		return "", "", err
	}
	switch kind {
	case "glob":
		return "", "", fmt.Errorf("%q is a glob(), whose files are read by the targets that add them, not an image; "+
			"name a pull(), layer() or index() to send", name)
	case "publication":
		return "", "", publicationRefused(name)
	case "task":
		hint := ""
		if w.declaration(l).hasOutput {
			hint = `, or export the task's files with "vamos export"`
		}
		return "", "", fmt.Errorf("%q is a task(), whose image is discarded once it runs; "+
			"name a pull(), layer() or index() to send%s", name, hint)
	}
	return l, kind, nil
}

// filesTarget returns the label of the task whose files to export that name, given on the command line, names.
func (w *workspace) filesTarget(name string) (string, error) {
	l, kind, err := w.target(name)
	if err != nil {
		return "", err
	}
	switch kind {
	case "task":
		if !w.declaration(l).hasOutput {
			return "", fmt.Errorf("%q is a task() without source, so it has no files to export; "+
				"%q runs it", name, "vamos build "+name)
		}
		return l, nil
	case "glob":
		return "", fmt.Errorf("%q is a glob(), whose files are already in the project; export writes a task's files",
			name)
	case "index":
		return "", fmt.Errorf("%q is an index(), not a task(); export writes a task's files", name)
	case "publication":
		return "", publicationRefused(name)
	}
	return "", fmt.Errorf(`%q is %s %s(), not a task(); export writes a task's files, and "vamos save" writes an image`,
		name, article(kind), kind)
}

// publicationRefused reports that name, given to a command that sends one image or a task's files, is a publication.
func publicationRefused(name string) error {
	return fmt.Errorf("%q is a publication(), which publishes images to repositories; "+
		"%q publishes it, and %q builds them",
		name, "vamos publish "+name+" --tag ...", "vamos build "+name)
}

// publicationTarget returns the label of the publication to publish that name, given on the command line, names.
func (w *workspace) publicationTarget(name string) (string, error) {
	l, kind, err := w.target(name)
	if err != nil {
		return "", err
	}
	if kind != "publication" {
		return "", fmt.Errorf("%q is %s %s(), not a publication(); publish takes a publication",
			name, article(kind), kind)
	}
	return l, nil
}

// declaration returns the declaration of the target with label l, whose package is loaded.
func (w *workspace) declaration(l string) *declaration {
	rest := strings.TrimPrefix(l, "//")
	i := strings.LastIndexByte(rest, ':')
	return w.packages[rest[:i]].decls[rest[i+1:]]
}

// readGlobs reads the files of each glob in g, whose files are globs, by node, with store,
// which returns their tree, setting the trees of g's Source nodes.
// A glob's files are read once, before anything is built,
// so that every target that adds them gets the same files, as the build cache stored them,
// even if they change during the build.
// Each glob is a span of ctx's trace, which those that add its files point back to.
func (w *workspace) readGlobs(
	ctx context.Context, g *graph.Graph, globs map[graph.Ref]*fileSet,
	store func(ctx context.Context, dir string, sel *fileset.Selection) (cache.Blob, fileset.Stats, error),
) error {
	for _, ref := range slices.Sorted(maps.Keys(globs)) {
		set := globs[ref]
		_, span := tracing.Start(ctx, g.Node(ref).Desc,
			"dir", set.dir,
			"include", set.sel.Include,
			"exclude", set.sel.Exclude)
		tree, stats, err := store(ctx, set.dir, set.sel)
		if err != nil {
			err = fmt.Errorf("%s: glob: %w", w.declaration(set.label).pos, err)
			if set.configuring != "" {
				err = fmt.Errorf("%w (configuring %s)", err, set.configuring)
			}
			span.EndWith(err)
			return err
		}
		span.Set(stats.Args()...)
		span.Set("tree", tree.Digest.String(), "tree bytes", tree.Size)
		span.Produce(graph.TreeFlow(tree))
		span.End()
		g.Node(ref).Source.Tree = tree
	}
	return nil
}

// closestName returns the name in names nearest to name, as a likely misspelling of it, or "" if none is near enough.
func closestName(name string, names []string) string {
	closest, best := "", max(2, len(name)/3)+1
	for _, candidate := range names {
		if d := editDistance(name, candidate); d < best {
			closest, best = candidate, d
		}
	}
	return closest
}

// editDistance returns the number of single-byte insertions, deletions and substitutions that turn a into b.
func editDistance(a, b string) int {
	row := make([]int, len(b)+1)
	for j := range row {
		row[j] = j
	}
	for i := 1; i <= len(a); i++ {
		diagonal := row[0]
		row[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			diagonal, row[j] = row[j], min(row[j]+1, row[j-1]+1, diagonal+cost)
		}
	}
	return row[len(b)]
}

// parsePullImage returns the image named by a pull() call's image and tag arguments.
func parsePullImage(image, tag string) (reference.NamedTagged, error) {
	if image == "" {
		return nil, errors.New("image must not be empty")
	}
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return nil, fmt.Errorf("invalid image %q: %w", image, err)
	}
	if tagged, ok := named.(reference.Tagged); ok {
		return nil, fmt.Errorf("image %q must not include a tag; use tag=%q", image, tagged.Tag())
	}
	if _, ok := named.(reference.Digested); ok {
		return nil, fmt.Errorf("image %q must not include a digest", image)
	}
	ref, err := reference.WithTag(named, tag)
	if err != nil {
		return nil, fmt.Errorf("invalid tag %q: %w", tag, err)
	}
	return ref, nil
}

// article returns the indefinite article for a builtin's name: "an" for index, "a" for the others.
func article(kind string) string {
	if kind == "index" {
		return "an"
	}
	return "a"
}

// describeEvalError reports e at the innermost position within a BUILD file,
// in the one-line "file:line:col: message" form of compiler errors.
func describeEvalError(e *starlark.EvalError) string {
	if at, ok := errors.AsType[*posError](e); ok {
		return at.Error()
	}
	for _, frame := range slices.Backward(e.CallStack) {
		if pos := frame.Pos; pos.Filename() != "<builtin>" && pos.IsValid() {
			return fmt.Sprintf("%s: %s", pos, e.Msg)
		}
	}
	return e.Msg
}

// lstat describes the file at path in fsys without following a final symbolic link, if fsys supports links.
func lstat(fsys afero.Fs, path string) (fs.FileInfo, bool, error) {
	if l, ok := fsys.(afero.Lstater); ok {
		return l.LstatIfPossible(path)
	}
	info, err := fsys.Stat(path)
	return info, false, err
}

// stringList converts an optional Starlark list into a []string.
func stringList(fnName, param string, list *starlark.List) ([]string, error) {
	if list == nil {
		return nil, nil
	}
	out := make([]string, 0, list.Len())
	for i := 0; i < list.Len(); i++ {
		s, ok := starlark.AsString(list.Index(i))
		if !ok {
			return nil, fmt.Errorf("%s: %s[%d] is %s, want string%s",
				fnName, param, i, list.Index(i).Type(), functionHint(list.Index(i)))
		}
		if strings.Contains(s, "\x00") {
			return nil, fmt.Errorf("%s: %s[%d] must not contain a NUL byte", fnName, param, i)
		}
		out = append(out, s)
	}
	return out, nil
}

// interrupted returns ctx's error if it has ended, so that a Starlark evaluation it cancelled reports
// the interruption as such, rather than as a failure of the BUILD file, and err otherwise.
func (w *workspace) interrupted(err error) error {
	if w.ctx != nil && w.ctx.Err() != nil {
		return w.ctx.Err()
	}
	return err
}

// cancelWith cancels thread when ctx ends, so that a BUILD file that runs on, as a loop that never ends does,
// stops when the command is interrupted, and returns what stops watching, which the caller defers.
// Starlark checks for cancellation between statements, so the thread stops promptly.
func cancelWith(ctx context.Context, thread *starlark.Thread) func() {
	if ctx == nil || ctx.Done() == nil {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			thread.Cancel("interrupted: " + ctx.Err().Error())
		case <-done:
		}
	}()
	return func() { close(done) }
}
