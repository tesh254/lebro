package decisions

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/internal/testkit"
)

// subQFixture describes SubQ for the shared contract suite.
func subQFixture() decisionFixture {
	return decisionFixture{
		name: "subq",
		newModel: func(baseURL string) (lebro.DecisionModel, error) {
			return NewSubQ(SubQConfig{APIKey: "test-key", BaseURL: baseURL, Model: "subq-snap-preview"})
		},
		scoreStyle:     scoreWeightedOnly,
		noulConfidence: false,
		model:          "subq-snap-preview",
	}
}

// TestSubQContract runs the shared decision suite against SubQ.
func TestSubQContract(t *testing.T) {
	testkit.RunDecisionContract(t, decisionContractFactory(subQFixture()))
}

// TestSubQEncodesImagesForTheWire pins that raw bytes become one line of
// base64 at the HTTP boundary and nowhere else.
func TestSubQEncodesImagesForTheWire(t *testing.T) {
	payload := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decoded := json.NewDecoder(r.Body)
		if err := decoded.Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"subq-snap-preview","answers":{"q":{"type":"noul","noul":0.5}}}`))
	}))
	defer server.Close()

	model, err := NewSubQ(SubQConfig{APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewSubQ() error = %v", err)
	}
	if _, err := model.Decide(context.Background(), lebro.DecisionRequest{
		State:  "A photo of a damaged product.",
		Images: []lebro.DecisionImage{{MediaType: "image/png", Bytes: payload}},
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
			"q": lebro.NoulQuestion{Instructions: "Is the product damaged?"},
		},
	}); err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	images, ok := body["images"].([]any)
	if !ok || len(images) != 1 {
		t.Fatalf("request images = %#v, want one image", body["images"])
	}
	image, ok := images[0].(map[string]any)
	if !ok {
		t.Fatalf("request image = %#v, want an object", images[0])
	}
	if image["mediaType"] != "image/png" {
		t.Fatalf("image media type = %#v, want image/png", image["mediaType"])
	}
	encoded, ok := image["base64"].(string)
	if !ok {
		t.Fatalf("image payload = %#v, want a string", image["base64"])
	}
	if strings.ContainsAny(encoded, "\r\n") {
		t.Fatal("the image payload is not a single line of base64")
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode the image payload: %v", err)
	}
	if !bytes.Equal(decoded, payload) {
		t.Fatalf("the image payload = %v, want %v", decoded, payload)
	}
}

// TestSubQRejectsUnsupportedImagesBeforeHTTP pins that image requests the
// provider cannot accept fail locally and never reach the network.
func TestSubQRejectsUnsupportedImagesBeforeHTTP(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G'}
	oversized := make([]byte, subQMaxBytesPerImage+1)
	cases := []struct {
		name   string
		images []lebro.DecisionImage
		want   error
	}{
		{
			name:   "unsupported media type",
			images: []lebro.DecisionImage{{MediaType: "image/gif", Bytes: png}},
			want:   lebro.ErrDecisionUnsupportedCapability,
		},
		{
			name: "too many images",
			images: []lebro.DecisionImage{
				{MediaType: "image/png", Bytes: png},
				{MediaType: "image/png", Bytes: png},
				{MediaType: "image/png", Bytes: png},
				{MediaType: "image/png", Bytes: png},
				{MediaType: "image/png", Bytes: png},
			},
			want: lebro.ErrDecisionInvalidRequest,
		},
		{
			name:   "image too large",
			images: []lebro.DecisionImage{{MediaType: "image/png", Bytes: oversized}},
			want:   lebro.ErrDecisionInvalidRequest,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"model":"subq-snap-preview","answers":{}}`))
			}))
			defer server.Close()

			model, err := NewSubQ(SubQConfig{APIKey: "test-key", BaseURL: server.URL})
			if err != nil {
				t.Fatalf("NewSubQ() error = %v", err)
			}
			_, err = model.Decide(context.Background(), lebro.DecisionRequest{
				State:     "state",
				Images:    testCase.images,
				Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{"q": lebro.NoulQuestion{Instructions: "Is it?"}},
			})
			if !errors.Is(err, testCase.want) {
				t.Fatalf("Decide() error = %v, want %v", err, testCase.want)
			}
			if requests != 0 {
				t.Fatalf("the provider received %d requests, want none", requests)
			}
		})
	}
}

