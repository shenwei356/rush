package main

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// FuzzContinueExpansionMatchesExecution checks that metadata tracking never
// changes the command parsed by normal execution, including composed macros.
func FuzzContinueExpansionMatchesExecution(f *testing.F) {
	for _, seed := range []struct{ command, input, variable string }{
		{"echo {#}:{?}:{}", "alpha beta", "unused"},
		{"echo {{#}{p}", "alpha beta", "}"},
		{"echo {#}:{@({#}){p}", "12", "}"},
		{"echo {p}", "a{1}c", "{%}:{?}"},
		{"echo {{#}}:{{?}}", "x", "}"},
	} {
		f.Add(seed.command, seed.input, seed.variable, uint8(11))
	}
	delimiter := regexp.MustCompile(`\s+`)
	f.Fuzz(func(t *testing.T, command, input, variable string, job uint8) {
		// NUL is reserved for metadata and cannot occur in an executed command.
		for _, value := range []string{command, input, variable} {
			if len(value) > 2048 || strings.ContainsRune(value, '\x00') {
				t.Skip()
			}
		}
		config := Config{
			Jobs: int(job)%8 + 1, Greedy: true, GreedyCount: 1, EscapeCurlyBrackets: true,
			reFieldDelimiter: delimiter, AssignMap: map[string]string{"p": variable, "q": "}"},
		}
		chunk := Chunk{ID: uint64(job) + 1, Data: []string{input}}
		actual, actualErr := fillCommand(config, command, chunk, 0)
		record, recordErr := fillCommandForContinue(config, command, chunk, 0)
		if actualErr != nil || recordErr != nil {
			if actualErr == nil || recordErr == nil || actualErr.Error() != recordErr.Error() {
				t.Fatalf("different expansion errors: execution=%v record=%v", actualErr, recordErr)
			}
			return
		}
		restored := strings.TrimPrefix(strings.TrimPrefix(record, continueComposedCommandPrefix), continueCommandPrefix)
		restored = strings.ReplaceAll(restored, continueJobIDMarker, strconv.FormatUint(chunk.ID, 10))
		restored = strings.ReplaceAll(restored, continueThreadsMarker, strconv.Itoa(max(runtime.NumCPU()/config.Jobs, 1)))
		if restored != actual {
			t.Fatalf("record changed execution: actual=%q restored=%q record=%q", actual, restored, record)
		}
	})
}

func TestInputReplacementsPreserveBraces(t *testing.T) {
	for _, test := range []struct {
		name, input, delimiter, command, want string
	}{
		{"whole input", "a{1}c", `\s+`, "echo {}", "echo a{1}c"},
		{"first CSV field", `{"a":1,"b":2}`, ",", "echo {1}", `echo {"a":1`},
		{"last CSV field", `{"a":1,"b":2}`, ",", "echo {2}", `echo "b":2}`},
		{"brace delimiter", "a{b}c", `\{`, "echo {2}", "echo b}c"},
		{"balanced field", "a{1}c rest", `\s+`, "echo {1}", "echo a{1}c"},
		{"basename", "dir{part/file}name", `\s+`, "echo {%}", "echo file}name"},
		{"dirname", "dir{part/file}name", `\s+`, "echo {/}", "echo dir{part"},
		{"regexp width", "a{b}c", `\s+`, "echo {@(....)}", "echo a{b}"},
		{"regexp capture", "a{b}c", `\s+`, `echo {@\x7b(.)}`, "echo b"},
		{"macro whole input", "a{1}c", `\s+`, "{whole}", "echo a{1}c"},
		{"macro field", "a{1}c rest", `\s+`, "{field}", "echo a{1}c"},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := Config{
				Jobs: 1, Greedy: true, GreedyCount: 1, EscapeCurlyBrackets: true,
				reFieldDelimiter: regexp.MustCompile(test.delimiter),
				AssignMap:        map[string]string{"whole": "echo {}", "field": "echo {1}"},
			}
			for _, fill := range []func(Config, string, Chunk, int) (string, error){fillCommand, fillCommandForContinue} {
				got, err := fill(config, test.command, Chunk{ID: 7, Data: []string{test.input}}, 1)
				if err != nil || got != test.want {
					t.Errorf("replacement=%q, err=%v; want %q", got, err, test.want)
				}
			}
		})
	}
}

