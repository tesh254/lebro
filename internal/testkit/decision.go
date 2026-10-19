package testkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/tesh254/lebro"
)

// DecisionMode tells a decision factory what behavior to arrange.
type DecisionMode string

const (
	// DecisionResponse asks the factory to serve a successful response.
	DecisionResponse DecisionMode = "response"
	// DecisionFailure asks the factory to serve a provider error.
	DecisionFailure DecisionMode = "failure"
	// DecisionCancellation asks the factory to hold the request open until
	// the caller cancels it.
	DecisionCancellation DecisionMode = "cancellation"
)

// DecisionCase is one adapter-neutral decision behavior.
//
// The request is the part every provider shares. What a provider reports for
// it is not shared: the providers differ in whether they report a confidence
// for a noul answer and in how they report a winning score level. A factory
// therefore supplies the expected response along with the harness.
type DecisionCase struct {
	// Name identifies the case.
	Name string
	// Mode tells the factory what to arrange.
	Mode DecisionMode
	// Request is the neutral request.
	Request lebro.DecisionRequest
	// ErrorKind is the expected normalized failure for a failure case.
	ErrorKind lebro.DecisionErrorKind
}

// DecisionHarness couples a decision adapter with the neutral response it
// must produce and the neutral request that reached its provider boundary.
type DecisionHarness struct {
	// Model is the adapter under test.
	Model lebro.DecisionModel
	// Expected is the neutral response the adapter must produce. A factory
	// fills it with the values its own provider reports.
	Expected lebro.DecisionResponse
	// ObservedRequest reports the neutral request that reached the provider
	// boundary.
	ObservedRequest func() lebro.DecisionRequest
}

// DecisionFactory builds a harness for one case. A factory serves the
// provider wire body for the case and reports the neutral request that
// arrived.
//
// The factory receives the caller's cancel function. A cancellation case
// calls it once the request reaches the provider, so the suite covers the
// in-flight path rather than a request that never starts.
type DecisionFactory func(*testing.T, DecisionCase, context.CancelFunc) DecisionHarness

// DecisionContractCases returns the canonical decision cases. Every case is
// accepted by all three System One providers, so the suite holds across
// providers instead of describing one of them.
func DecisionContractCases() []DecisionCase {
	cases := []DecisionCase{
		{
			Name: "noul",
			Mode: DecisionResponse,
			Request: lebro.DecisionRequest{
				State: "A customer asked for a refund three days after the purchase.",
				Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
					"refund_eligible": lebro.NoulQuestion{
						Instructions: "Does this request qualify for a refund?",
						Criteria:     &lebro.NoulCriteria{True: "Qualifies", False: "Does not qualify"},
					},
				},
			},
		},
		{
			Name: "choice",
			Mode: DecisionResponse,
			Request: lebro.DecisionRequest{
				State: "My card was charged twice for one order.",
				Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
					"department": lebro.ChoiceQuestion{
						Instructions: "Which team should handle this request?",
						Criteria: map[lebro.DecisionOptionID]lebro.JSONValue{
							"billing":   "Payment and refund questions",
							"technical": "Product faults and integrations",
							"shipping":  "Delivery and tracking",
						},
					},
				},
			},
		},
		{
			Name: "score",
			Mode: DecisionResponse,
			Request: lebro.DecisionRequest{
				State: "Export fails in one browser and works in another.",
				Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
					"severity": lebro.ScoreQuestion{
						Instructions: "How severe is this issue?",
						Criteria: []lebro.JSONValue{
							"Cosmetic: appearance only",
							"Workaround available: a task fails another way works",
							"Fully blocked: a task fails with no workaround",
						},
					},
				},
			},
		},
		{
			Name: "multiple questions",
			Mode: DecisionResponse,
			Request: lebro.DecisionRequest{
				State: "My card was charged twice for one order.",
				Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
					"is_billing": lebro.NoulQuestion{
						Instructions: "Is this a billing question?",
					},
					"department": lebro.ChoiceQuestion{
						Instructions: "Which team should handle this request?",
						Criteria: map[lebro.DecisionOptionID]lebro.JSONValue{
							"billing":   "Payment and refund questions",
							"technical": "Product faults and integrations",
						},
					},
					"urgency": lebro.ScoreQuestion{
						Instructions: "How urgent is this request?",
						Criteria: []lebro.JSONValue{
							"Can wait a week",
							"Should be handled today",
						},
					},
				},
			},
		},
		{
			Name: "structured state",
			Mode: DecisionResponse,
			Request: lebro.DecisionRequest{
				State: map[string]any{
					"order_id": "A-104",
					"message":  "My card was charged twice.",
					"amount":   "42.50",
				},
				Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
					"is_duplicate_charge": lebro.NoulQuestion{
						Instructions: "Is `message` about a duplicate charge for `order_id`?",
					},
				},
			},
		},
		{
			Name:      "failure",
			Mode:      DecisionFailure,
			ErrorKind: lebro.DecisionErrorUnavailable,
			Request: lebro.DecisionRequest{
				State: "A request the provider cannot answer.",
				Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
					"is_urgent": lebro.NoulQuestion{Instructions: "Does this convey urgency?"},
				},
			},
		},
		{
			Name: "cancellation",
			Mode: DecisionCancellation,
			Request: lebro.DecisionRequest{
				State: "A request that is cancelled before it is answered.",
				Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
					"is_urgent": lebro.NoulQuestion{Instructions: "Does this convey urgency?"},
				},
			},
		},
	}
	return cloneDecisionCases(cases)
}

