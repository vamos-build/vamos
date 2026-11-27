package load

import (
	"cmp"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/vamos-build/vamos/internal/graph"

	"go.starlark.net/starlark"
)

// toolCacheSpec is a tool cache, declared by a call to tool_cache(): a directory that the executor keeps between
// builds, or gives empty, and lends to the run() steps of the targets that mount it.
// It is no target: nothing builds it, and only mount() names it.
type toolCacheSpec struct {
	path         string // where a mount that doesn't override it puts it
	owner, group uint32 // of its directory; 0, root, if the rule gives neither
}

// makeToolCache makes the template of a tool_cache(): its path, and its owner and group.
// It is never given a function of ctx, so a mistake in its arguments fails as the BUILD file runs.
func (w *workspace) makeToolCache(
	_ *evalContext, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
) (string, any, error) {
	var name, p string
	var owner, group starlark.Value
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs,
		"name", &name,
		"path", &p,
		"owner?", &owner,
		"group?", &group,
	); err != nil {
		return "", nil, err
	}
	if err := graph.CheckCachePath("path", p); err != nil {
		return "", nil, fmt.Errorf("%s: %w", fn.Name(), err)
	}
	s := &toolCacheSpec{path: p}
	// None is no owner or group, so that a function can pass on its own arguments' defaults.
	if owner == starlark.None {
		owner = nil
	}
	if group == starlark.None {
		group = nil
	}
	switch {
	case owner == nil && group == nil:
	case owner == nil || group == nil:
		return "", nil, fmt.Errorf("%s: owner and group are given together, or neither, "+
			"since either alone would leave the other to a default", fn.Name())
	default:
		var err error
		if s.owner, err = parseID(fn.Name(), "owner", owner); err != nil {
			return "", nil, err
		}
		if s.group, err = parseID(fn.Name(), "group", group); err != nil {
			return "", nil, err
		}
	}
	return name, s, nil
}

// parseID returns the numeric ID that v, the argument param of a call to fnName, gives as a string.
func parseID(fnName, param string, v starlark.Value) (uint32, error) {
	s, ok := starlark.AsString(v)
	if !ok {
		return 0, fmt.Errorf(`%s: %s is %s, want a numeric ID as a string, such as "1000"`, fnName, param, v.Type())
	}
	id, err := graph.ParseID(param, s)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", fnName, err)
	}
	return id, nil
}

// mountValue is the value of a call to mount(): a tool cache that a target mounts, named by its label,
// at the cache's own path or at path, if given.
type mountValue struct {
	ref  string // the label as given
	pkg  string // the package ref is relative to
	path string // where to mount it instead of the cache's path, or ""
}

func (v *mountValue) Type() string          { return "mount" }
func (v *mountValue) Freeze()               {} // immutable
func (v *mountValue) Truth() starlark.Bool  { return true }
func (v *mountValue) Hash() (uint32, error) { return 0, errors.New("unhashable type: mount") }

func (v *mountValue) String() string {
	if v.path == "" {
		return fmt.Sprintf("mount(%s)", starlark.String(v.ref))
	}
	return fmt.Sprintf("mount(%s, path = %s)", starlark.String(v.ref), starlark.String(v.path))
}

// newMount implements mount(label, path), which a task's or layer's tool_caches lists.
// Labels are relative to the package of the BUILD file running, as dep()'s are.
// The target that mounts it resolves the label and checks the path, so that its mistakes name the target.
func (w *workspace) newMount(
	thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
) (starlark.Value, error) {
	var ref string
	var p starlark.Value
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs, "label", &ref, "path?", &p); err != nil {
		return nil, err
	}
	c := contextOf(thread)
	if c.file == nil && !strings.HasPrefix(ref, "//") {
		return nil, fmt.Errorf(`%s: label %q: as a .star file is loaded, labels must start with "//"`, fn.Name(), ref)
	}
	v := &mountValue{ref: ref}
	if c.file != nil {
		v.pkg = c.file.pkg
	}
	if _, _, err := parseLabel(ref, v.pkg); err != nil {
		return nil, fmt.Errorf("%s: %w", fn.Name(), err)
	}
	// None is no path, as for tool_cache()'s owner and group, so that a function can pass on its own default.
	if p != nil && p != starlark.None {
		s, ok := starlark.AsString(p)
		if !ok {
			return nil, fmt.Errorf("%s: path is %s, want string", fn.Name(), p.Type())
		}
		if s == "" {
			return nil, fmt.Errorf("%s: path must not be empty; leave it out to mount the cache at its own path",
				fn.Name())
		}
		v.path = s
	}
	return v, nil
}

