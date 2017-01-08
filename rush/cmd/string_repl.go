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

package cmd

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
var reCharsCheck = regexp.MustCompile(`^(\d+)*[^\d]*$`)

func fillCommand(config Config, command string, chunk Chunk) string {
	founds := rePlaceHolder.FindAllStringSubmatchIndex(command, -1)
	if len(founds) == 0 {
		return command
	}
	fieldsStr := strings.Join(chunk.Data, config.RecordDelimiter)
	switch config.Trim {
	case "l":
		fieldsStr = strings.TrimLeft(fieldsStr, " \t")
	case "r":
		fieldsStr = strings.TrimRight(fieldsStr, " \t")
	case "lr", "rl", "b":
		fieldsStr = strings.Trim(fieldsStr, " \t")
	}

	var fields []string
	if config.RecordDelimiter == config.FieldDelimiter {
		fields = chunk.Data
	} else {
		fields = config.reFieldDelimiter.Split(fieldsStr, -1)
	}

	var chars, char, target string
	var charsGroups []string
	var i, j int
	var buf bytes.Buffer
	for _, found := range founds {
		chars = command[found[2]:found[3]]

		if chars == "" {
			target = fieldsStr
		} else if !reCharsCheck.MatchString(chars) {
			// checkError(fmt.Errorf("illegal placeholder: {%s}", chars))
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

				for _, char = range charsGroups[i:] {
					switch char {
					case "#": // job number
						target = fmt.Sprintf("%d", chunk.ID)
					case ".": // remove last extension
						i = strings.LastIndex(target, ".")
						if i > 0 {
							target = target[0:i]
						}
					case ":": //remove any extension
						i = strings.Index(target, ".")
						if i > 0 {
							target = target[0:i]
						}
					case "/": //dirname
						target = filepath.Dir(target)
					case "%":
						target = filepath.Base(target)
					default:
						target = ""
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
