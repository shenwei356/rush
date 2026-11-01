package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/pflag"
)

// diagnosticConfig reads file options without changing flags or validating
// unrelated values. Unlike normal parsing, it continues past malformed flags
// so errors cannot corrupt a protected file named later in the arguments.
func diagnosticConfig(flags *pflag.FlagSet, args []string) Config {
	config := Config{OutFile: flags.Lookup("out-file").DefValue, SuccCmdFile: flags.Lookup("succ-cmd-file").DefValue}
	record := func(flag *pflag.Flag, value string) {
		switch flag.Name {
		case "infile":
			reader := csv.NewReader(strings.NewReader(value))
			reader.FieldsPerRecord = -1
			for {
				fields, err := reader.Read()
				// Protect recognizable fields even if the CSV is malformed.
				config.Infiles = append(config.Infiles, fields...)
				if err != nil {
					break
				}
			}
		case "out-file":
			config.OutFile = value
		case "succ-cmd-file":
			config.SuccCmdFile = value
		case "continue":
			var err error
			config.Continue, err = strconv.ParseBool(value)
			if err != nil {
				config.Continue = true // Protect records while reporting an invalid boolean.
			}
		}
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if strings.HasPrefix(arg, "--") {
			name, value, attached := strings.Cut(arg[2:], "=")
			flag := flags.Lookup(name)
			if flag == nil {
				continue
			}
			if !attached {
				value = flag.NoOptDefVal
				if value == "" {
					if i+1 == len(args) {
						continue
					}
					i++
					value = args[i]
				}
			}
			record(flag, value)
		} else if strings.HasPrefix(arg, "-") {
			for shorts := arg[1:]; len(shorts) > 0; {
				flag := flags.ShorthandLookup(shorts[:1])
				shorts = shorts[1:]
				if flag == nil {
					continue
				}
				value := flag.NoOptDefVal
				switch {
				case len(shorts) > 1 && shorts[0] == '=':
					value, shorts = shorts[1:], ""
				case value != "": // Boolean flags may precede more short flags.
				case len(shorts) > 0:
					value, shorts = shorts, ""
				case i+1 < len(args):
					i++
					value = args[i]
				default:
					continue
				}
				record(flag, value)
			}
		}
	}
	// GetStringSlice round-trips through CSV, collapsing one empty field to an
	// empty slice. Match that behavior so this spelling still protects stdin.
	if len(config.Infiles) == 1 && config.Infiles[0] == "" {
		config.Infiles = nil
	}
	return config
}

// protectRunStream rejects stdout/stderr aliases before informational output or
// diagnostics can modify an input or a successful-command file.
func protectRunStream(config Config, stream *os.File) error {
	role, fallback := "stderr", os.Stdout
	if stream == os.Stdout {
		role, fallback = "stdout", os.Stderr
	}
	diagnostics, err := inspectRunFile(role, "", stream, false)
	if err != nil {
		// Normal setup reports unusable streams. No alias can be identified.
		return nil
	}
	if diagnostics.info.Mode()&os.ModeCharDevice != 0 {
		return nil // Terminals and null devices may serve multiple streams.
	}
	var protected []runFile
	add := func(role, path string, stream *os.File) {
		if file, err := inspectRunFile(role, path, stream, false); err == nil {
			protected = append(protected, file)
		}
	}
	inputs := config.Infiles
	if len(inputs) == 0 {
		inputs = []string{"-"}
	}
	for _, path := range inputs {
		if isStdin(path) {
			add("input", path, os.Stdin)
		} else {
			add("input", path, nil)
		}
	}
	if config.Continue {
		add("successful-command file", config.SuccCmdFile, nil)
	}
	for _, file := range protected {
		if !diagnostics.aliases(file) {
			continue
		}
		// Report through the other stream only if it cannot modify a run file.
		logBackend.Logger.SetOutput(io.Discard)
		if !isStdin(config.OutFile) {
			add("output", config.OutFile, nil)
		}
		if alternative, err := inspectRunFile("diagnostic", "", fallback, false); err == nil {
			safe := true
			for _, target := range protected {
				if alternative.aliases(target) {
					safe = false
					break
				}
			}
			if safe {
				logBackend.Logger.SetOutput(fallback)
			}
		}
		return fmt.Errorf("%s and %s refer to the same file", role, file.role)
	}
	return nil
}