func TestEscapeSymbols(t *testing.T) {
	for _, test := range []struct{ input, symbols, want string }{
		{"a$\\", "$\\", "a\\$\\\\"},
		{"é$\xff", "é$", "\\é\\$\xff"},
		{"$\xff", "$$", "\\$\xff"},
		{"raw\xff", "", "raw\xff"},
	} {
		if got := escapeSymbols(test.input, test.symbols); got != test.want {
			t.Errorf("input=%q symbols=%q: got %q; want %q", test.input, test.symbols, got, test.want)
		}
	}
}

func TestEscapedInputPreservesBytes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows command lines use UTF-16")
	}
	input := "file\xff.txt\n"
	args := []string{"-q", "-c", "-C", t.TempDir() + "/records", `printf '%s\n' '{}'`}
	for _, want := range []string{input, ""} {
		stdout, stderr, code := runLegacyRush(t, input, nil, args...)
		if code != 0 || stdout != want {
			t.Fatalf("exit=%d stdout=%q stderr=%q; want %q", code, stdout, stderr, want)
		}
	}
}

func TestContinueTracksJobValuesThroughMacroPasses(t *testing.T) {
	for _, test := range []struct {
		name, command, close, actual, key string
		id                                uint64
	}{
		{"field", "echo {{#}{p}", "}", "echo alpha", "echo alpha", 1},
		{"regexp", "echo {#}:{@({#}){p}", "}", "echo 1:1", "echo " + continueJobIDMarker + ":1", 1},
		{"variable", "echo {v{#}{p}", "}", "echo assigned", "echo assigned", 1},
		{"missing field", "echo {{#}{p}", "}", "echo {9}", "echo {" + continueJobIDMarker + "}", 9},
		{"unsupported", "echo {{#}{p}", "?}", "echo {1?}", "echo {" + continueJobIDMarker + "?}", 1},
		{"missing regexp", "echo {{#}{p}", "@}", "echo {1@}", "echo {" + continueJobIDMarker + "@}", 1},
		{"escaped braces", "echo {{{#}{p}", "}}", "echo {1}", "echo {" + continueJobIDMarker + "}", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := Config{Jobs: 1, Greedy: true, GreedyCount: 1, EscapeCurlyBrackets: true,
				reFieldDelimiter: regexp.MustCompile(`\s+`), AssignMap: map[string]string{"p": test.close, "v1": "assigned"}}
			input := "alpha"
			if test.name == "regexp" {
				input = "12"
			}
			chunk := Chunk{ID: test.id, Data: []string{input}}
			actual, err := fillCommand(config, test.command, chunk, 1)
			if err != nil || actual != test.actual {
				t.Fatalf("actual=%q, err=%v; want %q", actual, err, test.actual)
			}
			key, err := fillCommandForContinue(config, test.command, chunk, 1)
			if err != nil || key != continueComposedCommandPrefix+test.key {
				t.Fatalf("key=%q, err=%v; want %q", key, err, continueComposedCommandPrefix+test.key)
			}
		})
	}
}

func TestComposedContinueKeyEscaping(t *testing.T) {
	config := Config{Jobs: 1, Greedy: true, GreedyCount: 1, EscapeCurlyBrackets: true,
		reFieldDelimiter: regexp.MustCompile(`\s+`), AssignMap: map[string]string{"p": "}"}}
	key, err := fillCommandForContinue(config, "echo {#}:{@({#}){p}", Chunk{ID: 1, Data: []string{"12"}}, 1)
	if err != nil {
		t.Fatal(err)
	}
	// Escaping applies to the evaluated operand, but not to the record prefix
	// or the surviving job-ID marker, even when their bytes are escape symbols.
	got := escapeContinueCommand(key, "\x00r1")
	want := continueComposedCommandPrefix + "echo " + continueJobIDMarker + ":\\1"
	want += continueDigitEscapePrefix + "1\x00"
	if got != want {
		t.Fatalf("escaped key=%q; want %q", got, want)
	}
}

