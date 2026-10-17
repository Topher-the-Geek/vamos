// Package cli coordinates loading, execution and delivery for vamos commands.
//
// # Invariants
//
// Argument and destination validation precede execution.
// Project reads use the filesystem supplied to NewRootCommand; output artifacts use the host filesystem.
// The CLI owns presentation through the invocation's writers, which Cobra supplies, including the events of jobs.
//
// # Lifecycle
//
// NewRootCommand builds the Cobra tree over an app, what every invocation shares, and each command's RunE
// makes an invocation, the state of one run with its options, writers and context, and calls its handler,
// a method of invocation that a test can call without the root command.
// Each command owns its executor and optional traceSession.
// The graph and source trees are stored before the executor is called;
// a build is a job, submitted and watched through api.Executor by watchJob, which also cancels it on Ctrl-C,
// and renders its failure against the graph; a gap in its events is warned about and makes the trace incomplete.
// A result is delivered only after execution succeeds; a trace may still be written when the command fails.
//
// # Changes
//
// NewRootCommand wires commands and parses flags into options; the handlers, build, push, load, save, export,
// prune and graph, hold the phase boundary; Config says which container store the local executor builds in,
// so that tests give it their own.
// Output goes through invocation.printf, which returns a failure to write.
// Inspect destination for delivery and platform selection, eventPrinter for events,
// and traceSession for trace file ownership.
// Tests in this package exercise the real command path.
package cli
