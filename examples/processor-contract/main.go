// The processor-contract example shows how to declare an input processor,
// retain it in ordered pipeline configuration, and return a typed transform.
// Pipeline execution is wired into Agent in a later runtime task.
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/tesh254/lebro"
)

func main() { must(run(os.Stdout)) }

func run(output io.Writer) error {
	pipeline, err := lebro.NewProcessorPipeline(prefixProcessor{})
	if err != nil {
		return err
	}

	request := lebro.ProcessorInputRequest{
		Run:   lebro.ProcessorRun{ID: "example-run"},
		Input: lebro.RunInput{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hello"}}},
	}
	processor := pipeline.Processors()[0].(lebro.InputProcessor)
	result, err := processor.ProcessInput(context.Background(), request.Clone())
	if err != nil {
		return lebro.NormalizeProcessorError(lebro.ProcessorPhaseInput, processor.Name(), err)
	}
	decision, err := lebro.NormalizeProcessorDecision(result.Decision)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "processor: %s\ndecision: %s\nmessage: %s\n", processor.Name(), decision.Kind, result.Input.Messages[0].Content)
	return err
}

type prefixProcessor struct{}

func (prefixProcessor) Name() string { return "prefix" }

func (prefixProcessor) ProcessInput(_ context.Context, request lebro.ProcessorInputRequest) (lebro.ProcessorInputResult, error) {
	result := lebro.ProcessorInputResult{Decision: lebro.ProcessorDecision{Kind: lebro.ProcessorTransform}, Input: request.Input}
	result.Input.Messages[0].Content = "processed: " + result.Input.Messages[0].Content
	return result, nil
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
