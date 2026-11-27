package graph

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// CheckWorkspaceName checks name against the rules of a workspace's name, which are those of a Bazel module's:
// lowercase ASCII letters, digits, ".", "-" and "_", starting with a letter and ending with a letter or digit.
// Its error names the name and the rule it breaks.
// Loading checks a name as WORKSPACE declares it, and an executor checks the one a request carries again, wherever it
// would choose a workspace's tool caches by it.
func CheckWorkspaceName(name string) error {
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
