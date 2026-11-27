package process

import (
	"bufio"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/shenwei356/rush/internal/runstate"
)

func TestAlreadyClosedCancelChannel(t *testing.T) {
	cancel := make(chan struct{})
	close(cancel)
	ctx, stop := contextFromCancel(cancel)
	defer stop()
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("scheduler received a live context after cancellation")
	}
}

func TestForceStopDuringStartup(t *testing.T) {
	for _, when := range []string{"before", "concurrent"} {
		t.Run(when, func(t *testing.T) {
			opts := &Options{DryRun: true, PropExitStatus: true}
			applied := make(chan struct{})
			go func() {
				opts.ForceStop()
				close(applied)
			}()
			if when == "before" {
				<-applied
			}
			observeRun[string](t, opts)
			<-applied
			if !isClosed(opts.forceStopChannel()) {
				t.Fatal("scheduler lost force request received during startup")
			}
		})
	}
}

func TestNegativeRetriesRejectRun(t *testing.T) {
	opts := &Options{DryRun: true, Retries: -1, PropExitStatus: true}
	got := observeRun(t, opts, "echo queued")
	if got.cause.Kind != runstate.Internal || got.cause.Status != 1 || len(got.statuses) != 0 {
		t.Fatalf("cause=%+v statuses=%v; want setup failure before scheduling", got.cause, got.statuses)
	}
}

func TestOutputWithoutProgressBar(t *testing.T) {
	input := make(chan string, 1)
	input <- "echo preview"
	close(input)
	out, _, done, _ := Run4OutputContext(&Options{DryRun: true, ETA: true}, context.Background(), nil, input)
	var text strings.Builder
	for s := range out {
		text.WriteString(s)
	}
	<-done
	if text.String() != "echo preview\n" {
		t.Fatalf("preview output=%q", text.String())
	}
}

func TestBufferedOutputWithNonpositiveChunkSize(t *testing.T) {
	old := OutputChunkSize
	t.Cleanup(func() { OutputChunkSize = old })
	for _, size := range []int{-1, 0} {
		OutputChunkSize = size
		cmd := &Command{reader: bufio.NewReader(strings.NewReader("result"))}
		output, done := make(chan string, 1), make(chan error, 1)
		cmd.sendOutput(output, done, nil)
		if got, err := <-output, <-done; got != "result" || err != nil {
			t.Fatalf("chunk size %d: output=%q err=%v", size, got, err)
		}
	}
}