func TestAdjacentInputBracesRemainLiteral(t *testing.T) {
	for _, test := range []struct {
		name, input, command, want string
	}{
		{"opening brace", "{", "echo {}1}", "echo {1}"},
		{"closing brace", "}", "echo {p{}", "echo {p}"},
		{"split braces", "{,}", "echo {1}p{2}", "echo {p}"},
		{"beside macro", "{,}", "echo {1}{p}{2}", "echo {changed}"},
		{"beside deferred macro", "{,}", "echo {1}{deferred}{2}", "echo {7}"},
		{"beside escaped template", "{,}", "echo {1}{{p}}{2}", "echo {{p}}"},
		{"multiple macros", "{,}", "echo {1}{p}{2} {p} {1}1{2}", "echo {changed} changed {1}"},
		{"literal job ID", "{,}", "echo {1}#{2}", "echo {#}"},
		{"literal threads", "{,}", "echo {1}?{2}", "echo {?}"},
		{"binary input", "\x00{", "echo {}1}", "echo \x00{1}"},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := Config{
				Jobs: 1, Greedy: true, GreedyCount: 1, EscapeCurlyBrackets: true,
				reFieldDelimiter: regexp.MustCompile(","), AssignMap: map[string]string{"p": "changed", "deferred": "{#}"},
			}
			for _, passes := range []int{1, 3} {
				config.GreedyCount = passes
				got, err := fillCommand(config, test.command, Chunk{ID: 7, Data: []string{test.input}}, 1)
				if err != nil || got != test.want {
					t.Errorf("passes=%d: replacement=%q, err=%v; want %q", passes, got, err, test.want)
				}
			}
		})
	}
}

func TestJobPlaceholdersRejectCombinedOperations(t *testing.T) {
	config := Config{Jobs: 1, Greedy: true, GreedyCount: 1, reFieldDelimiter: regexp.MustCompile(`\s+`), AssignMap: map[string]string{"p": "file.txt"}}
	chunk := Chunk{ID: 7, Data: []string{"data"}}
	for _, expression := range []string{"{?^1}", "{?^2}", "{?@([0-9])}", "{?@(}", "{?/}", "{?.?}", "{?#}", "{?x}", "{1?}", "{2?^1}", "{#^1}", "{1#}", "{.#}", "{p?}", "{p.x}", "{missing}"} {
		t.Run(expression, func(t *testing.T) {
			for _, fill := range []func(Config, string, Chunk, int) (string, error){fillCommand, fillCommandForContinue} {
				got, err := fill(config, "echo "+expression, chunk, 1)
				if err != nil || got != "echo "+expression {
					t.Errorf("got=%q err=%v; want unchanged expression", got, err)
				}
			}
		})
	}
	for _, expression := range []string{"{?}", "{#}"} {
		key, err := fillCommandForContinue(config, "echo "+expression, chunk, 1)
		if err != nil || !strings.HasPrefix(key, continueCommandPrefix) {
			t.Fatalf("key=%q err=%v", key, err)
		}
		want := key + continueDigitEscapePrefix + "0123456789\x00"
		if escaped := escapeContinueCommand(key, "R0123456789"); escaped != want {
			t.Errorf("escape identity=%q; want %q", escaped, want)
		}
		changed := config
		changed.Jobs = runtime.NumCPU() + 1
		other, err := fillCommandForContinue(changed, "echo "+expression, Chunk{ID: 99, Data: chunk.Data}, 1)
		if err != nil || other != key {
			t.Errorf("unstable key=%q err=%v; want %q", other, err, key)
		}
	}
	for expression, want := range map[string]string{"{^?}": "file", "{@(file)?}": "file"} {
		got, err := fillCommand(config, expression, Chunk{ID: 1, Data: []string{"file?"}}, 1)
		if err != nil || got != want {
			t.Errorf("valid operation %s: got=%q err=%v", expression, got, err)
		}
	}
}

