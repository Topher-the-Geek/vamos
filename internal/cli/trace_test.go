package cli

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/tracing"
)

// TestFinishTrace checks which error a traced command returns when its trace can't be written:
// that failure if the command succeeded, or else the command's own, with the trace's failure only warned about.
func TestFinishTrace(t *testing.T) {
	failed := errors.New("the build failed")
	for _, tt := range []struct {
		name    string
		err     error
		want    string
		warning string
	}{
		{
			name: "command succeeded",
			err:  nil,
			want: "writing trace t.json: ",
		},
		{
			name:    "command failed",
			err:     failed,
			want:    failed.Error(),
			warning: "vamos: warning: writing trace t.json: ",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// The folder is gone by the time the trace is written.
			session := &traceSession{
				path:     filepath.Join(t.TempDir(), "gone", "t.json"),
				recorder: tracing.NewRecorder(clientProcess, nil),
			}
			var stderr strings.Builder
			err := session.finishTrace(tt.err, "t.json", &stderr)
			if err == nil || !strings.HasPrefix(err.Error(), tt.want) {
				t.Errorf("error = %v, want %q", err, tt.want)
			}
			if got := stderr.String(); tt.warning == "" && got != "" || !strings.HasPrefix(got, tt.warning) {
				t.Errorf("stderr = %q, want %q", got, tt.warning)
			}
		})
	}
}
