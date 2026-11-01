package main

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"
)

func parseStartDelay(seconds float64) (time.Duration, error) {
	return parseSeconds("delay", seconds)
}

// parseSeconds rejects nonfinite values and overflow before converting to nanoseconds.
func parseSeconds(flag string, seconds float64) (time.Duration, error) {
	nanoseconds := seconds * float64(time.Second)
	// float64(MaxInt64) rounds up to 2^63, which is already out of range.
	if math.IsNaN(seconds) || seconds < 0 || nanoseconds >= float64(math.MaxInt64) {
		return 0, fmt.Errorf("invalid --%s value: %g", flag, seconds)
	}
	return time.Duration(nanoseconds), nil
}

// parseWholeSeconds preserves integer seconds exactly while checking overflow.
func parseWholeSeconds(flag string, seconds int) (time.Duration, error) {
	if seconds < 0 || int64(seconds) > math.MaxInt64/int64(time.Second) {
		return 0, fmt.Errorf("invalid --%s value: %d", flag, seconds)
	}
	return time.Duration(seconds) * time.Second, nil
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
	exact, ok := parseResourceNumber(text)
	if !ok || exact.Sign() < 0 {
		return 0, fmt.Errorf("invalid --load value: %q", value)
	}
	if exact.Sign() == 0 {
		// Only an actual zero selects the special 0.01 threshold. A nonzero
		// value rounded to zero must not silently weaken the requested limit.
		return 0.01, nil // GNU Parallel treats --load 0 as 0.01.
	}
	if percent {
		// Apply the percentage before rounding to avoid intermediate overflow
		// or underflow when the final threshold is representable.
		exact.Mul(exact, big.NewRat(int64(cpus), 100))
	}
	limit, _ := exact.Float64()
	if math.IsInf(limit, 0) || limit <= 0 {
		return 0, fmt.Errorf("invalid --load value: %q", value)
	}
	return limit, nil
}

func parseMemorySize(value string) (uint64, error) {
	if value == "" {
		return 0, nil
	}
	multiplier := int64(1)
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
			base := int64(1000)
			if value[len(value)-1] >= 'A' && value[len(value)-1] <= 'Z' {
				base = 1024
			}
			for range power {
				multiplier *= base
			}
		}
	}
	// Convert bytes exactly: float64 can round across integer boundaries.
	size, ok := parseResourceNumber(number)
	if !ok || size.Sign() < 0 {
		return 0, fmt.Errorf("invalid --memfree value: %q", value)
	}
	size.Mul(size, big.NewRat(multiplier, 1))
	if size.Sign() > 0 && size.Cmp(big.NewRat(1, 1)) < 0 {
		return 0, fmt.Errorf("invalid --memfree value: %q", value)
	}
	// Preserve truncation of fractional bytes, after validating the exact size.
	bytes := new(big.Int).Quo(size.Num(), size.Denom())
	if !bytes.IsUint64() {
		return 0, fmt.Errorf("invalid --memfree value: %q", value)
	}
	return bytes.Uint64(), nil
}

// parseResourceNumber retains float-literal syntax while parsing exactly.
// Rat alone also accepts fractions and other bases. Float range errors are
// deferred until the caller has applied units and checked the final bounds.
func parseResourceNumber(text string) (*big.Rat, bool) {
	approx, err := strconv.ParseFloat(text, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return nil, false
	}
	if approx == 0 {
		// A zero mantissa remains zero even if its exponent exceeds Rat's
		// parser bounds. Distinguish it from a nonzero mantissa that underflowed.
		mantissa := text
		if i := strings.IndexAny(mantissa, "pP"); i >= 0 {
			mantissa = mantissa[:i]
		} else if i := strings.IndexAny(mantissa, "eE"); i >= 0 {
			mantissa = mantissa[:i]
		}
		if strings.Trim(mantissa, "+-0._xX") == "" {
			return new(big.Rat), true
		}
	}
	return new(big.Rat).SetString(text)
}
