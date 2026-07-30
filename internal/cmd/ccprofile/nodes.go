package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/ccprofile"
)

func runResolveNode(args []string) error {
	flags := newFlagSet("ccprofile resolve-node")
	templatePath := flags.String("template", "", "validated CC command template")
	validationPath := flags.String("validation", "", "configured graph validation stamp")
	linker := flags.String("linker", "", "selected linker executable")
	kind := flags.String("kind", "", "Kbuild graph probe kind")
	language := flags.String("language", "", "Kbuild graph probe language")
	srcarch := flags.String("srcarch", "", "Linux SRCARCH")
	contextPath := flags.String("context", "", "newline-delimited probe context argv")
	whenTruePath := flags.String("when_true", "", "newline-delimited true-branch argv")
	whenFalsePath := flags.String("when_false", "", "newline-delimited false-branch argv")
	sourceRoot := flags.String("source_root", "", "Linux source root")
	objectRoot := flags.String("object_root", "", "Linux object root")
	out := flags.String("out", "", "selected newline-delimited argv")
	var candidateArgv repeatedStringFlag
	flags.Var(&candidateArgv, "candidate_arg", "candidate argument; repeat in command-line order")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 ||
		*templatePath == "" ||
		*validationPath == "" ||
		*linker == "" ||
		*kind == "" ||
		*language == "" ||
		*srcarch == "" ||
		*contextPath == "" ||
		*whenTruePath == "" ||
		*whenFalsePath == "" ||
		*sourceRoot == "" ||
		*out == "" {
		return fmt.Errorf(
			"ccprofile resolve-node requires template, validation, linker, kind, language, srcarch, context, both branches, source_root, out, and no positional arguments",
		)
	}
	template, err := readCommandTemplate(*templatePath)
	if err != nil {
		return err
	}
	if err := readValidationStamp(*validationPath); err != nil {
		return err
	}
	if err := validateSRCARCH(template.Architecture, *srcarch); err != nil {
		return err
	}
	if err := validateArgv(candidateArgv, "candidate argv"); err != nil {
		return err
	}
	contextArgv, err := readArgvFile(*contextPath)
	if err != nil {
		return fmt.Errorf("read context program: %w", err)
	}
	contextArgv, err = expandKbuildProbeMakeRefs(
		contextArgv,
		*sourceRoot,
		*objectRoot,
	)
	if err != nil {
		return fmt.Errorf("expand context program: %w", err)
	}
	candidateArgv, err = expandKbuildProbeMakeRefs(
		candidateArgv,
		*sourceRoot,
		*objectRoot,
	)
	if err != nil {
		return fmt.Errorf("expand candidate argv: %w", err)
	}
	whenTrue, err := readArgvFile(*whenTruePath)
	if err != nil {
		return fmt.Errorf("read true branch: %w", err)
	}
	whenFalse, err := readArgvFile(*whenFalsePath)
	if err != nil {
		return fmt.Errorf("read false branch: %w", err)
	}
	if len(candidateArgv) == 0 {
		if err := writeArgvFile(*out, whenFalse); err != nil {
			return fmt.Errorf("write selected false branch for empty candidate: %w", err)
		}
		return nil
	}
	request, err := ccprofile.NewKbuildGraphProbeIdentity(
		*kind,
		*language,
		contextArgv,
		candidateArgv,
		map[string]string{},
	)
	if err != nil {
		return fmt.Errorf("construct Kbuild graph probe: %w", err)
	}
	supported, err := ccprofile.EvaluateKbuildGraphProbe(
		context.Background(),
		request,
		ccprofile.KbuildGraphProbeTools{
			CommandTemplate: template,
			Linker:          *linker,
			SourceRoot:      *sourceRoot,
			ObjectRoot:      *objectRoot,
		},
	)
	if err != nil {
		return fmt.Errorf("resolve Kbuild graph probe node: %w", err)
	}
	selected := whenFalse
	if supported {
		selected = whenTrue
	}
	if err := writeArgvFile(*out, selected); err != nil {
		return fmt.Errorf("write selected branch: %w", err)
	}
	return nil
}

