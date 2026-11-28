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

	"github.com/spf13/afero"
)

// Pull is a pull() target, and the image and tag it pulls, as the lockfile keys its pin; it is comparable.
type Pull struct {
	Label string // as messages name it: "base" or "//ui:base"
	Image string // normalized, with its tag, as graph.Pull.Image gives it
	// Configured says that its image or tag is a function of ctx, so that Image is the one it pulls in the
	// configuration that the command line gives it, and it may pull others in others.
	Configured bool
}

// Pulls returns the pulls of the workspace holding req.Dir, and its root, where its lockfile is:
// the targets that req.Targets names, which must be pulls, or, if it names none, every pull that a BUILD file of the
// workspace declares, each in the configuration that the command line gives it, as building it would.
// Every BUILD file is found by walking the workspace from its root, past hidden directories, symbolic links and other
// workspaces, those beneath it with a WORKSPACE file of their own; a package that a target found uses, even through a
// function of ctx, is found too, by configuring every target as building it would. unsure says, if it may be, why a
// pull may be missing, such as a target that couldn't be configured so, whose functions of ctx may use pulls that
// weren't found.
// The lockfile is read, as loading reads it, so that a malformed one fails before anything else is done.
func Pulls(ctx context.Context, fsys afero.Fs, req Request) (pulls []Pull, root, unsure string, err error) {
	w, err := open(ctx, fsys, req)
	if err != nil {
		return nil, "", "", err
	}
	if len(req.Targets) == 0 {
		pulls, unsure, err := w.everyPull(req)
		if err != nil {
			return nil, "", "", err
		}
		return pulls, w.root, unsure, nil
	}
	var labels []string
	for _, name := range req.Targets {
		l, kind, err := w.target(name)
		if err != nil {
			return nil, "", "", err
		}
		if kind != "pull" {
			return nil, "", "", fmt.Errorf("%q is %s %s(), and only pulls are updated", name, article(kind), kind)
		}
		if slices.Contains(labels, l) {
			continue
		}
		labels = append(labels, l)
		p, err := w.commandPull(l, req)
		if err != nil {
			return nil, "", "", err
		}
		pulls = append(pulls, p)
	}
	return pulls, w.root, "", nil
}

// commandPull returns the pull l in the configuration that the command line req gives it.
func (w *workspace) commandPull(l string, req Request) (Pull, error) {
	c, err := w.commandConfigOf(l, "pull", req, false)
	if err != nil {
		return Pull{}, err
	}
	d := w.declaration(l)
	tmpl, err := w.template(d, confLabel{l, c})
	if err != nil {
		return Pull{}, err
	}
	return Pull{Label: w.display(l), Image: tmpl.(*pullSpec).image.String(), Configured: d.usesCtx}, nil
}

