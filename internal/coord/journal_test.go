package coord

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/api"
)

// TestJournalRecordSize checks that a record the journal writes is one it reads back: one as large as a record may
// be round-trips, and a larger one is refused when written rather than lost when read.
func TestJournalRecordSize(t *testing.T) {
	dir := t.TempDir()
	warn := func(err error) { t.Errorf("warned: %v", err) }
	j, _, _, err := openJournal(dir, warn)
	if err != nil {
		t.Fatal(err)
	}
	withMessage := func(id api.JobID, n int) *record {
		return &record{ID: id, Prune: true, Outcome: &api.Outcome{Failure: &api.Failure{
			Message: strings.Repeat("x", n)}}}
	}
	// The record's encoding without its message, so that the large one is the most a record may hold.
	empty, err := encodedSize(withMessage("0000000000000001", 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := j.put(withMessage("0000000000000001", maxRecordSize-empty)); err != nil {
		t.Fatalf("writing a record as large as one may be: %v", err)
	}
	if err := j.put(withMessage("0000000000000002", maxRecordSize-empty+1)); err == nil ||
		!strings.Contains(err.Error(), "more than the") {
		t.Errorf("writing a record larger than one may be: %v, want it refused", err)
	}
	j.close()

	_, _, kept, err := openJournal(dir, warn)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 || kept[0].ID != "0000000000000001" ||
		len(kept[0].Outcome.Failure.Message) != maxRecordSize-empty {
		t.Errorf("the records read back = %d, want the large one whole", len(kept))
	}
}

// encodedSize returns how many bytes r's record holds.
func encodedSize(r *record) (int, error) {
	data, err := json.Marshal(r)
	return len(data), err
}