func expandKbuildProbeMakeRefs(
	argv []string,
	sourceRoot string,
	objectRoot string,
) ([]string, error) {
	replacements := map[string]string{
		"obj":     filepath.ToSlash(objectRoot),
		"srctree": filepath.ToSlash(sourceRoot),
	}
	for _, name := range kbuildKnownEmptyMakeRefs {
		replacements[name] = ""
	}
	names := make([]string, 0, len(replacements))
	for name := range replacements {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]string, 0, len(argv))
	for index, original := range argv {
		value := original
		for _, name := range names {
			replacement := replacements[name]
			for _, reference := range []string{"$(" + name + ")", "${" + name + "}"} {
				if strings.Contains(value, reference) && replacement == "" &&
					(name == "srctree" || name == "obj") {
					return nil, fmt.Errorf(
						"argument %d requires a path for Make reference %s",
						index,
						reference,
					)
				}
				value = strings.ReplaceAll(value, reference, replacement)
			}
		}
		if strings.Contains(value, "$(") || strings.Contains(value, "${") {
			return nil, fmt.Errorf(
				"argument %d has unsupported Kbuild Make reference: %q",
				index,
				value,
			)
		}
		if value != "" {
			out = append(out, value)
		}
	}
	if err := validateArgv(out, "expanded Kbuild probe argv"); err != nil {
		return nil, err
	}
	if err := rejectResponseFileArguments(out, "expanded Kbuild probe argv"); err != nil {
		return nil, err
	}
	return out, nil
}

func runConcatNode(args []string) error {
	flags := newFlagSet("ccprofile concat-node")
	out := flags.String("out", "", "concatenated newline-delimited argv")
	var inputs repeatedStringFlag
	flags.Var(&inputs, "input", "newline-delimited argv input; repeat in order")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *out == "" {
		return fmt.Errorf("ccprofile concat-node requires -out and no positional arguments")
	}
	var combined []string
	for index, path := range inputs {
		argv, err := readArgvFile(path)
		if err != nil {
			return fmt.Errorf("read concat input %d: %w", index, err)
		}
		combined = append(combined, argv...)
	}
	if err := writeArgvFile(*out, combined); err != nil {
		return fmt.Errorf("write concatenated node: %w", err)
	}
	return nil
}

func runLink(args []string) error {
	flags := newFlagSet("ccprofile link")
	linker := flags.String("linker", "", "selected raw linker")
	validationPath := flags.String("validation", "", "graph profile validation stamp")
	output := flags.String("output", "", "link output")
	linkerScript := flags.String("linker_script", "", "optional raw linker script")
	var inputs repeatedStringFlag
	var baseArgv repeatedStringFlag
	flags.Var(&inputs, "input", "link input; repeat in order")
	flags.Var(&baseArgv, "base_arg", "base raw-linker argument; repeat in order")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 ||
		*linker == "" ||
		*validationPath == "" ||
		*output == "" {
		return fmt.Errorf(
			"ccprofile link requires linker, validation, output, and no positional arguments",
		)
	}
	if err := readValidationStamp(*validationPath); err != nil {
		return err
	}
	if err := validateArgv(baseArgv, "base linker argv"); err != nil {
		return err
	}
	if err := validateArgv(inputs, "link inputs"); err != nil {
		return err
	}
	if err := validatePathToken(*output, "link output"); err != nil {
		return err
	}
	if *linkerScript != "" {
		if err := validatePathToken(*linkerScript, "linker script"); err != nil {
			return err
		}
	}
	linkArgv := append([]string(nil), baseArgv...)
	if *linkerScript != "" {
		linkArgv = append(linkArgv, "-T", *linkerScript)
	}
	linkArgv = append(linkArgv, "-o", *output)
	linkArgv = append(linkArgv, inputs...)
	command := exec.CommandContext(context.Background(), *linker, linkArgv...)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("execute linker: %w", err)
	}
	return nil
}

func newFlagSet(name string) *flagSet {
	return newDiscardFlagSet(name)
}

