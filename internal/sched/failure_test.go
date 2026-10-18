package sched

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/cache"

	"github.com/opencontainers/go-digest"
)

// TestFailureCrosses checks that a Runner's error survives as data: a step's failure keeps its step, hint, cleanup
// and log, a corrupt input stays corrupt for errors.Is and names its blob, a logged failure keeps its log,
// and each reads the same after crossing as JSON.
func TestFailureCrosses(t *testing.T) {
	log := cache.Blob{Digest: digest.FromString("log"), Size: 3}
	blob := cache.Blob{Digest: digest.FromString("file"), Size: 4}
	cases := []struct {
		name string
		err  error
	}{
		{name: "step", err: &StepError{Step: 2, Err: errors.New("exit status 1"), Hint: "look", Log: &log}},
		{name: "step with cleanup", err: &StepError{Step: 0, Err: errors.New("exit 2"), Cleanup: errors.New("busy")}},
		{name: "corrupt input", err: &StepError{Step: 1, Err: cache.Corrupt(blob, "it changed"), Log: &log}},
		{name: "logged", err: WithLog(errors.New("commit failed"), &log)},
		{name: "plain", err: errors.New("the registry is gone")},
		{name: "corrupt without a step", err: cache.Corrupt(blob, "no tree")},
		{name: "wrapped corrupt", err: fmt.Errorf("adding the tree: %w", cache.Corrupt(blob, "it changed"))},
		{name: "wrapped corrupt in a step", err: &StepError{Step: 0, Err: fmt.Errorf("reading: %w",
			cache.Corrupt(blob, "short"))}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data, err := json.Marshal(FailureOf(c.err))
			if err != nil {
				t.Fatal(err)
			}
			var crossed AttemptFailure
			if err := json.Unmarshal(data, &crossed); err != nil {
				t.Fatal(err)
			}
			got := crossed.Err()
			if got.Error() != c.err.Error() {
				t.Errorf("after crossing, the error reads %q, want %q", got, c.err)
			}
			if errors.Is(got, cache.ErrCorrupt) != errors.Is(c.err, cache.ErrCorrupt) {
				t.Errorf("after crossing, corrupt = %v, want %v", errors.Is(got, cache.ErrCorrupt),
					errors.Is(c.err, cache.ErrCorrupt))
			}
			var ce *cache.CorruptError
			if errors.As(c.err, &ce) {
				var crossedCE *cache.CorruptError
				if !errors.As(got, &crossedCE) || crossedCE.Blob != ce.Blob {
					t.Errorf("after crossing, the corrupt blob = %v, want %v", crossedCE, ce.Blob)
				}
			}
			var se, crossedSE *StepError
			if errors.As(c.err, &se) {
				if !errors.As(got, &crossedSE) || crossedSE.Step != se.Step || crossedSE.Hint != se.Hint ||
					(crossedSE.Log == nil) != (se.Log == nil) || (crossedSE.Cleanup == nil) != (se.Cleanup == nil) {
					t.Errorf("after crossing, the step error = %+v, want %+v", crossedSE, se)
				}
			}
			var logged, crossedLogged *loggedError
			if errors.As(c.err, &logged) && (!errors.As(got, &crossedLogged) || *crossedLogged.log != *logged.log) {
				t.Errorf("after crossing, the log = %v, want %v", crossedLogged, logged.log)
			}
		})
	}
}
