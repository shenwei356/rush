package main

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// versionTestTransport supplies release redirects and failures without networking.
type versionTestTransport struct {
	mode       string // mode selects the response or transport fault.
	closedFile string // closedFile records response-body release in the subprocess.
}

// nextVersionForTest keeps the newer-release fixture ahead of version bumps.
func nextVersionForTest() string {
	major, _, _ := strings.Cut(VERSION, ".")
	n, err := strconv.Atoi(major)
	if err != nil {
		panic(fmt.Sprintf("invalid test version %q: %v", VERSION, err))
	}
	return fmt.Sprintf("%d.0.0", n+1)
}

func (transport versionTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if transport.mode == "network-error" {
		return nil, fmt.Errorf("injected transport failure")
	}
	if transport.mode == "deadline" {
		if deadline, ok := request.Context().Deadline(); !ok || time.Until(deadline) > 10*time.Second {
			return nil, fmt.Errorf("version request has no bounded deadline")
		}
	}
	response := &http.Response{
		StatusCode: http.StatusOK, Header: make(http.Header), Request: request,
		Body: &versionResponseBody{Reader: strings.NewReader("release page"), closedFile: transport.closedFile},
	}
	if request.URL.Path == "/shenwei356/rush/releases/latest" {
		target := "/shenwei356/rush/releases/tag/v" + nextVersionForTest()
		switch transport.mode {
		case "current":
			target = "/shenwei356/rush/releases/tag/v" + VERSION
		case "current-query":
			target = "/shenwei356/rush/releases/tag/v" + VERSION + "?expanded=true"
		case "older":
			target = "/shenwei356/rush/releases/tag/v0.1.0"
		case "wrong-repository":
			target = "/another/project/releases/tag/v99"
		case "wrong-host":
			target = "https://example.invalid/shenwei356/rush/releases/tag/v99"
		case "empty-tag":
			target = "/shenwei356/rush/releases/tag/"
		case "no-release":
			target = ""
		case "http-error":
			target = ""
			response.StatusCode = http.StatusServiceUnavailable
		}
		if target != "" {
			response.StatusCode = http.StatusFound
			response.Header.Set("Location", target)
			// Only the final response's Close should satisfy the assertion.
			response.Body = io.NopCloser(strings.NewReader("redirect"))
		}
	}
	response.Status = fmt.Sprintf("%d %s", response.StatusCode, http.StatusText(response.StatusCode))
	return response, nil
}

// versionResponseBody makes Close observable across the CLI subprocess.
type versionResponseBody struct {
	io.Reader
	closedFile string
}

func (body *versionResponseBody) Close() error {
	return os.WriteFile(body.closedFile, []byte("closed"), 0600)
}

func TestVersionCheckResponses(t *testing.T) {
	nextVersion := nextVersionForTest()
	for _, test := range []struct {
		mode, diagnostic string
		success          bool
	}{
		{"new", "rush v" + nextVersion + " at https://github.com/shenwei356/rush/releases/tag/v" + nextVersion, true},
		{"current", "You are using the latest version", true},
		{"current-query", "You are using the latest version", true},
		{"older", "Latest release: rush v0.1.0", true},
		{"deadline", "rush v" + nextVersion + " at", true},
		{"http-error", "503", false},
		{"no-release", "unexpected release URL", false},
		{"wrong-repository", "unexpected release URL", false},
		{"wrong-host", "unexpected release URL", false},
		{"empty-tag", "unexpected release URL", false},
		{"network-error", "injected transport failure", false},
	} {
		t.Run(test.mode, func(t *testing.T) {
			closed := t.TempDir() + "/closed"
			env := []string{"RUSH_TEST_VERSION_RESPONSE=" + test.mode, "RUSH_TEST_VERSION_BODY_CLOSED=" + closed}
			stdout, stderr, code := runLegacyRush(t, "", env, "--version")
			if (code == 0) != test.success || !strings.Contains(stdout+stderr, test.diagnostic) {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			if test.mode != "network-error" {
				if data, err := os.ReadFile(closed); err != nil || string(data) != "closed" {
					t.Fatalf("response body was not closed: %q, err=%v", data, err)
				}
			}
		})
	}
}

func TestSuccessfulCommandRecordRoundTrip(t *testing.T) {
	commands := []string{
		"echo plain", "echo 'two\nlines'", "echo '__CMD__\n'",
		"echo '\xff__CMD__\n'", quotedCommandPrefix + "literal", "\x00rush-command-v99:literal",
		continueCommandPrefix + "echo \xff{#}",
		jobContinueKey(continueCommandPrefix+"echo "+continueJobIDMarker, []string{"input"}, "", make(continueJobOccurrences)),
	}
	file, err := os.CreateTemp(t.TempDir(), "records")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	writer := bufio.NewWriter(file)
	for _, command := range commands {
		if err := appendSuccessfulCommand(writer, command); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	records, older := readSuccCmds(file.Name())
	if older {
		t.Error("new records were classified as older records")
	}
	if len(records) != len(commands) {
		t.Fatalf("read %d records; want %d", len(records), len(commands))
	}
	for _, command := range commands {
		if _, found := records[command]; !found {
			t.Errorf("lost command bytes: %q", command)
		}
	}
	data, err := os.ReadFile(file.Name())
	if err != nil || !strings.HasPrefix(string(data), quotedCommandPrefix+strconv.Quote(commands[0])+endMarkOfCMD+quotedCommandPrefix+strconv.Quote(commands[1])+endMarkOfCMD) {
		t.Fatalf("ordinary records lack versioned quoting: %q, err=%v", data, err)
	}
}

func TestMalformedQuotedSuccessfulCommandRecord(t *testing.T) {
	for _, test := range []struct{ name, record, diagnostic string }{
		{"current", quotedCommandPrefix + `"bad\q"`, "invalid quoted successful-command record"},
		{"older", legacyQuotedCommandPrefix + `"bad\q"`, "invalid quoted successful-command record"},
		{"unknown", "\x00rush-command-v99:" + strconv.Quote("echo x"), "unsupported successful-command record version"},
	} {
		t.Run(test.name, func(t *testing.T) {
			journal := t.TempDir() + "/records"
			original := test.record + endMarkOfCMD
			if err := os.WriteFile(journal, []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			stdout, stderr, code := runLegacyRush(t, "x\n", nil, "-c", "-C", journal, "echo {}")
			if code == 0 || stdout != "" || !strings.Contains(stderr, test.diagnostic) {
				t.Errorf("invalid record accepted: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			data, err := os.ReadFile(journal)
			if err != nil || string(data) != original {
				t.Errorf("invalid record was changed: %q, err=%v", data, err)
			}
		})
	}
}