// flagSet is the subset of flag.FlagSet used by node commands. The alias keeps
// construction centralized without changing the standard parser semantics.
type flagSet = flag.FlagSet

func newDiscardFlagSet(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	return flags
}

func readCommandTemplate(path string) (ccprofile.CommandTemplate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ccprofile.CommandTemplate{}, fmt.Errorf("read command template %s: %w", path, err)
	}
	template, err := ccprofile.DecodeCommandTemplate(data)
	if err != nil {
		return ccprofile.CommandTemplate{}, fmt.Errorf("decode command template %s: %w", path, err)
	}
	return template, nil
}

func readValidationStamp(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read validation stamp %s: %w", path, err)
	}
	if _, err := ccprofile.DecodeValidationStamp(data); err != nil {
		return fmt.Errorf("decode validation stamp %s: %w", path, err)
	}
	return nil
}

func validateSRCARCH(architecture, srcarch string) error {
	expected := map[string]string{
		"x86_64":  "x86",
		"aarch64": "arm64",
	}[architecture]
	if srcarch != expected {
		return fmt.Errorf(
			"srcarch %q does not match command-template architecture %q (want %q)",
			srcarch,
			architecture,
			expected,
		)
	}
	return nil
}

func readArgvFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return []string{}, nil
	}
	if data[len(data)-1] != '\n' {
		return nil, fmt.Errorf("argv file %s must end with a newline", path)
	}
	if strings.ContainsAny(string(data), "\r\x00") {
		return nil, fmt.Errorf("argv file %s contains CR or NUL", path)
	}
	argv := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if err := validateArgv(argv, "argv file "+path); err != nil {
		return nil, err
	}
	return argv, nil
}

func readGNUResponseFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return nil, fmt.Errorf("response file %s contains NUL", path)
	}

	var argv []string
	var token strings.Builder
	for index := 0; index < len(data); index++ {
		if token.Len() == 0 {
			for index < len(data) && isClangResponseWhitespace(data[index]) {
				index++
			}
			if index == len(data) {
				break
			}
		}
		value := data[index]
		if value == '\\' && index+1 < len(data) {
			index++
			token.WriteByte(data[index])
			continue
		}
		if value == '\'' || value == '"' {
			quote := value
			index++
			for index < len(data) && data[index] != quote {
				if data[index] == '\\' && index+1 < len(data) {
					index++
				}
				token.WriteByte(data[index])
				index++
			}
			if index == len(data) {
				break
			}
			continue
		}
		if isClangResponseWhitespace(value) {
			if token.Len() != 0 {
				argv = append(argv, token.String())
				token.Reset()
			}
			continue
		}
		token.WriteByte(value)
	}
	if token.Len() != 0 {
		argv = append(argv, token.String())
	}
	if err := validateArgv(argv, "response file "+path); err != nil {
		return nil, err
	}
	return argv, nil
}

func isClangResponseWhitespace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\r' || value == '\n'
}

func writeArgvFile(path string, argv []string) error {
	if err := validateArgv(argv, "output argv"); err != nil {
		return err
	}
	data := []byte{}
	if len(argv) != 0 {
		data = []byte(strings.Join(argv, "\n") + "\n")
	}
	return os.WriteFile(path, data, 0o644)
}

func validateArgv(argv []string, context string) error {
	for index, arg := range argv {
		if arg == "" || strings.ContainsAny(arg, "\r\n\x00") {
			return fmt.Errorf(
				"%s[%d] must be non-empty and contain no CR, LF, or NUL",
				context,
				index,
			)
		}
	}
	return nil
}

func validatePathToken(value, context string) error {
	if value == "" || strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("%s must be non-empty and contain no CR, LF, or NUL", context)
	}
	return nil
}

func expandKbuildResponseFiles(
	argv []string,
	configValues map[string]string,
	source string,
	sourceRoot string,
	objectPath string,
	objectRoot string,
	utsversionTmp string,
) ([]string, error) {
	return expandKbuildResponseFilesWithStack(
		argv,
		configValues,
		source,
		sourceRoot,
		objectPath,
		objectRoot,
		utsversionTmp,
		map[string]bool{},
	)
}

