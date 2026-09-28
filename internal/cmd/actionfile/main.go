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
	"regexp"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

const maxArgumentsFileBytes = 64 << 20

const validateConfigIndependentMacroHeaderFlag = "validate_config_independent_macro_header_v1"
const validateClosedIntegerMacroHeaderFlag = "validate_closed_integer_macro_header_v1"

var configIndependentMacroHeaderIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func stripConfigIndependentMacroHeaderComments(data []byte) ([]byte, error) {
	if len(data) == 0 || data[len(data)-1] != '\n' {
		return nil, fmt.Errorf("validated macro header must end with a newline")
	}
	if bytes.Contains(data, []byte("??")) {
		return nil, fmt.Errorf("validated macro header contains a possible trigraph")
	}
	if bytes.Contains(data, []byte("%:")) {
		return nil, fmt.Errorf("validated macro header contains a preprocessor digraph")
	}
	if bytes.Contains(data, []byte("\\\n")) {
		return nil, fmt.Errorf("validated macro header contains a line splice")
	}
	stripped := make([]byte, 0, len(data))
	inComment := false
	for index := 0; index < len(data); {
		character := data[index]
		if character == 0 || character == '\r' || character < 0x20 && character != '\n' && character != '\t' || character >= 0x7f {
			return nil, fmt.Errorf("validated macro header contains a non-text byte")
		}
		if inComment {
			if index+1 < len(data) && data[index] == '/' && data[index+1] == '*' {
				return nil, fmt.Errorf("validated macro header contains a nested block comment")
			}
			if index+1 < len(data) && data[index] == '*' && data[index+1] == '/' {
				stripped = append(stripped, ' ')
				index += 2
				inComment = false
				continue
			}
			if character == '\n' {
				stripped = append(stripped, '\n')
			} else {
				stripped = append(stripped, ' ')
			}
			index++
			continue
		}
		if index+1 < len(data) && data[index] == '/' && data[index+1] == '*' {
			stripped = append(stripped, ' ')
			index += 2
			inComment = true
			continue
		}
		if index+1 < len(data) && data[index] == '*' && data[index+1] == '/' {
			return nil, fmt.Errorf("validated macro header contains an unmatched block-comment terminator")
		}
		if index+1 < len(data) && data[index] == '/' && data[index+1] == '/' {
			return nil, fmt.Errorf("validated macro header contains a line comment")
		}
		stripped = append(stripped, character)
		index++
	}
	if inComment {
		return nil, fmt.Errorf("validated macro header contains an unterminated block comment")
	}
	return stripped, nil
}

func configIndependentMacroHeaderInteger(value string) bool {
	if value == "" {
		return false
	}
	if value[0] == '+' || value[0] == '-' {
		value = value[1:]
		if value == "" {
			return false
		}
	}
	base := byte(10)
	if len(value) > 2 && value[0] == '0' && (value[1] == 'x' || value[1] == 'X') {
		base = 16
		value = value[2:]
		if value == "" {
			return false
		}
	}
	for _, character := range []byte(value) {
		if character >= '0' && character <= '9' {
			continue
		}
		if base == 16 && (character >= 'a' && character <= 'f' || character >= 'A' && character <= 'F') {
			continue
		}
		return false
	}
	return true
}

