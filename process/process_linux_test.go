//go:build linux

package process

import (
	"os"
	"testing"
)

func TestSignalSelectionHonorsExclusions(t *testing.T) {
	tests := []struct {
		name           string
		noStop, noKill []string
		want           int
	}{
		{"both", nil, nil, SEND_CTRL_C_SIGNAL | SEND_CTRL_BREAK_SIGNAL | SEND_KILL_SIGNAL},
		{"no stop", []string{"worker"}, nil, SEND_KILL_SIGNAL},
		{"no kill", nil, []string{"worker"}, SEND_CTRL_C_SIGNAL | SEND_CTRL_BREAK_SIGNAL},
		{"none", []string{"all"}, []string{"all"}, SEND_NO_SIGNAL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := getSignalsToSend("worker", tt.noStop, tt.noKill)
			if err != nil || got != tt.want {
				t.Fatalf("got %d, %v; want %d", got, err, tt.want)
			}
		})
	}
}

func TestIntSetAddIsAtomic(t *testing.T) {
	var set IntSet
	winners := make(chan bool, 32)
	for i := 0; i < cap(winners); i++ {
		go func() { winners <- set.Add(os.Getpid()) }()
	}
	n := 0
	for i := 0; i < cap(winners); i++ {
		if <-winners {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("winners = %d; want 1", n)
	}
}

func TestForceStopIsIdempotent(t *testing.T) {
	opts := &Options{}
	opts.ForceStop()
	opts.ForceStop()
	select {
	case <-opts.forceStopChannel():
	default:
		t.Fatal("force channel is open")
	}
}