// mounts returns the tool caches that list, the tool_caches argument of the call to fn declaring the target name,
// mounts, sorted by path, refusing what loading can see would make a mount conflict: a path a cache can't be mounted
// at, one path or one cache twice, nested paths, and s's steps and output, which must be filled in, putting a path
// into a cache, or giving a cache no run() step to be lent to.
func (w *workspace) mounts(
	c *evalContext, fn *starlark.Builtin, name string, list *starlark.List, s *targetSpec,
) ([]graph.ToolCache, error) {
	who := fmt.Sprintf("%s %q", fn.Name(), name)
	mounts, err := w.resolveMounts(c, who, list)
	if err != nil || mounts == nil {
		return nil, err
	}
	if err := w.checkMountConflicts(who, mounts, s); err != nil {
		return nil, err
	}
	return mounts, nil
}

// resolveMounts returns the tool caches that list, the tool_caches argument of the target who, mounts, sorted by
// path, refusing a path a cache can't be mounted at, one path or one cache twice, and nested paths.
// A target given by a function of ctx resolves a list given as it is as it is declared, as a static one does,
// so that it names only caches declared earlier, and its mistakes are reported then.
func (w *workspace) resolveMounts(c *evalContext, who string, list *starlark.List) ([]graph.ToolCache, error) {
	if list == nil || list.Len() == 0 {
		return nil, nil
	}
	var mounts []graph.ToolCache
	for i := range list.Len() {
		param := fmt.Sprintf("tool_caches[%d]", i)
		v, ok := list.Index(i).(*mountValue)
		if !ok {
			return nil, fmt.Errorf(`%s: %s is %s, want mount(), such as mount("//tools:go-build")%s`,
				who, param, list.Index(i).Type(), functionHint(list.Index(i)))
		}
		ref := targetRef{ref: v.ref, pkg: v.pkg}
		d, err := w.resolveAs(c, who, param, &ref, useCache)
		if err != nil {
			return nil, err
		}
		cache := d.static.(*toolCacheSpec)
		m := graph.ToolCache{Path: cache.path, Label: ref.label, Owner: cache.owner, Group: cache.group}
		if v.path != "" {
			if err := graph.CheckCachePath("path", v.path); err != nil {
				return nil, fmt.Errorf("%s: %s: %w", who, v, err)
			}
			m.Path = v.path
		}
		for _, other := range mounts {
			switch {
			case other.Label == m.Label:
				return nil, fmt.Errorf("%s mounts %s twice, at %s and %s; a target mounts each tool cache once",
					who, w.display(m.Label), other.Path, m.Path)
			case other.Path == m.Path:
				return nil, fmt.Errorf("%s mounts %s and %s both at %s; each tool cache needs a path of its own",
					who, w.display(other.Label), w.display(m.Label), m.Path)
			case graph.Within(m.Path, other.Path), graph.Within(other.Path, m.Path):
				outer, inner := other, m
				if graph.Within(other.Path, m.Path) {
					outer, inner = m, other
				}
				return nil, fmt.Errorf("%s mounts %s at %s, within %s at %s; tool caches can't be nested",
					who, w.display(inner.Label), inner.Path, w.display(outer.Label), outer.Path)
			}
		}
		mounts = append(mounts, m)
	}
	slices.SortFunc(mounts, func(a, b graph.ToolCache) int { return cmp.Compare(a.Path, b.Path) })
	return mounts, nil
}