// RunDecisionContract runs the canonical decision suite against factory.
func RunDecisionContract(t *testing.T, factory DecisionFactory) {
	t.Helper()
	for _, contractCase := range DecisionContractCases() {
		contractCase := contractCase
		t.Run(contractCase.Name, func(t *testing.T) {
			if contractCase.Mode == DecisionResponse {
				assertNoError(t, contractCase.Request.Validate())
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			harness := factory(t, contractCase, cancel)

			response, err := harness.Model.Decide(ctx, cloneDecisionRequest(contractCase.Request))
			switch contractCase.Mode {
			case DecisionResponse:
				assertDecisionNoError(t, err)
				assertNoError(t, response.Validate())
				assertDecisionResponse(t, response, harness.Expected, contractCase.Request)
				assertDecisionObservedRequest(t, harness.ObservedRequest, contractCase.Request)
			case DecisionFailure:
				assertDecisionError(t, err, contractCase.ErrorKind)
			case DecisionCancellation:
				assertDecisionCancellation(t, err)
			}
		})
	}
}

// assertDecisionNoError fails when a decision call returned an error.
func assertDecisionNoError(t TestingT, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
}

// assertDecisionResponse compares the adapter response with the expected one.
func assertDecisionResponse(t TestingT, got, want lebro.DecisionResponse, request lebro.DecisionRequest) {
	t.Helper()
	if got.Provider != want.Provider {
		t.Fatalf("Decide() provider = %q, want %q", got.Provider, want.Provider)
	}
	if got.Model != want.Model {
		t.Fatalf("Decide() model = %q, want %q", got.Model, want.Model)
	}
	if got.RequestedModel != want.RequestedModel {
		t.Fatalf("Decide() requested model = %q, want %q", got.RequestedModel, want.RequestedModel)
	}
	if got.UpstreamProvider != want.UpstreamProvider {
		t.Fatalf("Decide() upstream provider = %q, want %q", got.UpstreamProvider, want.UpstreamProvider)
	}
	if got.Attempts < 1 {
		t.Fatalf("Decide() attempts = %d, want at least 1", got.Attempts)
	}
	if got.Duration < 0 {
		t.Fatalf("Decide() duration = %v, want a non-negative duration", got.Duration)
	}
	if len(got.Answers) != len(request.Questions) {
		t.Fatalf("Decide() answered %d questions, want %d", len(got.Answers), len(request.Questions))
	}
	for id := range request.Questions {
		answer, ok := got.Answers[id]
		if !ok {
			t.Fatalf("Decide() returned no answer for question %q", id)
		}
		expected, ok := want.Answers[id]
		if !ok {
			t.Fatalf("the expected response has no answer for question %q", id)
		}
		if !reflect.DeepEqual(answer, expected) {
			t.Fatalf("Decide() answer %q = %#v, want %#v", id, answer, expected)
		}
	}
	if !reflect.DeepEqual(got.Usage, want.Usage) {
		t.Fatalf("Decide() usage = %#v, want %#v", got.Usage, want.Usage)
	}
}

// assertDecisionObservedRequest compares the request that reached the
// provider with the request the caller sent.
func assertDecisionObservedRequest(t TestingT, observe func() lebro.DecisionRequest, want lebro.DecisionRequest) {
	t.Helper()
	if observe == nil {
		t.Fatalf("the decision harness does not expose its observed request")
		return
	}
	got := observe()
	if want.Model == "" {
		// The caller did not name a model, so the adapter used its
		// configured default. The provider must have received one.
		if got.Model == "" {
			t.Fatalf("the provider observed no model, want the adapter default")
		}
	} else if got.Model != want.Model {
		t.Fatalf("the provider observed model %q, want %q", got.Model, want.Model)
	}
	if len(got.Images) != len(want.Images) {
		t.Fatalf("the provider observed %d images, want %d", len(got.Images), len(want.Images))
	}
	if len(got.Questions) != len(want.Questions) {
		t.Fatalf("the provider observed %d questions, want %d", len(got.Questions), len(want.Questions))
	}
	for id, question := range want.Questions {
		observed, ok := got.Questions[id]
		if !ok {
			t.Fatalf("the provider observed no question %q", id)
		}
		if !reflect.DeepEqual(observed, question) {
			t.Fatalf("the provider observed question %q = %#v, want %#v", id, observed, question)
		}
	}
	if !equalDecisionState(got.State, want.State) {
		t.Fatalf("the provider observed state %#v, want %#v", got.State, want.State)
	}
}

// equalDecisionState compares two states by their JSON encoding, so a value
// that made a JSON round trip still matches its original.
func equalDecisionState(got, want lebro.JSONValue) bool {
	gotJSON, gotErr := json.Marshal(got)
	wantJSON, wantErr := json.Marshal(want)
	if gotErr != nil || wantErr != nil {
		return reflect.DeepEqual(got, want)
	}
	return bytes.Equal(gotJSON, wantJSON)
}

// assertDecisionError verifies the normalized failure category.
func assertDecisionError(t TestingT, err error, want lebro.DecisionErrorKind) {
	t.Helper()
	if err == nil {
		t.Fatalf("Decide() error = nil, want a decision failure")
	}
	var decisionErr *lebro.DecisionError
	if !errors.As(err, &decisionErr) {
		t.Fatalf("Decide() error = %v, want a *lebro.DecisionError", err)
	}
	if decisionErr.Kind != want {
		t.Fatalf("Decide() error kind = %q, want %q", decisionErr.Kind, want)
	}
	if !errors.Is(err, decisionErrorSentinel(want)) {
		t.Fatalf("Decide() error = %v, want it to match the %q sentinel", err, want)
	}
}

// assertDecisionCancellation verifies that the failure preserves context
// cancellation.
func assertDecisionCancellation(t TestingT, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("Decide() error = nil, want context cancellation")
	}
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Decide() error = %v, want context cancellation", err)
	}
}

