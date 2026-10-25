package features

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/cli"
	"github.com/Topher-the-Geek/vamos/internal/testenv"
	"github.com/cucumber/godog"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// projectDir is where a scenario's project is, in memory.
const projectDir = testenv.ProjectDir

// world is the state of one scenario: the project it declares, and what the last command it ran did.
type world struct {
	stores *testenv.Stores
	nonce  string            // replaces {{nonce}} in the scenario's text, for actions new to this run
	files  map[string]string // the project's files, by path within it, BUILD among them
	out    string            // the folder that holds each command's destination, removed with the scenario

	stdout   string
	err      error            // the last command's
	dest     string           // the folder that the last command sent to, its own
	sent     string           // what the last command reports once it has sent its image or files, if it sends any
	image    v1.Image         // the image that the last command saved, if it saved one
	entries  map[string]entry // the image's files, once a step has read them
	exported string           // the folder that the last command exported to, if it exported
	before   map[string]bool  // the store's unnamed images when the scenario began
}

// start readies the world for scenario s.
func (w *world) start() error {
	stores, err := testenv.Suite()
	if err != nil {
		return err
	}
	out, err := os.MkdirTemp("", "vamos-feature-")
	if err != nil {
		return err
	}
	*w = world{
		stores: stores,
		nonce:  fmt.Sprintf("%016x", rand.Uint64()),
		files:  map[string]string{},
		out:    out,
	}
	w.before, err = stores.UnnamedImages()
	return err
}

// finish removes what the scenario sent.
func (w *world) finish() error {
	if w.out == "" {
		return nil
	}
	return os.RemoveAll(w.out)
}

// register gives sc the steps that the features use.
func (w *world) register(sc *godog.ScenarioContext) {
	sc.Step(`^a project that pulls busybox$`, w.pullsBusybox)
	sc.Step(`^the BUILD file declares:$`, w.declares)
	sc.Step(`^the project holds:$`, w.holds)

	sc.Step(`^I save "([^"]*)"$`, w.save)
	sc.Step(`^I export "([^"]*)"$`, w.export)
	sc.Step(`^I build "([^"]*)"(?: again)?$`, w.build)

	sc.Step(`^it succeeds$`, w.succeeds)
	sc.Step(`^it fails with:$`, w.failsWith)
	sc.Step(`^the output includes "([^"]*)"$`, w.outputIncludes)
	sc.Step(`^(layer|task) "([^"]*)" (ran|was reused from the cache)$`, w.targetStatus)
	sc.Step(`^nothing is saved$`, w.nothingSent)
	sc.Step(`^nothing is exported$`, w.nothingSent)
	sc.Step(`^no image is left behind$`, w.noImageLeft)

	sc.Step(`^the image holds:$`, w.imageHolds)
	sc.Step(`^the image's files under "([^"]*)" are:$`, w.imageFilesUnder)
	sc.Step(`^the image's paths are:$`, w.imagePaths)
	sc.Step(`^the image holds nothing under "([^"]*)"$`, w.imageHoldsNothingUnder)
	sc.Step(`^the image has the directory "([^"]*)"(?: with mode (\d+))?$`, w.imageHasDirectory)
	sc.Step(`^the image's (command|entrypoint) is (none|\[.*\])$`, w.imageArgv)
	sc.Step(`^the image's environment sets "([^"]*)" to "([^"]*)"$`, w.imageEnvSets)
	sc.Step(`^the image's environment doesn't set "([^"]*)"$`, w.imageEnvUnset)
	sc.Step(`^the image's working directory is "([^"]*)"$`, w.imageWorkdir)

	sc.Step(`^the exported files are:$`, w.exportedFiles)
	sc.Step(`^the exported folder has the directory "([^"]*)" with mode (\d+)$`, w.exportedHasDirectory)
}

// text returns a scenario's text with {{nonce}} replaced.
func (w *world) text(text string) string {
	return strings.ReplaceAll(text, "{{nonce}}", w.nonce)
}

func (w *world) pullsBusybox() {
	w.files["BUILD"] = testenv.PullBusybox
}

func (w *world) declares(build *godog.DocString) {
	w.files["BUILD"] += w.text(build.Content) + "\n"
}

func (w *world) holds(table *godog.Table) {
	for _, row := range w.rows(table) {
		w.files[row["path"]] = row["content"]
	}
}

// run runs the vamos command line with args, in the scenario's project, as main does.
// It forgets what earlier commands saved and exported, so that checks see only this one's.
func (w *world) run(ctx context.Context, args ...string) error {
	w.image, w.entries, w.exported = nil, nil, ""
	fsys, err := testenv.MemProject(w.files)
	if err != nil {
		return err
	}
	cmd := cli.NewRootCommand(cli.Config{Fs: fsys, Store: &w.stores.Options})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(append(args, "-C", projectDir))
	w.err = cmd.ExecuteContext(ctx)
	w.stdout = stdout.String() + stderr.String()
	return nil
}

