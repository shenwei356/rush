package process

import (
	"context"
	"regexp"
	"runtime"
	"strings"
)

// NameMatcher matches process names literally or with a compiled expression.
type NameMatcher struct {
	name string         // Literal name, or the expression supplied by the caller.
	re   *regexp.Regexp // Nil selects whole-name matching.
}

// NewNameMatcher compiles an optional expression once for repeated process scans.
// Windows matching ignores case by default; expressions may override it with (?-i).
func NewNameMatcher(name string, useRegexp bool) (*NameMatcher, error) {
	m := &NameMatcher{name: name}
	if useRegexp {
		pattern := name
		if runtime.GOOS == "windows" {
			pattern = "(?i)" + pattern
		}
		var err error
		m.re, err = regexp.Compile(pattern)
		if err != nil {
			return nil, err
		}
	}
	return m, nil
}

// HasProcessName reports whether a visible live process has the exact name.
// The calling process and Unix zombies are excluded.
func HasProcessName(ctx context.Context, name string) (bool, error) {
	return (&NameMatcher{name: name}).Exists(ctx)
}

// matches applies the selected mode to the full process name.
func (m *NameMatcher) matches(actual string) bool {
	if m.re != nil {
		return m.re.MatchString(actual)
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(actual, m.name)
	}
	return actual == m.name
}

// mayMatchPrefix avoids expensive full-name lookups for unrelated literal names.
// A regular expression can match a suffix beyond the truncated kernel name.
func (m *NameMatcher) mayMatchPrefix(prefix string) bool {
	return m.re != nil || strings.HasPrefix(m.name, prefix)
}
