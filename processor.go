package lebro

import "github.com/tesh254/lebro/internal/runtime"

func NewProcessorPipeline(processors ...Processor) (ProcessorPipeline, error) {
	return runtime.NewProcessorPipeline(processors...)
}

func NormalizeProcessorDecision(decision ProcessorDecision) (ProcessorDecision, error) {
	return runtime.NormalizeProcessorDecision(decision)
}

func NormalizeProcessorError(phase ProcessorPhase, processor string, err error) error {
	return runtime.NormalizeProcessorError(phase, processor, err)
}