// validateConfigIndependentMacroHeader proves that a generated header cannot
// discover another file or read consumer configuration. Its unknown numeric
// macro values remain owned by the producer edge; dependency analysis only
// needs the stronger fact that scanning the header adds no file/config edges.
func validateConfigIndependentMacroHeader(data []byte) error {
	stripped, err := stripConfigIndependentMacroHeaderComments(data)
	if err != nil {
		return err
	}
	lines := []string{}
	for _, line := range strings.Split(string(stripped), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) < 3 {
		return fmt.Errorf("validated macro header has no complete outer guard")
	}
	ifndef := strings.Fields(lines[0])
	if len(ifndef) != 2 || ifndef[0] != "#ifndef" ||
		!configIndependentMacroHeaderIdentifier.MatchString(ifndef[1]) || strings.HasPrefix(ifndef[1], "CONFIG_") {
		return fmt.Errorf("validated macro header has an invalid outer guard")
	}
	guard := ifndef[1]
	guardDefine := strings.Fields(lines[1])
	if len(guardDefine) != 2 || guardDefine[0] != "#define" || guardDefine[1] != guard {
		return fmt.Errorf("validated macro header does not define its outer guard")
	}
	if lines[len(lines)-1] != "#endif" {
		return fmt.Errorf("validated macro header does not end at its outer guard")
	}
	for ordinal, line := range lines[2 : len(lines)-1] {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[0] != "#define" ||
			!configIndependentMacroHeaderIdentifier.MatchString(fields[1]) ||
			strings.HasPrefix(fields[1], "CONFIG_") || !configIndependentMacroHeaderInteger(fields[2]) {
			return fmt.Errorf("validated macro header body line %d is not one literal numeric object macro", ordinal+1)
		}
	}
	return nil
}

// closedIntegerMacroDefinition is the deliberately small C-preprocessor
// surface accepted from an execution-time generated header.  Names are
// collected before replacement lists are checked so a definition may refer to
// another definition which appears later in the same header.
type closedIntegerMacroDefinition struct {
	name        string
	parameters  map[string]bool
	replacement string
}

func closedIntegerMacroSignature(text string) (closedIntegerMacroDefinition, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return closedIntegerMacroDefinition{}, fmt.Errorf("empty macro definition")
	}
	end := 0
	for end < len(text) && (text[end] == '_' || text[end] >= 'a' && text[end] <= 'z' ||
		text[end] >= 'A' && text[end] <= 'Z' || end != 0 && text[end] >= '0' && text[end] <= '9') {
		end++
	}
	if end == 0 || !configIndependentMacroHeaderIdentifier.MatchString(text[:end]) {
		return closedIntegerMacroDefinition{}, fmt.Errorf("invalid macro name")
	}
	definition := closedIntegerMacroDefinition{name: text[:end], parameters: map[string]bool{}}
	if end < len(text) && text[end] == '(' {
		close := strings.IndexByte(text[end+1:], ')')
		if close < 0 {
			return closedIntegerMacroDefinition{}, fmt.Errorf("unterminated function-like macro parameters")
		}
		close += end + 1
		parameterText := strings.TrimSpace(text[end+1 : close])
		if parameterText != "" {
			for _, parameter := range strings.Split(parameterText, ",") {
				parameter = strings.TrimSpace(parameter)
				if !configIndependentMacroHeaderIdentifier.MatchString(parameter) || definition.parameters[parameter] {
					return closedIntegerMacroDefinition{}, fmt.Errorf("invalid or repeated macro parameter %q", parameter)
				}
				definition.parameters[parameter] = true
			}
		}
		end = close + 1
	}
	definition.replacement = strings.TrimSpace(text[end:])
	if definition.replacement == "" {
		return closedIntegerMacroDefinition{}, fmt.Errorf("macro %s has no integer replacement", definition.name)
	}
	return definition, nil
}

