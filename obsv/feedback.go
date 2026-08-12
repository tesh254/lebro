package obsv

import (
	"errors"
	"time"

	"github.com/tesh254/lebro"
)

// FeedbackKind classifies a feedback record.
type FeedbackKind string

const (
	// FeedbackKindRating is a numeric quality score.
	FeedbackKindRating FeedbackKind = "rating"
	// FeedbackKindThumb is a binary signal; Score is 1 for positive and 0 or
	// -1 for negative, as the application chooses.
	FeedbackKindThumb FeedbackKind = "thumb"
	// FeedbackKindCorrection is a corrected output supplied by a reviewer,
	// carried in Comment.
	FeedbackKindCorrection FeedbackKind = "correction"
	// FeedbackKindComment is free-form commentary with no score.
	FeedbackKindComment FeedbackKind = "comment"
)

// FeedbackRecord is a quality signal about a finished run.
//
// Feedback arrives after the run it describes — a reviewer rates an answer
// minutes or days later — so it is submitted through Observer.RecordFeedback
// rather than derived from run events. RunID is required; it is what correlates
// the record to the run's spans. TraceID is optional and resolved from the
// Observer's own record of the run when left empty, so a caller holding only a
// RunID still gets a correlated record.
//
// Comment can carry reviewer-supplied text, which is a sensitive field: a
// FeedbackFilter runs before export, and DefaultFeedbackFilter drops it.
type FeedbackRecord struct {
	ID        string            `json:"id,omitempty"`
	RunID     lebro.RunID       `json:"run_id"`
	TraceID   TraceID           `json:"trace_id,omitempty"`
	SpanID    SpanID            `json:"span_id,omitempty"`
	Kind      FeedbackKind      `json:"kind"`
\tScore     float64           `json:"score"`
	Comment   string            `json:"comment,omitempty"`
	Source    string            `json:"source,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// Clone returns a deep copy of the record.
func (r FeedbackRecord) Clone() FeedbackRecord {
	r.Metadata = cloneAttributes(r.Metadata)
	return r
}

// ErrInvalidFeedback is returned when a feedback record cannot be correlated to
// a run. It is the one error Observer.RecordFeedback reports to its caller,
// because a caller that submitted an unusable record can fix it; export failures
// stay internal.
var ErrInvalidFeedback = errors.New("lebro/obsv: invalid feedback record")

// Validate checks that the record carries the fields correlation requires.
func (r FeedbackRecord) Validate() error {
	if r.RunID == "" {
		return errors.New("lebro/obsv: feedback run ID is required")
	}
	switch r.Kind {
	case FeedbackKindRating, FeedbackKindThumb, FeedbackKindCorrection, FeedbackKindComment:
	case "":
		return errors.New("lebro/obsv: feedback kind is required")
	default:
		return errors.New("lebro/obsv: unknown feedback kind " + string(r.Kind))
	}
	return nil
}

func cloneFeedbackRecords(records []FeedbackRecord) []FeedbackRecord {
	if len(records) == 0 {
		return nil
	}
	cloned := make([]FeedbackRecord, len(records))
	for i, record := range records {
		cloned[i] = record.Clone()
	}
	return cloned
}
