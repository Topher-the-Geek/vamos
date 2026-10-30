package cli

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestDeadlineInBuildFile checks that a program embedding vamos, whose context has a deadline, gets the deadline's
// error when it passes while a BUILD file runs, loading or configuring a target, rather than a mistake in the file.
// A user's interrupt is specified by the interrupt feature; only a program sets a deadline.
func TestDeadlineInBuildFile(t *testing.T) {
	spin := "def spin(ctx = None):\n    for i in range(1 << 40):\n        pass\n"
	for _, tt := range []struct {
		name, build, target string
	}{
		{name: "loading", build: spin + "spin()\n", target: "busybox"},
		{name: "configuring", build: spin + `layer(name="app", base="busybox", steps=spin)` + "\n", target: "app"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			defer cancel()
			start := time.Now()
			r := vamosWithOutput(t, project(t, map[string]string{"BUILD": pullBusybox + tt.build}), ctx, nil,
				"build", tt.target, "-C", projectDir)
			if !errors.Is(r.err, context.DeadlineExceeded) {
				t.Errorf("vamos = %v, want the context's %v", r.err, context.DeadlineExceeded)
			}
			if took := time.Since(start); took > 5*time.Second {
				t.Errorf("the build ran for %s after its deadline", took)
			}
		})
	}
}
