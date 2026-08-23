package main

import "testing"

func TestAgentNetworkExample(t *testing.T) {
	if err := run(); err != nil {
		t.Fatal(err)
	}
}
