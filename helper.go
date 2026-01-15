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
	"strings"
)

// VERSION of this package
const VERSION = "0.8.0"

func isStdin(file string) bool {
	return file == "-"
}

func checkVersion() {
	app := "rush"
	fmt.Printf("%s v%s\n", app, VERSION)
	fmt.Println("\nChecking new version...")

	resp, err := http.Get(fmt.Sprintf("https://github.com/shenwei356/%s/releases/latest", app))
	if err != nil {
		checkError(fmt.Errorf("Network error"))
	}
	items := strings.Split(resp.Request.URL.String(), "/")
	var version string
	if items[len(items)-1] == "" {
		version = items[len(items)-2]
	} else {
		version = items[len(items)-1]
	}
	if version == "v"+VERSION {
		fmt.Printf("You are using the latest version of %s\n", app)
	} else {
		fmt.Printf("New version available: %s %s at %s\n", app, version, resp.Request.URL.String())
	}
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

func readSuccCmds(file string) map[string]struct{} {
	cmds := make(map[string]struct{})
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
			return len(data), data, nil
		}
		return 0, nil, nil
	}

	fh, err := os.Open(file)
	checkError(err)
	defer fh.Close()

	scanner := bufio.NewScanner(bufio.NewReader(fh))
	scanner.Buffer(make([]byte, 0, 16384), 2147483647)
	scanner.Split(split)

	var record string
	for scanner.Scan() {
		record = scanner.Text()
		if record == "" {
			continue
		}
		cmds[record] = struct{}{}
	}
	return cmds
}