func TestReplacementBatchEmptiness(t *testing.T) {
	for _, test := range []struct {
		name, trim, separator, command, want string
		records                              []string
	}{
		{"newline data", "", "\n", "[{}]", "[\n]", []string{"\n"}},
		{"carriage return data", "", "\n", "[{}]", "[\r]", []string{"\r"}},
		{"CRLF data", "", "\n", "[{}]", "[\r\n]", []string{"\r\n"}},
		{"three trimmed records", "b", "\n", "[{}]", "", []string{" ", "\t", " \t "}},
		{"empty with custom separator", "b", "|", "[{}]", "", []string{" ", "\t"}},
		{"mixed contents", "b", "|", "[{}]", "[|a|]", []string{" ", " a ", "\t"}},
		{"constant command", "b", "\n", "fixed", "fixed", []string{" ", "\t"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := Config{Jobs: 1, Trim: test.trim, RecordsJoinSeparator: test.separator, reFieldDelimiter: regexp.MustCompile(`\s+`)}
			chunk := Chunk{ID: 1, Data: test.records}
			for _, fill := range []func(Config, string, Chunk, int) (string, error){fillCommand, fillCommandForContinue} {
				got, err := fill(config, test.command, chunk, 1)
				if err != nil || got != test.want {
					t.Fatalf("replacement=%q, err=%v; want %q", got, err, test.want)
				}
			}
		})
	}
}

func TestSuffixReplacementOnlyMatchesTheEnd(t *testing.T) {
	config := Config{Jobs: 1, reFieldDelimiter: regexp.MustCompile(`\s+`), AssignMap: map[string]string{"p": "sample.txt.bak"}}
	for _, test := range []struct {
		name, input, expression, want string
	}{
		{"middle", "sample.txt.bak", "{^.txt}", "sample.txt.bak"},
		{"end", "sample.txt", "{^.txt}", "sample"},
		{"repeated", "sample.txt.bak.txt", "{^.txt}", "sample.txt.bak"},
		{"directory", "dir.txt/sample", "{^.txt}", "dir.txt/sample"},
		{"unicode", "样本片段尾", "{^片段}", "样本片段尾"},
		{"absent", "sample.bin", "{^.txt}", "sample.bin"},
		{"empty suffix", "sample.txt", "{^}", "sample.txt"},
		{"entire input", "txt", "{^txt}", ""},
		{"chained", "dir/sample_1.fq.gz", "{%:^_1}", "sample"},
		{"field", "one sample.txt.bak", "{2^.txt}", "sample.txt.bak"},
		{"variable", "ignored", "{p^.txt}", "sample.txt.bak"},
	} {
		t.Run(test.name, func(t *testing.T) {
			chunk := Chunk{ID: 1, Data: []string{test.input}}
			for _, fill := range []func(Config, string, Chunk, int) (string, error){fillCommand, fillCommandForContinue} {
				got, err := fill(config, test.expression, chunk, 1)
				if err != nil || got != test.want {
					t.Fatalf("replacement=%q, err=%v; want %q", got, err, test.want)
				}
			}
		})
	}
}

