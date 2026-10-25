package remote

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/api"
	"github.com/Topher-the-Geek/vamos/internal/cache"

	"github.com/opencontainers/go-digest"
)

// Server serves an executor and a content store over HTTP, for Clients.
// It holds no state of the builds: the executor does. What it keeps is which job each submission made,
// so that a client retrying a submission whose acknowledgement was lost gets the job already accepted;
// an executor that keeps its jobs across restarts, a SubmissionLog, answers for the submissions of its earlier runs.
// It doesn't serve ClearContent: clearing the store while builds use it is a prune's, which the executor excludes
// them from.
type Server struct {
	Executor api.Executor
	Content  cache.ContentStore
	// Middleware wraps every handler, so that one place can authenticate requests; nil wraps nothing.
	Middleware func(http.Handler) http.Handler
	// GracePeriod is reported in the capabilities when the executor reports none: how long a client may be gone
	// and still find its build; an executor that keeps jobs until it closes may report any.
	GracePeriod time.Duration
	// KeepAlive is how often an event stream with nothing to send sends an empty line, so that a client tells a
	// quiet build from a connection that has silently died; defaultKeepAlive if zero.
	KeepAlive time.Duration
	// Workers, if set, is served to workers: a coordinator's pool; a server of a local executor has none.
	Workers Workers

	mu          sync.Mutex
	submissions map[string]*submission // the job each submission ID made, or is making
	finished    []*submission          // the submissions whose jobs were made, in the order they were
	streams     context.Context        // ends when EndStreams is called; nil until first asked for
	endStreams  context.CancelFunc
}

// EndStreams ends every event stream and assignment poll being served, and those that arrive later, at once, as cut
// connections, which clients and workers retry, so that a shutdown that resumes its jobs can wait for what else is
// in flight, uploads and reports, with http.Server.Shutdown, without waiting for streams that never end.
func (s *Server) EndStreams() {
	s.streamContext()
	s.endStreams()
}

// streamContext returns the context that EndStreams ends.
func (s *Server) streamContext() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.streams == nil {
		s.streams, s.endStreams = context.WithCancel(context.Background())
	}
	return s.streams
}

// streaming returns r's context, ended too by EndStreams, for a request whose answer may last indefinitely.
func (s *Server) streaming(r *http.Request) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(s.streamContext(), cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

// submission is what a submission ID made: a job, once done is closed, or a refusal.
type submission struct {
	key      string // its submission ID
	done     chan struct{}
	id       api.JobID
	err      error
	finished time.Time // when done was closed, under Server.mu; zero while the job is being made
}

// submissionSlack is how long a submission is remembered beyond the grace period: a client retries for the grace
// period from when it saw the drop, which it sees up to a response header timeout after the job was made,
// and its last retry may wait out a backoff.
const submissionSlack = responseHeaderTimeout + maxBackoff

// defaultKeepAlive is how often an idle event stream sends an empty line.
const defaultKeepAlive = 15 * time.Second

// maxReadMany is how many blobs one ReadMany request may ask for, since the server holds them all before answering.
const maxReadMany = 256

// Handler returns the handler that serves the API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+pathCapabilities, s.capabilities)
	mux.HandleFunc("POST "+pathJobs, s.submit)
	mux.HandleFunc("GET "+pathEvents, s.events)
	mux.HandleFunc("POST "+pathCancel, s.cancel)
	mux.HandleFunc("POST "+pathRelease, s.release)
	mux.HandleFunc("GET "+pathResult, s.result)
	mux.HandleFunc("POST "+pathPrune, s.prune)
	mux.HandleFunc("POST "+pathMissing, s.missing)
	mux.HandleFunc("POST "+pathReadMany, s.readMany)
	mux.HandleFunc("POST "+pathBlobs, s.put)
	mux.HandleFunc("HEAD "+pathBlob, s.has)
	mux.HandleFunc("GET "+pathBlob, s.get)
	mux.HandleFunc("PUT "+pathBlob, s.importBlob)
	if s.Workers != nil {
		s.registerWorkers(mux, s.Workers)
	}
	var h http.Handler = mux
	if s.Middleware != nil {
		h = s.Middleware(h)
	}
	return h
}