// newDest gives the next command a folder of its own to send to, so that checks see only what it sent.
func (w *world) newDest() error {
	dest, err := os.MkdirTemp(w.out, "dest-")
	w.dest = dest
	return err
}

func (w *world) save(ctx context.Context, target string) error {
	if err := w.newDest(); err != nil {
		return err
	}
	path := filepath.Join(w.dest, target+".tar")
	w.sent = "Image built! "
	if err := w.run(ctx, "save", target, path); err != nil {
		return err
	}
	if w.err == nil {
		img, err := tarball.ImageFromPath(path, nil)
		if err != nil {
			return fmt.Errorf("reading the saved image: %w", err)
		}
		w.image = img
	}
	return nil
}

func (w *world) export(ctx context.Context, target string) error {
	if err := w.newDest(); err != nil {
		return err
	}
	dir := filepath.Join(w.dest, target)
	w.sent = "Files exported! "
	if err := w.run(ctx, "export", target, dir); err != nil {
		return err
	}
	if w.err == nil {
		w.exported = dir
	}
	return nil
}

func (w *world) build(ctx context.Context, target string) error {
	if err := w.newDest(); err != nil {
		return err
	}
	w.sent = ""
	return w.run(ctx, "build", target)
}

// succeeds checks that the command succeeded and, if it sends an image or files, reported sending them.
func (w *world) succeeds() error {
	if w.err != nil {
		return fmt.Errorf("vamos failed: %v\noutput:\n%s", w.err, w.stdout)
	}
	if !strings.Contains(w.stdout, w.sent) {
		return fmt.Errorf("output = %q, want it to report %q", w.stdout, w.sent)
	}
	return nil
}

// failsWith checks that the command failed, at run time, with an error that contains each line of want,
// and reported sending nothing.
func (w *world) failsWith(want *godog.DocString) error {
	if w.err == nil {
		return fmt.Errorf("vamos succeeded, want an error\noutput:\n%s", w.stdout)
	}
	var errs []error
	var wanted bool
	for line := range strings.Lines(w.text(want.Content)) {
		// A blank line would match any error.
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		wanted = true
		if !strings.Contains(w.err.Error(), line) {
			errs = append(errs, fmt.Errorf("error = %q, want it to contain %q", w.err, line))
		}
	}
	if !wanted {
		errs = append(errs, errors.New("the error to expect is empty"))
	}
	if strings.Contains(w.stdout, "Usage:") {
		errs = append(errs, fmt.Errorf("printed usage for a failure at run time; output:\n%s", w.stdout))
	}
	for _, sent := range []string{" built! ", "Files exported! "} {
		if strings.Contains(w.stdout, sent) {
			errs = append(errs, fmt.Errorf("output = %q, want nothing reported sent after an error", w.stdout))
			break
		}
	}
	return errors.Join(errs...)
}

func (w *world) outputIncludes(want string) error {
	if want = w.text(want); !strings.Contains(w.stdout, want) {
		return fmt.Errorf("output = %q, want it to include %q", w.stdout, want)
	}
	return nil
}

// targetStatus checks the status that the command reported for a target: building when it ran, or cached.
func (w *world) targetStatus(kind, name, status string) error {
	name = w.text(name)
	want := fmt.Sprintf("%s %q: building", kind, name)
	if status != "ran" {
		want = fmt.Sprintf("%s %q: cached", kind, name)
	}
	if !strings.Contains(w.stdout, want) {
		return fmt.Errorf("output = %q, want it to report %q", w.stdout, want)
	}
	return nil
}

// nothingSent checks that the last command left nothing where it sends images and files.
func (w *world) nothingSent() error {
	if w.dest == "" {
		return errors.New("no command has run")
	}
	entries, err := os.ReadDir(w.dest)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("%s holds %v, want nothing", w.dest, entries)
	}
	return nil
}

// noImageLeft checks that the store has no unnamed image that it didn't have when the scenario began.
func (w *world) noImageLeft() error {
	after, err := w.stores.UnnamedImages()
	if err != nil {
		return err
	}
	for id := range after {
		if !w.before[id] {
			return fmt.Errorf("image %s left in the container store", id)
		}
	}
	return nil
}

// entry is a file, directory or other node, such as a link, in an image or a folder.
type entry struct {
	dir     bool
	regular bool  // a regular file
	mode    int64 // permission and special bits
	content string
}

