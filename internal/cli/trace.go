package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/Topher-the-Geek/vamos/internal/finish"
	"github.com/Topher-the-Geek/vamos/internal/tracing"
)

// clientProcess names the client's spans' process in a trace.
const clientProcess = "vamos"

// traceSession collects the spans of a command's trace, the client's and the executor's, for the file that --trace
// names.
type traceSession struct {
	path     string // absolute
	recorder *tracing.Recorder

	mu     sync.Mutex
	remote []tracing.Span // the executor's, by its clock
	offset tracing.Offset // how far the executor's clock is ahead of the client's
}

// newTraceSession returns a session whose trace goes to path, checking that a file can be written there, and that
// it isn't the command's own output: a file, or else a folder, if outputDir, which the trace mustn't be in. output
// is "" if the command writes no file.
func newTraceSession(path, output string, outputDir bool) (*traceSession, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("--trace %s: %w", path, err)
	}
	if output != "" {
		if outputDir {
			// Export replaces the folder at its path without following links in it, so a trace is in it if its
			// path is, even through a link out of it, as well as if it is once links are resolved.
			if within(output, abs) || within(resolve(output), resolve(abs)) {
				return nil, fmt.Errorf("--trace %s: it is within the folder that export replaces", path)
			}
		} else if sameFile(abs, output) {
			return nil, fmt.Errorf("--trace %s: it is also the file that save writes", path)
		}
	}
	if err := finish.CheckSaveDest(abs); err != nil {
		return nil, fmt.Errorf("--trace %s: %w", path, err)
	}
	return &traceSession{path: abs, recorder: tracing.NewRecorder(clientProcess, nil)}, nil
}

// within reports whether the absolute path p is dir or beneath it.
func within(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && filepath.IsLocal(rel) // Rel gives "." for dir itself, which IsLocal accepts
}

// sameFile reports whether the absolute paths a and b name the same file, even through links, and even if neither
// exists yet.
func sameFile(a, b string) bool {
	if resolve(a) == resolve(b) {
		return true
	}
	ai, aErr := os.Stat(a)
	bi, bErr := os.Stat(b)
	return aErr == nil && bErr == nil && os.SameFile(ai, bi)
}

// resolve returns the absolute path p with the links in its folder resolved, as far as they exist.
func resolve(p string) string {
	if dir, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		return filepath.Join(dir, filepath.Base(p))
	}
	return p
}

// addRemote keeps spans that the executor sent.
func (t *traceSession) addRemote(spans []tracing.Span) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.remote = append(t.remote, spans...)
}

// setOffset records how far the executor's clock is ahead of the client's.
func (t *traceSession) setOffset(offset tracing.Offset) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.offset = offset
}

// write writes the trace: the client's spans, and the executor's, placed by the client's clock.
func (t *traceSession) write() error {
	t.mu.Lock()
	spans := append(t.recorder.Spans(), tracing.Shift(t.remote, t.offset)...)
	t.mu.Unlock()
	return finish.WriteFile(t.path, func(w io.Writer) error {
		return tracing.WriteChrome(w, spans)
	})
}

// finishTrace writes the trace of a command that returned err, and returns the command's error: err, if it failed,
// with a failure to write the trace only warned about, or else that failure.
func (t *traceSession) finishTrace(err error, display string, stderr io.Writer) error {
	werr := t.write()
	if werr == nil {
		return err
	}
	werr = fmt.Errorf("writing trace %s: %w", display, werr)
	if err != nil {
		fmt.Fprintf(stderr, "vamos: warning: %v\n", werr)
		return err
	}
	return werr
}
