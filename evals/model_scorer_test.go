package evals_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/evals"
)

// graderModel is a lebro.Model that returns a scripted reply and records the
// request it received. It keeps the test provider-neutral: the evals package
// never learns which adapter graded.
//
// Recording is mutex-guarded because an experiment scores cases in parallel, so
// one grader is called from several goroutines at once.
type graderModel struct {
	content    string
	structured string
	usage      lebro.ModelUsage
	err        error

	mu       sync.Mutex
	recorded lebro.ModelRequest
}

func (m *graderModel) Generate(_ context.Context, request lebro.ModelRequest) (lebro.ModelResponse, error) {
	m.mu.Lock()
	m.recorded = request
	m.mu.Unlock()
	if m.err != nil {
		return lebro.ModelResponse{}, m.err
	}
	message := lebro.Message{Role: lebro.RoleAssistant, Content: m.content}
	if m.structured != "" {
		message.StructuredOutput = lebro.NewModelStructuredOutput(json.RawMessage(m.structured))
	}
	return lebro.ModelResponse{Message: message, FinishReason: lebro.FinishReasonStop, Usage: m.usage}, nil
}

func TestModelScorerReadsStructuredVerdict(t *testing.T) {
	model := &graderModel{
		structured: `{"score":0.9,"passed":true,"reason":"accurate and complete"}`,
		usage:      lebro.ModelUsage{InputTokens: 40, OutputTokens: 8, TotalTokens: 48},
	}
	scorer, err := evals.NewModelScorer(evals.ModelScorerConfig{Model: model, ModelName: "grader-1"})
	if err != nil {
		t.Fatalf("NewModelScorer() = %v", err)
	}
	if scorer.Name() != "model_graded" {
		t.Fatalf("Name() = %q, want model_graded", scorer.Name())
	}

	score, err := scorer.Score(context.Background(),
		evals.Case{ID: "c1", Expected: "Paris"}, evals.Output{Text: "The capital is Paris."})
	if err != nil {
		t.Fatalf("Score() = %v", err)
	}
	if score.Value != 0.9 || !score.Passed {
		t.Fatalf("score = (%v, %t), want (0.9, true)", score.Value, score.Passed)
	}
	if score.Reason != "accurate and complete" {
		t.Fatalf("Reason = %q", score.Reason)
	}
	if score.CaseID != "c1" || score.Scorer != "model_graded" {
		t.Fatalf("identity = (%q, %q)", score.CaseID, score.Scorer)
	}
	if score.Metadata["grader.input_tokens"] != "40" {
		t.Fatalf("usage metadata = %v, want the grader's token counts", score.Metadata)
	}

	// The request goes out through the existing model protocol with the schema
	// attached, so a provider that supports structured output enforces the shape.
	if model.seen().Model != "grader-1" {
		t.Fatalf("request Model = %q, want grader-1", model.seen().Model)
	}
	if model.seen().OutputSchema == nil || len(model.seen().OutputSchema.Schema) == 0 {
		t.Fatal("request carries no output schema")
	}
	if len(model.seen().Messages) != 2 || model.seen().Messages[0].Role != lebro.RoleSystem {
		t.Fatalf("request messages = %+v, want a system prompt and a user payload", model.seen().Messages)
	}
	if !strings.Contains(model.seen().Messages[1].Content, "Paris") {
		t.Fatalf("grading prompt omits the output: %q", model.seen().Messages[1].Content)
	}
}

// TestModelScorerFallsBackToMessageContent covers a provider that ignores the
// requested schema and replies with JSON in the message body.
func TestModelScorerFallsBackToMessageContent(t *testing.T) {
	model := &graderModel{content: `{"score":0.25,"reason":"missed the question"}`}
	scorer, err := evals.NewModelScorer(evals.ModelScorerConfig{Model: model})
	if err != nil {
		t.Fatalf("NewModelScorer() = %v", err)
	}
	score, err := scorer.Score(context.Background(), evals.Case{ID: "c1"}, evals.Output{Text: "Lyon"})
	if err != nil {
		t.Fatalf("Score() = %v", err)
	}
	if score.Value != 0.25 {
		t.Fatalf("Value = %v, want 0.25", score.Value)
	}
	// No "passed" in the reply, so the threshold decides.
	if score.Passed {
		t.Fatal("Passed = true for 0.25 under the default 0.5 threshold")
	}
}

func TestModelScorerAppliesThreshold(t *testing.T) {
	model := &graderModel{structured: `{"score":0.7}`}
	strict, err := evals.NewModelScorer(evals.ModelScorerConfig{Model: model, Threshold: 0.8})
	if err != nil {
		t.Fatalf("NewModelScorer() = %v", err)
	}
	score, err := strict.Score(context.Background(), evals.Case{ID: "c1"}, evals.Output{Text: "answer"})
	if err != nil {
		t.Fatalf("Score() = %v", err)
	}
	if score.Passed {
		t.Fatal("Passed = true for 0.7 under a 0.8 threshold")
	}

	lenient, err := evals.NewModelScorer(evals.ModelScorerConfig{Model: model, Threshold: 0.6})
	if err != nil {
		t.Fatalf("NewModelScorer() = %v", err)
	}
	score, err = lenient.Score(context.Background(), evals.Case{ID: "c1"}, evals.Output{Text: "answer"})
	if err != nil {
		t.Fatalf("Score() = %v", err)
	}
	if !score.Passed {
		t.Fatal("Passed = false for 0.7 under a 0.6 threshold")
	}
}

