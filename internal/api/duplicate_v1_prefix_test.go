package api

import "testing"

func TestCollapseDuplicateV1Prefix(t *testing.T) {
	got, ok := collapseDuplicateV1Prefix("/v1/v1/messages")
	if !ok || got != "/v1/messages" {
		t.Fatalf("messages path = %q ok=%v", got, ok)
	}
	got, ok = collapseDuplicateV1Prefix("/v1/v1/chat/completions")
	if !ok || got != "/v1/chat/completions" {
		t.Fatalf("chat path = %q ok=%v", got, ok)
	}
	if _, ok = collapseDuplicateV1Prefix("/v1/messages"); ok {
		t.Fatal("single prefix should stay")
	}
}
