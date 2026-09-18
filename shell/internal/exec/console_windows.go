//go:build windows

package exec

import (
	"context"
	"os"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/term"
)

// watchResize polls, since Windows has no SIGWINCH.
func watchResize(ctx context.Context, fd int, resized func(width, height int)) {
	lastWidth, lastHeight, _ := term.GetSize(fd)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			width, height, err := term.GetSize(fd)
			if err != nil || (width == lastWidth && height == lastHeight) {
				continue
			}
			lastWidth, lastHeight = width, height
			resized(width, height)
		}
	}
}

// prepareConsole turns on VT processing for output, so the container's escape
// sequences draw rather than print. term.MakeRaw already does the input side.
func prepareConsole() func() {
	handle := windows.Handle(os.Stdout.Fd())
	var mode uint32
	if windows.GetConsoleMode(handle, &mode) != nil {
		return func() {}
	}
	windows.SetConsoleMode(handle, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING|windows.DISABLE_NEWLINE_AUTO_RETURN)
	return func() { windows.SetConsoleMode(handle, mode) }
}