func TestPathReplacementsPreserveLeadingDots(t *testing.T) {
	config := Config{Jobs: 1, reFieldDelimiter: regexp.MustCompile(`\s+`)}
	for _, test := range []struct{ name, last, all string }{
		{".sample.fq.gz", ".sample.fq", ".sample"},
		{"..sample.fq.gz", "..sample.fq", "..sample"},
		{".sample", ".sample", ".sample"},
		{"..sample", "..sample", "..sample"},
		{".", ".", "."}, {"..", "..", ".."}, {"...", "...", "..."},
		{".sample.", ".sample", ".sample"},
		{".sample..gz", ".sample.", ".sample"},
		{"sample.fq.gz", "sample.fq", "sample"},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, prefix := range []string{"", "dir.v1/"} {
				input := prefix + test.name
				for expression, want := range map[string]string{"{.}": prefix + test.last, "{:}": prefix + test.all} {
					for _, fill := range []func(Config, string, Chunk, int) (string, error){fillCommand, fillCommandForContinue} {
						got, err := fill(config, expression, Chunk{ID: 1, Data: []string{input}}, 1)
						if err != nil || got != want {
							t.Errorf("%s %s -> %q, err=%v; want %q", input, expression, got, err, want)
						}
					}
				}
			}
		})
	}
}

