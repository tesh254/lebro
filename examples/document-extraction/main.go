// document-extraction is the extraction-service build: messy document text in,
// schema-validated JSON out, behind one HTTP endpoint. A clean extraction
// returns validated invoice JSON; a malformed one fails loudly with a typed
// public error instead of leaking garbage to the caller.
//
// The example is network-free: it drives its own handler through httptest with
// a scripted model, so it runs without an API key or an open port. Serve the
// same handler with http.ListenAndServe and swap the model for a provider
// adapter to deploy it.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"time"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/httpapi"
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"
)

const invoiceSchema = `{
	"type":"object",
	"required":["invoice_id","vendor","total_cents","currency"],
	"properties":{
		"invoice_id":{"type":"string","minLength":1},
		"vendor":{"type":"string","minLength":1},
		"total_cents":{"type":"integer","minimum":0},
		"currency":{"type":"string","enum":["USD","EUR","KES"]}
	},
	"additionalProperties":false
}`

func main() {
	if err := run(os.Stdout); err != nil {
		log.Fatal(err)
	}
}

func run(output io.Writer) error {
	server, err := newServer()
	if err != nil {
		return err
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	if _, err := fmt.Fprintln(output, "== a clean extraction returns validated invoice JSON =="); err != nil {
		return fmt.Errorf("write clean extraction heading: %w", err)
	}
	clean, err := post(client, httpServer.URL+"/agents/invoice-parser/runs", invoiceText)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(output, clean); err != nil {
		return fmt.Errorf("write clean extraction: %w", err)
	}

	if _, err := fmt.Fprintln(output, "\n== a malformed extraction fails loudly, not silently =="); err != nil {
		return fmt.Errorf("write malformed extraction heading: %w", err)
	}
	malformed, err := post(client, httpServer.URL+"/agents/invoice-parser/runs", corruptedInvoiceText)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(output, malformed); err != nil {
		return fmt.Errorf("write malformed extraction: %w", err)
	}

	if _, err := fmt.Fprintln(output, "\n== the endpoint is documented by a generated contract =="); err != nil {
		return fmt.Errorf("write contract heading: %w", err)
	}
	document, err := server.OpenAPI()
	if err != nil {
		return err
	}
	var contract struct {
		Paths map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(document, &contract); err != nil {
		return err
	}
	for path := range contract.Paths {
		if _, err := fmt.Fprintln(output, path); err != nil {
			return fmt.Errorf("write contract path: %w", err)
		}
	}
	return nil
}

// newServer wires the extraction agent onto the embedded HTTP API at
// POST /agents/invoice-parser/runs.
func newServer() (*httpapi.Server, error) {
	agent, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{
			ID:           "invoice-parser",
			Name:         "Invoice Parser",
			Instructions: "Extract the invoice fields from the document text.",
			Model:        "fixture-model",
		},
		Model:          scriptedModel{},
		SchemaCompiler: lebrojsonschema.NewCompiler(),
		OutputSchema: &lebro.ModelOutputSchema{
			Name:   "invoice",
			Schema: json.RawMessage(invoiceSchema),
			Strict: true,
		},
	})
	if err != nil {
		return nil, err
	}
	server := httpapi.NewServer(httpapi.ServerConfig{
		Title:   "invoice-extraction-example",
		Version: "1.0.0",
	})
	if err := server.ExposeAgent(agent); err != nil {
		return nil, err
	}
	return server, nil
}

// scriptedModel stands in for the extraction model. It emits a valid invoice
// when the document carries a readable invoice number, and structurally broken
// fields when it does not, so both sides of the loud-failure contract are
// demonstrated against the same endpoint.
type scriptedModel struct{}

func (scriptedModel) Generate(_ context.Context, request lebro.ModelRequest) (lebro.ModelResponse, error) {
	document := ""
	for i := len(request.Messages) - 1; i >= 0; i-- {
		if request.Messages[i].Role == lebro.RoleUser {
			document = request.Messages[i].Content
			break
		}
	}
	var payload any
	if readable(document) {
		payload = map[string]any{
			"invoice_id":  "INV-2043",
			"vendor":      "Acme Supplies Ltd",
			"total_cents": 4198000,
			"currency":    "KES",
		}
	} else {
		// Every field violates the schema: empty strings where minLength is 1,
		// a string where an integer is required, a currency outside the enum.
		payload = map[string]any{
			"invoice_id":  "",
			"vendor":      "",
			"total_cents": "unreadable",
			"currency":    "???",
		}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return lebro.ModelResponse{}, err
	}
	return lebro.ModelResponse{
		Message: lebro.Message{
			Role:             lebro.RoleAssistant,
			StructuredOutput: lebro.NewModelStructuredOutput(encoded),
		},
		FinishReason: lebro.FinishReasonStop,
	}, nil
}

func readable(document string) bool { return strings.Contains(document, "INV-") }

// post sends one extraction request and renders status plus body.
func post(client *http.Client, url, body string) (string, error) {
	response, err := client.Post(url, "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		return "", fmt.Errorf("post extraction request: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		return "", fmt.Errorf("read extraction response: %w", err)
	}
	return fmt.Sprintf("%s\n%s", response.Status, payload), nil
}

const invoiceText = `{"messages":[{"content":"INVOICE #INV-2043 — Vendor: Acme Supplies Ltd — Total: 41,980.00 KES"}]}`

const corruptedInvoiceText = `{"messages":[{"content":"INVOICE (number unreadable) — vendor smudged"}]}`
