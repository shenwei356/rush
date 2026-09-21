package process

import (
	"context"
	"fmt"
	"math"
	"os/exec"
	"sync"
	"time"

	"github.com/shenwei356/rush/internal/runstate"
	"github.com/shirou/gopsutil/load"
	"github.com/shirou/gopsutil/mem"
)

var resourcePollInterval = 250 * time.Millisecond
var systemLoad = func() (float64, error) {
	avg, err := load.Avg()
	if err != nil {
		return 0, err
	}
	return avg.Load1, nil
}
var availableMemory = func() (available, total uint64, err error) {
	stats, err := mem.VirtualMemory()
	if err != nil {
		return 0, 0, err
	}
	return stats.Available, stats.Total, nil
}

// startGate serializes actual process starts, not just worker creation.
type startGate struct {
	startMu   sync.Mutex
	lastStart time.Time
	delay     time.Duration
	maxLoad   float64
	minMemory uint64

	activeMu sync.Mutex
	active   []*Command
	ctx      context.Context
	stop     context.CancelFunc
	done     chan struct{}
	state    *runstate.State
}

func newStartGate(parent context.Context, opts *Options, state *runstate.State) *startGate {
	ctx, stop := context.WithCancel(parent)
	g := &startGate{
		delay: opts.StartDelay, maxLoad: opts.MaxLoad, minMemory: opts.MinFreeMemory,
		ctx: ctx, stop: stop, done: make(chan struct{}), state: state,
	}
	if g.minMemory > 0 {
		go g.monitorMemory()
	} else {
		close(g.done)
	}
	return g
}

func (g *startGate) close() {
	g.stop()
	<-g.done
}

func (g *startGate) start(c *Command, cmd *exec.Cmd, controller processController) error {
	g.startMu.Lock()
	defer g.startMu.Unlock()
	for {
		if g.ctx.Err() != nil || isClosed(c.Cancel) {
			return ErrCancelled
		}
		wait := time.Duration(0)
		if g.delay > 0 && !g.lastStart.IsZero() {
			wait = time.Until(g.lastStart.Add(g.delay))
		}
		if wait <= 0 {
			if g.maxLoad > 0 {
				current, err := systemLoad()
				if err != nil {
					g.state.Stop(runstate.Cause{Kind: runstate.Internal, Status: 1})
					return fmt.Errorf("read system load: %w", err)
				}
				if math.IsNaN(current) || math.IsInf(current, 0) || current < 0 {
					g.state.Stop(runstate.Cause{Kind: runstate.Internal, Status: 1})
					return fmt.Errorf("invalid system load: %g", current)
				}
				if current >= g.maxLoad {
					wait = resourcePollInterval
				}
			}
			if g.minMemory > 0 {
				available, total, err := availableMemory()
				if err != nil {
					g.state.Stop(runstate.Cause{Kind: runstate.Internal, Status: 1})
					return fmt.Errorf("read available memory: %w", err)
				}
				if g.minMemory > total {
					g.state.Stop(runstate.Cause{Kind: runstate.Internal, Status: 1})
					return fmt.Errorf("--memfree %d bytes exceeds physical memory %d bytes", g.minMemory, total)
				}
				if available < g.minMemory {
					wait = resourcePollInterval
				}
			}
		}
		if wait <= 0 {
			break
		}
		if wait > resourcePollInterval {
			wait = resourcePollInterval
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-g.ctx.Done():
			timer.Stop()
			return ErrCancelled
		case <-c.Cancel:
			timer.Stop()
			return ErrCancelled
		}
	}
	if g.ctx.Err() != nil || isClosed(c.Cancel) {
		return ErrCancelled
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start cmd #%d: %s: %w", c.ID, c.Cmd, err)
	}
	g.lastStart = time.Now()
	if err := controller.Started(cmd); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("register cmd #%d: %w", c.ID, err)
	}
	if g.minMemory > 0 {
		g.activeMu.Lock()
		g.active = append(g.active, c)
		g.activeMu.Unlock()
	}
	return nil
}

func (g *startGate) finished(c *Command) {
	if g.minMemory == 0 {
		return
	}
	g.activeMu.Lock()
	for i, active := range g.active {
		if active == c {
			copy(g.active[i:], g.active[i+1:])
			g.active[len(g.active)-1] = nil
			g.active = g.active[:len(g.active)-1]
			break
		}
	}
	g.activeMu.Unlock()
}

func (g *startGate) monitorMemory() {
	defer close(g.done)
	ticker := time.NewTicker(resourcePollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-g.ctx.Done():
			return
		case <-ticker.C:
		}
		available, _, err := availableMemory()
		if err != nil {
			Log.Error(fmt.Errorf("monitor available memory: %w", err))
			g.state.Stop(runstate.Cause{Kind: runstate.Internal, Status: 1})
			return
		}
		if available >= g.minMemory/2+g.minMemory%2 {
			continue
		}
		g.activeMu.Lock()
		for i := len(g.active) - 1; i >= 0; i-- {
			if g.active[i].stopForMemory() {
				break
			}
		}
		g.activeMu.Unlock()
	}
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
