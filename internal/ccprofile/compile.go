package ccprofile

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

type ValidationStamp struct {
	ProfileDigest          string
	CompilerIdentityDigest string
	ValidationScope        string
}

func DecodeCommandTemplate(data []byte) (CommandTemplate, error) {
	if err := rejectDuplicateKeys(data); err != nil {
		return CommandTemplate{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var template CommandTemplate
	if err := decoder.Decode(&template); err != nil {
		return CommandTemplate{}, fmt.Errorf("decode CC command template: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return CommandTemplate{}, err
	}
	if err := ValidateCommandTemplate(template); err != nil {
		return CommandTemplate{}, fmt.Errorf("validate CC command template: %w", err)
	}
	return template, nil
}

func DecodeValidationStamp(data []byte) (ValidationStamp, error) {
	if len(data) == 0 || data[len(data)-1] != '\n' {
		return ValidationStamp{}, fmt.Errorf("validation stamp must end with a newline")
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) != 4 || lines[3] != "" {
		return ValidationStamp{}, fmt.Errorf("validation stamp must contain exactly three lines")
	}
	profileDigest, err := stampValue(lines[0], "profile_digest")
	if err != nil {
		return ValidationStamp{}, err
	}
	compilerIdentityDigest, err := stampValue(lines[1], "compiler_identity_digest")
	if err != nil {
		return ValidationStamp{}, err
	}
	validationScope, err := stampValue(lines[2], "validation_scope")
	if err != nil {
		return ValidationStamp{}, err
	}
	if err := validateDigest(profileDigest, "profile_digest"); err != nil {
		return ValidationStamp{}, err
	}
	if err := validateDigest(compilerIdentityDigest, "compiler_identity_digest"); err != nil {
		return ValidationStamp{}, err
	}
	if validationScope != "configured-graph" {
		return ValidationStamp{}, fmt.Errorf(
			"validation_scope %q, want %q",
			validationScope,
			"configured-graph",
		)
	}
	return ValidationStamp{
		ProfileDigest:          profileDigest,
		CompilerIdentityDigest: compilerIdentityDigest,
		ValidationScope:        validationScope,
	}, nil
}

func stampValue(line, name string) (string, error) {
	prefix := name + "="
	if !strings.HasPrefix(line, prefix) {
		return "", fmt.Errorf("validation stamp line for %s is missing or out of order", name)
	}
	value := strings.TrimPrefix(line, prefix)
	if value == "" || strings.ContainsRune(value, '\x00') {
		return "", fmt.Errorf("validation stamp %s must be non-empty and contain no NUL", name)
	}
	return value, nil
}

func validateDigest(value, name string) error {
	if len(value) != 64 || strings.ToLower(value) != value {
		return fmt.Errorf("validation stamp %s must be a lowercase SHA-256 digest", name)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return fmt.Errorf("validation stamp %s must be a lowercase SHA-256 digest", name)
	}
	return nil
}

// PrepareCompileArgv expands the one Kbuild insertion point and then applies
// exact removals to every top-level mutable argument, regardless of whether it
// came from the selected toolchain or the object. Response-file arguments are
// deliberately opaque; their contents must be filtered before this command.
func PrepareCompileArgv(
	template CommandTemplate,
	source string,
	output string,
	objectArgs []string,
	removals []string,
) ([]string, error) {
	if err := ValidateCommandTemplate(template); err != nil {
		return nil, err
	}
	if err := validateCompilePath(source, "source"); err != nil {
		return nil, err
	}
	if err := validateCompilePath(output, "output"); err != nil {
		return nil, err
	}
	if source == output {
		return nil, fmt.Errorf("source and output must be distinct")
	}

	for index, arg := range template.MutableArgv {
		if arg == template.KbuildFlagsSentinel {
			continue
		}
		if err := validateMutableCompileValue(
			arg,
			fmt.Sprintf("template argument %d", index),
			template.KbuildFlagsSentinel,
			source,
			output,
		); err != nil {
			return nil, err
		}
	}
	for index, arg := range objectArgs {
		if err := validateMutableCompileValue(
			arg,
			fmt.Sprintf("object argument %d", index),
			template.KbuildFlagsSentinel,
			source,
			output,
		); err != nil {
			return nil, err
		}
	}
	removeSet := make(map[string]bool, len(removals))
	for index, value := range removals {
		if err := validateMutableCompileValue(
			value,
			fmt.Sprintf("removal %d", index),
			template.KbuildFlagsSentinel,
			source,
			output,
		); err != nil {
			return nil, err
		}
		if removeSet[value] {
			return nil, fmt.Errorf("removal %q is repeated", value)
		}
		removeSet[value] = true
	}

	expanded := make([]string, 0, len(template.MutableArgv)+len(objectArgs)-1)
	sentinelCount := 0
	for _, arg := range template.MutableArgv {
		if arg == template.KbuildFlagsSentinel {
			sentinelCount++
			expanded = append(expanded, objectArgs...)
			continue
		}
		expanded = append(expanded, arg)
	}
	if sentinelCount != 1 {
		return nil, fmt.Errorf("mutable_argv must contain exactly one Kbuild flags sentinel, got %d", sentinelCount)
	}

	argv := make([]string, 0, len(expanded)+4)
	for _, arg := range expanded {
		if !removeSet[arg] {
			argv = append(argv, arg)
		}
	}
	argv = append(argv, "-c", source, "-o", output)
	return argv, nil
}

func Compile(
	ctx context.Context,
	template CommandTemplate,
	source string,
	output string,
	objectArgs []string,
	removals []string,
	stdout io.Writer,
	stderr io.Writer,
) error {
	argv, err := PrepareCompileArgv(template, source, output, objectArgs, removals)
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, template.Compiler, argv...)
	command.Env = environmentList(template.Environment)
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("execute compiler: %w", err)
	}
	return nil
}

func validateCompilePath(value, name string) error {
	if err := validateText(value, name); err != nil {
		return err
	}
	if strings.HasPrefix(value, "-") {
		return fmt.Errorf("%s %q must not begin with -", name, value)
	}
	return nil
}

func validateMutableCompileValue(
	value string,
	context string,
	kbuildFlagsSentinel string,
	source string,
	output string,
) error {
	if value == "" || strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("%s must be non-empty and contain no NUL", context)
	}
	if strings.Contains(value, kbuildFlagsSentinel) {
		return fmt.Errorf("%s attempts to inject the Kbuild flags sentinel", context)
	}
	if value == source || value == output {
		return fmt.Errorf("%s attempts to inject a source/output skeleton token", context)
	}
	if value == "-c" || value == "-E" || value == "-S" || value == "--" {
		return fmt.Errorf("%s attempts to inject structural compiler token %q", context, value)
	}
	if isGNUOutputOption(value) || value == "-frandom-seed="+output {
		return fmt.Errorf("%s attempts to inject an output skeleton token %q", context, value)
	}
	return nil
}

func isGNUOutputOption(value string) bool {
	return value == "-o" || (len(value) > 2 && strings.HasPrefix(value, "-o"))
}
