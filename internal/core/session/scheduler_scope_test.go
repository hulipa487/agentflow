package session

import (
	"context"
	"testing"
)

func TestSchedulerScopeCarriesTenantWithoutMaintenance(t *testing.T) {
	ctx := WithProvenanceKind(context.Background(), "timer")
	ctx = WithActingIdentity(ctx, PersonalIdentity("u_tenant"))
	ctx = WithSessionKey(ctx, "support|chat")

	scope := schedulerScopeFromCtx(ctx)
	if scope.UserUUID == nil || *scope.UserUUID != "u_tenant" {
		t.Fatalf("user scope = %v", scope.UserUUID)
	}
	if scope.Principal != "session:support|chat" {
		t.Fatalf("principal = %q", scope.Principal)
	}
	if MaintenanceFromCtx(ctx) {
		t.Fatal("a timer provenance must not be a maintenance context")
	}
	if !MaintenanceFromCtx(WithProvenanceKind(context.Background(), "scheduler")) {
		t.Fatal("engine triggers keep maintenance provenance")
	}
}

func TestSchedulerScopeKeepsKnownEmptyTenant(t *testing.T) {
	ctx := WithProvenanceKind(context.Background(), "timer")
	ctx = WithActingIdentity(ctx, ActingIdentity{})
	scope := schedulerScopeFromCtx(ctx)
	if scope.UserUUID == nil || *scope.UserUUID != "" {
		t.Fatalf("known-empty tenant must stay a pointer to \"\": %v", scope.UserUUID)
	}
}
