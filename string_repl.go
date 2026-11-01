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
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/shenwei356/util/stringutil"
)

var rePlaceHolder = regexp.MustCompile(`\{([^\{\}}]*)\}`)
var reChars = regexp.MustCompile(`\d+|.`)
var reCharsCheck = regexp.MustCompile(`^(\d+)*.*$`)
var reVariable = regexp.MustCompile(`^([a-zA-Z][A-Za-z0-9_]*)`)

const continueJobIDMarker = "\x00RUSH_CONTINUE_JOB_ID\x00"
const legacyContinueThreadsMarker = "\x00RUSH_CONTINUE_THREADS\x00"

// Keep the existing standalone-thread marker so valid resume records remain readable.
const continueThreadsPrefix = "\x00RUSH_CONTINUE_THREADS_V2:"
const continueThreadsMarker = continueThreadsPrefix + "\x00"

// NUL cannot occur in an executed command. Versioned prefixes distinguish
// resume metadata from literal command text without changing ordinary records.
const continueCommandPrefix = "\x00rush-continue-command-v1:"
const continuePipePrefix = "\x00rush-continue-pipe-v1:"

// Composed keys use numeric job values while parsing later macro passes. Older
// keys may have parsed opaque markers instead and cannot safely identify them.
const continueComposedCommandPrefix = "\x00rush-composed-command-v1:"
const continueComposedPipePrefix = "\x00rush-composed-pipe-v1:"

// continueDigitEscapePrefix marks trailing escape settings for dynamic job values.
const continueDigitEscapePrefix = "\x00rush-escape-digits-v1:"

func isComposedContinueRecord(record string) bool {
	record = continueJobCommand(record)
	return strings.HasPrefix(record, continueComposedCommandPrefix) || strings.HasPrefix(record, continueComposedPipePrefix)
}

// validateLegacyContinueExpansion checks every runnable batch before any output
// or record append. A later ambiguous batch must not modify an older journal.
func validateLegacyContinueExpansion(ctx context.Context, config Config, command string, input []inputRecord) error {
	id := uint64(1)
	for start := 0; start < len(input); {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := start + min(config.NRecords, len(input)-start)
		data := make([]string, end-start)
		for i := range data {
			data[i] = input[start+i].data
		}
		chunk := Chunk{ID: id, Data: data}
		actual, err := fillCommand(config, command, chunk, 0)
		if err != nil {
			// The normal producer will report this expansion error in order;
			// no later batch can execute or introduce a legacy ambiguity.
			return nil
		}
		if actual != "" {
			record, err := fillCommandForContinue(config, command, chunk, 0)
			if err != nil {
				return nil
			}
			if isComposedContinueRecord(record) {
				return fmt.Errorf("ambiguous older successful-command records for composed job placeholders: review completed jobs and use a new record file")
			}
			id++
		}
		start = end
	}
	return nil
}

var reContinueMarker = regexp.MustCompile(regexp.QuoteMeta(continueJobIDMarker) + "|" + regexp.QuoteMeta(continueThreadsPrefix) + "[0-9a-f]*\x00")
var reLegacyPipeKey = regexp.MustCompile("\n# rush --pipe stdin sha256: [0-9a-f]{64}$")