func validateClosedIntegerMacroReplacement(
	definition closedIntegerMacroDefinition,
	macroNames map[string]bool,
) error {
	text := definition.replacement
	for index := 0; index < len(text); {
		character := text[index]
		if character == ' ' || character == '\t' || character == '\n' {
			index++
			continue
		}
		if character == '_' || character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' {
			end := index + 1
			for end < len(text) && (text[end] == '_' || text[end] >= 'a' && text[end] <= 'z' ||
				text[end] >= 'A' && text[end] <= 'Z' || text[end] >= '0' && text[end] <= '9') {
				end++
			}
			identifier := text[index:end]
			if strings.HasPrefix(identifier, "CONFIG_") ||
				(!definition.parameters[identifier] && !macroNames[identifier]) {
				return fmt.Errorf("macro %s replacement references unbound identifier %q", definition.name, identifier)
			}
			index = end
			continue
		}
		if character >= '0' && character <= '9' {
			end := index
			hexadecimal := end+2 <= len(text) && text[end] == '0' && end+1 < len(text) &&
				(text[end+1] == 'x' || text[end+1] == 'X')
			if hexadecimal {
				end += 2
				digits := end
				for end < len(text) && (text[end] >= '0' && text[end] <= '9' ||
					text[end] >= 'a' && text[end] <= 'f' || text[end] >= 'A' && text[end] <= 'F') {
					end++
				}
				if end == digits {
					return fmt.Errorf("macro %s replacement contains a hexadecimal literal without digits", definition.name)
				}
			} else {
				end++
				for end < len(text) && text[end] >= '0' && text[end] <= '9' {
					if text[index] == '0' && text[end] >= '8' {
						return fmt.Errorf("macro %s replacement contains an invalid octal literal", definition.name)
					}
					end++
				}
			}
			suffixStart := end
			for end < len(text) && (text[end] == 'u' || text[end] == 'U' || text[end] == 'l' || text[end] == 'L') {
				end++
			}
			suffix := strings.ToLower(text[suffixStart:end])
			legalSuffix := suffix == "" || suffix == "u" || suffix == "l" || suffix == "ul" ||
				suffix == "lu" || suffix == "ll" || suffix == "ull" || suffix == "llu"
			if !legalSuffix {
				return fmt.Errorf("macro %s replacement contains invalid integer suffix %q", definition.name, text[suffixStart:end])
			}
			if end < len(text) && configDependencyLikeIdentifierByte(text[end]) {
				return fmt.Errorf("macro %s replacement joins an integer literal to an identifier", definition.name)
			}
			index = end
			continue
		}
		if strings.ContainsRune("()?:~!+-*/%<>=&|^,", rune(character)) {
			index++
			continue
		}
		return fmt.Errorf("macro %s replacement contains non-integer-expression byte %q", definition.name, character)
	}
	return nil
}

func configDependencyLikeIdentifierByte(character byte) bool {
	return character == '_' || character >= 'a' && character <= 'z' ||
		character >= 'A' && character <= 'Z' || character >= '0' && character <= '9'
}

// validateClosedIntegerMacroHeader proves an execution-generated header has
// no file edge and cannot introduce CONFIG identifiers into a consumer.  Its
// values may depend on the producer's separately validated config projection;
// the closure scanner therefore models its non-CONFIG definitions as unknown.
func validateClosedIntegerMacroHeader(data []byte) error {
	if bytes.Contains(data, []byte("??")) || bytes.Contains(data, []byte("%:")) {
		return fmt.Errorf("validated closed macro header contains a preprocessor alternate token")
	}
	// Phase-2 line splicing is required for ordinary multiline function macros.
	// Normalize it before comment/directive parsing so a splice cannot hide a
	// second directive from the grammar below.
	data = bytes.ReplaceAll(data, []byte("\\\r\n"), nil)
	data = bytes.ReplaceAll(data, []byte("\\\n"), nil)
	stripped, err := stripConfigIndependentMacroHeaderComments(data)
	if err != nil {
		return err
	}
	lines := []string{}
	for _, line := range strings.Split(string(stripped), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) < 3 {
		return fmt.Errorf("validated closed macro header has no complete outer guard")
	}
	ifndef := strings.Fields(lines[0])
	if len(ifndef) != 2 || ifndef[0] != "#ifndef" ||
		!configIndependentMacroHeaderIdentifier.MatchString(ifndef[1]) || strings.HasPrefix(ifndef[1], "CONFIG_") {
		return fmt.Errorf("validated closed macro header has an invalid outer guard")
	}
	guard := ifndef[1]
	guardDefine := strings.Fields(lines[1])
	if len(guardDefine) != 2 || guardDefine[0] != "#define" || guardDefine[1] != guard {
		return fmt.Errorf("validated closed macro header does not define its outer guard")
	}
	if lines[len(lines)-1] != "#endif" {
		return fmt.Errorf("validated closed macro header does not end at its outer guard")
	}
	definitions := make([]closedIntegerMacroDefinition, 0, len(lines)-3)
	macroNames := map[string]bool{guard: true}
	for ordinal, line := range lines[2 : len(lines)-1] {
		if !strings.HasPrefix(line, "#define") || len(line) == len("#define") ||
			line[len("#define")] != ' ' && line[len("#define")] != '\t' {
			return fmt.Errorf("validated closed macro header body line %d is not a macro definition", ordinal+1)
		}
		definition, err := closedIntegerMacroSignature(strings.TrimSpace(strings.TrimPrefix(line, "#define")))
		if err != nil {
			return fmt.Errorf("validated closed macro header body line %d: %w", ordinal+1, err)
		}
		if strings.HasPrefix(definition.name, "CONFIG_") || macroNames[definition.name] {
			return fmt.Errorf("validated closed macro header repeats or defines forbidden macro %q", definition.name)
		}
		macroNames[definition.name] = true
		definitions = append(definitions, definition)
	}
	for _, definition := range definitions {
		if err := validateClosedIntegerMacroReplacement(definition, macroNames); err != nil {
			return err
		}
	}
	return nil
}

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

