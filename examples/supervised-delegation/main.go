// The supervised-delegation example shows a supervisor agent delegating
// focused work to two named subagents. Each subagent is an ordinary agent
// exposed as a schema-backed tool, so the supervisor selects one through
// ordinary model tool-calling rather than a separate routing mechanism.
//
// Delegated runs are bounded independently of the supervisor and are
// correlated to it through the run event stream, while the child transcripts
// stay isolated from the supervisor's thread.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/internal/testkit"
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"
)

func main() { must(run(os.Stdout)) }

func run(output io.Writer) error {
	compiler := lebrojsonschema.NewCompiler()

	// Two specialists, each with its own instructions and model script.
	researcher, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{
			ID:           "researcher",
			Name:         "Researcher",
			Instructions: "Answer research questions with a single concise finding.",
		},
		Model: testkit.NewModel(
			testkit.Text("The Nairobi office opened in 2019."),
		),
	})
	if err != nil {
		return err
	}

	editor, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{
			ID:           "editor",
			Name:         "Editor",
			Instructions: "Tighten prose without changing its meaning.",
		},
		Model: testkit.NewModel(
			testkit.Text("Nairobi opened in 2019."),
		),
	})
	if err != nil {
		return err
	}

	// Expose each specialist as a delegation capability. The bounds apply to
	// the delegated run alone, and neither subagent shares the supervisor's
	// thread, so a child sees only the task it was handed.
	research, err := lebro.NewSubagent(lebro.SubagentConfig{
		ID:          "delegate.research",
		Agent:       researcher,
		Description: "Delegate a factual research question to the researcher.",
		MaxSteps:    4,
		Deadline:    30 * time.Second,
	})
	if err != nil {
		return err
	}

	edit, err := lebro.NewSubagent(lebro.SubagentConfig{
		ID:          "delegate.edit",
		Agent:       editor,
		Description: "Delegate a prose-tightening task to the editor.",
		MaxSteps:    2,
		Deadline:    15 * time.Second,
	})
	if err != nil {
		return err
	}

	registry, err := lebro.NewToolRegistry(compiler)
	if err != nil {
		return err
	}
	if err := registry.Register(research); err != nil {
		return err
	}
	if err := registry.Register(edit); err != nil {
		return err
	}

	// The supervisor picks a subagent by tool ID, reads its result, and
	// answers. Its allow-list governs which subagents it may reach.
	supervisorRecorder := lebro.NewRunRecorder()
	supervisor, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{
			ID:           "supervisor",
			Name:         "Supervisor",
			Instructions: "Delegate focused work to the most suitable subagent, then report the result.",
			Tools:        []lebro.ToolID{"delegate.research", "delegate.edit"},
		},
		Model: testkit.NewModel(
			testkit.ToolCallResponse(testkit.ToolCall{
				ToolID:    "delegate.research",
				Arguments: json.RawMessage(`{"task":"When did the Nairobi office open?"}`),
			}),
			testkit.Text("The Nairobi office opened in 2019."),
		),
		Tools:    registry,
		Listener: supervisorRecorder,
	})
	if err != nil {
		return err
	}

	result, err := supervisor.Run(context.Background(), lebro.RunInput{
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "When did the Nairobi office open?"}},
	})
	if err != nil {
		return err
	}

	fmt.Fprintf(output, "status: %s\n", result.Status)
	fmt.Fprintf(output, "answer: %s\n", finalAssistantContent(result))

	// The delegation result reaches the supervisor as a tool message carrying
	// the child's identity, run ID, and terminal status.
	for _, message := range result.Messages {
		if message.Role != lebro.RoleTool {
			continue
		}
		var delegation struct {
			AgentID string `json:"agent_id"`
			RunID   string `json:"run_id"`
			Status  string `json:"status"`
			Output  string `json:"output"`
		}
		if err := json.Unmarshal([]byte(message.Content), &delegation); err != nil {
			return err
		}
		fmt.Fprintf(output, "delegated to: %s\n", delegation.AgentID)
		fmt.Fprintf(output, "delegated status: %s\n", delegation.Status)
		fmt.Fprintf(output, "delegated output: %s\n", delegation.Output)
	}

	// Parent events identify the delegation boundary, so a reader can pair a
	// child run to the exact supervisor step that started it.
	for _, event := range supervisorRecorder.Events() {
		if event.Type == lebro.RunEventToolFinished {
			fmt.Fprintf(output, "delegation step: %d (%s)\n", event.Step, event.ToolID)
		}
	}

	return nil
}

func finalAssistantContent(result lebro.RunResult) string {
	for i := len(result.Messages) - 1; i >= 0; i-- {
		if result.Messages[i].Role == lebro.RoleAssistant && result.Messages[i].Content != "" {
			return result.Messages[i].Content
		}
	}
	return ""
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