// imageEntries returns the entries of the saved image's flattened filesystem, by absolute path.
// It reads the image once, for all the steps that check it.
func (w *world) imageEntries() (map[string]entry, error) {
	if w.image == nil {
		return nil, errors.New("no image was saved")
	}
	if w.entries != nil {
		return w.entries, nil
	}
	entries := map[string]entry{}
	var readErr error
	_, err := testenv.WalkImage(w.image, func(hdr *tar.Header, r io.Reader) bool {
		e := entry{dir: hdr.Typeflag == tar.TypeDir, regular: hdr.Typeflag == tar.TypeReg, mode: hdr.Mode & 0o7777}
		switch hdr.Typeflag {
		case tar.TypeReg:
			b, err := io.ReadAll(r)
			if err != nil {
				readErr = err
				return true
			}
			e.content = string(b)
		case tar.TypeLink:
			// A hard link, as busybox's applets are, is another name for a regular file that comes before it.
			if target, ok := entries[testenv.ImagePath(hdr.Linkname)]; ok && target.regular {
				e = entry{regular: true, mode: hdr.Mode & 0o7777, content: target.content}
			}
		}
		entries[testenv.ImagePath(hdr.Name)] = e
		return false
	})
	if err = errors.Join(err, readErr); err != nil {
		return nil, err
	}
	w.entries = entries
	return entries, nil
}

func (w *world) imageHolds(table *godog.Table) error {
	entries, err := w.imageEntries()
	if err != nil {
		return err
	}
	return matchFiles(entries, w.rows(table), false)
}

// imageFilesUnder checks that the files beneath dir in the image are the table's, with no other directories.
func (w *world) imageFilesUnder(dir string, table *godog.Table) error {
	entries, err := w.imageEntries()
	if err != nil {
		return err
	}
	return matchFiles(beneath(entries, w.text(dir)), w.rows(table), true)
}

// imagePaths checks that the paths in the image, directories among them, are exactly the table's.
func (w *world) imagePaths(table *godog.Table) error {
	entries, err := w.imageEntries()
	if err != nil {
		return err
	}
	var want []string
	for _, row := range w.rows(table) {
		want = append(want, row["path"])
	}
	slices.Sort(want)
	if got := slices.Sorted(maps.Keys(entries)); !slices.Equal(got, want) {
		return fmt.Errorf("the image holds %q, want %q", got, want)
	}
	return nil
}

func (w *world) imageHoldsNothingUnder(dir string) error {
	entries, err := w.imageEntries()
	if err != nil {
		return err
	}
	if under := beneath(entries, w.text(dir)); len(under) > 0 {
		return fmt.Errorf("the image holds %q, want nothing under %s", slices.Sorted(maps.Keys(under)), dir)
	}
	return nil
}

// imageHasDirectory checks that the image has a directory at path, with mode, in octal, unless it is empty.
func (w *world) imageHasDirectory(path, mode string) error {
	entries, err := w.imageEntries()
	if err != nil {
		return err
	}
	return hasDirectory(entries, w.text(path), mode)
}

// imageArgv checks the image's command or entrypoint, given as a JSON list, or none.
func (w *world) imageArgv(field, want string) error {
	config, err := w.imageConfig()
	if err != nil {
		return err
	}
	got := config.Config.Cmd
	if field == "entrypoint" {
		got = config.Config.Entrypoint
	}
	var argv []string
	if want = w.text(want); want != "none" {
		if err := json.Unmarshal([]byte(want), &argv); err != nil {
			return fmt.Errorf("the %s to expect, %s: %w", field, want, err)
		}
	}
	if !slices.Equal(got, argv) {
		return fmt.Errorf("%s = %q, want %s", field, got, want)
	}
	return nil
}

func (w *world) imageEnvSets(name, value string) error {
	config, err := w.imageConfig()
	if err != nil {
		return err
	}
	if want := w.text(name + "=" + value); !slices.Contains(config.Config.Env, want) {
		return fmt.Errorf("env = %q, want it to include %s", config.Config.Env, want)
	}
	return nil
}

func (w *world) imageEnvUnset(name string) error {
	name = w.text(name)
	config, err := w.imageConfig()
	if err != nil {
		return err
	}
	if slices.ContainsFunc(config.Config.Env, func(v string) bool { return strings.HasPrefix(v, name+"=") }) {
		return fmt.Errorf("env = %q, want no %s", config.Config.Env, name)
	}
	return nil
}

func (w *world) imageWorkdir(want string) error {
	config, err := w.imageConfig()
	if err != nil {
		return err
	}
	if got, want := config.Config.WorkingDir, w.text(want); got != want {
		return fmt.Errorf("working directory = %q, want %q", got, want)
	}
	return nil
}