// decisionErrorSentinel maps a decision failure category to its sentinel.
func decisionErrorSentinel(kind lebro.DecisionErrorKind) error {
	switch kind {
	case lebro.DecisionErrorInvalidRequest:
		return lebro.ErrDecisionInvalidRequest
	case lebro.DecisionErrorUnsupportedCapability:
		return lebro.ErrDecisionUnsupportedCapability
	case lebro.DecisionErrorAuthentication:
		return lebro.ErrDecisionAuthentication
	case lebro.DecisionErrorPermissionDenied:
		return lebro.ErrDecisionPermissionDenied
	case lebro.DecisionErrorRateLimited:
		return lebro.ErrDecisionRateLimited
	case lebro.DecisionErrorUnavailable:
		return lebro.ErrDecisionUnavailable
	case lebro.DecisionErrorTimeout:
		return lebro.ErrDecisionTimeout
	case lebro.DecisionErrorCancelled:
		return lebro.ErrDecisionCancelled
	case lebro.DecisionErrorTransport:
		return lebro.ErrDecisionTransport
	case lebro.DecisionErrorMalformedResponse:
		return lebro.ErrDecisionMalformedResponse
	default:
		return lebro.ErrDecisionUnknown
	}
}

func cloneDecisionCases(cases []DecisionCase) []DecisionCase {
	result := make([]DecisionCase, len(cases))
	for i, contractCase := range cases {
		result[i] = cloneDecisionCase(contractCase)
	}
	return result
}

