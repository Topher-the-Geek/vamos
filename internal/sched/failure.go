package sched

import (
	"errors"

	"github.com/Topher-the-Geek/vamos/internal/cache"
)

// AttemptFailure is why an attempt failed, as a worker reports it to its coordinator: everything the Scheduler
// tells apart in a Runner's error, as data that crosses as JSON.
type AttemptFailure struct {
	Message string      `json:"message"`
	Step    *int        `json:"step,omitempty"`    // the index of the step that failed, if one did
	Hint    string      `json:"hint,omitempty"`    // what may have caused it, if there is advice
	Cleanup string      `json:"cleanup,omitempty"` // a failure to remove the attempt's container afterwards
	Log     *cache.Blob `json:"log,omitempty"`     // what the attempt printed, saved to the content store
	// Corrupt names the blob that was found corrupt, if that is why the attempt failed, so that the Scheduler
	// regenerates it; Message describes the corruption.
	Corrupt *cache.Blob `json:"corrupt,omitempty"`
}

// FailureOf returns err, a Runner's, as data.
func FailureOf(err error) *AttemptFailure {
	f := &AttemptFailure{Message: err.Error()}
	var se *StepError
	if errors.As(err, &se) {
		step := se.Step
		f.Step, f.Hint, f.Log, f.Message = &step, se.Hint, se.Log, se.Err.Error()
		if se.Cleanup != nil {
			f.Cleanup = se.Cleanup.Error()
		}
		err = se.Err
	}
	var logged *loggedError
	if f.Log == nil && errors.As(err, &logged) {
		f.Log = logged.log
	}
	var ce *cache.CorruptError
	if errors.As(err, &ce) {
		f.Corrupt = &ce.Blob
	}
	return f
}

// Err returns the failure as the error a Runner would have returned: a StepError for a step's failure,
// with the corrupt blob as its cause if there was one, and otherwise the message with its log.
// The error reads as the message, whatever wrapped the corruption on the worker.
func (f *AttemptFailure) Err() error {
	var cause error
	if f.Corrupt != nil {
		cause = corruptCause(f.Message, *f.Corrupt)
	} else {
		cause = errors.New(f.Message)
	}
	if f.Step == nil {
		return WithLog(cause, f.Log)
	}
	se := &StepError{Step: *f.Step, Err: cause, Hint: f.Hint, Log: f.Log}
	if f.Cleanup != "" {
		se.Cleanup = errors.New(f.Cleanup)
	}
	return se
}

// corruptCause returns the error that reads as message and is the corruption of b: a cache.CorruptError itself
// when message is one's description, and otherwise message wrapping one, as the worker's error wrapped it.
func corruptCause(message string, b cache.Blob) error {
	prefix := (&cache.CorruptError{Blob: b}).Error()
	if len(message) > len(prefix) && message[:len(prefix)] == prefix {
		return &cache.CorruptError{Blob: b, Reason: message[len(prefix):]}
	}
	return &wrappedCorrupt{message: message, corrupt: &cache.CorruptError{Blob: b, Reason: message}}
}

// wrappedCorrupt is a message that wrapped a corruption, as errors.As finds it.
type wrappedCorrupt struct {
	message string
	corrupt *cache.CorruptError
}

// Error returns the message as the worker had it.
func (e *wrappedCorrupt) Error() string {
	return e.message
}

// Unwrap returns the corruption.
func (e *wrappedCorrupt) Unwrap() error {
	return e.corrupt
}
