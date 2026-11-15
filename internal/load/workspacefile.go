package load

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/spf13/afero"
	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

// evalWorkspaceFile evaluates the WORKSPACE file at w's root, setting w.name if it calls workspace().
// It runs on a thread of its own, whose only builtin is workspace(), beside the Starlark universe: a BUILD file's
// builtins are undefined there, and load() fails, since the thread has no Load function.
// Its mistakes are reported as a BUILD file's are, saying where.
func (w *workspace) evalWorkspaceFile() error {
	path := filepath.Join(w.root, "WORKSPACE")
	src, err := afero.ReadFile(w.fs, path)
	if err != nil {
		return fmt.Errorf("reading the WORKSPACE file: %w", err)
	}
	var name string           // that workspace() gives, once called
	var named syntax.Position // of the call to workspace(), once called
	builtin := starlark.NewBuiltin("workspace", func(
		thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
	) (starlark.Value, error) {
		if len(args) > 0 {
			return nil, fmt.Errorf(`%s: give the name by keyword, as name = "com.example.app"`, fn.Name())
		}
		var v starlark.Value
		if err := starlark.UnpackArgs(fn.Name(), args, kwargs, "name", &v); err != nil {
			return nil, err
		}
		if named.IsValid() {
			return nil, fmt.Errorf("%s: the workspace is already named %q, at %s", fn.Name(), name, named)
		}
		s, ok := v.(starlark.String)
		if !ok {
			return nil, fmt.Errorf("%s: name must be a string, not %s", fn.Name(), v.Type())
		}
		if err := checkWorkspaceName(string(s)); err != nil {
			return nil, fmt.Errorf("%s: %w", fn.Name(), err)
		}
		name, named = string(s), thread.CallFrame(1).Pos
		return starlark.None, nil
	})
	thread := &starlark.Thread{Name: path}
	defer cancelWith(w.ctx, thread)()
	predeclared := starlark.StringDict{"workspace": builtin}
	if _, err := starlark.ExecFileOptions(&syntax.FileOptions{}, thread, path, src, predeclared); err != nil {
		return w.interrupted(describeExecError(err))
	}
	w.name = name
	return nil
}

// checkWorkspaceName checks name against the rules of a workspace's name, which are those of a Bazel module's:
// lowercase ASCII letters, digits, ".", "-" and "_", starting with a letter and ending with a letter or digit.
// Its error names the name and the rule it breaks.
func checkWorkspaceName(name string) error {
	if name == "" {
		return errors.New("name must not be empty")
	}
	if c := name[0]; c < 'a' || c > 'z' {
		return fmt.Errorf("name %q must start with a lowercase letter", name)
	}
	for i, r := range name {
		if r >= utf8.RuneSelf || !isLowerOrDigit(byte(r)) && !strings.ContainsRune(".-_", r) {
			bad := string(r)
			if _, size := utf8.DecodeRuneInString(name[i:]); r == utf8.RuneError && size == 1 {
				bad = name[i : i+1] // a byte that isn't UTF-8, named as itself rather than as U+FFFD
			}
			return fmt.Errorf(`name %q must not contain %q; a workspace name holds only lowercase letters, `+
				`digits, ".", "-" and "_"`, name, bad)
		}
	}
	if !isLowerOrDigit(name[len(name)-1]) {
		return fmt.Errorf("name %q must end with a lowercase letter or digit", name)
	}
	return nil
}

// isLowerOrDigit reports whether c is a lowercase ASCII letter or a digit.
func isLowerOrDigit(c byte) bool { return 'a' <= c && c <= 'z' || '0' <= c && c <= '9' }