// literalPath returns p, a step's path, as it is once its "$$" escapes are expanded, and whether it refers to no
// variable, which only the image's environment settles.
func literalPath(p string) (string, bool) {
	variable := false
	expanded, err := graph.ExpandVars(p, func(string) string { variable = true; return "" })
	return expanded, err == nil && !variable
}

// checkMountConflicts refuses what s, the target who, does that loading can see would put a path in one of
// mounts: a literal add() destination or workdir() within one, once cleaned, a secret's file at, within or above one,
// or a task's source within one with no ".." component, which the executor resolves through symbolic links before
// applying it; and a target with no run() step to lend them to.
// A path with a variable, though not one with only "$$", which is a literal "$", or a source with "..", is left to the
// executor, which knows what they resolve to.
func (w *workspace) checkMountConflicts(who string, mounts []graph.ToolCache, s *targetSpec) error {
	within := func(p string) (graph.ToolCache, bool) {
		for _, m := range mounts {
			if graph.Within(p, m.Path) {
				return m, true
			}
		}
		return graph.ToolCache{}, false
	}
	runs := false
	for _, in := range s.steps {
		switch in := in.(type) {
		case *addStep:
			dest, ok := literalPath(in.dest)
			if !ok {
				continue
			}
			if m, ok := within(path.Clean(dest)); ok {
				return fmt.Errorf("%s: %s puts files within the tool cache %s, mounted at %s; "+
					"what a step writes there stays in the cache", who, in, w.display(m.Label), m.Path)
			}
		case *workdirStep:
			dir, ok := literalPath(in.dir)
			if !ok {
				continue
			}
			if m, ok := within(path.Clean(dir)); ok {
				return fmt.Errorf("%s: %s is within the tool cache %s, mounted at %s; "+
					"the working directory can't be in a tool cache", who, in, w.display(m.Label), m.Path)
			}
		case *runStep:
			runs = true
			for _, secret := range in.secrets {
				if secret.File == "" {
					continue
				}
				for _, m := range mounts {
					if graph.Within(secret.File, m.Path) || graph.Within(m.Path, secret.File) {
						return fmt.Errorf("%s: %s gives secret %q as the file %s, which overlaps the tool cache %s, "+
							"mounted at %s", who, in, secret.ID, secret.File, w.display(m.Label), m.Path)
					}
				}
			}
		}
	}
	if s.task && s.source != "" && !slices.Contains(strings.Split(s.source, "/"), "..") {
		if m, ok := within(path.Clean(s.source)); ok {
			return fmt.Errorf("%s: source %q is within the tool cache %s, mounted at %s; "+
				"a task's output can't be in a tool cache", who, s.source, w.display(m.Label), m.Path)
		}
	}
	if !runs {
		return fmt.Errorf("%s mounts tool caches but has no run() step; tool caches are mounted only for run() steps",
			who)
	}
	return nil
}

// checkNamedFor refuses the target that d declares, configured as cl, if it mounts tool caches and the workspace has
// no name, which a cache is kept under. It is checked as the target is configured, so that an unused tool_cache rule,
// or a target nothing builds, costs nothing.
func (w *workspace) checkNamedFor(d *declaration, cl confLabel, s *targetSpec) error {
	if len(s.mounts) == 0 || w.name != "" {
		return nil
	}
	return fmt.Errorf("%s: %s mounts tool caches, so the workspace needs a name in WORKSPACE, "+
		`such as workspace(name = "com.example.app")`, d.pos, w.describe(d.kind, cl))
}

// noTarget reports that ref, a label given where a target is wanted, names a tool cache.
func noTarget(ref string) error {
	return fmt.Errorf("%q is a tool_cache(), which is no target: a tool cache is mounted with mount(), "+
		"in a task's or layer's tool_caches", ref)
}
