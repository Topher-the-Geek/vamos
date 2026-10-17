// Package cli coordinates loading, execution and delivery for vamos commands.
//
// # Invariants
//
// Argument and destination validation precede execution.
// Project reads use the filesystem supplied to NewRootCommand; output artifacts use the host filesystem.
// The CLI owns presentation through Cobra's writers, including run.Events output.
//
// # Lifecycle
//
// Each command owns its executor and optional traceSession.
// The graph and source trees are stored before the executor is called.
// A result is delivered only after execution succeeds; a trace may still be written when the command fails.
//
// # Changes
//
// NewRootCommand wires commands and the phase boundary.
// Inspect destination for delivery and platform selection, eventPrinter for callbacks,
// and traceSession for trace file ownership.
// Tests in this package exercise the real command path.
package cli
