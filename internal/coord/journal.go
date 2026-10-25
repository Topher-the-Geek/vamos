package coord

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/api"

	"golang.org/x/sys/unix"
)

// journal keeps the coordinator's jobs on disk, in a directory of its own, so that a restart resumes them:
// the instance in the file "instance", and a record of each job in jobs/<id>.json.
// Each file is written whole to a temporary file, synced and renamed into place, and the directory synced after,
// so that a restart, even of the machine, finds each record as it was last written, or as it was before;
// a removal syncs the directory too.
// A job's record is written once the job is admitted and before its submission is acknowledged, and again as it
// changes: when it is cancelled, released or resumed, and when it ends; an evicted job's record becomes a tombstone,
// which says that it expired, and the oldest tombstones beyond tombstones are removed.
// The journal holds an exclusive lock on the file "lock" while open, so that two coordinators never share it.
// Once closed, as the coordinator shuts down, the journal writes nothing more, so that the jobs that the shutdown
// ends are resumed rather than recorded as cancelled.
// Writes are serialized by io, so that the records of one job reach the disk in the order they were made, while mu
// guards the tables alone, so that a lookup never waits for a write's syncs.
// A nil journal, a coordinator's that keeps its jobs in memory, records nothing.
type journal struct {
	dir  string
	lock *os.File

	io sync.Mutex // held across each write, and taken before mu

	mu          sync.Mutex
	records     map[api.JobID]*record
	submissions map[string]api.JobID // the jobs the submissions made, for records that aren't tombstones
	tombstones  []api.JobID          // the evicted jobs' records, oldest first
	closed      bool
}

// record is what the journal keeps of a job: the request that started it, the submission that made it, and how it
// has changed. Exactly one of Build, Push and Prune is set, until the job is evicted, when only ID and Evicted are.
type record struct {
	ID         api.JobID         `json:"id"`
	Submission string            `json:"submission,omitempty"` // the ID its client gave the submission
	Build      *api.GraphRequest `json:"build,omitempty"`
	Push       *api.PushRequest  `json:"push,omitempty"`
	Prune      bool              `json:"prune,omitempty"`
	Restarts   int64             `json:"restarts,omitempty"` // how many times a restart resumed or ended it
	Cancelled  bool              `json:"cancelled,omitempty"`
	Released   bool              `json:"released,omitempty"`
	Outcome    *api.Outcome      `json:"outcome,omitempty"` // once it ended
	Done       int64             `json:"done,omitempty"`    // the position of its terminal event, once it ended
	Ended      time.Time         `json:"ended,omitzero"`
	Evicted    time.Time         `json:"evicted,omitzero"` // once it was evicted: a tombstone
}

// tombstones is how many evicted jobs the journal remembers, as sched.Jobs does in memory.
const tombstones = 4096

// maxRecordSize is the most bytes a record may have, written or read, since a build's outcome lists its results.
const maxRecordSize = 16 << 20

// errCorruptRecord says that a job's record holds what isn't a record, as a write cut short would leave it.
var errCorruptRecord = errors.New("the record is corrupt")

// openJournal opens the journal in dir, making it if need be, and returns the coordinator's instance, made when the
// journal was, and the jobs' records, the tombstones oldest first. A record that is corrupt is reported to warn and
// removed, so that the job it was is forgotten, as a restart without a journal would forget it; one that can't be
// read fails the open, so that a passing fault of the disk loses nothing.
func openJournal(dir string, warn func(error)) (*journal, string, []*record, error) {
	for _, sub := range []string{"jobs", "tmp"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return nil, "", nil, err
		}
	}
	lock, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, "", nil, err
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lock.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, "", nil, errors.New("another coordinator is using it; a coordinator's state is its alone")
		}
		return nil, "", nil, err
	}
	j, instance, kept, err := loadJournal(dir, lock, warn)
	if err != nil {
		lock.Close()
		return nil, "", nil, err
	}
	return j, instance, kept, nil
}

