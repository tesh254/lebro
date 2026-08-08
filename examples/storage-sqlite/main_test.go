package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tesh254/lebro"
)

func TestExample(t *testing.T) {
	path := filepath.Join(t.TempDir(), "example.db")
	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()
	os.Args = []string{"storage-sqlite", path}
	main()

	store, err := lebro.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("reopen Migrate(): %v", err)
	}
	ctx := context.Background()
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

func TestTransactionCallbackReturnsErrorsAndAborts(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tx.db")
	store, err := lebro.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	want := errors.New("transaction step failed")
	err = store.Transaction(ctx, func(ctx context.Context, repositories lebro.Repositories) error {
		return runSteps(
			func() error {
				return repositories.Threads().CreateThread(ctx, lebro.ThreadRecord{ID: "thread-1"})
			},
			func() error { return want },
		)
	})
	if !errors.Is(err, want) {
		t.Fatalf("Transaction() error = %v, want %v", err, want)
	}
	if _, err := store.Threads().GetThread(ctx, "thread-1"); !errors.Is(err, lebro.ErrNotFound) {
		t.Fatalf("GetThread() error = %v, want ErrNotFound", err)
	}
}
