// Package runstate coordinates cancellation and the first terminal cause of one run.
package runstate

import (
	"context"
	"sync"
)

// Kind identifies why a run stopped.
type Kind uint8

const (
	None Kind = iota
	Signal
	Timeout
	StopOnError
	Internal
)

// Cause is the immutable first terminal reason recorded for a run.
type Cause struct {
	Kind   Kind
	Status int
}

type contextKey struct{}

// State owns cancellation, force escalation, and first-cause arbitration.
type State struct {
	mu        sync.Mutex
	cause     Cause
	cancel    context.CancelFunc
	force     chan struct{}
	forceOnce sync.Once
}

// New creates one run state and attaches it to a child of parent.
func New(parent context.Context) (*State, context.Context) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	s := &State{cancel: cancel, force: make(chan struct{})}
	return s, context.WithValue(ctx, contextKey{}, s)
}

// FromContext returns the run state attached by New.
func FromContext(ctx context.Context) (*State, bool) {
	if ctx == nil {
		return nil, false
	}
	s, ok := ctx.Value(contextKey{}).(*State)
	return s, ok
}

// Record stores cause if no terminal cause has been recorded yet.
func (s *State) Record(cause Cause) bool {
	if s == nil || cause == (Cause{}) {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cause != (Cause{}) {
		return false
	}
	s.cause = cause
	return true
}

// Stop records cause and cancels the run. It reports whether cause won.
func (s *State) Stop(cause Cause) bool {
	recorded := s.Record(cause)
	s.Cancel()
	return recorded
}

// Cancel cancels the run without choosing a cause.
func (s *State) Cancel() {
	if s != nil && s.cancel != nil {
		s.cancel()
	}
}

// Force requests immediate escalation. It is safe to call repeatedly.
func (s *State) Force() {
	if s != nil {
		s.forceOnce.Do(func() { close(s.force) })
	}
}

// ForceChan is closed after Force is called.
func (s *State) ForceChan() <-chan struct{} {
	if s == nil {
		return nil
	}
	return s.force
}

// Cause returns a snapshot of the first terminal cause.
func (s *State) Cause() Cause {
	if s == nil {
		return Cause{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cause
}
