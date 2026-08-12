package evals

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/tesh254/lebro"
)

// DefaultModelScorerPrompt is the instruction used when ModelScorerConfig.Prompt
// is empty. It asks for the same JSON shape ModelScorerSchema describes.
const DefaultModelScorerPrompt = `You are grading the output of an AI system.

Judge whether the output satisfies the expectation. Respond with JSON only:
{"score": <number between 0 and 1>, "passed": <true|false>, "reason": "<short explanation>"}`

// ModelScorerSchema is the JSON Schema a grader model is asked to conform to. It
// is passed as the model's output schema so a provider that supports structured
// output enforces the shape, and it documents what the scorer decodes when the
// provider does not.
var ModelScorerSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "score": {"type": "number", "minimum": 0, "maximum": 1},
    "passed": {"type": "boolean"},
    "reason": {"type": "string"}
  },
  "required": ["score"],
  "additionalProperties": false
}`)

// modelVerdict is the grader's decoded judgment. Score is a pointer so a reply
// omitting it — or sending JSON null — is distinguishable from a deliberate 0,
// which is the difference between "the grader did not answer" and "the grader
// scored this zero". Passed is likewise a pointer so its absence falls back to
// the score threshold rather than reading as an explicit false.
type modelVerdict struct {
	Score  *float64 `json:"score"`
	Passed *bool    `json:"passed"`
	Reason string   `json:"reason"`
}

// ModelScorerConfig configures a ModelScorer. Model is required; everything else
// has a working default.
type ModelScorerConfig struct {
	// Name identifies the scorer in stored records. Defaults to "model_graded".
	Name string
	// Model grades the output. The caller supplies the adapter, so no provider
	// dependency enters this package.
	Model lebro.Model
	// ModelName is passed through as ModelRequest.Model. An adapter configured
	// with its own default model can leave it empty.
	ModelName string
	// Prompt is the grader's system instruction. Empty selects
	// DefaultModelScorerPrompt.
	Prompt string
	// Threshold is the score at or above which a verdict counts as passed when
	// the grader does not report "passed" itself. Zero selects 0.5.
	Threshold float64
	// Selector chooses which part of the output is graded. Empty selects
	// SelectText.
	Selector TextSelector
	// IncludeInput adds the case's input or messages to the grading prompt.
	// Payload data reaches the grader either way through the output, so this is
	// about whether the question is needed to judge the answer.
	IncludeInput bool
	// Schema requests a JSON output schema from the model. Nil (the default)
	// requests none, so grading works against any lebro.Model — including the
	// openai package's text-generation adapter, which rejects every request
	// carrying an output schema — by asking for JSON in the prompt and decoding
	// it from the message content. Set it to ModelScorerSchema, or a stricter
	// schema, only for a provider known to support structured output.
	Schema json.RawMessage
}

// ModelScorer grades an output with a language model behind the existing
// lebro.Model protocol.
//
// It is a Scorer like any other, so a model-graded and a rule-based judgment are
// recorded and aggregated identically. A model failure — an unavailable provider,
// a reply that is not the requested JSON — is returned as an error and therefore
// recorded as a ScorerFailure, never as a low score: "the judge was unreachable"
// and "the answer was bad" are different findings, and scoring 0 for the former
// would silently manufacture a regression.
type ModelScorer struct {
	name      string
	model     lebro.Model
	modelName string
	prompt    string
	threshold float64
	selector  TextSelector
	withInput bool
	schema    json.RawMessage
}

// NewModelScorer returns a ModelScorer. It reports ErrInvalidScorer when no model
// is supplied, when the selector is unknown, or when the threshold is outside
// [0,1] — a threshold no score can reach would make every case fail for a reason
// the record would not explain.
func NewModelScorer(config ModelScorerConfig) (*ModelScorer, error) {
	// A typed-nil Model is non-nil as an interface, so a plain nil check would
	// miss it and the first Generate call would panic through the adapter.
	if isNilInterface(config.Model) {
		return nil, fmt.Errorf("%w: model scorer requires a model", ErrInvalidScorer)
	}
	if !validSelector(config.Selector) {
		return nil, fmt.Errorf("%w: unknown selector %q", ErrInvalidScorer, config.Selector)
	}
	if math.IsNaN(config.Threshold) {
		return nil, fmt.Errorf("%w: model scorer threshold must be a number", ErrInvalidScorer)
	}
	if config.Threshold < 0 || config.Threshold > 1 {
		return nil, fmt.Errorf("%w: model scorer threshold %v is outside [0,1]", ErrInvalidScorer, config.Threshold)
	}
	name := config.Name
	if name == "" {
		name = "model_graded"
	}
	prompt := config.Prompt
	if prompt == "" {
		prompt = DefaultModelScorerPrompt
	}
	threshold := config.Threshold
	if threshold == 0 {
		threshold = 0.5
	}
	// No schema by default: some adapters — the openai package's text-generation
	// Model among them — reject any request carrying an output schema, and the
	// default prompt already asks for plain JSON that decodeVerdict can read from
	// the message content. A caller sets Schema explicitly for a provider known
	// to support structured output.
	schema := config.Schema
	return &ModelScorer{
		name:      name,
		model:     config.Model,
		modelName: config.ModelName,
		prompt:    prompt,
		threshold: threshold,
		selector:  config.Selector,
		withInput: config.IncludeInput,
		schema:    cloneRawJSON(schema),
	}, nil
}

var _ Scorer = (*ModelScorer)(nil)

// Name returns the scorer's name.
func (s *ModelScorer) Name() string { return s.name }

// Score asks the grader model to judge the output and decodes its verdict.
func (s *ModelScorer) Score(ctx context.Context, testCase Case, output Output) (Score, error) {
	request := lebro.ModelRequest{
		Model: s.modelName,
		Messages: []lebro.Message{
			{Role: lebro.RoleSystem, Content: s.prompt},
			{Role: lebro.RoleUser, Content: s.gradingPrompt(testCase, output)},
		},
	}
	if len(s.schema) > 0 {
		request.OutputSchema = &lebro.ModelOutputSchema{
			Name:        "eval_verdict",
			Description: "Numeric quality judgment of an evaluated output.",
			Schema:      cloneRawJSON(s.schema),
			Strict:      true,
		}
	}
	response, err := s.model.Generate(ctx, request)
	if err != nil {
		return Score{}, fmt.Errorf("grade case %q: %w", testCase.ID, err)
	}
	verdict, err := decodeVerdict(response)
	if err != nil {
		return Score{}, fmt.Errorf("grade case %q: %w", testCase.ID, err)
	}

	score := Score{
		Scorer: s.name,
		CaseID: testCase.ID,
		Value:  *verdict.Score,
		Reason: verdict.Reason,
	}
	if verdict.Passed != nil {
		score.Passed = *verdict.Passed
	} else {
		score.Passed = score.Value >= s.threshold
	}
	if response.Usage != (lebro.ModelUsage{}) {
		score.Metadata = map[string]string{
			"grader.input_tokens":  fmt.Sprint(response.Usage.InputTokens),
			"grader.output_tokens": fmt.Sprint(response.Usage.OutputTokens),
		}
	}
	return score, nil
}

// gradingPrompt renders the case and output for the grader.
func (s *ModelScorer) gradingPrompt(testCase Case, output Output) string {
	var builder strings.Builder
	if s.withInput {
		builder.WriteString("Input:\n")
		if len(testCase.Messages) > 0 {
			for _, message := range testCase.Messages {
				fmt.Fprintf(&builder, "%s: %s\n", message.Role, message.Content)
			}
		} else {
			builder.Write(testCase.Input)
			builder.WriteString("\n")
		}
		builder.WriteString("\n")
	}
	if testCase.Expected != "" {
		fmt.Fprintf(&builder, "Expectation:\n%s\n\n", testCase.Expected)
	}
	fmt.Fprintf(&builder, "Output:\n%s\n", selectText(s.selector, output))
	return builder.String()
}

// decodeVerdict reads the grader's judgment from a response, preferring the
// structured payload and falling back to the message content for a provider that
// ignores the output schema. A reply carrying no score is an error: a verdict that
// omits its own answer has not measured anything.
func decodeVerdict(response lebro.ModelResponse) (modelVerdict, error) {
	payloads := make([]json.RawMessage, 0, 2)
	if structured := response.Message.StructuredOutput; structured != "" {
		payloads = append(payloads, structured.Raw())
	}
	if content := strings.TrimSpace(response.Message.Content); content != "" {
		payloads = append(payloads, json.RawMessage(content))
	}
	if len(payloads) == 0 {
		return modelVerdict{}, fmt.Errorf("%w: grader returned an empty response", ErrScorerFailed)
	}

	var lastErr error
	for _, payload := range payloads {
		var verdict modelVerdict
		if err := json.Unmarshal(payload, &verdict); err != nil {
			lastErr = err
			continue
		}
		if verdict.Score == nil {
			lastErr = fmt.Errorf("verdict omits a score")
			continue
		}
		if *verdict.Score < 0 || *verdict.Score > 1 {
			lastErr = fmt.Errorf("verdict score %v is outside [0,1]", *verdict.Score)
			continue
		}
		return verdict, nil
	}
	return modelVerdict{}, fmt.Errorf("%w: decode grader verdict: %v", ErrScorerFailed, lastErr)
}
