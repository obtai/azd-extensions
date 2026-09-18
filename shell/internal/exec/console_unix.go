//go:build !windows

package exec

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"
)

// watchResize reports the terminal's new size each time it changes.
func watchResize(ctx context.Context, fd int, resized func(width, height int)) {
	changes := make(chan os.Signal, 1)
	signal.Notify(changes, syscall.SIGWINCH)
	defer signal.Stop(changes)
	for {
		select {
		case <-ctx.Done():
			return
		case <-changes:
			if width, height, err := term.GetSize(fd); err == nil {
				resized(width, height)
			}
		}
	}
}

// prepareConsole has nothing to do: a unix terminal already speaks VT.
func prepareConsole() func() { return func() {} }