// TestSubQRejectsTotalImageSize pins the aggregate image limit.
func TestSubQRejectsTotalImageSize(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"subq-snap-preview","answers":{}}`))
	}))
	defer server.Close()

	model, err := NewSubQ(SubQConfig{APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewSubQ() error = %v", err)
	}
	image := make([]byte, subQMaxBytesPerImage)
	_, err = model.Decide(context.Background(), lebro.DecisionRequest{
		State: "state",
		Images: []lebro.DecisionImage{
			{MediaType: "image/png", Bytes: image},
			{MediaType: "image/png", Bytes: image},
			{MediaType: "image/png", Bytes: image},
		},
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{"q": lebro.NoulQuestion{Instructions: "Is it?"}},
	})
	if !errors.Is(err, lebro.ErrDecisionInvalidRequest) {
		t.Fatalf("Decide() error = %v, want an invalid request error", err)
	}
	if requests != 0 {
		t.Fatalf("the provider received %d requests, want none", requests)
	}
}

// TestSubQRejectsRemoteImageReferences pins that a URL or a data URL fails
// locally. An adapter never fetches an image, so a reference is not image
// data and must not reach the provider.
func TestSubQRejectsRemoteImageReferences(t *testing.T) {
	references := []string{
		"https://example.com/photo.png",
		"http://example.com/photo.png",
		"data:image/png;base64,iVBORw0KGgo=",
	}
	for _, reference := range references {
		t.Run(reference[:min(len(reference), 16)], func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"model":"subq-snap-preview","answers":{}}`))
			}))
			defer server.Close()

			model, err := NewSubQ(SubQConfig{APIKey: "test-key", BaseURL: server.URL})
			if err != nil {
				t.Fatalf("NewSubQ() error = %v", err)
			}
			_, err = model.Decide(context.Background(), lebro.DecisionRequest{
				State:  "state",
				Images: []lebro.DecisionImage{{MediaType: "image/png", Bytes: []byte(reference)}},
				Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
					"q": lebro.NoulQuestion{Instructions: "Is it?"},
				},
			})
			if !errors.Is(err, lebro.ErrDecisionInvalidRequest) {
				t.Fatalf("Decide() error = %v, want an invalid request error", err)
			}
			if requests != 0 {
				t.Fatalf("the provider received %d requests, want none", requests)
			}
		})
	}
}

// TestSubQRejectsEmptyImageBytes pins that an image without bytes is
// rejected before the request is sent.
func TestSubQRejectsEmptyImageBytes(t *testing.T) {
	model, err := NewSubQ(SubQConfig{APIKey: "test-key", BaseURL: "https://api.subq.ai"})
	if err != nil {
		t.Fatalf("NewSubQ() error = %v", err)
	}
	_, err = model.Decide(context.Background(), lebro.DecisionRequest{
		State:     "state",
		Images:    []lebro.DecisionImage{{MediaType: "image/png"}},
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{"q": lebro.NoulQuestion{Instructions: "Is it?"}},
	})
	if !errors.Is(err, lebro.ErrDecisionInvalidRequest) {
		t.Fatalf("Decide() error = %v, want an invalid request error", err)
	}
}

