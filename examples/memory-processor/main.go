// The memory-processor example recalls an approved user fact into system
// context, then extracts and approves one targeted fact update after success.
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/tesh254/lebro"
)

func main() {
	if err := run(os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(output io.Writer) error {
	ctx := context.Background()
	store := lebro.NewMemoryStore()
	scope := lebro.WorkingMemoryScope{Namespace: "demo", OwnerID: "ada"}
	processor, err := lebro.NewMemoryProcessor(store, &lebro.MemoryProcessorConfig{
		Scope: scope,
		Extractor: extractorFunc(func(context.Context, lebro.MemoryExtractionRequest) ([]lebro.MemoryFactProposal, error) {
			return []lebro.MemoryFactProposal{{Key: "name", Value: []byte(`"Ada"`)}}, nil
		}),
		Approval: func(context.Context, lebro.MemoryWriteRequest) (bool, error) { return true, nil },
		Audit: func(_ context.Context, event lebro.MemoryAuditEvent) error {
			_, err := fmt.Fprintf(output, "approved: %t\n", event.Approved)
			return err
		},
	})
	if err != nil {
		return err
	}
	_, err = processor.ProcessOutput(ctx, lebro.ProcessorOutputRequest{Run: lebro.ProcessorRun{ID: "demo-run"}, Result: lebro.RunResult{Status: lebro.RunStatusSucceeded}})
	if err != nil {
		return err
	}
	fact, err := store.WorkingMemory().GetWorkingMemoryFact(ctx, scope, "name")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "%s: %s\n", "name", fact.Value)
	return err
}

type extractorFunc func(context.Context, lebro.MemoryExtractionRequest) ([]lebro.MemoryFactProposal, error)

func (f extractorFunc) ExtractMemoryFacts(ctx context.Context, request lebro.MemoryExtractionRequest) ([]lebro.MemoryFactProposal, error) {
	return f(ctx, request)
}
