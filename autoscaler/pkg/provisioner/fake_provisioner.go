// Package provisioner — FakeNodeProvisioner is a deterministic, in-memory
// provisioner for use in unit and integration tests.
//
// It does not call Docker, exec any commands, or interact with any external system.
// It satisfies the NodeProvisioner interface and records calls for assertion.
package provisioner

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// FakeNodeProvisioner implements NodeProvisioner for testing.
//
// By default every call to CreateNode succeeds and returns a unique node name.
// Tests can inject FailAfter to make the provisioner return errors after N
// successful calls.
type FakeNodeProvisioner struct {
	mu sync.Mutex

	// CreatedNodes records the names returned by successful CreateNode calls.
	CreatedNodes []string

	// CordonnedNodes records nodes on which CordonNode was called.
	CordonedNodes []string

	// TerminatedNodes records nodes on which TerminateNode was called.
	TerminatedNodes []string

	// CreateError, when non-nil, is returned by every CreateNode call
	// (after FailAfter successful calls, if set).
	CreateError error

	// FailAfter, when > 0, lets the first FailAfter CreateNode calls succeed
	// and returns CreateError (or a generic error) afterwards.
	FailAfter int

	// CreateDelay is slept (respecting ctx) before completing CreateNode.
	CreateDelay time.Duration

	// createAttempts counts every CreateNode invocation, including failures.
	createAttempts int

	// CordonError, when non-nil, is returned by every CordonNode call.
	CordonError error

	// TerminateError, when non-nil, is returned by every TerminateNode call.
	TerminateError error

	// counter is the source for unique node names.
	counter atomic.Int64
}

// NewFakeProvisioner returns a FakeNodeProvisioner ready to use.
func NewFakeProvisioner() *FakeNodeProvisioner {
	return &FakeNodeProvisioner{}
}

// CreateNode records the call and returns a unique fake node name, or
// CreateError if set.
func (f *FakeNodeProvisioner) CreateNode(ctx context.Context) (string, error) {
	f.mu.Lock()
	delay := f.CreateDelay
	failAfter := f.FailAfter
	createErr := f.CreateError
	already := len(f.CreatedNodes)
	f.createAttempts++
	f.mu.Unlock()

	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-timer.C:
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if failAfter > 0 && already >= failAfter {
		if createErr == nil {
			createErr = fmt.Errorf("fake provisioner FailAfter=%d", failAfter)
		}
		return "", createErr
	}
	if createErr != nil && failAfter == 0 {
		return "", createErr
	}

	n := f.counter.Add(1)
	name := fmt.Sprintf("fake-node-%d", n)
	f.CreatedNodes = append(f.CreatedNodes, name)
	return name, nil
}

// CordonNode records the call and returns CordonError if set.
func (f *FakeNodeProvisioner) CordonNode(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.CordonError != nil {
		return f.CordonError
	}

	f.CordonedNodes = append(f.CordonedNodes, name)
	return nil
}

// TerminateNode records the call and returns TerminateError if set.
func (f *FakeNodeProvisioner) TerminateNode(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.TerminateError != nil {
		return f.TerminateError
	}

	f.TerminatedNodes = append(f.TerminatedNodes, name)
	return nil
}

// CreateCount returns the number of successful CreateNode calls recorded so far.
func (f *FakeNodeProvisioner) CreateCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.CreatedNodes)
}

// CreateAttempts returns the number of CreateNode invocations, including failures.
func (f *FakeNodeProvisioner) CreateAttempts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createAttempts
}

// CordonCount returns the number of successful CordonNode calls recorded so far.
func (f *FakeNodeProvisioner) CordonCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.CordonedNodes)
}

// TerminateCount returns the number of successful TerminateNode calls recorded so far.
func (f *FakeNodeProvisioner) TerminateCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.TerminatedNodes)
}

// Reset clears all recorded calls. Useful for test isolation.
func (f *FakeNodeProvisioner) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.CreatedNodes = nil
	f.CordonedNodes = nil
	f.TerminatedNodes = nil
	f.createAttempts = 0
}
