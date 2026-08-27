// actionfile materializes small planner-owned files without a shell. It is
// intentionally generic: the execution-time Kconfig/Kbuild planner owns the
// bytes and paths, while this helper only validates and writes them.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

const maxArgumentsFileBytes = 64 << 20

type repeatedLine []string

func (values *repeatedLine) String() string { return fmt.Sprint([]string(*values)) }
func (values *repeatedLine) Set(value string) error {
	if strings.ContainsAny(value, "\x00\r\n") {
		return fmt.Errorf("line contains a control character")
	}
	*values = append(*values, value)
	return nil
}

type repeatedInput []string

func (values *repeatedInput) String() string { return fmt.Sprint([]string(*values)) }
func (values *repeatedInput) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("input path must not be empty")
	}
	*values = append(*values, value)
	return nil
}

type treeCopy struct {
	relative string
	input    string
}

type repeatedTreeCopy []treeCopy

func (values *repeatedTreeCopy) String() string { return fmt.Sprint([]treeCopy(*values)) }
func (values *repeatedTreeCopy) Set(value string) error {
	relative, input, ok := strings.Cut(value, "=")
	if !ok || relative == "" || input == "" {
		return fmt.Errorf("tree copy must be RELATIVE=INPUT")
	}
	if strings.ContainsAny(relative, "\\\x00\r\n") || path.IsAbs(relative) || path.Clean(relative) != relative || relative == "." || relative == ".." || strings.HasPrefix(relative, "../") {
		return fmt.Errorf("tree copy path %q is not canonical and relative", relative)
	}
	*values = append(*values, treeCopy{relative: relative, input: input})
	return nil
}

func copyFile(input, output string) error {
	in, err := os.Open(input)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func decodeArgumentsFile(filename string) ([]string, error) {
	if filename == "" {
		return nil, fmt.Errorf("arguments file path is empty")
	}
	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("open arguments file %q: %w", filename, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat arguments file %q: %w", filename, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("arguments file %q is not a regular file", filename)
	}
	if info.Size() > maxArgumentsFileBytes {
		return nil, fmt.Errorf("arguments file %q exceeds %d bytes", filename, maxArgumentsFileBytes)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxArgumentsFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read arguments file %q: %w", filename, err)
	}
	if len(data) > maxArgumentsFileBytes {
		return nil, fmt.Errorf("arguments file %q exceeds %d bytes", filename, maxArgumentsFileBytes)
	}
	var arguments []string
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&arguments); err != nil {
		return nil, fmt.Errorf("decode arguments file %q: %w", filename, err)
	}
	if arguments == nil {
		return nil, fmt.Errorf("arguments file %q must contain a JSON string array", filename)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("decode arguments file %q: trailing JSON value", filename)
		}
		return nil, fmt.Errorf("decode arguments file %q: %w", filename, err)
	}
	for ordinal, argument := range arguments {
		if strings.ContainsRune(argument, 0) {
			return nil, fmt.Errorf("arguments file %q argument %d contains NUL", filename, ordinal)
		}
		if argument == "-arguments_file" || strings.HasPrefix(argument, "-arguments_file=") {
			return nil, fmt.Errorf("arguments file %q nests the arguments-file protocol", filename)
		}
	}
	canonical, err := json.Marshal(arguments)
	if err != nil {
		return nil, fmt.Errorf("canonicalize arguments file %q: %w", filename, err)
	}
	canonical = append(canonical, '\n')
	if !bytes.Equal(data, canonical) {
		return nil, fmt.Errorf("arguments file %q is not canonically encoded", filename)
	}
	return arguments, nil
}

func run(args []string) error {
	if len(args) != 0 && args[0] == "-arguments_file" {
		if len(args) != 2 {
			return fmt.Errorf("-arguments_file requires exactly one path and no other arguments")
		}
		decoded, err := decodeArgumentsFile(args[1])
		if err != nil {
			return err
		}
		return runDirect(decoded)
	}
	return runDirect(args)
}

