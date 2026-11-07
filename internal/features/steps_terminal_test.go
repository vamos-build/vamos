package features

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/Topher-the-Geek/vamos/internal/testenv"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// GivenStandardOutputIsATerminal makes standard output a terminal for the rest of the scenario, as it is where a
// user runs a command by hand: a pseudo-terminal, whose output the Then steps see as they see any other.
// It skips the scenario where the host can't open a pseudo-terminal, or fails it if VAMOS_REQUIRE_INTEGRATION is
// set, as steps that need buildah do.
func (w *world) GivenStandardOutputIsATerminal() {
	w.t.Helper()
	tty, err := openTerminal(&bytes.Buffer{})
	if err != nil {
		testenv.Unavailable(w.t, "giving a command a terminal", err)
	}
	tty.close()
	w.outputTTY = true
}

// terminal is a pseudo-terminal that a command writes to as its standard output, and what it has written.
type terminal struct {
	out  *os.File      // the side the command writes to, which is a terminal
	read chan struct{} // closed once everything written to out has been read into written
}

// openTerminal opens a pseudo-terminal in raw mode, so that what is written to its out arrives as it was, and
// copies everything written there into written until close.
func openTerminal(written *bytes.Buffer) (*terminal, error) {
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, fmt.Errorf("opening a pseudo-terminal: %w", err)
	}
	pts, err := func() (*os.File, error) {
		fd := int(ptmx.Fd())
		if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
			return nil, fmt.Errorf("unlocking the pseudo-terminal: %w", err)
		}
		n, err := unix.IoctlGetUint32(fd, unix.TIOCGPTN)
		if err != nil {
			return nil, fmt.Errorf("naming the pseudo-terminal: %w", err)
		}
		pts, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
		if err != nil {
			return nil, fmt.Errorf("opening the pseudo-terminal's other side: %w", err)
		}
		if _, err := term.MakeRaw(int(pts.Fd())); err != nil {
			pts.Close()
			return nil, fmt.Errorf("making the pseudo-terminal raw: %w", err)
		}
		return pts, nil
	}()
	if err != nil {
		ptmx.Close()
		return nil, err
	}
	t := &terminal{out: pts, read: make(chan struct{})}
	go func() {
		defer close(t.read)
		defer ptmx.Close()
		_, err := io.Copy(written, ptmx)
		// Once the last of its other side closes, reading a pseudo-terminal fails with EIO rather than io.EOF.
		if err != nil && !errors.Is(err, syscall.EIO) {
			fmt.Fprintf(written, "\n(reading the terminal failed: %v)\n", err)
		}
	}()
	return t, nil
}

// close closes the side the command wrote to, and returns once all it wrote has been read.
func (t *terminal) close() {
	t.out.Close()
	<-t.read
}