// capabilities answers with the executor's capabilities.
func (s *Server) capabilities(w http.ResponseWriter, r *http.Request) {
	caps, err := s.Executor.Capabilities(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	if caps.GracePeriod == 0 {
		caps.GracePeriod = s.GracePeriod
	}
	writeJSON(w, http.StatusOK, caps)
}

// submit starts the job the body asks for, or returns the job that the same submission started before.
func (s *Server) submit(w http.ResponseWriter, r *http.Request) {
	var req api.Request
	if err := decodeJSON(r.Body, &req); err != nil {
		writeError(w, r, fmt.Errorf("decoding the request: %w", err))
		return
	}
	s.accept(w, r, func(ctx context.Context) (api.JobID, error) { return s.Executor.Submit(ctx, req) })
}

// prune starts a prune job, or returns the job that the same submission started before.
func (s *Server) prune(w http.ResponseWriter, r *http.Request) {
	s.accept(w, r, func(ctx context.Context) (api.JobID, error) { return s.Executor.Prune(ctx) })
}

// accept answers r, a submission, with the job that start starts, or with the job the submission ID the request
// carries started before, if it did, so that a retried submission runs once: a repeat that arrives while the first
// request is still being served waits for its outcome.
// A retry names the instance it was first sent to, and another instance refuses it with errOtherInstance:
// it can't know whether the first accepted it.
// The submission is remembered for as long as its client may retry it: until the grace period, and some slack,
// has passed since its job was made; one an earlier run of a SubmissionLog accepted is its to answer for.
// A submission without an ID is accepted every time.
func (s *Server) accept(w http.ResponseWriter, r *http.Request, start func(context.Context) (api.JobID, error)) {
	id := r.Header.Get(headerSubmission)
	if id == "" {
		jobID, err := start(r.Context())
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, jobHandle{ID: jobID})
		return
	}
	caps, err := s.Executor.Capabilities(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	if want := r.Header.Get(headerInstance); want != "" && want != caps.Instance {
		writeError(w, r, fmt.Errorf("%w: it was sent to %s, and this is %s", errOtherInstance, want, caps.Instance))
		return
	}
	grace := cmp.Or(caps.GracePeriod, s.GracePeriod, defaultGrace)
	sub, first := s.claim(id, grace+submissionSlack)
	if !first {
		select {
		case <-sub.done:
		case <-r.Context().Done():
			return
		}
		if sub.err != nil {
			writeError(w, r, sub.err)
			return
		}
		writeJSON(w, http.StatusOK, jobHandle{ID: sub.id})
		return
	}
	if log, ok := s.Executor.(SubmissionLog); ok {
		if job, found := log.Submitted(id); found {
			sub.id = job
			s.finish(sub)
			writeJSON(w, http.StatusOK, jobHandle{ID: job})
			return
		}
	}
	sub.id, sub.err = start(context.WithValue(r.Context(), submissionKey{}, id))
	s.finish(sub)
	if sub.err != nil {
		s.forget(id)
		writeError(w, r, sub.err)
		return
	}
	writeJSON(w, http.StatusCreated, jobHandle{ID: sub.id})
}

// SubmissionLog is an executor that keeps its jobs across its restarts, and with each the ID of the submission that
// made it, which the context of Submit and Prune carries, as Submission returns it.
type SubmissionLog interface {
	// Submitted returns the job that the submission id made, if it made one that the executor still holds.
	Submitted(id string) (api.JobID, bool)
}

type submissionKey struct{}

// Submission returns the ID of the submission that ctx, a Submit's or a Prune's that a Server serves, carries,
// or "" for a submission without one.
func Submission(ctx context.Context) string {
	id, _ := ctx.Value(submissionKey{}).(string)
	return id
}

// jobHandle is how a job's ID crosses.
type jobHandle struct {
	ID api.JobID `json:"id"`
}

// claim returns the submission with ID id, and whether this call made it, so that the caller starts its job.
// It forgets the submissions whose jobs were made more than keep ago, and never one whose job is being made.
func (s *Server) claim(id string, keep time.Duration) (*submission, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.submissions == nil {
		s.submissions = map[string]*submission{}
	}
	for len(s.finished) > 0 && time.Since(s.finished[0].finished) > keep {
		if old := s.finished[0]; s.submissions[old.key] == old { // not forgotten, nor claimed again since
			delete(s.submissions, old.key)
		}
		s.finished = s.finished[1:]
	}
	if sub, found := s.submissions[id]; found {
		return sub, false
	}
	sub := &submission{key: id, done: make(chan struct{})}
	s.submissions[id] = sub
	return sub, true
}

// finish records that sub has made its job, or failed to, and tells those waiting for it.
func (s *Server) finish(sub *submission) {
	s.mu.Lock()
	sub.finished = time.Now()
	s.finished = append(s.finished, sub)
	s.mu.Unlock()
	close(sub.done)
}

// forget drops the submission id, whose job was refused, so that a repeat is tried again.
func (s *Server) forget(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.submissions, id)
}