// loadJournal reads the journal in dir, whose lock is held, as openJournal describes.
func loadJournal(dir string, lock *os.File, warn func(error)) (*journal, string, []*record, error) {
	// What a crash left mid-write is never a record.
	pending, err := os.ReadDir(filepath.Join(dir, "tmp"))
	if err != nil {
		return nil, "", nil, err
	}
	for _, e := range pending {
		if err := os.Remove(filepath.Join(dir, "tmp", e.Name())); err != nil {
			return nil, "", nil, err
		}
	}
	j := &journal{dir: dir, lock: lock, records: map[api.JobID]*record{}, submissions: map[string]api.JobID{}}
	instance, err := j.instance()
	if err != nil {
		return nil, "", nil, err
	}
	entries, err := os.ReadDir(filepath.Join(dir, "jobs"))
	if err != nil {
		return nil, "", nil, err
	}
	var records []*record
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		path := filepath.Join(dir, "jobs", e.Name())
		r, err := readJobRecord(path, api.JobID(name))
		switch {
		case errors.Is(err, errCorruptRecord):
			warn(fmt.Errorf("forgetting the job %s, whose record can't be read: %w", name, err))
			if err := j.removeFile(path); err != nil {
				return nil, "", nil, err
			}
			continue
		case err != nil:
			return nil, "", nil, fmt.Errorf("reading the record of the job %s: %w", name, err)
		}
		records = append(records, r)
	}
	// The tombstones go first, oldest first, so that the jobs table remembers them in the order they were evicted.
	slices.SortStableFunc(records, func(a, b *record) int { return a.Evicted.Compare(b.Evicted) })
	for _, r := range records {
		j.index(r)
		if !r.Evicted.IsZero() {
			j.tombstones = append(j.tombstones, r.ID)
		}
	}
	if err := j.trim(); err != nil {
		return nil, "", nil, err
	}
	// The caller gets copies, which it may change without changing what the journal holds.
	kept := make([]*record, 0, len(j.records))
	for _, r := range records {
		if _, ok := j.records[r.ID]; ok {
			c := *r
			kept = append(kept, &c)
		}
	}
	return j, instance, kept, nil
}

// instance returns the instance the journal was made with, making it if there is none yet.
func (j *journal) instance() (string, error) {
	path := filepath.Join(j.dir, "instance")
	data, err := os.ReadFile(path)
	switch {
	case err == nil && len(bytes.TrimSpace(data)) > 0:
		return string(bytes.TrimSpace(data)), nil
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return "", err
	}
	var id [8]byte
	rand.Read(id[:])
	instance := hex.EncodeToString(id[:])
	if err := j.write(path, []byte(instance+"\n")); err != nil {
		return "", err
	}
	return instance, nil
}

// readJobRecord reads the record at path, which must be the job id's: errCorruptRecord if what it holds isn't one.
func readJobRecord(path string, id api.JobID) (*record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxRecordSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxRecordSize {
		return nil, fmt.Errorf("%w: it holds more than %d bytes", errCorruptRecord, maxRecordSize)
	}
	var r record
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("%w: %w", errCorruptRecord, err)
	}
	kinds := 0
	for _, set := range []bool{r.Build != nil, r.Push != nil, r.Prune} {
		if set {
			kinds++
		}
	}
	switch {
	case r.ID != id:
		return nil, fmt.Errorf("%w: it is the record of the job %s", errCorruptRecord, r.ID)
	case kinds != 1 && r.Evicted.IsZero():
		return nil, fmt.Errorf("%w: it records %d kinds of job, want 1", errCorruptRecord, kinds)
	}
	return &r, nil
}

// index adds r to the tables, in place of any record of its job; j.mu must be held, or the journal not yet shared.
func (j *journal) index(r *record) {
	if old, ok := j.records[r.ID]; ok && old.Submission != "" {
		delete(j.submissions, old.Submission)
	}
	j.records[r.ID] = r
	if r.Submission != "" && r.Evicted.IsZero() {
		j.submissions[r.Submission] = r.ID
	}
}

