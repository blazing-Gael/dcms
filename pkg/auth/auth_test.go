package auth

import (
	"context"
	"testing"
)

func TestPrincipal_HasRole(t *testing.T) {
	p := Principal{Roles: []string{"admin", "author"}}
	if !p.HasRole("author") {
		t.Error("expected HasRole(author) true")
	}
	if p.HasRole("editor") {
		t.Error("expected HasRole(editor) false")
	}
	if (Principal{}).HasRole("admin") {
		t.Error("zero principal should hold no roles")
	}
}

func TestContext_RoundTrip(t *testing.T) {
	p := Principal{ID: "u1", Roles: []string{"admin"}, Authenticated: true, Claims: map[string]string{"sub": "appwrite:123"}}
	ctx := NewContext(context.Background(), p)

	got := FromContext(ctx)
	if got.ID != "u1" || !got.Authenticated || !got.HasRole("admin") || got.Claims["sub"] != "appwrite:123" {
		t.Fatalf("round-trip lost data: %+v", got)
	}
}

func TestFromContext_AbsentIsAnonymousZero(t *testing.T) {
	got := FromContext(context.Background())
	if got.Authenticated || got.ID != "" || got.Roles != nil {
		t.Fatalf("expected zero anonymous principal, got %+v", got)
	}
}
