//go:build daemon

package monitor

import (
	"os"

	"golang.org/x/term"
)

// enterRaw puts the terminal into raw mode and hides the cursor. On Windows
// the term.MakeRaw call enables ENABLE_VIRTUAL_TERMINAL_PROCESSING as a side
// effect, which is required for the ANSI escape sequences emitted by render()
// to work on Windows 10 1909+. Older Windows builds will see literal escape
// sequences; documented as a known limitation.
func enterRaw() (int, *term.State, error) {
	fd := int(os.Stdin.Fd())
	st, err := term.MakeRaw(fd)
	if err != nil {
		return 0, nil, err
	}
	os.Stdout.WriteString(hideCur)
	return fd, st, nil
}

func leaveRaw(fd int, st *term.State) {
	if st != nil {
		term.Restore(fd, st)
	}
	os.Stdout.WriteString(showCur)
}
