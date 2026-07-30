package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
	whenTrue, err := readArgvFile(*whenTruePath)
	if err != nil {
		return fmt.Errorf("read true branch: %w", err)
	}
	whenFalse, err := readArgvFile(*whenFalsePath)
	if err != nil {
		return fmt.Errorf("read false branch: %w", err)
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
		template.AnalysisIdentity,
		request,
		ccprofile.KbuildGraphProbeTools{
			Compiler:    template.Compiler,
			Linker:      *linker,
			SourceRoot:  *sourceRoot,
			ObjectRoot:  *objectRoot,
			Environment: template.Environment,
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
	flagsPath := flags.String("flags_file", "", "newline-delimited resolved linker arguments")
	removeFlagsPath := flags.String("remove_flags_file", "", "newline-delimited exact removals")
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
		*flagsPath == "" ||
		*removeFlagsPath == "" ||
		*output == "" {
		return fmt.Errorf(
			"ccprofile link requires linker, validation, flags_file, remove_flags_file, output, and no positional arguments",
		)
	}
	if err := readValidationStamp(*validationPath); err != nil {
		return err
	}
	resolved, err := readArgvFile(*flagsPath)
	if err != nil {
		return fmt.Errorf("read resolved linker flags: %w", err)
	}
	removals, err := readArgvFile(*removeFlagsPath)
	if err != nil {
		return fmt.Errorf("read linker removals: %w", err)
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
	filtered, err := filterExactArgs(
		append(append([]string(nil), baseArgv...), resolved...),
		removals,
	)
	if err != nil {
		return err
	}
	linkArgv := filtered
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

func filterExactArgs(argv, removals []string) ([]string, error) {
	if err := validateArgv(argv, "linker argv"); err != nil {
		return nil, err
	}
	if err := validateArgv(removals, "linker removals"); err != nil {
		return nil, err
	}
	removeSet := make(map[string]bool, len(removals))
	for _, removal := range removals {
		if removeSet[removal] {
			return nil, fmt.Errorf("linker removal %q is repeated", removal)
		}
		removeSet[removal] = true
	}
	filtered := make([]string, 0, len(argv))
	for _, arg := range argv {
		if !removeSet[arg] {
			filtered = append(filtered, arg)
		}
	}
	return filtered, nil
}

func expandArgvResponseFiles(argv []string) ([]string, error) {
	return expandArgvResponseFilesWithStack(argv, map[string]bool{})
}

func expandArgvResponseFilesWithStack(
	argv []string,
	stack map[string]bool,
) ([]string, error) {
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
		nested, err := readArgvFile(path)
		if err != nil {
			delete(stack, path)
			return nil, fmt.Errorf("read response file %s: %w", path, err)
		}
		nested, err = expandArgvResponseFilesWithStack(nested, stack)
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
	source string,
	sourceRoot string,
	objectPath string,
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
	replacements := map[string]string{
		"src":     filepath.ToSlash(filepath.Dir(source)),
		"srctree": filepath.ToSlash(sourceRoot),
	}
	if objectPath != "" {
		replacements["obj"] = filepath.ToSlash(filepath.Dir(objectPath))
	}
	for _, name := range []string{
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
	} {
		replacements[name] = ""
	}

	out := make([]string, 0, len(argv))
	for index, original := range argv {
		value := original
		for name, replacement := range replacements {
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
			return nil, fmt.Errorf(
				"argument %d requires an object-local utsversion-tmp.h: %q",
				index,
				value,
			)
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
