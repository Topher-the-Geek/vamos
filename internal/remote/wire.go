package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/Topher-the-Geek/vamos/internal/api"
	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/sched"
)

// The paths of the API, beneath a prefix that versions the wire format.
const (
	pathCapabilities = "/v1/capabilities"
	pathJobs         = "/v1/jobs"
	pathJob          = "/v1/jobs/{id}"
	pathEvents       = "/v1/jobs/{id}/events"
	pathCancel       = "/v1/jobs/{id}/cancel"
	pathRelease      = "/v1/jobs/{id}/release"
	pathResult       = "/v1/jobs/{id}/result"
	pathPrune        = "/v1/prune"
	pathBlobs        = "/v1/blobs"
	pathBlob         = "/v1/blobs/{digest}"
	pathMissing      = "/v1/blobs/missing"
	pathReadMany     = "/v1/blobs/read"
)

// The headers of the API.
const (
	// headerSubmission carries the ID a client chose for a submission, so that a retry returns the job accepted.
	headerSubmission = "Vamos-Submission"
	// headerInstance carries the instance of the coordinator that a retried submission was first sent to,
	// so that another instance, which can't know whether the first accepted it, refuses it rather than start it again.
	headerInstance = "Vamos-Instance"
	// headerErrorCode carries an error's code on a response that can't carry a body, such as HEAD's.
	headerErrorCode = "Vamos-Error-Code"
)

// contentJSON is the media type of the values that cross as JSON.
const contentJSON = "application/json"

// An errorCode names one of the sentinel errors of api and cache, which cross by code so that errors.Is holds.
type errorCode string

// The codes, and the sentinel each stands for.
const (
	codeNoJob      errorCode = "no_job"
	codeRunning    errorCode = "running"
	codeFellBehind errorCode = "fell_behind"
	codeMissing    errorCode = "missing"
	codeCorrupt    errorCode = "corrupt"
	codeOverLimit  errorCode = "over_limit"
	codeRestarted  errorCode = "restarted"
	codeBusy       errorCode = "busy"
	codeExpired    errorCode = "expired"
	codeSettling   errorCode = "settling"
)

// sentinels maps each code to its error, and the status a response carrying it has.
var sentinels = []struct {
	code   errorCode
	err    error
	status int
}{
	{codeNoJob, api.ErrNoJob, http.StatusNotFound},
	{codeRunning, api.ErrRunning, http.StatusConflict},
	{codeFellBehind, api.ErrFellBehind, http.StatusGone},
	{codeMissing, cache.ErrMissing, http.StatusNotFound},
	{codeCorrupt, cache.ErrCorrupt, http.StatusUnprocessableEntity},
	{codeOverLimit, cache.ErrOverLimit, http.StatusRequestEntityTooLarge},
	{codeRestarted, errOtherInstance, http.StatusPreconditionFailed},
	{codeStale, ErrStaleAttempt, http.StatusConflict},
	{codeBusy, sched.ErrBusy, http.StatusServiceUnavailable},
	{codeExpired, api.ErrExpired, http.StatusGone},
	{codeSettling, api.ErrSettling, http.StatusConflict},
}

// errOtherInstance says that a submission meant for one instance of the coordinator reached another.
var errOtherInstance = errors.New("this is another instance of the coordinator than the submission was sent to")

// wireError is an error as it crosses: its message, and the code of the sentinel it is, if it is one.
type wireError struct {
	Code    errorCode `json:"code,omitempty"`
	Message string    `json:"message"`
}

// Error returns the message, as the other side described the failure.
func (e *wireError) Error() string {
	return e.Message
}

// Is reports whether target is the sentinel the code stands for.
func (e *wireError) Is(target error) bool {
	for _, s := range sentinels {
		if s.code == e.Code {
			return target == s.err
		}
	}
	return false
}

// encodeError returns err as it crosses, with the status of a response carrying it:
// a sentinel by its code, which an error that crossed once already keeps, since wireError.Is makes it the sentinel,
// and any other error by its message.
func encodeError(err error) (*wireError, int) {
	for _, s := range sentinels {
		if errors.Is(err, s.err) {
			return &wireError{Code: s.code, Message: err.Error()}, s.status
		}
	}
	return &wireError{Message: err.Error()}, http.StatusInternalServerError
}

// writeError answers r with err, as data: its code in a header and, unless the response can't have a body,
// as JSON in the body.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	we, status := encodeError(err)
	if we.Code != "" {
		w.Header().Set(headerErrorCode, string(we.Code))
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", contentJSON)
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(we)
}

// writeJSON answers with v as JSON, with status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", contentJSON)
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// readError returns the error a failed response carries: the body's, or the header's code for a response that
// has no body, or else one that names the status.
func readError(resp *http.Response) error {
	we := &wireError{Code: errorCode(resp.Header.Get(headerErrorCode))}
	if resp.Request != nil && resp.Request.Method != http.MethodHead {
		data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		if err == nil && len(data) > 0 {
			var decoded wireError
			if json.Unmarshal(data, &decoded) == nil && decoded.Message != "" {
				decoded.Code = errorCode(firstNonEmpty(string(decoded.Code), string(we.Code)))
				we = &decoded
			}
		}
	}
	if we.Message == "" {
		we.Message = fmt.Sprintf("the coordinator answered %s", resp.Status)
		for _, s := range sentinels {
			if s.code == we.Code {
				we.Message = s.err.Error()
			}
		}
	}
	return we
}

// firstNonEmpty returns the first of strs that isn't "".
func firstNonEmpty(strs ...string) string {
	for _, s := range strs {
		if s != "" {
			return s
		}
	}
	return ""
}

// maxJSONBody is the most bytes a JSON request or response body may have: a request, an outcome or a list of
// blobs; a graph crosses as a blob, not here.
const maxJSONBody = 16 << 20

// decodeJSON decodes r, up to maxJSONBody bytes, into v.
func decodeJSON(r io.Reader, v any) error {
	data, err := io.ReadAll(io.LimitReader(r, maxJSONBody+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > maxJSONBody {
		return fmt.Errorf("the body holds more than %d bytes", maxJSONBody)
	}
	return json.Unmarshal(data, v)
}

// streamLine is one line of an event stream: an event, an error that ended the watch, or its clean end.
// Exactly one field is set.
type streamLine struct {
	Event *api.Event `json:"event,omitempty"`
	Error *wireError `json:"error,omitempty"`
	Done  bool       `json:"done,omitempty"` // the watch ended as api.Executor.Watch returns nil
}

// transportError says that a request didn't get an answer: the connection failed, dropped or timed out,
// so that the client may retry; it isn't an answer the other side gave.
type transportError struct {
	err error
}

// Error describes the failure.
func (e *transportError) Error() string {
	return e.err.Error()
}

// Unwrap returns the cause.
func (e *transportError) Unwrap() error {
	return e.err
}

// isTransport reports whether err is a failure to get an answer, as opposed to an answer that reports a failure
// or the caller cancelling the call. A call that ran out of time, by its own deadline or waiting for the answer's
// headers, got no answer either: a worker whose heartbeat timed out doesn't know what the coordinator thinks.
func isTransport(err error) bool {
	var te *transportError
	return errors.As(err, &te) && !errors.Is(err, context.Canceled)
}

// unreachableError says that the coordinator never answered at all: there is nothing to retry.
type unreachableError struct {
	base string
	err  error
}

// Error describes the failure.
func (e *unreachableError) Error() string {
	return fmt.Sprintf("can't reach the coordinator at %s: %v", e.base, e.err)
}

// Unwrap returns the cause.
func (e *unreachableError) Unwrap() error {
	return e.err
}
