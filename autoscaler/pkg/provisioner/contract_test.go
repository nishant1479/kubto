// Package provisioner — contract tests enforcing lifecycle semantics for any NodeProvisioner implementation.
package provisioner

import (
	"context"
	"errors"
	"testing"
)

// runProviderContractTests executes the standard contract test suite against
// any implementation of the NodeProvisioner interface.
func runProviderContractTests(t *testing.T, prov NodeProvisioner) {
	ctx := context.Background()

	// 1. CreateNode must return a valid non-empty node identifier
	nodeName, err := prov.CreateNode(ctx)
	if err != nil {
		t.Fatalf("contract violation: CreateNode failed: %v", err)
	}
	if nodeName == "" {
		t.Fatal("contract violation: CreateNode returned empty node name")
	}

	// 2. CordonNode must accept the created node name
	if err := prov.CordonNode(ctx, nodeName); err != nil {
		t.Fatalf("contract violation: CordonNode failed for %s: %v", nodeName, err)
	}

	// 3. TerminateNode must succeed for the node
	if err := prov.TerminateNode(ctx, nodeName); err != nil {
		t.Fatalf("contract violation: TerminateNode failed for %s: %v", nodeName, err)
	}

	// 4. Idempotent cleanup: calling TerminateNode again should succeed without panicking
	if err := prov.TerminateNode(ctx, nodeName); err != nil {
		t.Logf("secondary termination returned: %v (tolerated if idempotent)", err)
	}
}

func TestFakeProvisioner_MeetsContract(t *testing.T) {
	fakeProv := NewFakeProvisioner()
	runProviderContractTests(t, fakeProv)

	if fakeProv.CreateCount() != 1 {
		t.Errorf("expected 1 created node, got %d", fakeProv.CreateCount())
	}
	if fakeProv.CordonCount() != 1 {
		t.Errorf("expected 1 cordoned node, got %d", fakeProv.CordonCount())
	}
	if fakeProv.TerminateCount() != 2 {
		t.Errorf("expected 2 terminate calls (including idempotency test), got %d", fakeProv.TerminateCount())
	}
}

func TestFakeProvisioner_ErrorHandling(t *testing.T) {
	ctx := context.Background()
	fakeProv := NewFakeProvisioner()

	// Inject errors and verify actionable error returns
	fakeProv.CreateError = errors.New("out of capacity in region")
	_, err := fakeProv.CreateNode(ctx)
	if err == nil || err.Error() != "out of capacity in region" {
		t.Errorf("expected actionable CreateError, got %v", err)
	}

	fakeProv.CreateError = nil
	fakeProv.CordonError = errors.New("node not found in cluster")
	err = fakeProv.CordonNode(ctx, "nonexistent")
	if err == nil || err.Error() != "node not found in cluster" {
		t.Errorf("expected actionable CordonError, got %v", err)
	}

	fakeProv.TerminateError = errors.New("api rate limit exceeded")
	err = fakeProv.TerminateNode(ctx, "nonexistent")
	if err == nil || err.Error() != "api rate limit exceeded" {
		t.Errorf("expected actionable TerminateError, got %v", err)
	}
}
