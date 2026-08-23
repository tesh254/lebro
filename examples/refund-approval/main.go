// refund-approval is the dispute-flow build: the workflow proposes a refund,
// parks durably until a human approves, and only then commits it. The run
// survives a process restart while suspended, and the resume boundary checks
// that the approver actually holds the refunds:approve capability before the
// workflow is resumed.
//
// The capability gate lives at the application boundary — the same place an
// HTTP handler would authenticate the approver — because lebro deliberately
// ships no identity provider. The suspend/resume machinery underneath is all
// library: schema-checked resume inputs and a durable snapshot that outlives
// the process.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/tesh254/lebro"
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"
)

// Capability refunds:approve mirrors a permission in the operator's own auth
// system; only identities holding it may resume a suspended refund.
const approveRefunds = lebro.Capability("refunds:approve")

func main() {
	if err := run(os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(output io.Writer) error {
	ctx := context.Background()
	store, reopenStore, err := openStores(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = reopenStore.close() }()

	wf, err := newRefundWorkflow(store.store)
	if err != nil {
		return err
	}

	// 1. The copilot proposes; the run suspends instead of moving money.
	result, err := wf.Run(ctx, lebro.WorkflowRunInput{
		Input: json.RawMessage(`{"order_id":"ORD-77","amount_cents":8400,"reason":"item arrived damaged"}`),
	})
	if err != nil {
		return err
	}
	writef(output, "proposed refund for %s; status: %s\n", "ORD-77", result.Status)
	writef(output, "suspended at %q; resume contract: %s\n", result.Suspend.StepID, result.Suspend.Contract)

	// 2. A support agent without refunds:approve tries to resume. The
	// application gate refuses before the workflow is touched.
	requester := lebro.Identity{Subject: "support-agent", Tenant: "acme"}
	if gateErr := authorizeApprover(requester); gateErr == nil {
		return fmt.Errorf("expected the gate to refuse %s", requester.Subject)
	} else {
		writef(output, "%s refused (%v); run untouched\n", requester.Subject, gateErr)
	}

	// 3. An approver signs off but submits malformed consent first: rejected by
	// the resume schema without corrupting the snapshot.
	_, err = wf.Resume(ctx, lebro.WorkflowResumeInput{
		RunID: result.ID,
		Input: json.RawMessage(`{"approved":false,"approver":"dana"}`),
	})
	if !errors.Is(err, lebro.ErrInvalidResumeInput) {
		return fmt.Errorf("expected ErrInvalidResumeInput, got %v", err)
	}
	writef(output, "invalid consent rejected: %v\n", err)

	// 4. The process restarts. The suspension lives in the durable snapshot.
	if err := store.close(); err != nil {
		return err
	}
	wf2, err := newRefundWorkflow(reopenStore.store)
	if err != nil {
		return err
	}

	// 5. A human with refunds:approve resumes the parked run; only now does the
	// refund commit.
	approver := lebro.Identity{
		Subject:      "dana",
		Tenant:       "acme",
		Capabilities: []lebro.Capability{approveRefunds},
	}
	if err := authorizeApprover(approver); err != nil {
		return err
	}
	resumed, err := wf2.Resume(ctx, lebro.WorkflowResumeInput{
		RunID:    result.ID,
		Input:    json.RawMessage(`{"approved":true,"approver":"dana"}`),
		Metadata: map[string]string{"approver_subject": approver.Subject},
	})
	if err != nil {
		return err
	}
	writef(output, "resumed by %s; status: %s\n", approver.Subject, resumed.Status)
	writef(output, "final output: %s\n", resumed.Output)
	return nil
}

// authorizeApprover is the human-sign-off boundary: refuse anyone who does not
// hold refunds:approve before resuming the workflow.
func authorizeApprover(identity lebro.Identity) error {
	if !identity.HasCapability(approveRefunds) {
		return fmt.Errorf("subject %q lacks %s", identity.Subject, approveRefunds)
	}
	return nil
}

// newRefundWorkflow builds the propose/suspend/commit pipeline over any store,
// so the same definition runs before and after the simulated restart.
func newRefundWorkflow(store lebro.Store) (*lebro.LinearWorkflow, error) {
	return lebro.NewLinearWorkflow(lebro.LinearWorkflowConfig{
		Definition:     lebro.WorkflowDefinition{ID: "refund-copilot", Name: "Refund Copilot", Version: "v1"},
		SchemaCompiler: lebrojsonschema.NewCompiler(),
		Store:          store,
		Steps: []lebro.Step{
			{
				Definition: lebro.StepDefinition{
					ID: "propose-refund",
					InputSchema: json.RawMessage(`{
						"type":"object",
						"required":["order_id","amount_cents","reason"],
						"properties":{
							"order_id":{"type":"string"},
							"amount_cents":{"type":"integer","minimum":1},
							"reason":{"type":"string"}
						},
						"additionalProperties":false
					}`),
				},
				Handler: lebro.StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
					var proposal map[string]any
					if err := json.Unmarshal(input, &proposal); err != nil {
						return nil, err
					}
					return json.Marshal(map[string]any{
						"proposal": proposal,
						"state":    "awaiting human approval",
					})
				}),
			},
			{
				Definition: lebro.StepDefinition{
					ID: "await-approval",
					SuspendSchema: json.RawMessage(`{
						"type":"object",
						"required":["approved","approver"],
						"properties":{
							"approved":{"type":"boolean","const":true},
							"approver":{"type":"string","minLength":1}
						},
						"additionalProperties":false
					}`),
				},
				Handler: lebro.StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
					return nil, &lebro.SuspendError{Signal: lebro.SuspendSignal{
						StepID:   "await-approval",
						Contract: json.RawMessage(`{"approved":true,"approver":"dana"}`),
						Payload:  json.RawMessage(`{"pending":"human approval with refunds:approve"}`),
					}}
				}),
			},
			{
				Definition: lebro.StepDefinition{ID: "commit-refund"},
				Handler: lebro.StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
					var consent struct {
						Approved bool   `json:"approved"`
						Approver string `json:"approver"`
					}
					if err := json.Unmarshal(input, &consent); err != nil {
						return nil, err
					}
					// In production this is where the payment provider call goes;
					// it only ever executes after a valid human approval.
					return json.Marshal(map[string]any{
						"refunded":     true,
						"amount_cents": 8400,
						"approver":     consent.Approver,
					})
				}),
			},
		},
	})
}

// db pairs a SQLite store with its file so the restart reopens exactly what
// was saved.
type db struct {
	store *lebro.SQLiteStore
	dsn   string
}

func (d *db) close() error { return d.store.Close() }

func openStores(ctx context.Context) (*db, *db, error) {
	dir, err := os.MkdirTemp("", "lebro-refund-approval-")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	first, err := openDB(ctx, filepath.Join(dir, "refund.db"))
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	reopened, err := openDB(ctx, first.dsn)
	if err != nil {
		_ = first.close()
		cleanup()
		return nil, nil, err
	}
	return first, reopened, nil
}

func openDB(ctx context.Context, dsn string) (*db, error) {
	store, err := lebro.NewSQLiteStore(dsn)
	if err != nil {
		return nil, err
	}
	if err := store.Migrate(ctx); err != nil {
		_ = store.Close()
		return nil, err
	}
	return &db{store: store, dsn: dsn}, nil
}

func writef(output io.Writer, format string, args ...any) {
	if _, err := fmt.Fprintf(output, format, args...); err != nil {
		panic(err)
	}
}
