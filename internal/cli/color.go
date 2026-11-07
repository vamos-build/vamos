package cli

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"golang.org/x/term"
)

// The choices of --color, for the statuses a command prints as it builds.
const (
	colorAuto   = "auto"   // color them in a terminal, unless NO_COLOR is set to something or TERM is dumb
	colorAlways = "always" // color them wherever the output goes
	colorNever  = "never"  // print them plain
)

// colorChoices are the values --color takes.
var colorChoices = []string{colorAuto, colorAlways, colorNever}

// colorFlag is the value of --color. Its default is auto; an explicit choice outranks NO_COLOR, as no-color.org
// asks of a command-line option.
type colorFlag struct {
	choice string // as given, or "" for auto
}

// String gives the choice, auto when none was given, so that --help shows the default.
func (f *colorFlag) String() string {
	if f.choice == "" {
		return colorAuto
	}
	return f.choice
}

func (f *colorFlag) Type() string { return "when" }

// Set checks that value is a choice, so that a mistaken one is refused as the command line is parsed.
func (f *colorFlag) Set(value string) error {
	if !slices.Contains(colorChoices, value) {
		return fmt.Errorf("%q isn't a choice for coloring; want auto, always or never", value)
	}
	f.choice = value
	return nil
}

// colors reports whether the command colors the statuses it writes to stdout, as --color says.
func (f colorFlag) colors(stdout io.Writer) bool {
	switch f.choice {
	case colorAlways:
		return true
	case colorNever:
		return false
	}
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	file, ok := stdout.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

// The terminal's own colors, so that statuses suit its theme: SGR dim and green, each with the code that ends
// it alone, so that a status never resets more than it set.
const (
	dimOn, dimOff     = "\x1b[2m", "\x1b[22m"
	greenOn, greenOff = "\x1b[32m", "\x1b[39m"
)

// paintStatus colors the word that a status starts with: cached is dimmed, so that what reruns stands out, and
// building and rebuilding are green; the rest of the status, such as why a target builds, and every other status,
// stay as they are.
func paintStatus(status string) string {
	end := strings.IndexFunc(status, func(r rune) bool { return r < 'a' || r > 'z' })
	if end < 0 {
		end = len(status)
	}
	word, rest := status[:end], status[end:]
	switch word {
	case "cached":
		return dimOn + word + dimOff + rest
	case "building", "rebuilding":
		return greenOn + word + greenOff + rest
	}
	return status
}