// ambiguousLegacyContinueRecord detects old metadata that could equally have
// come from literal input, including backslashes added by --escape.
func ambiguousLegacyContinueRecord(record string) bool {
	plain := strings.ReplaceAll(record, `\`, "")
	return strings.Contains(plain, "{#}") || strings.Contains(plain, "{?}") || reLegacyPipeKey.MatchString(record)
}

func fillCommand(config Config, command string, chunk Chunk, NRemainingJobs int) (string, error) {
	s, err := _fillCommand(config, command, chunk, NRemainingJobs,
		fmt.Sprintf("%d", chunk.ID), strconv.Itoa(max(runtime.NumCPU()/config.Jobs, 1)), nil, nil)
	if err != nil {
		return s, err
	}
	return s, err
}

func fillCommandForContinue(config Config, command string, chunk Chunk, nRemainingJobs int) (string, error) {
	resume := &continueExpansion{}
	s, err := _fillCommand(config, command, chunk, nRemainingJobs,
		strconv.FormatUint(chunk.ID, 10), strconv.Itoa(max(runtime.NumCPU()/config.Jobs, 1)), nil, resume)
	if err != nil {
		return s, err
	}
	if resume.composed {
		return continueComposedCommandPrefix + s, nil
	}
	if strings.ContainsRune(s, '\x00') || ambiguousLegacyContinueRecord(s) {
		return continueCommandPrefix + s, nil
	}
	return s, nil
}

// escapeContinueCommand escapes command text while preserving internal tokens
// and the record prefix, even when --escape-symbols includes their characters.
func escapeContinueCommand(record, symbols string) string {
	prefix := continueCommandPrefix
	if strings.HasPrefix(record, continueComposedCommandPrefix) {
		prefix = continueComposedCommandPrefix
	}
	if !strings.HasPrefix(record, prefix) {
		return stringutil.EscapeSymbols(record, symbols)
	}
	body := strings.TrimPrefix(record, prefix)
	var escaped strings.Builder
	escaped.WriteString(prefix)
	start := 0
	markers := reContinueMarker.FindAllStringIndex(body, -1)
	for _, bounds := range markers {
		escaped.WriteString(stringutil.EscapeSymbols(body[start:bounds[0]], symbols))
		escaped.WriteString(body[bounds[0]:bounds[1]])
		start = bounds[1]
	}
	escaped.WriteString(stringutil.EscapeSymbols(body[start:], symbols))
	if len(markers) > 0 {
		// Normalized markers hide their numeric values. Retain the digit escape
		// policy so changing it cannot silently skip a different shell command.
		digits := strings.Map(func(digit rune) rune {
			if strings.ContainsRune(symbols, digit) {
				return digit
			}
			return -1
		}, "0123456789")
		if digits != "" {
			escaped.WriteString(continueDigitEscapePrefix + digits + "\x00")
		}
	}
	return escaped.String()
}

// removePathExtension changes only the final filename. Split preserves symlink
// traversal through '..', unlike Dir followed by Join, which cleans the path.
func removePathExtension(path string, all bool) string {
	dir, base := filepath.Split(path)
	// Leading dots belong to the filename, not to an extension. This also
	// preserves names consisting entirely of dots, including "." and "..".
	name := strings.TrimLeft(base, ".")
	dot := strings.LastIndexByte(name, '.')
	if all {
		dot = strings.IndexByte(name, '.')
	}
	if dot < 0 {
		return path
	}
	return dir + base[:len(base)-len(name)+dot]
}

// pathDirWithoutCleaning retains directory components that the filesystem must
// resolve, while preserving the platform's handling of bare filenames/volumes.
func pathDirWithoutCleaning(path string) string {
	// Ignore trailing separators before selecting the final component,
	// while retaining root-only paths and their platform-specific volume.
	volume := filepath.VolumeName(path)
	end := len(path)
	for end > len(volume) && os.IsPathSeparator(path[end-1]) {
		end--
	}
	if end == len(volume) && end < len(path) {
		return path
	}
	path = path[:end]
	dir, _ := filepath.Split(path)
	volume = filepath.VolumeName(dir)
	if dir == volume {
		return filepath.Dir(path)
	}
	end = len(dir)
	for end > len(volume) && os.IsPathSeparator(dir[end-1]) {
		end--
	}
	if end == len(volume) {
		return dir // Keep the original root separator(s).
	}
	return dir[:end]
}

// inputLiteralSpan marks original input in an expanded command. Its braces must
// not become placeholder syntax, including when adjacent to template braces.
type inputLiteralSpan struct {
	start, end int // Half-open byte offsets, kept sorted and nonoverlapping.
}

// continueMarkerSpan tracks a numeric job value that survives command expansion.
type continueMarkerSpan struct {
	start, end int    // Half-open byte offsets in the actual command text.
	marker     string // Stable resume token substituted only after all parsing.
}

type continueExpansion struct {
	markers  []continueMarkerSpan // Sorted, nonoverlapping surviving job values.
	composed bool                 // A later placeholder contains a job value.
}

func (s *continueExpansion) render(command string) string {
	if s == nil || len(s.markers) == 0 {
		return command
	}
	var result strings.Builder
	start := 0
	for _, span := range s.markers {
		result.WriteString(command[start:span.start])
		result.WriteString(span.marker)
		start = span.end
	}
	result.WriteString(command[start:])
	return result.String()
}

func _fillCommand(config Config, command string, chunk Chunk, nRemainingJobs int, jobID, threads string, literals []inputLiteralSpan, resume *continueExpansion) (string, error) {
	founds := rePlaceHolder.FindAllStringSubmatchIndex(command, -1)
	if len(founds) == 0 { // no place holder
		return resume.render(command), nil
	}

	// trim space in the input data
	records := make([]string, len(chunk.Data))
	hasData := false
	for i, r := range chunk.Data {
		records[i] = trimInputRecord(r, config.Trim)
		hasData = hasData || records[i] != ""
	}

	// Joining separators are not input data. Conversely, line breaks within
	// a record remain data unless the requested trim mode removes them.
	if !hasData {
		return "", nil
	}
	fieldsStr := strings.Join(records, config.RecordsJoinSeparator)

	var fields []string

	var chars, char, target string
	var charsGroups []string
	var i, j int
	var buf bytes.Buffer
	morePasses := config.Greedy && config.GreedyCount > 0
	var nextLiterals []inputLiteralSpan
	var nextMarkers []continueMarkerSpan
	isLiteral := func(index int) bool {
		i := sort.Search(len(literals), func(i int) bool { return literals[i].end > index })
		return i < len(literals) && literals[i].start <= index
	}
	// Copy input provenance along with unchanged command text as replacements
	// shift its offsets. No sentinel text is inserted into the user's command.
	writeCommand := func(start, end int) {
		if resume != nil {
			offset := buf.Len() - start
			for i := sort.Search(len(resume.markers), func(i int) bool { return resume.markers[i].end > start }); i < len(resume.markers) && resume.markers[i].start < end; i++ {
				span := resume.markers[i]
				if span.start >= start && span.end <= end {
					nextMarkers = append(nextMarkers, continueMarkerSpan{span.start + offset, span.end + offset, span.marker})
				}
			}
		}
		if morePasses {
			offset := buf.Len() - start
			for i := sort.Search(len(literals), func(i int) bool { return literals[i].end > start }); i < len(literals) && literals[i].start < end; i++ {
				nextLiterals = append(nextLiterals, inputLiteralSpan{
					start: max(start, literals[i].start) + offset,
					end:   min(end, literals[i].end) + offset,
				})
			}
		}
		buf.WriteString(command[start:end])
	}
	l := len(command) - 2
	for _, found := range founds {
		if isLiteral(found[0]) || isLiteral(found[1]-1) {
			continue
		}
		if resume != nil {
			i := sort.Search(len(resume.markers), func(i int) bool { return resume.markers[i].end > found[0] })
			if i < len(resume.markers) && resume.markers[i].start < found[1] {
				resume.composed = true
			}
		}
		// somethings like these: {{}}, {{1}}, {{1,}}, {{a}}
		if found[2] >= 2 && found[3] <= l && command[found[2]-2] == '{' && command[found[3]+1] == '}' && !isLiteral(found[0]-1) && !isLiteral(found[1]) {
			writeCommand(j, found[0]-1)
			if config.GreedyCount > 0 { // keep the original content: {{}}
				writeCommand(found[2]-2, found[3]+2)
			} else { // last round: output brackets: {}
				writeCommand(found[2]-1, found[3]+1)
			}
			j = found[3] + 2

			continue
		}

		chars = command[found[2]:found[3]] // content between "{" and "}"

		target = fieldsStr
		marker := ""
		keepExpression := false // Unsupported syntax retains any surviving job values.
		fromInput := true       // Input braces are literal; assigned values may contain macros.
		if chars == "" {        // {}
			target = fieldsStr
		} else if chars == "?" { // {?} is a standalone job placeholder.
			target = threads
			marker = continueThreadsMarker
			fromInput = false
		} else if chars == "#" { // {#}
			target = jobID
			marker = continueJobIDMarker
			fromInput = false
		} else if !reCharsCheck.MatchString(chars) { // something weird
			target = fmt.Sprintf("{%s}", chars)
			keepExpression = true
			fromInput = false
		} else {
			// target = fieldsStr

			// replace the possible predefind variable
			found2 := reVariable.FindAllStringIndex(chars, 1)
			if len(found2) > 0 { // rush -v file=abc.txt 'echo {file:}'
				if v, ok := config.AssignMap[chars[found2[0][0]:found2[0][1]]]; ok { // check if the variable is definded
					target = v
					fromInput = false

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
							// An absent field leaves the entire expression intact;
							// do not apply path or regex operations to its syntax.
							writeCommand(j, found[1])
							j = found[1]
							continue
						}
						target = fields[i-1]

					}
					i = 1
				} else { // handle whole fieldStr or value
					// target = fieldsStr
					i = 0
				}

				var x int
				var rmSuffix bool
				var captureGroup bool
			LOOP:
				for x, char = range charsGroups[i:] {
					switch char {
					case ".": // remove last extension of the basename
						target = removePathExtension(target, false)
					case ":": // remove any extension of the basename
						target = removePathExtension(target, true)
					case "/": // dirname
						target = pathDirWithoutCleaning(target)
					case "%": // basename
						target = filepath.Base(target)
					case "^": // remove suffix
						rmSuffix = true
						break LOOP
					case "@": // capturing group
						captureGroup = true
						break LOOP
					default:
						fromInput = false
						// Preserve the complete expression, including a preset variable name.
						target = command[found[0]:found[1]]
						keepExpression = true
						break LOOP
					}
				}
				if rmSuffix {
					// A match inside a filename or directory is not a suffix.
					target = strings.TrimSuffix(target, strings.Join(charsGroups[i:][x+1:], ""))
				}
				if captureGroup {
					if x+1 == len(charsGroups[i:]) {
						// A missing pattern leaves the full expression intact,
						// including any field selector or preceding operations.
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

		writeCommand(j, found[0])
		if keepExpression {
			writeCommand(found[0], found[1])
			j = found[1]
			continue
		}
		if resume != nil && marker != "" {
			nextMarkers = append(nextMarkers, continueMarkerSpan{buf.Len(), buf.Len() + len(target), marker})
		}
		// Remember the resulting input span without changing its bytes.
		if fromInput && config.EscapeCurlyBrackets && morePasses && strings.ContainsAny(target, "{}") {
			nextLiterals = append(nextLiterals, inputLiteralSpan{start: buf.Len(), end: buf.Len() + len(target)})
		}
		buf.WriteString(target)
		j = found[3] + 1
	}
	writeCommand(j, len(command))
	if resume != nil {
		resume.markers = nextMarkers
	}

	if !config.Greedy || config.GreedyCount <= 0 {
		return resume.render(buf.String()), nil
	}
	config.GreedyCount--
	return _fillCommand(config, buf.String(), chunk, nRemainingJobs, jobID, threads, nextLiterals, resume)
}