// TestSubQMapsStatusCodes pins the normalized failure categories.
func TestSubQMapsStatusCodes(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   lebro.DecisionErrorKind
	}{
		{status: http.StatusBadRequest, body: `{"detail":{"error_type":"validation_error","message":"bad request"}}`, want: lebro.DecisionErrorInvalidRequest},
		{status: http.StatusUnauthorized, body: `{"detail":{"error_type":"auth","message":"missing key"}}`, want: lebro.DecisionErrorAuthentication},
		{status: http.StatusPaymentRequired, body: `{"detail":{"error_type":"billing","message":"out of credits"}}`, want: lebro.DecisionErrorPermissionDenied},
		{status: http.StatusForbidden, body: `{"detail":{"error_type":"forbidden","message":"model not allowed"}}`, want: lebro.DecisionErrorPermissionDenied},
		{status: http.StatusUnprocessableEntity, body: `{"detail":{"error_type":"validation_error","message":"images/0: image bytes must match the PNG, JPEG, or WebP mediaType."}}`, want: lebro.DecisionErrorInvalidRequest},
		{status: http.StatusTooManyRequests, body: `{"detail":{"error_type":"rate_limit","message":"slow down"}}`, want: lebro.DecisionErrorRateLimited},
		{status: http.StatusServiceUnavailable, body: `{"detail":{"error_type":"unavailable","message":"try later"}}`, want: lebro.DecisionErrorUnavailable},
	}
	for _, testCase := range cases {
		t.Run(http.StatusText(testCase.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(testCase.status)
				_, _ = w.Write([]byte(testCase.body))
			}))
			defer server.Close()

			model, err := NewSubQ(SubQConfig{APIKey: "test-key", BaseURL: server.URL, MaxRetries: intPointer(0)})
			if err != nil {
				t.Fatalf("NewSubQ() error = %v", err)
			}
			_, err = model.Decide(context.Background(), lebro.DecisionRequest{
				State:     "state",
				Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{"q": lebro.NoulQuestion{Instructions: "Is it?"}},
			})
			if err == nil {
				t.Fatal("Decide() error = nil, want a provider failure")
			}
			var decisionErr *lebro.DecisionError
			if !errors.As(err, &decisionErr) {
				t.Fatalf("Decide() error = %v, want a *lebro.DecisionError", err)
			}
			if decisionErr.Kind != testCase.want {
				t.Fatalf("Decide() kind = %q, want %q", decisionErr.Kind, testCase.want)
			}
			if decisionErr.StatusCode != testCase.status {
				t.Fatalf("Decide() status = %d, want %d", decisionErr.StatusCode, testCase.status)
			}
			if !strings.Contains(decisionErr.Message, "") || decisionErr.Message == "" {
				t.Fatalf("Decide() message = %q, want the provider description", decisionErr.Message)
			}
		})
	}
}

// TestSubQCredentialsAndImagesStayOutOfErrors pins that neither a credential
// nor image bytes reach an error message.
func TestSubQCredentialsAndImagesStayOutOfErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"detail":{"error_type":"validation_error","message":"images/0: image bytes must match the PNG, JPEG, or WebP mediaType.","input":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="}}`))
	}))
	defer server.Close()

	model, err := NewSubQ(SubQConfig{APIKey: "super-secret-key", BaseURL: server.URL, MaxRetries: intPointer(0)})
	if err != nil {
		t.Fatalf("NewSubQ() error = %v", err)
	}
	_, err = model.Decide(context.Background(), lebro.DecisionRequest{
		State:  "state",
		Images: []lebro.DecisionImage{{MediaType: "image/png", Bytes: []byte("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")}},
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
			"q": lebro.NoulQuestion{Instructions: "Is it?"},
		},
	})
	if err == nil {
		t.Fatal("Decide() error = nil, want a provider failure")
	}
	if strings.Contains(err.Error(), "super-secret-key") {
		t.Fatalf("Decide() error = %v, want the credential kept out of the message", err)
	}
	if strings.Contains(err.Error(), "iVBORw0KGgo") {
		t.Fatalf("Decide() error = %v, want the image payload kept out of the message", err)
	}
}

// TestSubQRejectsTooManyQuestions pins the documented question limit.
func TestSubQRejectsTooManyQuestions(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"subq-snap-preview","answers":{}}`))
	}))
	defer server.Close()

	model, err := NewSubQ(SubQConfig{APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewSubQ() error = %v", err)
	}
	questions := make(map[lebro.DecisionQuestionID]lebro.DecisionQuestion, 17)
	for i := 0; i < 17; i++ {
		questions[lebro.DecisionQuestionID(strings.Repeat("q", i+1))] = lebro.NoulQuestion{Instructions: "Is it?"}
	}
	_, err = model.Decide(context.Background(), lebro.DecisionRequest{
		State:     "state",
		Questions: questions,
	})
	if !errors.Is(err, lebro.ErrDecisionInvalidRequest) {
		t.Fatalf("Decide() error = %v, want an invalid request error", err)
	}
	if requests != 0 {
		t.Fatalf("the provider received %d requests, want none", requests)
	}
}