// events streams the events of a job from the position asked for, one JSON line each, with an empty line
// whenever nothing has been sent for a while, until the watch ends: cleanly, with a line that says so,
// or with an error, as a line too, so that a client tells either from a drop.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	from, err := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
	if err != nil {
		writeError(w, r, fmt.Errorf("the position to watch from isn't a number: %w", err))
		return
	}
	id := api.JobID(r.PathValue("id"))
	ctx, cancel := s.streaming(r)
	defer cancel()
	lines := make(chan streamLine)
	ended := make(chan error, 1)
	go func() {
		ended <- s.Executor.Watch(ctx, id, from, func(e api.Event) error {
			select {
			case lines <- streamLine{Event: &e}:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	flusher, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	started := false
	send := func(line streamLine) {
		if !started {
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			started = true
		}
		if err := enc.Encode(line); err != nil {
			cancel() // the client has gone: the watch ends
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	keepAlive := s.KeepAlive
	if keepAlive <= 0 {
		keepAlive = defaultKeepAlive
	}
	ticker := time.NewTicker(keepAlive)
	defer ticker.Stop()
	for {
		select {
		case line := <-lines:
			send(line)
		case <-ticker.C:
			send(streamLine{})
		case err := <-ended:
			switch {
			case err == nil:
				send(streamLine{Done: true})
			case ctx.Err() != nil:
				// The client has gone: nothing can be told it.
			case !started:
				writeError(w, r, err) // such as an unknown job, which is a plain answer
			default:
				we, _ := encodeError(err)
				send(streamLine{Error: we})
			}
			return
		}
	}
}

// cancel asks a job to stop.
func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	if err := s.Executor.Cancel(r.Context(), api.JobID(r.PathValue("id"))); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// release lets the executor free a job the client is done with.
func (s *Server) release(w http.ResponseWriter, r *http.Request) {
	if err := s.Executor.Release(r.Context(), api.JobID(r.PathValue("id"))); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// result answers with a job's outcome, or that it is running.
func (s *Server) result(w http.ResponseWriter, r *http.Request) {
	outcome, err := s.Executor.Result(r.Context(), api.JobID(r.PathValue("id")))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, outcome)
}

// blobOf returns the blob that r names: its digest in the path and its size in the query.
func blobOf(r *http.Request) (cache.Blob, error) {
	size, err := strconv.ParseInt(r.URL.Query().Get("size"), 10, 64)
	if err != nil {
		return cache.Blob{}, fmt.Errorf("the blob's size isn't a number: %w", err)
	}
	b := cache.Blob{Digest: digest.Digest(r.PathValue("digest")), Size: size}
	return b, b.Validate()
}

// limitOf returns the limit that r's query gives.
func limitOf(r *http.Request) (int64, error) {
	limit, err := strconv.ParseInt(r.URL.Query().Get("limit"), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("the limit isn't a number: %w", err)
	}
	return limit, nil
}

// has answers whether the store holds the blob.
func (s *Server) has(w http.ResponseWriter, r *http.Request) {
	b, err := blobOf(r)
	if err == nil {
		err = s.Content.Has(r.Context(), b)
	}
	if err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// rangeStart returns the offset that r's Range header asks to start from, "bytes=N-", or 0 without one.
func rangeStart(r *http.Request) (int64, error) {
	h := r.Header.Get("Range")
	if h == "" {
		return 0, nil
	}
	spec, ok := strings.CutPrefix(h, "bytes=")
	start, hasEnd := strings.CutSuffix(spec, "-")
	if !ok || !hasEnd {
		return 0, fmt.Errorf("range %q: want bytes=N-", h)
	}
	n, err := strconv.ParseInt(start, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("range %q: want bytes=N-", h)
	}
	return n, nil
}

// get answers with the blob's content from the offset a Range header asks for, chunked, which the client verifies.
// A blob found corrupt as it is read is cut short, and the error's code follows as a trailer,
// so that the client tells a corrupt blob from a dropped connection, which ends the body without one.
func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	b, err := blobOf(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	start, err := rangeStart(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if start > b.Size {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	rc, err := s.Content.Get(r.Context(), b)
	if err != nil {
		writeError(w, r, err)
		return
	}
	defer rc.Close()
	if _, err := io.CopyN(io.Discard, rc, start); err != nil {
		writeError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Trailer", headerErrorCode)
	if start > 0 {
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	if _, err := io.Copy(w, rc); err != nil {
		if we, _ := encodeError(err); we.Code != "" {
			w.Header().Set(headerErrorCode, string(we.Code))
		}
	}
}

// put stores the body, up to the limit, and answers with its blob.
func (s *Server) put(w http.ResponseWriter, r *http.Request) {
	limit, err := limitOf(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	b, err := s.Content.Put(r.Context(), r.Body, limit)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, b)
}

// importBlob stores the body as the blob named, which it must be.
func (s *Server) importBlob(w http.ResponseWriter, r *http.Request) {
	b, err := blobOf(r)
	if err == nil {
		err = s.Content.Import(r.Context(), b, r.Body)
	}
	if err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// missing answers which of the blobs in the body the store lacks.
func (s *Server) missing(w http.ResponseWriter, r *http.Request) {
	var blobs []cache.Blob
	if err := decodeJSON(r.Body, &blobs); err != nil {
		writeError(w, r, fmt.Errorf("decoding the blobs: %w", err))
		return
	}
	missing, err := s.Content.Missing(r.Context(), blobs)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if missing == nil {
		missing = []cache.Blob{}
	}
	writeJSON(w, http.StatusOK, missing)
}

// readMany answers with the content of each blob in the body, in order, one after the other:
// the client knows each blob's size, and verifies each.
// Every blob is read before anything is sent, so that a failure is an answer rather than a short body,
// which bounds a request to maxReadMany blobs.
func (s *Server) readMany(w http.ResponseWriter, r *http.Request) {
	limit, err := limitOf(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	var blobs []cache.Blob
	if err := decodeJSON(r.Body, &blobs); err != nil {
		writeError(w, r, fmt.Errorf("decoding the blobs: %w", err))
		return
	}
	if len(blobs) > maxReadMany {
		writeError(w, r, fmt.Errorf("%d blobs asked for at once; at most %d may be", len(blobs), maxReadMany))
		return
	}
	contents, err := s.Content.ReadMany(r.Context(), blobs, limit)
	if err != nil {
		writeError(w, r, err)
		return
	}
	total := int64(0)
	for _, c := range contents {
		total += int64(len(c))
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(total, 10))
	w.WriteHeader(http.StatusOK)
	for _, c := range contents {
		if _, err := w.Write(c); err != nil {
			return
		}
	}
}
