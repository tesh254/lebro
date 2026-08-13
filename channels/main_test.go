package channels_test

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/tesh254/lebro"
	"go.uber.org/goleak"
)

// TestMain verifies no goroutine leaks across the suite. The handler streams
// runs on their own goroutines, so a leaked run goroutine — for example one
// left parked on an undrained delta channel — is a real defect this catches.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// scriptedStreamModel is a minimal streaming model that emits a fixed sequence
// of text deltas followed by a terminal stop. It is the streaming counterpart a
// channel run needs; it deliberately does not support Generate so a test proves
// the run went through the streaming path.
type scriptedStreamModel struct {
	mu    sync.Mutex
	parts []string
	calls int
}

func newScriptedStreamModel(parts ...string) *scriptedStreamModel {
	return &scriptedStreamModel{parts: parts}
}

func (m *scriptedStreamModel) Generate(context.Context, lebro.ModelRequest) (lebro.ModelResponse, error) {
	return lebro.ModelResponse{}, errors.New("scripted stream model does not support Generate")
}

func (m *scriptedStreamModel) Stream(ctx context.Context, _ lebro.ModelRequest) (lebro.StreamReader, error) {
	m.mu.Lock()
	m.calls++
	parts := append([]string(nil), m.parts...)
	m.mu.Unlock()

	deltas := make([]lebro.StreamDelta, 0, len(parts)+1)
	for _, part := range parts {
		deltas = append(deltas, lebro.StreamDelta{Text: part})
	}
	deltas = append(deltas, lebro.StreamDelta{FinishReason: lebro.FinishReasonStop})

	out := make(chan lebro.StreamDelta, len(deltas))
	closed := make(chan struct{})
	go func() {
		defer close(out)
		for _, delta := range deltas {
			select {
			case out <- delta:
			case <-ctx.Done():
				return
			case <-closed:
				return
			}
		}
	}()

	return &lebro.StreamReaderFunc{
		NextFn: func() (lebro.StreamDelta, error) {
			delta, ok := <-out
			if !ok {
				return lebro.StreamDelta{}, io.EOF
			}
			return delta, nil
		},
		CloseFn: func() error {
			select {
			case <-closed:
			default:
				close(closed)
			}
			return nil
		},
	}, nil
}

// streamCalls reports how many times Stream was invoked, letting a test assert
// a duplicate delivery did not re-run the model.
func (m *scriptedStreamModel) streamCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

var (
	_ lebro.Model          = (*scriptedStreamModel)(nil)
	_ lebro.StreamingModel = (*scriptedStreamModel)(nil)
)

// newTestAgent builds an agent over the scripted model with a stable ID.
func newTestAgent(t *testing.T, id string, model lebro.Model, store lebro.Store) *lebro.Agent {
	t.Helper()
	agent, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{ID: lebro.AgentID(id), Name: id},
		Model:      model,
		Store:      store,
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	return agent
}
