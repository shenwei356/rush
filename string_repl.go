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
	"bytes"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var rePlaceHolder = regexp.MustCompile(`\{([^\{\}}]*)\}`)
var reChars = regexp.MustCompile(`\d+|.`)
var reCharsCheck = regexp.MustCompile(`^(\d+)*.*$`)
var reVariable = regexp.MustCompile(`^([a-zA-Z][A-Za-z0-9_]*)`)

func fillCommand(config Config, command string, chunk Chunk) (string, error) {
	s, err := _fillCommand(config, command, chunk)
	if err != nil {
		return s, err
	}
	return s, err
}

func _fillCommand(config Config, command string, chunk Chunk) (string, error) {
	founds := rePlaceHolder.FindAllStringSubmatchIndex(command, -1)
	if len(founds) == 0 { // no place holder
		return command, nil
	}

	// trim space in the input data
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

	// skip emtpy data
	if fieldsStr == "" || fieldsStr == "\n" || fieldsStr == "\r\n" || fieldsStr == "\r" {
		return "", nil
	}

	var fields []string

	var chars, char, target string
	var charsGroups []string
	var i, j int
	var buf bytes.Buffer
	l := len(command) - 2
	for _, found := range founds {
		// somethings like these: {{}}, {{1}}, {{1,}}, {{a}}
		if found[2] >= 2 && found[3] <= l && command[found[2]-2] == '{' && command[found[3]+1] == '}' {
			if config.GreedyCount > 0 { // keep the original content: {{}}
				target = command[found[2]-2 : found[3]+2]
			} else { // last round: output brackets: {}
				target = command[found[2]-1 : found[3]+1]
			}

			buf.WriteString(command[j : found[0]-1])
			buf.WriteString(target)
			j = found[3] + 2

			continue
		}

		chars = command[found[2]:found[3]] // content between "{" and "}"

		target = fieldsStr
		if chars == "" { // {}
			target = fieldsStr
		} else if chars == "#" { // {#}
			target = fmt.Sprintf("%d", chunk.ID)
		} else if !reCharsCheck.MatchString(chars) { // something weird
			target = fmt.Sprintf("{%s}", chars)
		} else {
			// target = fieldsStr

			// replace the possible predefind variable
			found2 := reVariable.FindAllStringIndex(chars, 1)
			if len(found2) > 0 { // rush -v file=abc.txt 'echo {file:}'
				if v, ok := config.AssignMap[chars[found2[0][0]:found2[0][1]]]; ok { // check if the variable is definded
					target = v

					chars = chars[found2[0][1]:] // might be empty, e.g., {file}
				}
			}

			if len(chars) > 0 { // for {file:} or {.}
				charsGroups = reChars.FindAllString(chars, -1)

				char = charsGroups[0]
				if char[0] >= 48 && char[0] <= 57 { // digits, handle specific field
					i, _ = strconv.Atoi(char)
					if i == 0 {
						// target = fieldsStr
					} else {
						if len(fields) == 0 {
							fields = config.reFieldDelimiter.Split(target, -1)
						}

						if i > len(fields) {
							target = fmt.Sprintf("{%s}", chars)
						} else {
							target = fields[i-1]
						}

					}
					i = 1
				} else { // handle whole fieldStr or value
					// target = fieldsStr
					i = 0
				}

				var x, y int
				var rmSuffix bool
				var captureGroup bool
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
					case "@": // capturing group
						captureGroup = true
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
				if captureGroup {
					if len(charsGroups[i:]) == 1 {
						continue
					}
					re, err := regexp.Compile(strings.Join(charsGroups[i:][x+1:], ""))
					if err != nil {
						return "", err
					}
					groups := re.FindStringSubmatch(target)
					if len(groups) == 0 {
					} else if len(groups) == 1 {
						target = groups[0]
					} else {
						target = groups[1]
					}
				}
			}
		}

		buf.WriteString(command[j:found[0]])
		buf.WriteString(target)
		j = found[3] + 1
	}
	buf.WriteString(command[j:])

	if !config.Greedy || config.GreedyCount <= 0 {
		return buf.String(), nil
	}
	config.GreedyCount--
	return _fillCommand(config, buf.String(), chunk)
}
