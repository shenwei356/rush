package main

import (
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

func TestResourceFlagValues(t *testing.T) {
	for _, tt := range []struct {
		value string
		want  uint64
	}{
		{"", 0}, {"0", 0}, {"1024", 1024}, {"1K", 1024}, {"1k", 1000},
		{"1.5M", 1572864}, {"2G", 2 << 30},
		{"1.001k", 1001}, {"0.99999999999999999k", 999}, {"1.9", 1},
		{"9007199254740993", 9007199254740993},
		{"18446744073709551615", math.MaxUint64},
		{"18446744073709551615.5", math.MaxUint64},
		{"1e3K", 1024000}, {"0x1p10", 1024}, {"1_024", 1024},
		{"010", 10}, {"08", 8}, {"-0", 0}, {"0e-9999", 0},
		{"0e99999999999999999999", 0}, {"0x0p99999999999999999999", 0},
	} {
		got, err := parseMemorySize(tt.value)
		if err != nil || got != tt.want {
			t.Errorf("parseMemorySize(%q) = %d, %v; want %d", tt.value, got, err, tt.want)
		}
	}
	for _, value := range []string{"-1G", "abc", "1E", "NaN", "Inf", "18446744073709551616",
		"1e-9999", "-1e-9999", "0.99999999999999999", "0.0009999999999999999999k",
		"1/2K", "0b10", "0x1", "1e10000000",
	} {
		if _, err := parseMemorySize(value); err == nil {
			t.Errorf("parseMemorySize(%q) accepted an invalid size", value)
		}
	}
	for _, tt := range []struct {
		value string
		want  float64
	}{
		{"", 0}, {"2.5", 2.5}, {"100%", 8}, {"50%", 4}, {"0", 0.01},
		{"1e308%", 8e306}, {"-0", 0.01}, {"0e-9999%", 0.01},
	} {
		got, err := parseMaxLoad(tt.value, 8)
		if err != nil || math.IsNaN(got) || math.Abs(got-tt.want) > math.Max(1, math.Abs(tt.want))*1e-15 {
			t.Errorf("parseMaxLoad(%q) = %g, %v; want %g", tt.value, got, err, tt.want)
		}
	}
	for _, value := range []string{"-1", "-1%", "NaN", "Inf", "bad%", "1e-9999", "-1e-9999", "5e-324%"} {
		if _, err := parseMaxLoad(value, 8); err == nil {
			t.Errorf("parseMaxLoad(%q) accepted an invalid load", value)
		}
	}
	if got, err := parseStartDelay(0.25); err != nil || got != 250*time.Millisecond {
		t.Fatalf("parseStartDelay(0.25) = %s, %v", got, err)
	}
	for _, value := range []float64{-1, math.NaN(), math.Inf(1), 1e20, float64(math.MaxInt64) / float64(time.Second)} {
		if _, err := parseStartDelay(value); err == nil {
			t.Errorf("parseStartDelay(%g) accepted an invalid delay", value)
		}
	}
}

func TestLoadPercentageBounds(t *testing.T) {
	for _, test := range []struct {
		value string
		cpus  int
		want  float64
	}{
		{"1e309%", 8, 8e307},
		{"1e-324%", 1000, 1e-323},
	} {
		got, err := parseMaxLoad(test.value, test.cpus)
		if err != nil || got != test.want {
			t.Errorf("parseMaxLoad(%q, %d)=%g, %v; want %g", test.value, test.cpus, got, err, test.want)
		}
	}
	for _, value := range []string{"1e309", "1e308%"} {
		if _, err := parseMaxLoad(value, 1000); err == nil {
			t.Errorf("overflowing load accepted: %s", value)
		}
	}
}

func TestInvalidResourceFlagsPreserveOutput(t *testing.T) {
	for _, test := range []struct{ flag, value string }{
		{"memfree", "1e-9999"}, {"memfree", "-1e-9999"},
		{"memfree", "0.99999999999999999"}, {"memfree", "0.0009999999999999999999k"},
		{"load", "1e-9999"}, {"load", "-1e-9999"}, {"load", "1e-9999%"},
	} {
		t.Run(test.flag+"/"+test.value, func(t *testing.T) {
			output := t.TempDir() + "/output"
			if err := os.WriteFile(output, []byte("preserved"), 0600); err != nil {
				t.Fatal(err)
			}
			// Dry runs must validate thresholds too, without waiting on load.
			stdout, stderr, code := runLegacyRush(t, "x\n", nil, "--dry-run", "-o", output, "--"+test.flag, test.value, "echo executed")
			if code == 0 || stdout != "" || !strings.Contains(stderr, test.flag) {
				t.Errorf("invalid threshold accepted: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			if data, err := os.ReadFile(output); err != nil || string(data) != "preserved" {
				t.Errorf("output changed on invalid threshold: %q, err=%v", data, err)
			}
		})
	}
}

func TestInvalidTimeFlagsRejectedBeforeExecution(t *testing.T) {
	for _, test := range []struct{ flag, value string }{
		{"delay", "9223372036.854776"},
		{"retry-interval", "NaN"},
		{"retry-interval", "+Inf"},
		{"retry-interval", "9223372036.854776"},
		{"timeout", "9223372037"},
		{"cleanup-time", "9223372037"},
	} {
		t.Run(test.flag+"/"+test.value, func(t *testing.T) {
			stdout, stderr, code := runLegacyRush(t, "x\n", nil, "--"+test.flag, test.value, "echo executed")
			if code == 0 || stdout != "" || !strings.Contains(stderr, test.flag) {
				t.Fatalf("invalid duration: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
		})
	}
}
