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

	fmt.Fprintln(output, "== a clean extraction returns validated invoice JSON ==")
	fmt.Fprintln(output, post(httpServer.URL+"/agents/invoice-parser/runs", invoiceText))

	fmt.Fprintln(output, "\n== a malformed extraction fails loudly, not silently ==")
	fmt.Fprintln(output, post(httpServer.URL+"/agents/invoice-parser/runs", corruptedInvoiceText))

	fmt.Fprintln(output, "\n== the endpoint is documented by a generated contract ==")
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
		fmt.Fprintln(output, path)
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
func post(url, body string) string {
	response, err := http.Post(url, "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		log.Fatal(err)
	}
	return fmt.Sprintf("%s\n%s", response.Status, payload)
}

const invoiceText = `{"messages":[{"content":"INVOICE #INV-2043 — Vendor: Acme Supplies Ltd — Total: 41,980.00 KES"}]}`

const corruptedInvoiceText = `{"messages":[{"content":"INVOICE (number unreadable) — vendor smudged"}]}`
