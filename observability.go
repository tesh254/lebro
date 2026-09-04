package lebro

import (
	"time"

	"github.com/tesh254/lebro/internal/runtime"
)

func NewRunRecorder() *RunRecorder    { return runtime.NewRunRecorder() }
func NewFixedClock(t time.Time) Clock { return runtime.NewFixedClock(t) }
func NewFixedIDSource(runIDs []RunID, stepIDs []StepID) IDSource {
	return runtime.NewFixedIDSource(runIDs, stepIDs)
}

// NewUUIDIDSource creates process-independent UUIDv4 run and step IDs.
func NewUUIDIDSource() IDSource { return runtime.NewUUIDIDSource() }
