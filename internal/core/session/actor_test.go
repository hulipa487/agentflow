package session

import (
	"context"
	"testing"
)

func TestIsConfirmRequest(t *testing.T) {
	if isConfirmRequest(`{"ok":false,"needs_confirm":true,"tool":"test"}`) != true {
		t.Fatal("expected confirm request")
	}
	if isConfirmRequest(`{"ok":false,"needs_confirm":false}`) != false {
		t.Fatal("expected not confirm request")
	}
	if isConfirmRequest(`{"ok":true}`) != false {
		t.Fatal("expected not confirm request")
	}
	if isConfirmRequest(`not json`) != false {
		t.Fatal("expected not confirm request for bad JSON")
	}
}

func TestOwnerContext(t *testing.T) {
	ctx := WithOwner(context.Background(), "session-42")
	if OwnerFromCtx(ctx) != "session-42" {
		t.Fatalf("expected session-42, got %q", OwnerFromCtx(ctx))
	}
}