// everyPull returns every pull that a BUILD file of the workspace declares, or that a package such a BUILD file uses
// declares, by package, then in the order declared: in the configuration the command line gives it, in a package the
// walk finds, and elsewhere in each that the targets using it configure it in, or else the command line's; and, if a
// package its targets use may be missing, why, as unsure describes it: what may use, or be, a pull that is.
// Every target of a package that the walk finds, and every target those reach without a function of ctx, is checked as
// building it would: its rule is made, or, if that needs a configuration, the targets its arguments name as they are
// given are resolved, which finds the packages it names even where the command line can't configure the target, and
// a mistake fails, as building the target would, as do targets that use each other so in a cycle, which no
// configuration can build. Then every target is configured as building it would, until no new
// package is loaded, so that a package used only through a function of ctx is found. What else these lead to is unsure,
// never a failure, since unsure keeps every pin: a target that can't be configured so, a target that only a function
// of ctx leads to whose rule has a mistake, and a package that a label given to add() or dep() as a BUILD file or a
// .star file it loads ran names, which a function of ctx that can't run here may use, and which is loaded only to find
// it, but can't be.
func (w *workspace) everyPull(req Request) (pulls []Pull, unsure string, err error) {
	pkgs, err := w.packageDirs()
	if err != nil {
		return nil, "", err
	}
	var reached []string // the targets checked as building them would, in the order found
	strict := map[string]bool{}
	walked := map[string]bool{}
	for _, pkg := range pkgs {
		f, err := w.load(pkg)
		if err != nil {
			return nil, "", err
		}
		walked[pkg] = true
		for _, name := range f.names {
			reached, strict[label(pkg, name)] = append(reached, label(pkg, name)), true
		}
	}
	uses := map[string][]use{} // by target checked, the targets it uses as given
	for i := 0; i < len(reached); i++ {
		used, err := w.checkRule(reached[i])
		if err != nil {
			return nil, "", err
		}
		uses[reached[i]] = used
		for _, u := range used {
			if !strict[u.label] {
				reached, strict[u.label] = append(reached, u.label), true
			}
		}
	}
	// Targets that use each other as they are given, in a cycle, can't be built in any configuration.
	if err := w.staticCycle(reached, uses); err != nil {
		return nil, "", err
	}

	// A mistake beyond them is unsure, and an interruption always ends the sweep.
	note := func(err error, why string) error {
		if err := w.ctx.Err(); err != nil {
			return err
		}
		if unsure == "" {
			unsure = why
		}
		return nil
	}
	cr := &configurer{w: w, pulls: map[confLabel]*pullSpec{}, targets: map[confLabel]*targetSpec{},
		globs: map[confLabel]*fileSet{}, indexes: map[confLabel][]confLabel{}, pinning: true}
	configured := map[string]bool{}
	searched := map[string]bool{}   // packages whose labels given to add() and dep() are followed
	unloadable := map[string]bool{} // packages those labels name that couldn't be loaded
	broken := map[string]bool{}     // targets beyond those checked whose rules have a mistake
	for grew := true; grew; {
		grew = false
		for _, pkg := range slices.Sorted(maps.Keys(w.packages)) {
			f := w.packages[pkg]
			if !searched[pkg] {
				searched[pkg], grew = true, true
				for _, named := range w.namedPackages(f) {
					if unloadable[named] {
						continue
					}
					if _, err := w.load(named); err != nil {
						unloadable[named] = w.ctx.Err() == nil // which another package naming it needn't load again
						why := fmt.Sprintf("the package %q, which couldn't be loaded (%v), may use a pull that does",
							named, err)
						if err := note(err, why); err != nil {
							return nil, "", err
						}
					}
				}
			}
			for _, name := range f.names {
				l := label(pkg, name)
				if configured[l] {
					continue
				}
				configured[l], grew = true, true
				var err error
				if !strict[l] {
					if _, err = w.checkRule(l); err != nil {
						broken[l] = true
					}
				}
				if err == nil {
					// A target whose configuration fails leaves none of what it configured, which no build would use.
					forget, keep := cr.mark()
					if err = w.configureTarget(cr, l, req); err != nil {
						forget()
					} else {
						keep()
					}
				}
				if err != nil {
					why := fmt.Sprintf("%q, which couldn't be configured as building it would, "+
						"may use a pull that does", w.display(l))
					if err := note(err, why); err != nil {
						return nil, "", err
					}
				}
			}
		}
	}
	configuredPulls := map[string][]*pullSpec{}
	for _, p := range cr.pulls {
		configuredPulls[p.label] = append(configuredPulls[p.label], p)
	}
	// Those of every package loaded, those the walk skips among them, as a hidden one that another uses.
	for _, pkg := range slices.Sorted(maps.Keys(w.packages)) {
		f := w.packages[pkg]
		for _, name := range f.names {
			l := label(pkg, name)
			if f.decls[name].kind != "pull" || broken[l] {
				continue
			}
			// A pull is pinned in each configuration that the targets using it configure it in, as building them
			// would, and in the command line's, which is a pull's own in a package the walk finds; elsewhere, it is
			// taken only if no target used the pull, and a pull given a function of ctx that fails in it is unsure.
			var used []Pull
			for _, p := range configuredPulls[l] {
				found := Pull{Label: w.display(l), Image: p.image.String(), Configured: f.decls[name].usesCtx}
				if !slices.Contains(used, found) {
					used = append(used, found)
				}
			}
			slices.SortFunc(used, func(a, b Pull) int { return strings.Compare(a.Image, b.Image) })
			if walked[pkg] || len(used) == 0 {
				// The command line's configuration, unless a target used the pull in it already.
				var p Pull
				var err error
				c, cerr := w.commandConfigOf(l, "pull", req, false)
				if spec, ok := cr.pulls[confLabel{l, c}]; cerr == nil && ok {
					p = Pull{Label: w.display(l), Image: spec.image.String(), Configured: f.decls[name].usesCtx}
				} else {
					p, err = w.commandPull(l, req)
				}
				if err != nil && walked[pkg] {
					return nil, "", err
				} else if err != nil {
					why := fmt.Sprintf("the pull %q, which couldn't be configured as building it would, may be one "+
						"that does", w.display(l))
					if err := note(err, why); err != nil {
						return nil, "", err
					}
					continue
				}
				if !slices.Contains(used, p) {
					pulls = append(pulls, p)
				}
			}
			pulls = append(pulls, used...)
		}
	}
	return pulls, unsure, nil
}

// staticCycle reports targets among reached that use each other in a cycle by uses, the targets each uses as they
// are given, which every configuration of them has, at the declaration of the target whose reference closes it, as
// configuring them reports one.
func (w *workspace) staticCycle(reached []string, uses map[string][]use) error {
	const (
		unseen = iota
		walking
		done
	)
	state := map[string]int{}
	var path []string
	var visit func(l, what string) error
	visit = func(l, what string) error {
		switch state[l] {
		case walking:
			cycle := append(slices.Clone(path[slices.Index(path, l):]), l)
			closing := w.declaration(path[len(path)-1])
			displayed := make([]string, len(cycle))
			for i, c := range cycle {
				displayed[i] = w.display(c)
			}
			return fmt.Errorf("%s: %s: %s: targets use each other in a cycle: %s", closing.pos, closing.kind, what,
				strings.Join(displayed, " -> "))
		case done:
			return nil
		}
		state[l] = walking
		path = append(path, l)
		for _, u := range uses[l] {
			if err := visit(u.label, u.what); err != nil {
				return err
			}
		}
		path = path[:len(path)-1]
		state[l] = done
		return nil
	}
	for _, l := range reached {
		if err := visit(l, ""); err != nil {
			return err
		}
	}
	return nil
}