func copyFile(input, output string, preserveMode bool) error {
	in, err := os.Open(input)
	if err != nil {
		return err
	}
	defer in.Close()
	outputMode := os.FileMode(0o644)
	if preserveMode {
		info, err := in.Stat()
		if err != nil {
			return err
		}
		outputMode |= info.Mode().Perm() & 0o111
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, outputMode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Chmod(output, outputMode)
}

// validateTreeOutputRoot permits the empty parent directories Bazel creates
// for declared TreeFile outputs, but rejects any pre-existing leaf or symlink.
// The action therefore cannot silently merge with stale output bytes.
func validateTreeOutputRoot(root string) error {
	info, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("inspect tree output %q: %w", root, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("tree output %q is not a real directory", root)
	}
	return filepath.WalkDir(root, func(filename string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if filename == root {
			return nil
		}
		if !entry.IsDir() {
			return fmt.Errorf("tree output %q contains pre-existing leaf %q", root, filename)
		}
		return nil
	})
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
	treeOut := flags.String("tree_out", "", "output directory optionally populated by -copy mappings")
	viewPlanRoot := flags.String("family_view_plan_root", "", "family-plan root containing exact view markers")
	viewVariant := flags.String("family_view_variant", "", "family-plan variant to project")
	viewTree := flags.String("family_view_tree", "", "family-plan logical tree to project")
	viewStoreRoot := flags.String("family_view_store_root", "", "content-addressed family store root")
	viewOutputRoot := flags.String("family_view_output_root", "", "declared public view output root")
	viewExpectedCount := flags.Int("family_view_expected_count", 0, "number of exact view markers")
	var viewMarkers repeatedLine
	flags.Var(&viewMarkers, "family_view_marker", "exact declared view marker (repeatable)")
	content := flags.String("content_base64", "", "base64-encoded file contents")
	preserveMode := flags.Bool("preserve_mode", false, "preserve executable permission bits from a single -input")
	validateMacroHeader := flags.Bool(validateConfigIndependentMacroHeaderFlag, false, "validate a guarded numeric object-macro header before publishing it")
	validateClosedMacroHeader := flags.Bool(validateClosedIntegerMacroHeaderFlag, false, "validate a guarded closed integer-expression macro header before publishing it")
	var inputs repeatedInput
	flags.Var(&inputs, "input", "input file to copy or concatenate byte-for-byte (repeatable)")
	var compareInputs repeatedInput
	flags.Var(&compareInputs, "compare_input", "input file to compare byte-for-byte (exactly two required)")
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
	provided := map[string]bool{}
	flags.Visit(func(value *flag.Flag) { provided[value.Name] = true })
	viewMode := provided["family_view_plan_root"] || provided["family_view_variant"] || provided["family_view_tree"] || provided["family_view_store_root"] || provided["family_view_output_root"] || provided["family_view_expected_count"] || provided["family_view_marker"]
	if viewMode {
		allowed := map[string]bool{
			"family_view_plan_root": true, "family_view_variant": true, "family_view_tree": true,
			"family_view_store_root": true, "family_view_output_root": true,
			"family_view_expected_count": true, "family_view_marker": true, "preserve_mode": true,
		}
		for name := range provided {
			if !allowed[name] {
				return fmt.Errorf("family view projection does not accept -%s", name)
			}
		}
		for name := range allowed {
			if !provided[name] {
				return fmt.Errorf("family view projection requires -%s", name)
			}
		}
		return projectFamilyView(*viewPlanRoot, *viewVariant, *viewTree, *viewStoreRoot, *viewOutputRoot, *viewExpectedCount, *preserveMode, viewMarkers)
	}
	if (*out == "") == (*treeOut == "") {
		return fmt.Errorf("exactly one of -out or -tree_out is required")
	}
	if *treeOut != "" {
		if provided["state"] || provided["content_base64"] || provided["input"] || provided["compare_input"] || provided["line"] {
			return fmt.Errorf("-tree_out accepts only -copy mappings and -preserve_mode")
		}
		seen := map[string]bool{}
		if err := os.MkdirAll(*treeOut, 0o755); err != nil {
			return fmt.Errorf("create tree output: %w", err)
		}
		if err := validateTreeOutputRoot(*treeOut); err != nil {
			return err
		}
		for _, copy := range copies {
			if seen[copy.relative] {
				return fmt.Errorf("tree copy path %q is repeated", copy.relative)
			}
			seen[copy.relative] = true
			if err := copyFile(copy.input, filepath.Join(*treeOut, filepath.FromSlash(copy.relative)), *preserveMode); err != nil {
				return fmt.Errorf("copy %q to %q: %w", copy.input, copy.relative, err)
			}
		}
		return nil
	}
	forms := 0
	for _, name := range []string{"state", "content_base64", "input", "compare_input", "line"} {
		if provided[name] {
			forms++
		}
	}
	if forms != 1 {
		return fmt.Errorf("exactly one of -state, -content_base64, -input, -compare_input, or -line is required")
	}
	if (*validateMacroHeader || *validateClosedMacroHeader) && (!provided["input"] || provided["preserve_mode"]) {
		name := validateConfigIndependentMacroHeaderFlag
		if *validateClosedMacroHeader {
			name = validateClosedIntegerMacroHeaderFlag
		}
		return fmt.Errorf("-%s requires input concatenation without mode preservation", name)
	}
	if *validateMacroHeader && *validateClosedMacroHeader {
		return fmt.Errorf("macro-header validation modes are mutually exclusive")
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
	} else if provided["compare_input"] {
		if len(compareInputs) != 2 {
			return fmt.Errorf("-compare_input requires exactly two inputs")
		}
		left, readErr := os.ReadFile(compareInputs[0])
		if readErr != nil {
			return fmt.Errorf("read -compare_input %q: %w", compareInputs[0], readErr)
		}
		right, readErr := os.ReadFile(compareInputs[1])
		if readErr != nil {
			return fmt.Errorf("read -compare_input %q: %w", compareInputs[1], readErr)
		}
		if !bytes.Equal(left, right) {
			return fmt.Errorf("-compare_input files differ")
		}
		data = []byte("validated\n")
	} else if provided["content_base64"] {
		data, err = base64.StdEncoding.DecodeString(*content)
		if err != nil {
			return fmt.Errorf("decode -content_base64: %w", err)
		}
	} else {
		data = []byte(strings.Join(lines, "\n") + "\n")
	}
	if *validateMacroHeader {
		if err := validateConfigIndependentMacroHeader(data); err != nil {
			return fmt.Errorf("-%s: %w", validateConfigIndependentMacroHeaderFlag, err)
		}
	}
	if *validateClosedMacroHeader {
		if err := validateClosedIntegerMacroHeader(data); err != nil {
			return fmt.Errorf("-%s: %w", validateClosedIntegerMacroHeaderFlag, err)
		}
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
