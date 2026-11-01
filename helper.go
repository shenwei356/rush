// Copyright © 2017-2023 Wei Shen <shenwei356@gmail.com>
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// VERSION of this package
const VERSION = "0.11.1"

func isStdin(file string) bool {
	return file == "-"
}

func checkVersion() {
	app := "rush"
	fmt.Printf("%s v%s\n", app, VERSION)
	fmt.Println("\nChecking new version...")

	version, releaseURL, err := latestRelease()
	checkError(err)
	if version == "v"+VERSION {
		fmt.Printf("You are using the latest version of %s\n", app)
	} else {
		// A local development build can be ahead of the latest published release.
		fmt.Printf("Latest release: %s %s at %s\n", app, version, releaseURL)
	}
}

// latestRelease validates GitHub's release redirect and releases the response
// before checkVersion can exit on error. The timeout also covers redirects.
func latestRelease() (version, releaseURL string, err error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("https://github.com/shenwei356/rush/releases/latest")
	if err != nil {
		return "", "", fmt.Errorf("check latest release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("check latest release: HTTP %s", resp.Status)
	}
	const tagPath = "/shenwei356/rush/releases/tag/"
	finalURL := resp.Request.URL
	if finalURL.Scheme != "https" || finalURL.Host != "github.com" || !strings.HasPrefix(finalURL.Path, tagPath) {
		return "", "", fmt.Errorf("check latest release: unexpected release URL %s", finalURL)
	}
	version = strings.TrimSuffix(strings.TrimPrefix(finalURL.Path, tagPath), "/")
	if version == "" {
		return "", "", fmt.Errorf("check latest release: unexpected release URL %s", finalURL)
	}
	return version, finalURL.String(), nil
}

func exists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

const endMarkOfCMD string = "__CMD__\n"

// A real executed command cannot contain NUL, so this versioned prefix cannot
// collide with an ordinary legacy record. Quoting also preserves non-UTF-8 bytes.
const legacyQuotedCommandPrefix = "\x00rush-command-v1:"
const quotedCommandPrefix = "\x00rush-command-v2:"

// encodeSuccessfulCommand versions every new record, including ordinary text,
// so mixed command batches remain distinguishable from older ambiguous keys.
func encodeSuccessfulCommand(command string) string {
	return quotedCommandPrefix + strconv.Quote(command)
}

// readSuccCmds also reports records written before numeric job-value tracking.
func readSuccCmds(file string) (map[string]struct{}, bool) {
	fh, err := os.Open(file)
	checkError(err)
	defer fh.Close()
	return readSuccCmdsFrom(fh)
}

// readSuccCmdsFrom reads through the locked handle, including on Windows where
// reopening the path would conflict with our own exclusive byte-range lock.
func readSuccCmdsFrom(fh *os.File) (map[string]struct{}, bool) {
	file := fh.Name()
	cmds := make(map[string]struct{})
	hasOlderRecords := false
	sep := []byte(endMarkOfCMD)
	split := func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		if atEOF && len(data) == 0 {
			return 0, nil, nil
		}
		i := bytes.Index(data, sep)
		if i >= 0 {
			return i + len(endMarkOfCMD), data[0:i], nil
		}
		if atEOF {
			// A torn final write is not evidence that this command succeeded.
			return 0, nil, fmt.Errorf("incomplete successful-command record (missing %q)", endMarkOfCMD)
		}
		return 0, nil, nil
	}

	scanner := bufio.NewScanner(bufio.NewReader(fh))
	scanner.Buffer(make([]byte, 0, 16384), 2147483647)
	scanner.Split(split)

	var record string
	for scanner.Scan() {
		record = scanner.Text()
		if record == "" {
			continue
		}
		if strings.HasPrefix(record, "\x00rush-command-") && !strings.HasPrefix(record, quotedCommandPrefix) && !strings.HasPrefix(record, legacyQuotedCommandPrefix) {
			checkError(fmt.Errorf("unsupported successful-command record version in %s", file))
		}
		prefix := quotedCommandPrefix
		if !strings.HasPrefix(record, prefix) {
			hasOlderRecords = true
			prefix = legacyQuotedCommandPrefix
		}
		if strings.HasPrefix(record, prefix) {
			decoded, decodeErr := strconv.Unquote(record[len(prefix):])
			if decodeErr != nil {
				checkError(fmt.Errorf("invalid quoted successful-command record in %s: %w", file, decodeErr))
			}
			record = decoded
		}
		if strings.HasPrefix(record, continueJobPrefix) && !validContinueJobKey(record) {
			checkError(fmt.Errorf("invalid job identity in successful-command record in %s", file))
		}
		// Old thread markers lost operations, and normalized job IDs without
		// batch identity can conflate completed and unfinished tasks.
		if strings.Contains(record, legacyContinueThreadsMarker) ||
			(strings.Contains(record, continueJobIDMarker) && !strings.HasPrefix(record, continueJobPrefix)) ||
			(!strings.HasPrefix(record, continueJobPrefix) && !strings.HasPrefix(record, continueCommandPrefix) && !strings.HasPrefix(record, continuePipePrefix) && !isComposedContinueRecord(record) && ambiguousLegacyContinueRecord(record)) {
			checkError(fmt.Errorf("ambiguous legacy successful-command record in %s: review completed jobs and rebuild the record file before resuming", file))
		}
		cmds[record] = struct{}{}
	}
	if err := scanner.Err(); err != nil {
		checkError(fmt.Errorf("failed to read file %s: %s", file, err))
	}
	return cmds, hasOlderRecords
}