func cloneDecisionCase(contractCase DecisionCase) DecisionCase {
	contractCase.Request = cloneDecisionRequest(contractCase.Request)
	return contractCase
}

// cloneDecisionRequest copies a request deeply enough that an adapter which
// mutates the state or the criteria cannot change the expectation the suite
// compares against.
func cloneDecisionRequest(request lebro.DecisionRequest) lebro.DecisionRequest {
	cloned := lebro.DecisionRequest{
		Model:  request.Model,
		State:  cloneDecisionValue(request.State),
		Images: append([]lebro.DecisionImage(nil), request.Images...),
	}
	if request.Images != nil {
		cloned.Images = make([]lebro.DecisionImage, len(request.Images))
		for i, image := range request.Images {
			cloned.Images[i] = lebro.DecisionImage{
				MediaType: image.MediaType,
				Bytes:     append([]byte(nil), image.Bytes...),
			}
		}
	}
	if request.Questions != nil {
		cloned.Questions = make(map[lebro.DecisionQuestionID]lebro.DecisionQuestion, len(request.Questions))
		for id, question := range request.Questions {
			cloned.Questions[id] = cloneDecisionQuestion(question)
		}
	}
	return cloned
}

// cloneDecisionQuestion copies one question and its criteria.
func cloneDecisionQuestion(question lebro.DecisionQuestion) lebro.DecisionQuestion {
	switch typed := question.(type) {
	case lebro.NoulQuestion:
		cloned := lebro.NoulQuestion{Instructions: cloneDecisionValue(typed.Instructions)}
		if typed.Criteria != nil {
			cloned.Criteria = &lebro.NoulCriteria{
				True:  cloneDecisionValue(typed.Criteria.True),
				False: cloneDecisionValue(typed.Criteria.False),
			}
		}
		return cloned
	case *lebro.NoulQuestion:
		if typed == nil {
			return typed
		}
		cloned := *typed
		return &cloned
	case lebro.ChoiceQuestion:
		cloned := lebro.ChoiceQuestion{Instructions: cloneDecisionValue(typed.Instructions)}
		if typed.Criteria != nil {
			cloned.Criteria = make(map[lebro.DecisionOptionID]lebro.JSONValue, len(typed.Criteria))
			for option, description := range typed.Criteria {
				cloned.Criteria[option] = cloneDecisionValue(description)
			}
		}
		return cloned
	case *lebro.ChoiceQuestion:
		if typed == nil {
			return typed
		}
		cloned := *typed
		return &cloned
	case lebro.ScoreQuestion:
		cloned := lebro.ScoreQuestion{Instructions: cloneDecisionValue(typed.Instructions)}
		if typed.Criteria != nil {
			cloned.Criteria = make([]lebro.JSONValue, len(typed.Criteria))
			for level, description := range typed.Criteria {
				cloned.Criteria[level] = cloneDecisionValue(description)
			}
		}
		return cloned
	case *lebro.ScoreQuestion:
		if typed == nil {
			return typed
		}
		cloned := *typed
		return &cloned
	default:
		return question
	}
}

// cloneDecisionValue copies a JSON-compatible value through its JSON form.
func cloneDecisionValue(value lebro.JSONValue) lebro.JSONValue {
	if value == nil {
		return nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var decoded lebro.JSONValue
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return value
	}
	return decoded
}
