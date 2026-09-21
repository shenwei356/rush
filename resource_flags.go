package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

func parseStartDelay(seconds float64) (time.Duration, error) {
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 || seconds > float64(math.MaxInt64)/float64(time.Second) {
		return 0, fmt.Errorf("invalid --delay value: %g", seconds)
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

func parseMaxLoad(value string, cpus int) (float64, error) {
	if value == "" {
		return 0, nil
	}
	percent := strings.HasSuffix(value, "%")
	text := value
	if percent {
		text = strings.TrimSuffix(value, "%")
	}
	limit, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(limit) || math.IsInf(limit, 0) || limit < 0 {
		return 0, fmt.Errorf("invalid --load value: %q", value)
	}
	if percent {
		limit = limit * float64(cpus) / 100
	}
	if math.IsInf(limit, 0) {
		return 0, fmt.Errorf("invalid --load value: %q", value)
	}
	if limit == 0 {
		limit = 0.01 // GNU Parallel treats --load 0 as 0.01.
	}
	return limit, nil
}

func parseMemorySize(value string) (uint64, error) {
	if value == "" {
		return 0, nil
	}
	multiplier := float64(1)
	number := value
	if len(value) > 1 {
		var power int
		switch value[len(value)-1] {
		case 'K', 'k':
			power = 1
		case 'M', 'm':
			power = 2
		case 'G', 'g':
			power = 3
		case 'T', 't':
			power = 4
		case 'P', 'p':
			power = 5
		}
		if power != 0 {
			number = value[:len(value)-1]
			base := float64(1000)
			if value[len(value)-1] >= 'A' && value[len(value)-1] <= 'Z' {
				base = 1024
			}
			multiplier = math.Pow(base, float64(power))
		}
	}
	size, err := strconv.ParseFloat(number, 64)
	size *= multiplier
	if err != nil || math.IsNaN(size) || math.IsInf(size, 0) || size < 0 || size >= math.Exp2(64) || (size > 0 && size < 1) {
		return 0, fmt.Errorf("invalid --memfree value: %q", value)
	}
	return uint64(size), nil
}
