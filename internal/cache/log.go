package cache

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

// The streams a log records.
const (
	StreamStdout byte = 1
	StreamStderr byte = 2
)

// LogRecorder keeps what a target prints to stdout and stderr, in the order it was written,
// so that a later build can replay it when it reuses the target's result.
// It writes the log to a temporary file as it comes, so that a long one isn't held in memory,
// and stores it as a blob when saved.
// A log is capped at MaxLogSize: what is printed past the cap is passed on but not recorded,
// and the log ends with Truncated, on stderr, saying so.
// Writers serialize log records but forward to their destination outside the lock;
// that destination must handle any concurrent writes itself.
// Stop writers before Save or Discard; Discard is not synchronized.
type LogRecorder struct {
	mu        sync.Mutex
	file      *os.File // records of a stream byte, a uvarint length and that many bytes
	size      int64
	truncated bool  // the cap was reached, and Truncated written
	err       error // the first error writing file
}

// Truncated ends a log that reached MaxLogSize.
const Truncated = "\n[vamos: the output was longer than the log keeps; the rest is not recorded]\n"

// NewLog starts recording a log, in a temporary file.
// The caller saves or discards it.
func NewLog() (*LogRecorder, error) {
	f, err := os.CreateTemp("", "vamos-log-")
	if err != nil {
		return nil, err
	}
	return &LogRecorder{file: f}, nil
}

// Writer returns a writer that passes output for stream on to w, recording it as well.
func (l *LogRecorder) Writer(stream byte, w io.Writer) io.Writer {
	return logWriter{l, stream, w}
}

// logWriter records what is written to one stream of a LogRecorder.
type logWriter struct {
	l      *LogRecorder
	stream byte
	w      io.Writer
}

// Write records data, unless the log is full, and passes it on.
// An error recording it is kept for save to report, so that the target's output still reaches the user.
func (lw logWriter) Write(data []byte) (int, error) {
	lw.l.mu.Lock()
	lw.l.record(lw.stream, data)
	lw.l.mu.Unlock()
	return lw.w.Write(data)
}

// record appends data, printed to stream, to the log, under the lock, or Truncated once the cap is reached.
func (l *LogRecorder) record(stream byte, data []byte) {
	if l.err != nil || l.truncated {
		return
	}
	marker := []byte(Truncated)
	if l.size+int64(len(data))+2*binary.MaxVarintLen64+2+int64(len(marker)) > MaxLogSize {
		l.truncated = true
		stream, data = StreamStderr, marker
	}
	header := binary.AppendUvarint([]byte{stream}, uint64(len(data)))
	n, err := l.file.Write(append(header, data...))
	l.size += int64(n)
	l.err = err
}

// Save stores the log as a blob in store, returning nil if nothing was written, and removes the file.
func (l *LogRecorder) Save(ctx context.Context, store ContentStore) (*Blob, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	defer l.remove()
	if l.err != nil {
		return nil, l.err
	}
	if l.size == 0 {
		return nil, nil
	}
	if _, err := l.file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	b, err := store.Put(ctx, l.file, MaxLogSize)
	if err != nil {
		return nil, fmt.Errorf("saving the log: %w", err)
	}
	return &b, nil
}

// Discard removes the log, unless it has been saved.
func (l *LogRecorder) Discard() {
	l.remove()
}

// remove closes and removes the file, once.
func (l *LogRecorder) remove() {
	if l.file != nil {
		l.file.Close()
		os.Remove(l.file.Name())
		l.file = nil
	}
}

// CheckLog reports whether data is a log that a LogRecorder recorded, whole.
func CheckLog(data []byte) error {
	return ReplayLog(context.Background(), bytes.NewReader(data), io.Discard, io.Discard)
}

// ReplayLog writes a log recorded by a LogRecorder to stdout and stderr, in its original order,
// stopping between records if ctx ends.
// A log that isn't whole fails once its good records have been written.
func ReplayLog(ctx context.Context, log io.Reader, stdout, stderr io.Writer) error {
	r := bufio.NewReader(log)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		stream, err := r.ReadByte()
		if errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
		n, err := binary.ReadUvarint(r)
		if err != nil {
			return fmt.Errorf("reading log: %w", err)
		}
		w := stdout
		if stream == StreamStderr {
			w = stderr
		}
		if _, err := io.CopyN(w, r, int64(n)); err != nil {
			return fmt.Errorf("reading log: %w", err)
		}
	}
}
