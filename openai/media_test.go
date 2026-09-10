package openai

import "testing"

func TestMediaConstructor(t *testing.T) {
	if _, e := NewMedia(MediaConfig{}); e == nil {
		t.Fatal("missing credentials")
	}
	m, e := NewMedia(MediaConfig{APIKey: "fixture", Model: "gpt-image-1"})
	if e != nil {
		t.Fatalf("NewMedia with fixture config: %v", e)
	}
	if !m.Capabilities().Image {
		t.Fatal("gpt-image-1 must report Image capability")
	}
}