// TestModelScorerPrefersExplicitPassed pins that a grader's own verdict wins over
// the threshold when it supplies one.
func TestModelScorerPrefersExplicitPassed(t *testing.T) {
	model := &graderModel{structured: `{"score":0.95,"passed":false,"reason":"hallucinated a citation"}`}
	scorer, err := evals.NewModelScorer(evals.ModelScorerConfig{Model: model})
	if err != nil {
		t.Fatalf("NewModelScorer() = %v", err)
	}
	score, err := scorer.Score(context.Background(), evals.Case{ID: "c1"}, evals.Output{Text: "answer"})
	if err != nil {
		t.Fatalf("Score() = %v", err)
	}
	if score.Passed {
		t.Fatal("Passed = true despite the grader reporting false")
	}
}

// TestModelScorerDistinguishesZeroFromAbsentScore pins the pointer decode: a
// grader that scored 0 must not read the same as a grader that answered nothing.
func TestModelScorerDistinguishesZeroFromAbsentScore(t *testing.T) {
	zero := &graderModel{structured: `{"score":0,"reason":"entirely wrong"}`}
	scorer, err := evals.NewModelScorer(evals.ModelScorerConfig{Model: zero})
	if err != nil {
		t.Fatalf("NewModelScorer() = %v", err)
	}
	score, err := scorer.Score(context.Background(), evals.Case{ID: "c1"}, evals.Output{Text: "wrong"})
	if err != nil {
		t.Fatalf("Score() = %v, want an explicit zero to be a measurement", err)
	}
	if score.Value != 0 || score.Passed {
		t.Fatalf("score = (%v, %t), want (0, false)", score.Value, score.Passed)
	}

	for _, reply := range []string{`{"reason":"no score"}`, `{"score":null}`} {
		absent := &graderModel{structured: reply}
		scorer, err := evals.NewModelScorer(evals.ModelScorerConfig{Model: absent})
		if err != nil {
			t.Fatalf("NewModelScorer() = %v", err)
		}
		if _, err := scorer.Score(context.Background(), evals.Case{ID: "c1"}, evals.Output{Text: "x"}); err == nil {
			t.Fatalf("Score(%s) = nil, want a scorer failure for a verdict with no score", reply)
		}
	}
}

// TestModelScorerReportsModelFailureAsScorerFailure is the reason a model scorer
// returns an error rather than a zero: an unreachable judge must not manufacture
// a regression.
func TestModelScorerReportsModelFailureAsScorerFailure(t *testing.T) {
	wantErr := errors.New("rate limited")
	model := &graderModel{err: wantErr}
	scorer, err := evals.NewModelScorer(evals.ModelScorerConfig{Model: model})
	if err != nil {
		t.Fatalf("NewModelScorer() = %v", err)
	}
	if _, err := scorer.Score(context.Background(), evals.Case{ID: "c1"}, evals.Output{Text: "answer"}); !errors.Is(err, wantErr) {
		t.Fatalf("Score() = %v, want %v", err, wantErr)
	}
}

func TestModelScorerRejectsUnusableVerdicts(t *testing.T) {
	tests := []struct {
		name  string
		model *graderModel
	}{
		{name: "not JSON", model: &graderModel{content: "The answer looks good to me."}},
		{name: "empty reply", model: &graderModel{}},
		{name: "score above one", model: &graderModel{structured: `{"score":5}`}},
		{name: "score below zero", model: &graderModel{structured: `{"score":-1}`}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scorer, err := evals.NewModelScorer(evals.ModelScorerConfig{Model: test.model})
			if err != nil {
				t.Fatalf("NewModelScorer() = %v", err)
			}
			_, err = scorer.Score(context.Background(), evals.Case{ID: "c1"}, evals.Output{Text: "answer"})
			if !errors.Is(err, evals.ErrScorerFailed) {
				t.Fatalf("Score() = %v, want ErrScorerFailed", err)
			}
		})
	}
}

func TestModelScorerIncludesInputWhenRequested(t *testing.T) {
	model := &graderModel{structured: `{"score":1}`}
	scorer, err := evals.NewModelScorer(evals.ModelScorerConfig{Model: model, IncludeInput: true})
	if err != nil {
		t.Fatalf("NewModelScorer() = %v", err)
	}
	testCase := evals.Case{
		ID:       "c1",
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "What is the capital of France?"}},
		Expected: "Paris",
	}
	if _, err := scorer.Score(context.Background(), testCase, evals.Output{Text: "Paris"}); err != nil {
		t.Fatalf("Score() = %v", err)
	}
	prompt := model.seen().Messages[1].Content
	for _, want := range []string{"What is the capital of France?", "Paris"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt %q omits %q", prompt, want)
		}
	}
}

