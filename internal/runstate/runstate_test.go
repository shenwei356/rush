package runstate

import (
	"context"
	"sync"
	"testing"
)

func TestFirstCauseWinsAndStopCancels(t *testing.T) {
	s, ctx := New(context.Background())
	if got, ok := FromContext(ctx); !ok || got != s {
		t.Fatal("state was not attached to context")
	}
	if !s.Stop(Cause{Kind: Signal, Status: 130}) {
		t.Fatal("first stop did not record its cause")
	}
	if s.Stop(Cause{Kind: Internal, Status: 1}) {
		t.Fatal("a later stop replaced the first cause")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("Stop did not cancel the run context")
	}
	if got := s.Cause(); got.Kind != Signal || got.Status != 130 {
		t.Fatalf("cause = %#v; want signal/130", got)
	}
}

func TestConcurrentRecordHasOneStableWinner(t *testing.T) {
	s, _ := New(context.Background())
	causes := []Cause{{Kind: Signal, Status: 130}, {Kind: Signal, Status: 143}, {Kind: Internal, Status: 1}}
	var wg sync.WaitGroup
	for _, cause := range causes {
		cause := cause
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Record(cause)
		}()
	}
	wg.Wait()
	got := s.Cause()
	if got == (Cause{}) {
		t.Fatal("no concurrent cause was recorded")
	}
	for i := 0; i < 100; i++ {
		s.Record(Cause{Kind: Internal, Status: 99})
		if next := s.Cause(); next != got {
			t.Fatalf("cause changed from %#v to %#v", got, next)
		}
	}
}

func TestForceIsIdempotentAndDoesNotChangeCause(t *testing.T) {
	s, _ := New(context.Background())
	s.Record(Cause{Kind: Signal, Status: 143})
	s.Force()
	s.Force()
	select {
	case <-s.ForceChan():
	default:
		t.Fatal("force channel is not closed")
	}
	if got := s.Cause(); got.Status != 143 {
		t.Fatalf("force changed cause: %#v", got)
	}
}
