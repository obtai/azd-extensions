// Package ui is the pickers and spinners, drawn with Charm's huh. Everything
// renders to stderr, so stdout carries only what the container says and
// `azd shell -c env > env.txt` can still prompt.
package ui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/huh/spinner"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"
)

// ErrAborted is Esc or Ctrl-C in a picker or spinner.
var ErrAborted = errors.New("aborted")

// UI decides whether anything is drawn at all.
type UI struct {
	// Interactive is false without a terminal, or with --no-prompt. A choice
	// that needs a prompt is then an error naming the flag that would settle it.
	Interactive bool
}

// New draws only when both ends of the conversation are a terminal: huh reads
// stdin and we draw on stderr.
func New(noPrompt bool) UI {
	return UI{
		Interactive: !noPrompt &&
			term.IsTerminal(int(os.Stdin.Fd())) &&
			term.IsTerminal(int(os.Stderr.Fd())),
	}
}

// Item is one row of a picker.
type Item[T any] struct {
	Label string
	Value T
}

// Choice describes what is being picked, for the prompt and for the error
// when there is no prompt.
type Choice struct {
	Title string // "Container app"
	Noun  string // "container apps"
	Flag  string // "--app"
}

// Pick returns the only item without asking, asks when there are several,
// and fails when there are none — the ecsgo flow.
func Pick[T any](ctx context.Context, u UI, choice Choice, items []Item[T]) (T, error) {
	var zero T
	switch {
	case len(items) == 0:
		return zero, fmt.Errorf("no %s found", choice.Noun)
	case len(items) == 1:
		return items[0].Value, nil
	case !u.Interactive:
		labels := make([]string, len(items))
		for i, item := range items {
			labels[i] = "  " + item.Label
		}
		return zero, fmt.Errorf("%d %s match; pass %s to pick one:\n%s",
			len(items), choice.Noun, choice.Flag, strings.Join(labels, "\n"))
	}

	// huh wants a comparable value; the index is, whatever T is.
	options := make([]huh.Option[int], len(items))
	for i, item := range items {
		options[i] = huh.NewOption(item.Label, i)
	}
	var picked int
	field := huh.NewSelect[int]().
		Title(choice.Title).
		Options(options...).
		Height(min(len(items)+2, 15)).
		Value(&picked)

	err := huh.NewForm(huh.NewGroup(field)).
		WithTheme(huh.ThemeCharm()).
		WithOutput(os.Stderr).
		WithShowHelp(false).
		RunWithContext(ctx)
	if errors.Is(err, huh.ErrUserAborted) {
		return zero, ErrAborted
	}
	if err != nil {
		return zero, err
	}

	// The form clears itself on exit; leave the answer on screen, as a
	// shell prompt would.
	fmt.Fprintf(os.Stderr, "%s %s\n", muted.Render(choice.Title+":"), items[picked].Label)
	return items[picked].Value, nil
}

// Loading runs fn under a spinner, or just runs it when nothing is drawn.
func Loading[T any](ctx context.Context, u UI, title string, fn func(context.Context) (T, error)) (T, error) {
	if !u.Interactive {
		return fn(ctx)
	}
	var value T
	err := spinner.New().
		Title(" " + title).
		Output(os.Stderr).
		Context(ctx).
		ActionWithErr(func(ctx context.Context) error {
			var err error
			value, err = fn(ctx)
			return err
		}).
		Run()
	if errors.Is(err, tea.ErrInterrupted) || errors.Is(err, context.Canceled) {
		var zero T
		return zero, ErrAborted
	}
	return value, err
}

var (
	muted  = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#6B6B6B", Dark: "#9B9B9B"})
	accent = lipgloss.NewStyle().Foreground(lipgloss.Color("#F780E2")).Bold(true)
)

// Path prints where the session is going: app › revision › replica › container.
func Path(parts ...string) {
	separator := muted.Render(" › ")
	styled := make([]string, len(parts))
	for i, part := range parts {
		styled[i] = accent.Render(part)
	}
	fmt.Fprintln(os.Stderr, muted.Render("Connecting to ")+strings.Join(styled, separator))
}
