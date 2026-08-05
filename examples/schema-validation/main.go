package main

import (
	"encoding/json"
	"fmt"

	"github.com/tesh254/lebro"
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"
)

func main() {
	validator := mustValue(lebro.NewToolSchemaValidator(lebrojsonschema.NewCompiler(), lebro.ToolDefinition{
		ID: "lookup-weather",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"required":["city"],
			"properties":{"city":{"type":"string"}},
			"additionalProperties":false
		}`),
		OutputSchema: json.RawMessage(`{
			"type":"object",
			"required":["temperature_c"],
			"properties":{"temperature_c":{"type":"number"}}
		}`),
	}))

	must(validator.ValidateInput(json.RawMessage(`{"city":"Nairobi"}`)))
	must(validator.ValidateOutput(json.RawMessage(`{"temperature_c":24.5}`)))

	fmt.Println("tool input and output are valid")
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func mustValue[T any](value T, err error) T {
	must(err)
	return value
}
