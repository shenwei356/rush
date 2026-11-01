package main

import (
	"fmt"
	"regexp"
	"testing"
)

func TestETAJobCounts(t *testing.T) {
	counts := regexp.MustCompile(`\(([0-9]+)/([0-9]+)(?:,|\))`)
	for _, test := range []struct {
		name, input, command, output string
		args                         []string
		jobs                         int
	}{
		{"empty records", "a\n\n\nb\n", "echo {1}", "a", []string{"-n", "2"}, 1},
		{"only empty records", "\n\n", "echo {}", "", nil, 0},
		{"empty input", "", "echo {}", "", nil, 0},
		{"trimmed empty batch", " \t\n", "echo {}", "", []string{"-T", "b"}, 0},
		{"three trimmed empty records", " \n \n \n", "echo {#}", "", []string{"-T", "b", "-n", "3"}, 0},
		{"trimmed empty custom join", " \n \n", "echo {#}", "", []string{"-T", "b", "-n", "2", "-J", "|"}, 0},
		{"trimmed empty partial batch", " \n", "echo {}", "", []string{"-T", "b", "-n", "2"}, 0},
		{"mixed trim", " \na\n", "echo {}", "a", []string{"-T", "b"}, 1},
		{"mixed partial trim", "a\nb\n \n", "echo {1}", "a", []string{"-T", "b", "-n", "2"}, 1},
		{"empty expansion", "a\n", "{^a}", "", nil, 0},
		{"constant command", " \n", "echo fixed", "fixed", []string{"-T", "b"}, 1},
		{"partial batch", "a\nb\nc\n", "echo {1}", "a\nc", []string{"-n", "2"}, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := append([]string{"--eta", "-j", "1", "-k"}, test.args...)
			stdout, stderr, code := runLegacyRush(t, test.input, nil, append(args, test.command)...)
			if code != 0 || normalizedLines(stdout) != test.output {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			matches := counts.FindAllStringSubmatch(stderr, -1)
			want := fmt.Sprint(test.jobs)
			if len(matches) == 0 || matches[len(matches)-1][1] != want || matches[len(matches)-1][2] != want {
				t.Fatalf("want final count %s/%s; stderr=%q", want, want, stderr)
			}
		})
	}
}