func runDirect(args []string) error {
	flags := flag.NewFlagSet("actionfile", flag.ContinueOnError)
	out := flags.String("out", "", "output file")
	treeOut := flags.String("tree_out", "", "output directory populated by -copy mappings")
	content := flags.String("content_base64", "", "base64-encoded file contents")
	preserveMode := flags.Bool("preserve_mode", false, "preserve executable permission bits from a single -input")
	var inputs repeatedInput
	flags.Var(&inputs, "input", "input file to copy or concatenate byte-for-byte (repeatable)")
	var states repeatedInput
	flags.Var(&states, "state", "absolute observed-output state to merge (repeatable)")
	var copies repeatedTreeCopy
	flags.Var(&copies, "copy", "tree output mapping in RELATIVE=INPUT form (repeatable)")
	var lines repeatedLine
	flags.Var(&lines, "line", "line to write (repeatable)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional argument %q", flags.Arg(0))
	}
	if (*out == "") == (*treeOut == "") {
		return fmt.Errorf("exactly one of -out or -tree_out is required")
	}
	provided := map[string]bool{}
	flags.Visit(func(value *flag.Flag) { provided[value.Name] = true })
	if *treeOut != "" {
		if !provided["copy"] || provided["state"] || provided["content_base64"] || provided["input"] || provided["line"] || provided["preserve_mode"] {
			return fmt.Errorf("-tree_out requires only one or more -copy mappings")
		}
		seen := map[string]bool{}
		if err := os.MkdirAll(*treeOut, 0o755); err != nil {
			return fmt.Errorf("create tree output: %w", err)
		}
		entries, err := os.ReadDir(*treeOut)
		if err != nil {
			return fmt.Errorf("read tree output: %w", err)
		}
		if len(entries) != 0 {
			return fmt.Errorf("tree output %q is not empty", *treeOut)
		}
		for _, copy := range copies {
			if seen[copy.relative] {
				return fmt.Errorf("tree copy path %q is repeated", copy.relative)
			}
			seen[copy.relative] = true
			if err := copyFile(copy.input, filepath.Join(*treeOut, filepath.FromSlash(copy.relative))); err != nil {
				return fmt.Errorf("copy %q to %q: %w", copy.input, copy.relative, err)
			}
		}
		return nil
	}
	forms := 0
	for _, name := range []string{"state", "content_base64", "input", "line"} {
		if provided[name] {
			forms++
		}
	}
	if forms != 1 {
		return fmt.Errorf("exactly one of -state, -content_base64, -input, or -line is required")
	}
	if *preserveMode && (!provided["input"] || len(inputs) != 1) {
		return fmt.Errorf("-preserve_mode requires exactly one -input")
	}
	var data []byte
	var err error
	outputMode := os.FileMode(0o644)
	if provided["state"] {
		decoded := make([]toolaction.ObservedOutputState, len(states))
		for ordinal, input := range states {
			encoded, readErr := os.ReadFile(input)
			if readErr != nil {
				return fmt.Errorf("read -state %q: %w", input, readErr)
			}
			state, decodeErr := toolaction.DecodeObservedOutputState(encoded)
			if decodeErr != nil {
				return fmt.Errorf("decode -state %q: %w", input, decodeErr)
			}
			decoded[ordinal] = state
		}
		merged, mergeErr := toolaction.MergeObservedOutputStates(decoded)
		if mergeErr != nil {
			return mergeErr
		}
		switch merged.Disposition {
		case toolaction.ObservedOutputAbsent:
			return fmt.Errorf("no -state input has a writer")
		case toolaction.ObservedOutputDeleted:
			return fmt.Errorf("merged -state records final deletion by writer %s", merged.Writer)
		case toolaction.ObservedOutputPresent:
		default:
			return fmt.Errorf("merged -state has unsupported disposition %q", merged.Disposition)
		}
		data = merged.Content
		outputMode |= os.FileMode(merged.ExecutableMode)
	} else if provided["input"] {
		for _, input := range inputs {
			part, readErr := os.ReadFile(input)
			if readErr != nil {
				return fmt.Errorf("read -input %q: %w", input, readErr)
			}
			data = append(data, part...)
		}
		if *preserveMode {
			info, statErr := os.Stat(inputs[0])
			if statErr != nil {
				return fmt.Errorf("stat -input %q: %w", inputs[0], statErr)
			}
			outputMode |= info.Mode().Perm() & 0o111
		}
	} else if provided["content_base64"] {
		data, err = base64.StdEncoding.DecodeString(*content)
		if err != nil {
			return fmt.Errorf("decode -content_base64: %w", err)
		}
	} else {
		data = []byte(strings.Join(lines, "\n") + "\n")
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	if err := os.WriteFile(*out, data, outputMode); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	if err := os.Chmod(*out, outputMode); err != nil {
		return fmt.Errorf("set output mode: %w", err)
	}
	return nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "actionfile: %v\n", err)
		os.Exit(1)
	}
}
