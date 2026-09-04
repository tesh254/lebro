package lebro

import "github.com/tesh254/lebro/internal/runtime"

func NewMemoryProcessor(store Store, config *MemoryProcessorConfig) (*MemoryProcessor, error) {
	return runtime.NewMemoryProcessor(store, config)
}