// checkRule checks the rule of the target l as building it would, without a configuration: it makes its template, or,
// if that needs a configuration, resolves the targets its arguments name as they are given; and returns the targets it
// uses so.
func (w *workspace) checkRule(l string) ([]use, error) {
	d := w.declaration(l)
	if d.usesCtx {
		return w.givenRefs(d, l)
	}
	tmpl, err := w.template(d, confLabel{label: l})
	if err != nil {
		return nil, err
	}
	return usedBy(tmpl), nil
}

// use is a target that another uses, by its label, and the reference that names it, such as base "b", as a cycle
// is reported.
type use struct {
	label, what string
}

// usedBy returns the targets that tmpl, a template, uses, but scratch and tool caches.
func usedBy(tmpl any) []use {
	var used []use
	add := func(ref targetRef, what string) {
		if ref.label != scratch {
			used = append(used, use{ref.label, fmt.Sprintf("%s %q", what, ref.ref)})
		}
	}
	switch t := tmpl.(type) {
	case *targetSpec:
		add(t.baseRef, "base")
		for _, in := range t.steps {
			if step, ok := in.(*addStep); ok {
				add(step.target, "add: source")
			}
		}
	case *indexSpec:
		for _, ref := range t.manifests {
			add(ref, "layer")
		}
	case *publicationSpec:
		for _, ref := range t.checks {
			add(ref, "checks")
		}
		for _, image := range t.images {
			add(image.target, "images")
		}
	}
	return used
}

// namedPackages returns the packages that the labels given to add() and dep() as f ran name, sorted, but f's own.
func (w *workspace) namedPackages(f *buildFile) []string {
	var pkgs []string
	for _, ref := range f.refs {
		if pkg, _, err := parseLabel(ref.ref, ref.pkg); err == nil && pkg != f.pkg && !slices.Contains(pkgs, pkg) {
			pkgs = append(pkgs, pkg)
		}
	}
	slices.Sort(pkgs)
	return pkgs
}

// configureTarget configures the target l as the command line req would, as building it would, loading the packages
// that it uses; a pull or glob uses none, and a tool cache is mounted, never configured.
func (w *workspace) configureTarget(cr *configurer, l string, req Request) error {
	kind := w.declaration(l).kind
	if kind == "pull" || kind == "glob" || kind == "tool_cache" {
		return nil
	}
	if kind == "index" {
		req.Platform = "" // whose manifests are each configured for their own
	}
	c, err := w.commandConfigOf(l, kind, req, false)
	if err != nil {
		return err
	}
	roots := []confLabel{{l, c}}
	if kind == "publication" {
		if roots, _, err = w.expandPublication(roots[0]); err != nil {
			return err
		}
	}
	for _, cl := range roots {
		if err := cr.image(cl); err != nil {
			return err
		}
	}
	return nil
}

// packageDirs returns the packages of the workspace, sorted: its root, and the directories beneath it, that hold a
// BUILD file, itself a link or not, found without entering linked directories, though the root itself may be one, as
// a linked $PWD makes it, hidden directories, or directories that hold a WORKSPACE file, which are other workspaces.
func (w *workspace) packageDirs() ([]string, error) {
	var pkgs []string
	var walk func(pkg string) error
	walk = func(pkg string) error {
		if err := w.ctx.Err(); err != nil {
			return err
		}
		dir := filepath.Join(w.root, filepath.FromSlash(pkg))
		entries, err := afero.ReadDir(w.fs, dir) // which describes each entry as itself, a link as a link
		if err != nil {
			return err
		}
		found := false
		for _, e := range entries {
			name := e.Name()
			switch {
			case !e.IsDir(): // a BUILD file may be a link, which the loader follows; a linked directory isn't entered
				found = found || slices.Contains(buildFileNames, name)
			case strings.HasPrefix(name, "."):
			default:
				sub := path.Join(pkg, name)
				if _, err := w.fs.Stat(filepath.Join(dir, name, "WORKSPACE")); err == nil {
					continue
				} else if !errors.Is(err, fs.ErrNotExist) {
					return err
				}
				if err := walk(sub); err != nil {
					return err
				}
			}
		}
		if found {
			pkgs = append(pkgs, pkg)
		}
		return nil
	}
	if err := walk(""); err != nil {
		return nil, fmt.Errorf("looking for BUILD files in %s: %w", w.root, err)
	}
	slices.Sort(pkgs)
	return pkgs, nil
}