func expandKbuildProgramArgv(
	argv []string,
	configValues map[string]string,
	source string,
	sourceRoot string,
	objectPath string,
	objectRoot string,
	utsversionTmp string,
) ([]string, error) {
	expanded, err := expandKbuildArgv(
		argv,
		configValues,
		source,
		sourceRoot,
		objectPath,
		objectRoot,
		utsversionTmp,
	)
	if err != nil {
		return nil, err
	}
	if err := rejectResponseFileArguments(expanded, "expanded Kbuild flag program"); err != nil {
		return nil, err
	}
	return expanded, nil
}

func rejectResponseFileArguments(argv []string, context string) error {
	for index, arg := range argv {
		if strings.HasPrefix(arg, "@") {
			return fmt.Errorf(
				"%s[%d] contains unsupported response-file argument %q",
				context,
				index,
				arg,
			)
		}
	}
	return nil
}

func expandKbuildResponseFilesWithStack(
	argv []string,
	configValues map[string]string,
	source string,
	sourceRoot string,
	objectPath string,
	objectRoot string,
	utsversionTmp string,
	stack map[string]bool,
) ([]string, error) {
	argv, err := expandKbuildArgv(
		argv,
		configValues,
		source,
		sourceRoot,
		objectPath,
		objectRoot,
		utsversionTmp,
	)
	if err != nil {
		return nil, err
	}
	var expanded []string
	for _, arg := range argv {
		if !strings.HasPrefix(arg, "@") {
			expanded = append(expanded, arg)
			continue
		}
		path := strings.TrimPrefix(arg, "@")
		if path == "" {
			return nil, fmt.Errorf("response-file argument %q has an empty path", arg)
		}
		path = filepath.Clean(path)
		if stack[path] {
			return nil, fmt.Errorf("response-file cycle at %s", path)
		}
		stack[path] = true
		nested, err := readGNUResponseFile(path)
		if err != nil {
			delete(stack, path)
			return nil, fmt.Errorf("read response file %s: %w", path, err)
		}
		nested, err = expandKbuildResponseFilesWithStack(
			nested,
			configValues,
			source,
			sourceRoot,
			objectPath,
			objectRoot,
			utsversionTmp,
			stack,
		)
		delete(stack, path)
		if err != nil {
			return nil, err
		}
		expanded = append(expanded, nested...)
	}
	return expanded, nil
}

