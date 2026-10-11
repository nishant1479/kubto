// Package provisioner — unit tests for FakeNodeProvisioner.
package provisioner

import (
	"context"
	"errors"
	"testing"
)

func TestFakeProvisioner_CreateNode_Success(t *testing.T) {
	f := NewFakeProvisioner()
	name, err := f.CreateNode(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name == "" {
		t.Fatal("expected non-empty node name")
	}
	if f.CreateCount() != 1 {
		t.Errorf("expected CreateCount=1, got %d", f.CreateCount())
	}
}

func TestFakeProvisioner_CreateNode_UniqueNames(t *testing.T) {
	f := NewFakeProvisioner()
	seen := make(map[string]bool)
	for i := 0; i < 10; i++ {
		name, err := f.CreateNode(context.Background())
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
		if seen[name] {
			t.Errorf("duplicate name: %s", name)
		}
		seen[name] = true
	}
}

func TestFakeProvisioner_CreateNode_Error(t *testing.T) {
	f := NewFakeProvisioner()
	f.CreateError = errors.New("provider down")

	_, err := f.CreateNode(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if f.CreateCount() != 0 {
		t.Errorf("no node should be recorded on error")
	}
}

func TestFakeProvisioner_CordonNode_Success(t *testing.T) {
	f := NewFakeProvisioner()
	if err := f.CordonNode(context.Background(), "worker-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.CordonCount() != 1 {
		t.Errorf("expected CordonCount=1, got %d", f.CordonCount())
	}
}

func TestFakeProvisioner_CordonNode_Error(t *testing.T) {
	f := NewFakeProvisioner()
	f.CordonError = errors.New("cannot cordon")
	if err := f.CordonNode(context.Background(), "w1"); err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestFakeProvisioner_Reset(t *testing.T) {
	f := NewFakeProvisioner()
	f.CreateNode(context.Background())
	f.CordonNode(context.Background(), "w1")

	f.Reset()
	if f.CreateCount() != 0 || f.CordonCount() != 0 {
		t.Error("Reset should clear all recorded calls")
	}
}
