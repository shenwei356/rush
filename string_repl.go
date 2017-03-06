// Copyright © 2017 Wei Shen <shenwei356@gmail.com>
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
	"bytes"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var rePlaceHolder = regexp.MustCompile(`\{([^\}]*)\}`)
var reChars = regexp.MustCompile(`\d+|.`)
var reCharsCheck = regexp.MustCompile(`^(\d+)*.*$`)

func fillCommand(config Config, command string, chunk Chunk) string {
	founds := rePlaceHolder.FindAllStringSubmatchIndex(command, -1)
	if len(founds) == 0 {
		return command
	}

	records := make([]string, len(chunk.Data))
	for i, r := range chunk.Data {
		switch config.Trim {
		case "l":
			records[i] = strings.TrimLeft(r, " \t\r\n")
		case "r":
			records[i] = strings.TrimRight(r, " \t\r\n")
		case "lr", "rl", "b":
			records[i] = strings.Trim(r, " \t\r\n")
		default:
			records[i] = r
		}
	}

	fieldsStr := strings.Join(records, config.RecordsJoinSeparator)

	if fieldsStr == "" || fieldsStr == "\n" || fieldsStr == "\r\n" || fieldsStr == "\r" {
		return ""
	}

	var fields []string

	var chars, char, target string
	var charsGroups []string
	var i, j int
	var buf bytes.Buffer
	for _, found := range founds {
		chars = command[found[2]:found[3]]
		target = fieldsStr

		if chars == "" {
			target = fieldsStr
		} else if chars == "#" {
			target = fmt.Sprintf("%d", chunk.ID)
		} else if !reCharsCheck.MatchString(chars) {
			target = fmt.Sprintf("{%s}", chars)
		} else {
			if v, ok := config.AssignMap[chars]; ok { // key-value
				target = v
			} else {
				charsGroups = reChars.FindAllString(chars, -1)

				char = charsGroups[0]
				if char[0] >= 48 && char[0] <= 57 { // digits, handle specific field
					i, _ = strconv.Atoi(char)
					if i == 0 {
						target = fieldsStr
					} else {
						if len(fields) == 0 {
							fields = config.reFieldDelimiter.Split(fieldsStr, -1)
						}

						if i > len(fields) {
							i = len(fields)
						}

						target = fields[i-1]
					}
					i = 1
				} else { // handle whole fieldStr
					target = fieldsStr
					i = 0
				}

				var x, y int
				var rmSuffix bool
				var td, tb string
			LOOP:
				for x, char = range charsGroups[i:] {
					switch char {
					case "#": // job number
						target = fmt.Sprintf("%d", chunk.ID)
						if x == 0 && len(charsGroups[i:]) > 1 {
							target = fmt.Sprintf("{%s}", chars)
						}
						break LOOP
					case ".": // remove last extension of the basename
						td = filepath.Dir(target)
						tb = filepath.Base(target)
						x = strings.LastIndex(tb, ".")
						if x > 0 {
							tb = tb[0:x]
						}
						target = filepath.Join(td, tb)
					case ":": // remove any extension of the basename
						td = filepath.Dir(target)
						tb = filepath.Base(target)
						x = strings.Index(tb, ".")
						if x > 0 {
							tb = tb[0:x]
						}
						target = filepath.Join(td, tb)
					case "/": // dirname
						target = filepath.Dir(target)
					case "%": // basename
						target = filepath.Base(target)
					case "^": // remove suffix
						rmSuffix = true
						break LOOP
					default:
						target = fmt.Sprintf("{%s}", chars)
						break LOOP
					}
				}
				if rmSuffix {
					y = strings.LastIndex(target, strings.Join(charsGroups[i:][x+1:], ""))
					if y >= 0 {
						target = target[0:y]
					}
				}
			}
		}

		buf.WriteString(command[j:found[0]])
		buf.WriteString(target)
		j = found[3] + 1
	}
	buf.WriteString(command[j:])
	return buf.String()
}
