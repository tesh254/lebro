package openrouter

import "testing"

func TestMediaConstructor(t *testing.T) {
	if _, e := NewMedia(MediaConfig{}); e == nil {
		t.Fatal("missing credentials")
	}
	m, e := NewMedia(MediaConfig{APIKey: "fixture", Model: "google/veo-3.1"})
	if e != nil || !m.Capabilities().Video {
		t.Fatal(e)
	}
}
