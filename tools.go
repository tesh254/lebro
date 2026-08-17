package lebro

import (
	"context"

	"github.com/tesh254/lebro/internal/runtime"
)

func NewToolRegistry(compiler SchemaCompiler) (*ToolRegistry, error) {
	return runtime.NewToolRegistry(compiler)
}

func NewToolSchemaValidator(compiler SchemaCompiler, definition ToolDefinition) (*ToolSchemaValidator, error) {
	return runtime.NewToolSchemaValidator(compiler, definition)
}

func NewToolStep(tool *RegisteredTool) (*ToolStep, error) { return runtime.NewToolStep(tool) }

func ToolMetadataFromContext(ctx context.Context) map[string]string {
	return runtime.ToolMetadataFromContext(ctx)
}
