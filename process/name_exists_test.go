package process

import (
	"runtime"
	"testing"
)

func TestNameMatcher(t *testing.T) {
	for _, test := range []struct {
		pattern, name string
		regexp, want  bool
	}{
		{"worker.exe", "worker.exe", false, true},
		{"worker.exe", "workerXexe", false, false},
		{"worker.exe", "prefixworker.exe", false, false},
		{"winword.exe", "WINWORD.EXE", false, runtime.GOOS == "windows"},
		{"word", "winword.exe", true, true},
		{"word", "WINWORD.EXE", true, runtime.GOOS == "windows"},
		{`^worker[0-9]+\.exe$`, "worker12.exe", true, true},
		{`^worker[0-9]+\.exe$`, "prefixworker12.exe", true, false},
		{`^worker[0-9]+\.exe$`, "worker12Xexe", true, false},
		{`^(worker|helper)$`, "helper", true, true},
		{`(?i)^winword\.exe$`, "WINWORD.EXE", true, true},
		{`(?-i)word`, "WORD", true, false},
	} {
		matcher, err := NewNameMatcher(test.pattern, test.regexp)
		if err != nil {
			t.Fatal(err)
		}
		if got := matcher.matches(test.name); got != test.want {
			t.Errorf("pattern=%q regexp=%t name=%q: got %t, want %t", test.pattern, test.regexp, test.name, got, test.want)
		}
	}
	if _, err := NewNameMatcher("[", true); err == nil {
		t.Fatal("invalid expression accepted")
	}
}
