package main

import (
	"errors"
	"testing"
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