// runFile describes a file used by rush before any existing data is modified.
type runFile struct {
	role   string      // role identifies the input, output, or continue record.
	path   string      // path is empty for an inherited standard stream.
	info   os.FileInfo // info is nil only for a new output file.
	parent os.FileInfo // parent identifies the directory of a new output file.
	base   string      // base is the final filename within parent.
}

// inspectRunFile follows aliases without opening or truncating the target.
func inspectRunFile(role, path string, stream *os.File, output bool) (runFile, error) {
	f := runFile{role: role, path: path}
	var err error
	if stream != nil {
		f.path = ""
		f.info, err = stream.Stat()
	} else {
		if path == "" {
			return f, fmt.Errorf("empty %s path", role)
		}
		f.info, err = os.Stat(path)
	}
	if err != nil {
		if stream != nil || !output || !os.IsNotExist(err) {
			return f, fmt.Errorf("inspect %s %q: %w", role, path, err)
		}
		// A dangling final symlink still determines where a new file would
		// be created. Validate its target before opening any run files.
		if link, linkErr := os.Lstat(path); linkErr == nil && link.Mode()&os.ModeSymlink != 0 {
			target, readErr := os.Readlink(path)
			if readErr != nil {
				return f, fmt.Errorf("inspect %s symlink %q: %w", role, path, readErr)
			}
			if filepath.VolumeName(target) == "" && (len(target) == 0 || !os.IsPathSeparator(target[0])) {
				// Preserve relative symlink/.. components for filesystem resolution.
				parentPath, _ := filepath.Split(path)
				target = parentPath + target
			}
			resolved, resolveErr := inspectRunFile(role, target, nil, true)
			resolved.path = path
			return resolved, resolveErr
		}
		// Split preserves symlink/.. components; Dir and Abs would clean
		// them before the filesystem can resolve their actual destination.
		parentPath, base := filepath.Split(path)
		if parentPath == "" {
			parentPath = "."
		}
		parent, parentErr := os.Stat(parentPath)
		if parentErr != nil {
			return f, fmt.Errorf("inspect %s directory: %w", role, parentErr)
		}
		if !parent.IsDir() {
			return f, fmt.Errorf("%s parent is not a directory: %s", role, parentPath)
		}
		f.parent, f.base = parent, base
	} else if f.info.IsDir() {
		return f, fmt.Errorf("%s is a directory: %s", role, path)
	}
	return f, nil
}

func (f runFile) aliases(other runFile) bool {
	// A terminal or /dev/null can legitimately serve both input and output.
	if f.info != nil && f.info.Mode()&os.ModeCharDevice != 0 ||
		other.info != nil && other.info.Mode()&os.ModeCharDevice != 0 {
		return false
	}
	if f.info != nil && other.info != nil {
		return os.SameFile(f.info, other.info)
	}
	return f.parent != nil && other.parent != nil &&
		os.SameFile(f.parent, other.parent) && f.base == other.base
}

// validateRunFiles rejects destructive aliases and invalid paths before output
// creation, and is repeated before truncation to cover newly created aliases.
func validateRunFiles(config Config) error {
	inputs := make([]runFile, 0, len(config.Infiles))
	for _, path := range config.Infiles {
		var stream *os.File
		if isStdin(path) {
			stream = os.Stdin
		}
		input, err := inspectRunFile("input", path, stream, false)
		if err != nil {
			return err
		}
		inputs = append(inputs, input)
	}
	var stream *os.File
	if isStdin(config.OutFile) {
		stream = os.Stdout
	}
	output, err := inspectRunFile("output", config.OutFile, stream, true)
	if err != nil {
		return err
	}
	outputs := []runFile{output}
	if config.Continue {
		record, err := inspectRunFile("successful-command file", config.SuccCmdFile, nil, true)
		if err != nil {
			return err
		}
		// Resume records must support finite reads, appending, and rollback.
		// Reject streams before reading them or creating the output file.
		if record.info != nil && !record.info.Mode().IsRegular() {
			return fmt.Errorf("successful-command file must be a regular file: %s", config.SuccCmdFile)
		}
		if output.aliases(record) {
			return fmt.Errorf("output and successful-command file refer to the same file")
		}
		outputs = append(outputs, record)
	}
	for _, output := range outputs {
		for _, input := range inputs {
			if output.aliases(input) {
				return fmt.Errorf("%s and input %q refer to the same file", output.role, input.path)
			}
		}
	}
	return nil
}