// put writes r, a new job's record, failing if it can't be written, since the job would then not be resumed.
func (j *journal) put(r *record) error {
	if j == nil {
		return nil
	}
	j.io.Lock()
	defer j.io.Unlock()
	if j.isClosed() {
		return errPoolClosed
	}
	if err := j.store(r); err != nil {
		return err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.index(r)
	return nil
}

// update changes the record of the job id with change and writes it, if the job has a record that isn't a
// tombstone, and returns the write's failure.
func (j *journal) update(id api.JobID, change func(*record)) error {
	if j == nil {
		return nil
	}
	j.io.Lock()
	defer j.io.Unlock()
	j.mu.Lock()
	r, ok := j.records[id]
	closed := j.closed
	j.mu.Unlock()
	if closed || !ok || !r.Evicted.IsZero() {
		return nil
	}
	changed := *r
	change(&changed)
	if err := j.store(&changed); err != nil {
		return err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.index(&changed)
	return nil
}

// remove forgets the job id, which never started, whether or not the journal is closed, since a shutdown may refuse
// the start of a job that it recorded.
func (j *journal) remove(id api.JobID) error {
	if j == nil {
		return nil
	}
	j.io.Lock()
	defer j.io.Unlock()
	j.mu.Lock()
	r, ok := j.records[id]
	j.mu.Unlock()
	if !ok {
		return nil
	}
	if err := j.removeFile(j.path(id)); err != nil {
		return err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	delete(j.records, id)
	if r.Submission != "" {
		delete(j.submissions, r.Submission)
	}
	return nil
}

// evict replaces the record of the job id, which the jobs table has evicted, with a tombstone, and removes the
// oldest tombstones beyond those remembered.
func (j *journal) evict(id api.JobID) error {
	if j == nil {
		return nil
	}
	j.io.Lock()
	defer j.io.Unlock()
	j.mu.Lock()
	r, ok := j.records[id]
	closed := j.closed
	j.mu.Unlock()
	if closed || !ok || !r.Evicted.IsZero() {
		return nil
	}
	tombstone := &record{ID: id, Evicted: time.Now()}
	if err := j.store(tombstone); err != nil {
		return err
	}
	j.mu.Lock()
	j.index(tombstone)
	j.tombstones = append(j.tombstones, id)
	j.mu.Unlock()
	return j.trim()
}

// trim removes the oldest tombstones beyond those remembered; j.io must be held, or the journal not yet shared.
func (j *journal) trim() error {
	for {
		j.mu.Lock()
		if len(j.tombstones) <= tombstones {
			j.mu.Unlock()
			return nil
		}
		id := j.tombstones[0]
		j.mu.Unlock()
		if err := j.removeFile(j.path(id)); err != nil {
			return err
		}
		j.mu.Lock()
		delete(j.records, id)
		j.tombstones = j.tombstones[1:]
		j.mu.Unlock()
	}
}

// submitted returns the job that the submission id made, if the journal holds it and it isn't evicted.
func (j *journal) submitted(id string) (api.JobID, bool) {
	if j == nil || id == "" {
		return "", false
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	job, ok := j.submissions[id]
	return job, ok
}

// close makes the journal write nothing more, once the write in progress, if any, has ended, and gives its lock up.
func (j *journal) close() {
	if j == nil {
		return
	}
	j.io.Lock()
	defer j.io.Unlock()
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return
	}
	j.closed = true
	j.lock.Close() // closing releases the lock
}

// isClosed reports whether the journal is closed.
func (j *journal) isClosed() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.closed
}

// path returns where the record of the job id is kept.
func (j *journal) path(id api.JobID) string {
	return filepath.Join(j.dir, "jobs", string(id)+".json")
}

// store writes r at its path, refusing a record larger than one can be read back; j.io must be held.
func (j *journal) store(r *record) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if len(data) > maxRecordSize {
		return fmt.Errorf("its record would hold %d bytes, more than the %d a record may", len(data), maxRecordSize)
	}
	return j.write(j.path(r.ID), data)
}

// write replaces the file at path with data durably: written whole to a temporary file, synced, renamed into place,
// and its directory synced, so that a crash leaves either the old file or the new one.
func (j *journal) write(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Join(j.dir, "tmp"), "record-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // fails harmlessly once renamed
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// removeFile removes the file at path durably, syncing its directory; a file already gone is removed.
func (j *journal) removeFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// syncDir syncs the directory dir, so that the names it holds are on disk.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