func (w *world) imageConfig() (*v1.ConfigFile, error) {
	if w.image == nil {
		return nil, errors.New("no image was saved")
	}
	return w.image.ConfigFile()
}

// exportedHasDirectory checks that the folder exported to has a directory at path, with mode, in octal.
func (w *world) exportedHasDirectory(path, mode string) error {
	entries, err := w.exportedEntries()
	if err != nil {
		return err
	}
	return hasDirectory(entries, w.text(path), mode)
}

// exportedFiles checks that the files in the folder exported to are the table's, with no other directories.
func (w *world) exportedFiles(table *godog.Table) error {
	entries, err := w.exportedEntries()
	if err != nil {
		return err
	}
	return matchFiles(entries, w.rows(table), true)
}

// exportedEntries returns the entries of the folder exported to, by path relative to it.
func (w *world) exportedEntries() (map[string]entry, error) {
	if w.exported == "" {
		return nil, errors.New("nothing was exported")
	}
	entries := map[string]entry{}
	err := filepath.WalkDir(w.exported, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(w.exported, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil // the folder itself
		}
		e := entry{dir: d.IsDir(), regular: d.Type().IsRegular(), mode: int64(cache.ChmodBits(info))}
		if e.regular {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			e.content = string(b)
		}
		entries[filepath.ToSlash(rel)] = e
		return nil
	})
	return entries, err
}

// hasDirectory checks that entries have a directory at path, with mode, in octal, unless it is empty.
func hasDirectory(entries map[string]entry, path, mode string) error {
	e, ok := entries[path]
	if !ok || !e.dir {
		return fmt.Errorf("no directory %s", path)
	}
	if mode == "" {
		return nil
	}
	want, err := strconv.ParseInt(mode, 8, 64)
	if err != nil {
		return fmt.Errorf("the mode to expect of %s, %s: %w", path, mode, err)
	}
	if e.mode != want {
		return fmt.Errorf("mode of %s = %#o, want %#o", path, e.mode, want)
	}
	return nil
}

// matchFiles checks entries against a table of regular files, by path, with their content and, if the table has a
// mode column, their modes, in octal. Exactly, the table must list every file in entries, and every directory in
// entries must hold one of the table's files.
func matchFiles(entries map[string]entry, want []map[string]string, exactly bool) error {
	var errs []error
	listed := map[string]bool{}
	for _, row := range want {
		path := row["path"]
		listed[path] = true
		e, ok := entries[path]
		switch {
		case !ok:
			errs = append(errs, fmt.Errorf("%s not found", path))
			continue
		case e.dir:
			errs = append(errs, fmt.Errorf("%s is a directory", path))
			continue
		case !e.regular:
			errs = append(errs, fmt.Errorf("%s is not a regular file", path))
			continue
		case e.content != row["content"]:
			errs = append(errs, fmt.Errorf("%s = %q, want %q", path, e.content, row["content"]))
		}
		if m, ok := row["mode"]; ok {
			mode, err := strconv.ParseInt(m, 8, 64)
			if err != nil {
				errs = append(errs, fmt.Errorf("the mode to expect of %s, %s: %w", path, m, err))
			} else if e.mode != mode {
				errs = append(errs, fmt.Errorf("mode of %s = %#o, want %#o", path, e.mode, mode))
			}
		}
	}
	if exactly {
		for _, path := range slices.Sorted(maps.Keys(entries)) {
			if dir := entries[path].dir; dir && !holdsListed(path, listed) || !dir && !listed[path] {
				errs = append(errs, fmt.Errorf("%s is there too", path))
			}
		}
	}
	return errors.Join(errs...)
}

// holdsListed reports whether directory dir holds any of the listed paths.
func holdsListed(dir string, listed map[string]bool) bool {
	for path := range listed {
		if strings.HasPrefix(path, dir+"/") {
			return true
		}
	}
	return false
}

// beneath returns the entries whose paths are beneath dir.
func beneath(entries map[string]entry, dir string) map[string]entry {
	prefix := strings.TrimSuffix(dir, "/") + "/"
	under := map[string]entry{}
	for path, e := range entries {
		if strings.HasPrefix(path, prefix) {
			under[path] = e
		}
	}
	return under
}

// rows returns a table's rows after its header, each by the header's column names, with {{nonce}} replaced.
func (w *world) rows(table *godog.Table) []map[string]string {
	var rows []map[string]string
	header := table.Rows[0].Cells
	for _, row := range table.Rows[1:] {
		cells := map[string]string{}
		for i, cell := range row.Cells {
			cells[header[i].Value] = w.text(cell.Value)
		}
		rows = append(rows, cells)
	}
	return rows
}
