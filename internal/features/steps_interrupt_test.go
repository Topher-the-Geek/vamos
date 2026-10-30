package features

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"time"
)

// Steps about interrupting a command, as Ctrl-C does: main cancels the command's context, which these steps do.

// interruptedOnceItPrints interrupts the command once it has printed text, as a user who presses Ctrl-C on seeing
// it does. Every command takes it.
func interruptedOnceItPrints(text string) option {
	commands := append(slices.Clone(everyCommand), "coordinator")
	return option{"interruptedOnceItPrints", commands, func(w *world, _ *commandLine) {
		w.interruptOnce()
		w.interruptOn = text
	}}
}

// interruptedAfter interrupts the command once it has run for d, as a user who presses Ctrl-C then does.
// Every command takes it.
func interruptedAfter(d time.Duration) option {
	return option{"interruptedAfter", everyCommand, func(w *world, _ *commandLine) {
		w.interruptOnce()
		if d <= 0 {
			w.t.Fatalf("interruptedAfter(%s) never interrupts", d)
		}
		w.interruptIn = d
	}}
}

// interruptOnce stops the scenario if the command is to be interrupted already, by another option.
func (w *world) interruptOnce() {
	w.t.Helper()
	if w.interruptOn != "" || w.interruptIn != 0 || w.interruptBy != 0 {
		w.t.Fatal("the command is given more than one option that interrupts it")
	}
	if w.outputClosed {
		w.t.Fatal("a command whose standard output is closed stops on its first write, before any interrupt")
	}
}

// interrupter writes what a command prints to out, and cancels the command once it has printed text, noting in
// fired that it did, unless the command's context was done already, as after a deadline.
type interrupter struct {
	out    *bytes.Buffer
	text   []byte
	ctx    context.Context
	cancel context.CancelFunc
	fired  bool
}

func (i *interrupter) Write(p []byte) (int, error) {
	start := max(0, i.out.Len()-len(i.text)+1) // so that text written across writes is found
	n, err := i.out.Write(p)
	if !i.fired && i.ctx.Err() == nil && bytes.Contains(i.out.Bytes()[start:], i.text) {
		i.fired = true
		i.cancel()
	}
	return n, err
}

// ThenItIsInterrupted checks that the last command failed because it was interrupted, and not otherwise, such as
// with a mistake in the BUILD file that it was running: that its error says so, as ThenItFailsWith checks it.
// It stops the scenario if not.
func (w *world) ThenItIsInterrupted() {
	w.t.Helper()
	w.ThenItFailsWith("context canceled")
	if !errors.Is(w.err, context.Canceled) {
		w.t.Fatalf("the command's error is %v, want it interrupted\nstdout:\n%s\nstderr:\n%s", w.err, w.stdout,
			w.stderr)
	}
}

// ThenItEndsCleanlyOnceInterrupted checks that the last command printed what the step that ran it waited for before
// interrupting it, and then ended without an error, as a command that serves until it is interrupted does.
func (w *world) ThenItEndsCleanlyOnceInterrupted() {
	w.t.Helper()
	if !w.printedCue {
		w.t.Fatalf("the command never printed what its interrupt waited for\n%s", w.outputs())
	}
	if w.err != nil {
		w.t.Fatalf("the command ended with %v, want it to end cleanly once interrupted\n%s", w.err, w.outputs())
	}
}

// ThenItStopsWithin checks that the last command ran for no longer than d.
func (w *world) ThenItStopsWithin(d time.Duration) {
	w.t.Helper()
	if !w.ran {
		w.t.Fatal("no command has run")
	}
	if w.took > d {
		w.t.Errorf("the command ran for %s, want it to stop within %s", w.took, d)
	}
}
