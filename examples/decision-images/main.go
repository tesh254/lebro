// The decision-images example sends a bounded image to a decision provider
// and escalates an uncertain answer to a human.
//
// It points the real SubQ adapter at a local endpoint, so the example runs
// without a network call or an API key while still exercising the adapter's
// image handling: raw bytes go in, one line of base64 goes on the wire, and
// an unsupported image never reaches the network.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/decisions"
)

func main() {
	must(run(os.Stdout))
}

func run(output io.Writer) error {
	// A one pixel PNG stands in for an authorized customer photo.
	image, err := base64.StdEncoding.DecodeString(pngOnePixel)
	if err != nil {
		return err
	}

	var observedImages []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Images []map[string]any `json:"images"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		observedImages = body.Images
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model": "subq-snap-preview",
			"answers": {
				"damage": {
					"type": "score",
					"score": 1.4,
					"legend": {"0": "No damage", "1": "Light damage", "2": "Heavy damage"},
					"probabilities": {"0": 0.1, "1": 0.5, "2": 0.4},
					"confidence": 0.41
				}
			},
			"usage": {"input_tokens": 477, "output_tokens": 0}
		}`))
	}))
	defer server.Close()

	model, err := decisions.NewSubQ(decisions.SubQConfig{
		APIKey:  "example-key",
		BaseURL: server.URL,
		Model:   "subq-snap-preview",
		Timeout: 0,
	})
	if err != nil {
		return err
	}

	response, err := model.Decide(context.Background(), lebro.DecisionRequest{
		State:  "A customer sent a photo of a damaged product.",
		Images: []lebro.DecisionImage{{MediaType: "image/png", Bytes: image}},
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
			"damage": lebro.ScoreQuestion{
				Instructions: "How much damage does the product show?",
				Criteria: []lebro.JSONValue{
					"No damage",
					"Light damage",
					"Heavy damage",
				},
			},
		},
	})
	if err != nil {
		return err
	}
	answer, ok := response.Answers["damage"].(lebro.ScoreAnswer)
	if !ok {
		return fmt.Errorf("expected a score answer, got %T", response.Answers["damage"])
	}
	fmt.Fprintf(output, "images on the wire: %d\n", len(observedImages))
	if len(observedImages) == 1 {
		fmt.Fprintf(output, "image media type: %v\n", observedImages[0]["mediaType"])
	}
	fmt.Fprintf(output, "expected level: %.2f\n", *answer.ExpectedLevel)
	if answer.Confidence == nil {
		return errors.New("expected the provider confidence")
	}
	fmt.Fprintf(output, "confidence: %.2f\n", *answer.Confidence)

	// The provider's confidence is low, so the application escalates instead
	// of acting. A score answer is not a binary judgment, so its level is
	// not routed through the binary gate and no probability is invented for
	// it: the confidence is compared on its own.
	minimumConfidence := 0.6
	if answer.Confidence == nil || *answer.Confidence < minimumConfidence {
		fmt.Fprint(output, "action: escalated to a human reviewer\n")
	} else {
		fmt.Fprint(output, "action: handled automatically\n")
	}

	// An image the provider cannot accept fails locally. The adapter never
	// sends the request, and the image bytes never reach an error message.
	_, err = model.Decide(context.Background(), lebro.DecisionRequest{
		State:  "A customer sent a photo of a damaged product.",
		Images: []lebro.DecisionImage{{MediaType: "image/gif", Bytes: image}},
		Questions: map[lebro.DecisionQuestionID]lebro.DecisionQuestion{
			"damage": lebro.ScoreQuestion{
				Instructions: "How much damage does the product show?",
				Criteria:     []lebro.JSONValue{"No damage", "Heavy damage"},
			},
		},
	})
	if !errors.Is(err, lebro.ErrDecisionUnsupportedCapability) {
		return fmt.Errorf("expected an unsupported capability error, got %v", err)
	}
	fmt.Fprint(output, "unsupported image: rejected before the request was sent\n")
	return nil
}

// pngOnePixel is a one pixel PNG used as a stand-in for an authorized photo.
const pngOnePixel = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII="

func must(err error) {
	if err != nil {
		panic(err)
	}
}
