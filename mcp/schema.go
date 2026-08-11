package mcp

import (
	"encoding/json"
	"fmt"

	"github.com/tesh254/lebro"
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"
)

func mustCompileMCPInputSchema(schema json.RawMessage) lebro.CompiledSchema {
	compiler := lebrojsonschema.NewCompiler()
	compiled, err := compiler.Compile(schema)
	if err != nil {
		panic(fmt.Sprintf("lebro/mcp: compile MCP input schema: %v", err))
	}
	return compiled
}