func TestPathReplacementsPreserveDirectorySpelling(t *testing.T) {
	config := Config{Jobs: 1, reFieldDelimiter: regexp.MustCompile(`\s+`)}
	type pathCase struct {
		input, expression, want string
	}
	cases := []pathCase{
		{"link/../file.tar.gz", "{.}", "link/../file.tar"},
		{"link/../file.tar.gz", "{:}", "link/../file"},
		{"link/../file.tar.gz", "{/}", "link/.."},
		{"./dir//file.txt", "{.}", "./dir//file"},
		{"link/../file", "{:}", "link/../file"},
		{"link/..", "{.}", "link/.."},
		{"link/.", "{:}", "link/."},
		{"dir.txt/", "{.}", "dir.txt/"},
		{"dir.txt/", "{:}", "dir.txt/"},
		{"/file.txt", "{/}", "/"},
		{"dir/subdir/", "{/}", "dir"},
		{"dir/subdir///", "{/}", "dir"},
		{"name/", "{/}", "."},
		{"./dir/link/../", "{/}", "./dir/link"},
		{"/", "{/}", "/"},
		{"///", "{/}", "///"},
	}
	if runtime.GOOS == "windows" {
		cases = append(cases, []pathCase{
			{`C:dir\..\file.txt`, "{.}", `C:dir\..\file`},
			{`C:dir\..\file.txt`, "{/}", `C:dir\..`},
			{`C:file.txt`, "{/}", `C:.`},
			{`C:/file.txt`, "{/}", `C:/`},
			{`C:dir\`, "{/}", `C:.`},
			{`C:\dir\subdir\`, "{/}", `C:\dir`},
			{`C:\`, "{/}", `C:\`},
			{`\\server\share\dir\`, "{/}", `\\server\share\`},
			{`\\server\share\dir\..\file.txt`, "{.}", `\\server\share\dir\..\file`},
			{`\\server\share\file.txt`, "{/}", `\\server\share\`},
		}...)
	} else {
		cases = append(cases, pathCase{`dir\file.txt`, "{.}", `dir\file`})
	}
	for _, test := range cases {
		t.Run(test.input+test.expression, func(t *testing.T) {
			got, err := fillCommand(config, test.expression, Chunk{ID: 1, Data: []string{test.input}}, 1)
			if err != nil || got != test.want {
				t.Fatalf("replacement=%q, err=%v; want %q", got, err, test.want)
			}
		})
	}
}

func TestPathReplacementsThroughSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	child := filepath.Join(target, "child")
	if err := os.MkdirAll(child, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(child, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("creating a directory symlink is unavailable: %v", err)
		}
		t.Fatal(err)
	}
	for folder, content := range map[string]string{root: "wrong", target: "right"} {
		for _, name := range []string{"sample", "sample.txt"} {
			if err := os.WriteFile(filepath.Join(folder, name), []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Join would itself erase the path components this regression must exercise.
	parent := link + string(os.PathSeparator) + ".." + string(os.PathSeparator)
	input := parent + "sample.txt.gz"
	// Use native filesystem resolution of the original directory as the reference.
	want, err := os.ReadFile(parent + "sample")
	if err != nil {
		t.Fatal(err)
	}
	config := Config{Jobs: 1, reFieldDelimiter: regexp.MustCompile(`\s+`)}
	for _, expression := range []string{"{.}", "{:}", "{/}"} {
		path, err := fillCommand(config, expression, Chunk{ID: 1, Data: []string{input}}, 1)
		if err != nil {
			t.Fatal(err)
		}
		if expression == "{/}" {
			path += string(os.PathSeparator) + "sample"
		}
		data, err := os.ReadFile(path)
		if err != nil || string(data) != string(want) {
			t.Errorf("%s resolved to %q: content=%q, err=%v; want %q", expression, path, data, err, want)
		}
	}
}

func TestMissingFieldPreservesReplacementExpression(t *testing.T) {
	config := Config{
		Jobs: 1, Greedy: true, GreedyCount: 1, EscapeCurlyBrackets: true,
		reFieldDelimiter: regexp.MustCompile(`\s+`),
	}
	for _, expression := range []string{
		"{2}", "{2.}", "{2:}", "{2/}", "{2%}", "{2%.}", "{2^x}",
		"{2@(2)}", "{999999999999999999999999999.}",
	} {
		t.Run(expression, func(t *testing.T) {
			command := "before " + expression + " after {1}"
			chunk := Chunk{ID: 1, Data: []string{"only.txt"}}
			want := "before " + expression + " after only.txt"
			got, err := fillCommand(config, command, chunk, 1)
			if err != nil || got != want {
				t.Fatalf("replacement=%q, err=%v; want %q", got, err, want)
			}
			recorded, err := fillCommandForContinue(config, command, chunk, 1)
			if err != nil || recorded != want {
				t.Fatalf("continue replacement=%q, err=%v; want %q", recorded, err, want)
			}
		})
	}
}

func TestExistingFieldOperations(t *testing.T) {
	config := Config{
		Jobs: 1, Greedy: true, GreedyCount: 1, EscapeCurlyBrackets: true,
		reFieldDelimiter: regexp.MustCompile(`\s+`),
	}
	for expression, want := range map[string]string{
		"{1.}": "only", "{1:}": "only", "{1/}": ".", "{1%}": "only.txt",
		"{1^txt}": "only.", "{1@([^.]+)}": "only",
	} {
		got, err := fillCommand(config, expression, Chunk{ID: 1, Data: []string{"only.txt"}}, 1)
		if err != nil || got != want {
			t.Errorf("%s: replacement=%q, err=%v; want %q", expression, got, err, want)
		}
	}
}

func TestRegexReplacementRequiresPattern(t *testing.T) {
	config := Config{
		Jobs: 1, Greedy: true, GreedyCount: 1, EscapeCurlyBrackets: true,
		reFieldDelimiter: regexp.MustCompile(`\s+`),
		AssignMap:        map[string]string{"file": "dir/sample.txt", "macro": "{.%@}"},
	}
	chunk := Chunk{ID: 1, Data: []string{"alpha.txt beta.csv"}}
	for _, expression := range []string{"{@}", "{1@}", "{.@}", "{2.%@}", "{file@}", "{file.%@}", "{macro}"} {
		t.Run(expression, func(t *testing.T) {
			want := "before " + expression + " after alpha.txt"
			if expression == "{macro}" {
				want = "before {.%@} after alpha.txt"
			}
			for _, fill := range []func(Config, string, Chunk, int) (string, error){fillCommand, fillCommandForContinue} {
				got, err := fill(config, "before "+expression+" after {1}", chunk, 1)
				if err != nil || got != want {
					t.Errorf("got=%q err=%v; want %q", got, err, want)
				}
			}
		})
	}
	// An explicit regexp may legitimately match an empty string.
	for _, expression := range []string{"{@()}", "{.@()}", "{2.%@()}", "{file.%@()}"} {
		got, err := fillCommand(config, "before "+expression+" after", chunk, 1)
		if err != nil || got != "before  after" {
			t.Errorf("valid empty match %s: got=%q err=%v", expression, got, err)
		}
	}
}
