package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/tesh254/lebro"
)

func TestExample(t *testing.T) {
	dsn := os.Getenv("LEBRO_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("skipping PostgreSQL example: set LEBRO_POSTGRES_TEST_DSN to a disposable database DSN")
	}
	cleanupDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanupDB.Close() }()
	ctx := context.Background()
	for _, table := range []string{"workflow_snapshots", "workflow_runs", "messages", "threads", "schema_migrations"} {
		if _, err := cleanupDB.ExecContext(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s CASCADE", table)); err != nil {
			t.Fatalf("drop %s: %v", table, err)
		}
	}

	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()
	os.Args = []string{"storage-postgres", dsn}
	main()

	store, err := lebro.NewPostgresStore(dsn, lebro.PostgresStoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("reopen Migrate(): %v", err)
	}
	run, err := store.WorkflowRuns().GetWorkflowRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("GetWorkflowRun(): %v", err)
	}
	if run.Status != lebro.RunStatusRunning {
		t.Fatalf("run status = %q, want running", run.Status)
	}
	messages, err := store.Messages().ListMessages(ctx, "support-42", lebro.PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages.Records) != 1 || messages.Records[0].Message.Content != "Where is my order?" {
		t.Fatalf("messages = %#v", messages)
	}
}

func TestRunStepsReturnsTheFirstError(t *testing.T) {
	t.Parallel()

	want := errors.New("transaction step failed")
	secondCalled := false
	err := runSteps(
		func() error { return want },
		func() error {
			secondCalled = true
			return nil
		},
	)
	if !errors.Is(err, want) {
		t.Fatalf("runSteps() error = %v, want %v", err, want)
	}
	if secondCalled {
		t.Fatal("runSteps() continued after an error")
	}
}