// TestModelScorerOmitsInputByDefault pins that a grader is not handed the case's
// question unless the caller asked, keeping the default prompt minimal.
func TestModelScorerOmitsInputByDefault(t *testing.T) {
	model := &graderModel{structured: `{"score":1}`}
	scorer, err := evals.NewModelScorer(evals.ModelScorerConfig{Model: model})
	if err != nil {
		t.Fatalf("NewModelScorer() = %v", err)
	}
	testCase := evals.Case{ID: "c1", Input: json.RawMessage(`"secret question"`)}
	if _, err := scorer.Score(context.Background(), testCase, evals.Output{Text: "answer"}); err != nil {
		t.Fatalf("Score() = %v", err)
	}
	if strings.Contains(model.seen().Messages[1].Content, "secret question") {
		t.Fatalf("prompt includes the input without IncludeInput: %q", model.seen().Messages[1].Content)
	}
}

func TestModelScorerRejectsInvalidConfig(t *testing.T) {
	if _, err := evals.NewModelScorer(evals.ModelScorerConfig{}); !errors.Is(err, evals.ErrInvalidScorer) {
		t.Fatalf("NewModelScorer(no model) = %v, want ErrInvalidScorer", err)
	}
	model := &graderModel{structured: `{"score":1}`}
	if _, err := evals.NewModelScorer(evals.ModelScorerConfig{Model: model, Threshold: 1.5}); !errors.Is(err, evals.ErrInvalidScorer) {
		t.Fatalf("NewModelScorer(threshold 1.5) = %v, want ErrInvalidScorer", err)
	}
	if _, err := evals.NewModelScorer(evals.ModelScorerConfig{Model: model, Threshold: -0.1}); !errors.Is(err, evals.ErrInvalidScorer) {
		t.Fatalf("NewModelScorer(threshold -0.1) = %v, want ErrInvalidScorer", err)
	}
	if _, err := evals.NewModelScorer(evals.ModelScorerConfig{Model: model, Selector: "elsewhere"}); !errors.Is(err, evals.ErrInvalidScorer) {
		t.Fatalf("NewModelScorer(bad selector) = %v, want ErrInvalidScorer", err)
	}
}

// TestModelScorerHonorsCustomSchemaOptOut covers the documented escape hatch for a
// provider that rejects an output schema.
func TestModelScorerHonorsCustomSchemaOptOut(t *testing.T) {
	model := &graderModel{content: `{"score":1}`}
	scorer, err := evals.NewModelScorer(evals.ModelScorerConfig{Model: model, Schema: json.RawMessage{}})
	if err != nil {
		t.Fatalf("NewModelScorer() = %v", err)
	}
	if _, err := scorer.Score(context.Background(), evals.Case{ID: "c1"}, evals.Output{Text: "answer"}); err != nil {
		t.Fatalf("Score() = %v", err)
	}
	if model.seen().OutputSchema != nil {
		t.Fatal("request carries an output schema despite an explicit empty schema")
	}
}

// TestModelScorerInExperimentIsReportedSeparately ties the model scorer to the
// acceptance criterion: a grader outage is a scorer failure on a target that
// worked.
func TestModelScorerInExperimentIsReportedSeparately(t *testing.T) {
	scorer, err := evals.NewModelScorer(evals.ModelScorerConfig{Model: &graderModel{err: errors.New("grader down")}})
	if err != nil {
		t.Fatalf("NewModelScorer() = %v", err)
	}
	experiment, err := evals.New(evals.ExperimentConfig{
		Dataset: answerDataset(),
		Target:  answerTarget{answers: map[evals.CaseID]string{"france": "Paris", "japan": "Tokyo", "peru": "Lima"}},
		Scorers: []evals.Scorer{scorer},
	})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	record, results, err := experiment.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if record.ScorerFailures != 3 || record.TargetFailures != 0 {
		t.Fatalf("record = (%d scorer failures, %d target failures), want (3, 0)",
			record.ScorerFailures, record.TargetFailures)
	}
	for _, result := range results {
		if !result.TargetSucceeded() {
			t.Fatalf("case %q reports a target failure: %q", result.CaseID, result.Failure)
		}
		if len(result.Scores) != 0 {
			t.Fatalf("case %q carries scores despite a failed grader: %+v", result.CaseID, result.Scores)
		}
	}
	// The aggregate exists and reports zero measurements, so "was not measured" is
	// distinguishable from "measured zero".
	aggregate, ok := record.Aggregate("model_graded")
	if !ok {
		t.Fatal("record carries no model_graded aggregate")
	}
	if aggregate.Scored != 0 || aggregate.Failures != 3 {
		t.Fatalf("aggregate = (%d scored, %d failures), want (0, 3)", aggregate.Scored, aggregate.Failures)
	}
}

// seen returns the last request the grader received.
func (m *graderModel) seen() lebro.ModelRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.recorded
}
