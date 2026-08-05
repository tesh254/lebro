package main

import (
	"context"
	"errors"
	"testing"

	"github.com/tesh254/lebro"
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
	store := lebro.NewMemoryStore()
	want := errors.New("transaction step failed")
	err := store.Transaction(ctx, func(ctx context.Context, repositories lebro.Repositories) error {
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
