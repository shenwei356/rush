package main

import (
	"math"
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
	} {
		got, err := parseMemorySize(tt.value)
		if err != nil || got != tt.want {
			t.Errorf("parseMemorySize(%q) = %d, %v; want %d", tt.value, got, err, tt.want)
		}
	}
	for _, value := range []string{"-1G", "abc", "1E", "NaN", "Inf", "18446744073709551616"} {
		if _, err := parseMemorySize(value); err == nil {
			t.Errorf("parseMemorySize(%q) accepted an invalid size", value)
		}
	}
	for _, tt := range []struct {
		value string
		want  float64
	}{
		{"", 0}, {"2.5", 2.5}, {"100%", 8}, {"50%", 4}, {"0", 0.01},
	} {
		got, err := parseMaxLoad(tt.value, 8)
		if err != nil || got != tt.want {
			t.Errorf("parseMaxLoad(%q) = %g, %v; want %g", tt.value, got, err, tt.want)
		}
	}
	for _, value := range []string{"-1", "-1%", "NaN", "Inf", "bad%"} {
		if _, err := parseMaxLoad(value, 8); err == nil {
			t.Errorf("parseMaxLoad(%q) accepted an invalid load", value)
		}
	}
	if got, err := parseStartDelay(0.25); err != nil || got != 250*time.Millisecond {
		t.Fatalf("parseStartDelay(0.25) = %s, %v", got, err)
	}
	for _, value := range []float64{-1, math.NaN(), math.Inf(1), 1e20} {
		if _, err := parseStartDelay(value); err == nil {
			t.Errorf("parseStartDelay(%g) accepted an invalid delay", value)
		}
	}
}
