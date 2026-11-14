package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/shenwei356/rush/process"
)

// processWait describes a one-time condition checked before opening run files.
type processWait struct {
	name     string               // Process name or expression, used in diagnostics.
	matcher  *process.NameMatcher // Compiled once before opening any run files.
	present  bool                 // Wait for any match when true, or no matches otherwise.
	duration time.Duration        // Minimum uninterrupted time satisfying the condition.
	interval time.Duration        // Pause between scans; scans never overlap.
}

// parseProcessWait rejects ambiguous modes and invalid values before file setup.
func parseProcessWait(appear, disappear string, seconds, intervalSeconds float64, durationSet, intervalSet, useRegexp bool) (processWait, error) {
	duration, err := parseSeconds("wait-for-process-duration", seconds)
	if err != nil {
		return processWait{}, err
	}
	interval, err := parseSeconds("wait-for-process-interval", intervalSeconds)
	if err != nil {
		return processWait{}, err
	}
	if interval <= 0 {
		return processWait{}, fmt.Errorf("invalid --wait-for-process-interval value: %g (must be at least one nanosecond)", intervalSeconds)
	}
	if appear != "" && disappear != "" {
		return processWait{}, fmt.Errorf("--wait-for-process and --wait-for-process-exit are mutually exclusive")
	}
	name := appear
	if name == "" {
		name = disappear
	}
	if name == "" && durationSet {
		return processWait{}, fmt.Errorf("--wait-for-process-duration requires --wait-for-process or --wait-for-process-exit")
	}
	if name == "" && intervalSet {
		return processWait{}, fmt.Errorf("--wait-for-process-interval requires --wait-for-process or --wait-for-process-exit")
	}
	if name == "" && useRegexp {
		return processWait{}, fmt.Errorf("--wait-for-process-regexp requires --wait-for-process or --wait-for-process-exit")
	}
	if !useRegexp && (strings.ContainsAny(name, "/\\\x00\r\n") || (name != "" && strings.TrimSpace(name) == "")) {
		return processWait{}, fmt.Errorf("invalid process name %q: use a name without a path or arguments", name)
	}
	matcher, err := process.NewNameMatcher(name, useRegexp)
	if err != nil {
		return processWait{}, fmt.Errorf("invalid process name expression %q: %w", name, err)
	}
	return processWait{name: name, matcher: matcher, present: appear != "", duration: duration, interval: interval}, nil
}

// waitForProcess checks immediately, then pauses between sequential scans.
// Any contrary observation resets the clock; changes between scans can be missed.
// probe is supplied explicitly so tests can reproduce restarts and lookup errors.
func waitForProcess(ctx context.Context, condition processWait, probe func(context.Context) (bool, error)) error {
	var since time.Time
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		present, err := probe(ctx)
		if err != nil {
			return fmt.Errorf("inspect processes: %w", err)
		}
		now := time.Now()
		if present == condition.present {
			if since.IsZero() {
				since = now
			}
			if now.Sub(since) >= condition.duration {
				return ctx.Err()
			}
		} else {
			since = time.Time{}
		}
		pause := condition.interval
		if !since.IsZero() {
			pause = min(pause, condition.duration-now.Sub(since))
		}
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// waitBeforeRun keeps interrupts responsive before the job signal monitor exists.
func waitBeforeRun(condition processWait, verbose bool) (int, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, terminationSignals()...)
	defer signal.Stop(signals)
	done := make(chan error, 1)
	go func() {
		if verbose {
			state := "disappear"
			if condition.present {
				state = "appear"
			}
			log.Infof("waiting for process %q to %s for %s", condition.name, state, condition.duration)
		}
		done <- waitForProcess(ctx, condition, condition.matcher.Exists)
	}()
	select {
	case sig := <-signals:
		return terminationStatus(sig), nil
	case err := <-done:
		// Prefer an already queued interrupt to releasing the jobs.
		select {
		case sig := <-signals:
			return terminationStatus(sig), nil
		default:
		}
		return 0, err
	}
}
