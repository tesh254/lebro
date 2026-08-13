// The memory-processor example recalls an approved user fact into system
// context, then extracts and approves one targeted fact update after success.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/tesh254/lebro"
)

func main() {
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
			_, err := fmt.Fprintf(os.Stdout, "approved: %t\n", event.Approved)
			return err
		},
	})
	if err != nil {
		panic(err)
	}
	_, err = processor.ProcessOutput(ctx, lebro.ProcessorOutputRequest{Run: lebro.ProcessorRun{ID: "demo-run"}, Result: lebro.RunResult{Status: lebro.RunStatusSucceeded}})
	if err != nil {
		panic(err)
	}
	fact, err := store.WorkingMemory().GetWorkingMemoryFact(ctx, scope, "name")
	if err != nil {
		panic(err)
	}
	fmt.Println(string(fact.Value))
}

type extractorFunc func(context.Context, lebro.MemoryExtractionRequest) ([]lebro.MemoryFactProposal, error)

func (f extractorFunc) ExtractMemoryFacts(ctx context.Context, request lebro.MemoryExtractionRequest) ([]lebro.MemoryFactProposal, error) {
	return f(ctx, request)
}