func expandKbuildArgv(
	argv []string,
	configValues map[string]string,
	source string,
	sourceRoot string,
	objectPath string,
	objectRoot string,
	utsversionTmp string,
) ([]string, error) {
	if err := validatePathToken(source, "compile source"); err != nil {
		return nil, err
	}
	if sourceRoot != "" {
		if err := validatePathToken(sourceRoot, "source root"); err != nil {
			return nil, err
		}
	}
	if objectPath != "" {
		if err := validatePathToken(objectPath, "object path"); err != nil {
			return nil, err
		}
	}
	if objectRoot != "" {
		if err := validatePathToken(objectRoot, "object root"); err != nil {
			return nil, err
		}
	}
	if utsversionTmp != "" {
		if err := validatePathToken(utsversionTmp, "utsversion-tmp.h"); err != nil {
			return nil, err
		}
	}
	replacements := make(map[string]string, len(configValues)+12)
	for name, value := range configValues {
		replacements[name] = value
	}
	replacements["src"] = filepath.ToSlash(filepath.Dir(source))
	replacements["srctree"] = filepath.ToSlash(sourceRoot)
	if objectRoot != "" {
		replacements["obj"] = filepath.ToSlash(objectRoot)
	} else if objectPath != "" {
		replacements["obj"] = filepath.ToSlash(filepath.Dir(objectPath))
	}
	for _, name := range kbuildKnownEmptyMakeRefs {
		replacements[name] = ""
	}
	replacementNames := make([]string, 0, len(replacements))
	for name := range replacements {
		replacementNames = append(replacementNames, name)
	}
	sort.Strings(replacementNames)

	out := make([]string, 0, len(argv))
	for index, original := range argv {
		value := original
		for _, name := range replacementNames {
			replacement := replacements[name]
			for _, reference := range []string{"$(" + name + ")", "${" + name + "}"} {
				if strings.Contains(value, reference) && replacement == "" &&
					(name == "srctree" || name == "obj") {
					return nil, fmt.Errorf(
						"argument %d requires -%s for Make reference %s",
						index,
						map[string]string{"srctree": "source_root", "obj": "object_path"}[name],
						reference,
					)
				}
				value = strings.ReplaceAll(value, reference, replacement)
			}
		}
		if strings.Contains(value, "$(") || strings.Contains(value, "${") {
			return nil, fmt.Errorf(
				"argument %d has unresolved Kbuild Make reference: %q",
				index,
				value,
			)
		}
		if strings.Contains(value, "utsversion-tmp.h") {
			rewritten, ok := rewriteUTSVersionTmp(
				value,
				objectPath,
				objectRoot,
				utsversionTmp,
			)
			if !ok {
				return nil, fmt.Errorf(
					"argument %d requires an object-local utsversion-tmp.h: %q",
					index,
					value,
				)
			}
			value = rewritten
		}
		if value != "" {
			out = append(out, value)
		}
	}
	if err := validateArgv(out, "expanded Kbuild argv"); err != nil {
		return nil, err
	}
	return out, nil
}

var kbuildKnownEmptyMakeRefs = []string{
	"CC_FLAGS_CFI",
	"CC_FLAGS_FTRACE",
	"CC_FLAGS_LTO",
	"CC_FLAGS_SCS",
	"CLANG_FLAGS",
	"DISABLE_KSTACK_ERASE",
	"DISABLE_LATENT_ENTROPY_PLUGIN",
	"DISABLE_STACKLEAK_PLUGIN",
	"RANDSTRUCT_CFLAGS",
	"cflags-nogcse-yy",
}

func readKbuildConfig(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	values := map[string]string{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var name, value string
		switch {
		case strings.HasPrefix(line, "# CONFIG_") &&
			strings.HasSuffix(line, " is not set"):
			name = strings.TrimSuffix(
				strings.TrimPrefix(line, "# "),
				" is not set",
			)
			value = "n"
		case strings.HasPrefix(line, "#"):
			continue
		default:
			var ok bool
			name, value, ok = strings.Cut(line, "=")
			if !ok {
				return nil, fmt.Errorf(
					"%s:%d: expected CONFIG_* assignment",
					path,
					lineNumber,
				)
			}
			name = strings.TrimSpace(name)
			value = strings.TrimSpace(value)
		}
		if !strings.HasPrefix(name, "CONFIG_") ||
			len(name) == len("CONFIG_") {
			return nil, fmt.Errorf(
				"%s:%d: expected CONFIG_* key, got %q",
				path,
				lineNumber,
				name,
			)
		}
		if _, exists := values[name]; exists {
			return nil, fmt.Errorf(
				"%s:%d: duplicate config key %q",
				path,
				lineNumber,
				name,
			)
		}
		values[name] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return values, nil
}
func rewriteUTSVersionTmp(
	value string,
	objectPath string,
	objectRoot string,
	utsversionTmp string,
) (string, bool) {
	if utsversionTmp == "" {
		return "", false
	}
	candidates := map[string]bool{
		"utsversion-tmp.h": true,
	}
	if objectPath != "" {
		objectDir := filepath.ToSlash(filepath.Dir(objectPath))
		if objectDir != "." {
			candidates[objectDir+"/utsversion-tmp.h"] = true
		}
	}
	if objectRoot != "" {
		candidates[filepath.ToSlash(filepath.Join(objectRoot, "utsversion-tmp.h"))] = true
	}
	if !candidates[filepath.ToSlash(value)] {
		return "", false
	}
	return utsversionTmp, true
}
