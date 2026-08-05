package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/tesh254/lebro"
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"
)

func TestExample(t *testing.T) {
	main()
	if got := mustValue(42, nil); got != 42 {
		t.Fatalf("mustValue() = %d, want 42", got)
	}

	want := errors.New("example failure")
	defer func() {
		if got := recover(); !errors.Is(got.(error), want) {
			t.Fatalf("panic = %v, want %v", got, want)
		}
	}()
	must(want)
}

func TestWeatherToolRejectsInvalidInputBeforeExecution(t *testing.T) {
	t.Parallel()

	registry, err := lebro.NewToolRegistry(lebrojsonschema.NewCompiler())
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(weatherTool{}); err != nil {
		t.Fatal(err)
	}
	result := registry.Execute(context.Background(), "weather.lookup", lebro.ToolExecutionRequest{
		Arguments: json.RawMessage(`{"city":42}`),
	})
	if result.State != lebro.ToolExecutionInvalidInput {
		t.Fatalf("state = %q, error = %v", result.State, result.Err)
	}
}
