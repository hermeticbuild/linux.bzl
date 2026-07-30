package ccprofile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type KbuildGraphProbeTools struct {
	CommandTemplate CommandTemplate
	Archiver        string
	Coreutils       string
	Grep            string
	Linker          string
	NM              string
	Objcopy         string
	Shell           string
	SourceRoot      string
	ObjectRoot      string
}

var configuredKconfigCoreutils = []string{
	"cat",
	"dirname",
	"env",
	"head",
	"mkdir",
	"mktemp",
	"rm",
}

// EvaluateConfiguredKconfigCommand executes one command missing from a seeded
// graph profile using the selected tools. Relative paths are resolved from the
// Linux source root, matching Kconfig's configured-tree maintenance workflow.
func EvaluateConfiguredKconfigCommand(
	ctx context.Context,
	commandText string,
	environment map[string]string,
	tools KbuildGraphProbeTools,
) (string, error) {
	if err := validateConfiguredKconfigTools(tools); err != nil {
		return "", fmt.Errorf("configured Kconfig command: %w", err)
	}
	if tools.Shell == "" {
		return "", fmt.Errorf("configured Kconfig command shell must be non-empty")
	}
	if tools.SourceRoot == "" {
		return "", fmt.Errorf("configured Kconfig command source root must be non-empty")
	}
	workDir, err := os.MkdirTemp("", "linux-bzl-kconfig-command-")
	if err != nil {
		return "", fmt.Errorf("create configured Kconfig command directory: %w", err)
	}
	defer os.RemoveAll(workDir)
	shimDir := filepath.Join(workDir, "bin")
	if err := os.Mkdir(shimDir, 0o755); err != nil {
		return "", fmt.Errorf("create configured Kconfig command tool directory: %w", err)
	}
	createdShims := map[string]string{}
	for _, utility := range configuredKconfigCoreutils {
		if err := ensureToolShim(shimDir, createdShims, utility, tools.Coreutils); err != nil {
			return "", err
		}
	}
	if err := ensureToolShim(shimDir, createdShims, "grep", tools.Grep); err != nil {
		return "", err
	}
	if err := ensureConfiguredKconfigToolShims(
		shimDir,
		createdShims,
		environment,
		tools,
	); err != nil {
		return "", err
	}
	sourceRoot := absolutePathOrOriginal(tools.SourceRoot)
	commandText = strings.ReplaceAll(
		commandText,
		GraphProfileSourceRoot,
		filepath.ToSlash(filepath.Clean(sourceRoot)),
	)
	command := exec.CommandContext(ctx, tools.Shell, "-c", commandText)
	command.Dir = sourceRoot
	runtimeEnvironment := cloneStringMap(tools.CommandTemplate.Environment)
	for name, value := range environment {
		runtimeEnvironment[name] = value
	}
	runtimeEnvironment["PATH"] = shimDir
	command.Env = environmentList(runtimeEnvironment)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, commandErr := command.Output()
	if commandErr != nil {
		var exitError *exec.ExitError
		if !errors.As(commandErr, &exitError) {
			return "", fmt.Errorf("execute configured Kconfig command: %w", commandErr)
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return normalizeKconfigCommandOutput(output), nil
}

// RefreshConfiguredKconfigCommands evaluates every Kconfig command that
// directly invokes the configured compiler/linker or one of Linux's compiler
// and linker probe scripts. Other deterministic model results are preserved.
func RefreshConfiguredKconfigCommands(
	ctx context.Context,
	commands []KconfigCommand,
	tools KbuildGraphProbeTools,
) ([]KconfigCommand, int, error) {
	return refreshConfiguredKconfigCommands(ctx, commands, tools, false)
}

func refreshConfiguredKconfigCommands(
	ctx context.Context,
	commands []KconfigCommand,
	tools KbuildGraphProbeTools,
	requireMatchingInputs bool,
) ([]KconfigCommand, int, error) {
	if err := validateConfiguredKconfigTools(tools); err != nil {
		return nil, 0, fmt.Errorf("configured Kconfig replay: %w", err)
	}
	if tools.Shell == "" {
		return nil, 0, fmt.Errorf("configured Kconfig replay shell must be non-empty")
	}
	if tools.SourceRoot == "" {
		return nil, 0, fmt.Errorf("configured Kconfig replay source root must be non-empty")
	}
	workDir, err := os.MkdirTemp("", "linux-bzl-kconfig-replay-")
	if err != nil {
		return nil, 0, fmt.Errorf("create configured Kconfig replay directory: %w", err)
	}
	defer os.RemoveAll(workDir)
	shimDir := filepath.Join(workDir, "bin")
	if err := os.Mkdir(shimDir, 0o755); err != nil {
		return nil, 0, fmt.Errorf("create configured Kconfig replay tool directory: %w", err)
	}
	createdShims := map[string]string{}
	for _, utility := range configuredKconfigCoreutils {
		if err := ensureToolShim(shimDir, createdShims, utility, tools.Coreutils); err != nil {
			return nil, 0, err
		}
	}
	if err := ensureToolShim(shimDir, createdShims, "grep", tools.Grep); err != nil {
		return nil, 0, err
	}
	refreshed := make([]KconfigCommand, len(commands))
	replayed := 0
	for index, expected := range commands {
		refreshed[index] = cloneKconfigCommand(expected)
		if !isConfiguredKconfigCommand(expected.Command, expected.Environment) {
			continue
		}
		inputsMatch, err := configuredGraphInputsMatch(
			expected.Inputs,
			tools.SourceRoot,
		)
		if err != nil {
			return nil, replayed, fmt.Errorf(
				"match configured Kconfig command %s inputs: %w",
				expected.ID,
				err,
			)
		}
		if !inputsMatch {
			if requireMatchingInputs {
				return nil, replayed, fmt.Errorf(
					"configured Kconfig command %s inputs do not match the current source tree",
					expected.ID,
				)
			}
			continue
		}
		if err := ensureConfiguredKconfigToolShims(
			shimDir,
			createdShims,
			expected.Environment,
			tools,
		); err != nil {
			return nil, replayed, err
		}
		sourceRoot := absolutePathOrOriginal(tools.SourceRoot)
		commandText := strings.ReplaceAll(
			expected.Command,
			GraphProfileSourceRoot,
			filepath.ToSlash(filepath.Clean(sourceRoot)),
		)
		command := exec.CommandContext(ctx, tools.Shell, "-c", commandText)
		command.Dir = workDir
		environment := cloneStringMap(tools.CommandTemplate.Environment)
		for name, value := range expected.Environment {
			environment[name] = value
		}
		environment["PATH"] = shimDir
		command.Env = environmentList(environment)
		var stderr bytes.Buffer
		command.Stderr = &stderr
		output, commandErr := command.Output()
		replayed++
		switch expected.Kind {
		case KconfigCommandKindSuccess:
			succeeded := commandErr == nil
			var exitError *exec.ExitError
			if commandErr != nil && !errors.As(commandErr, &exitError) {
				return nil, replayed, fmt.Errorf(
					"replay configured Kconfig command %s: %w",
					expected.ID,
					commandErr,
				)
			}
			refreshed[index].Success = &succeeded
		case KconfigCommandKindStdout:
			if commandErr != nil {
				detail := strings.TrimSpace(stderr.String())
				if detail != "" {
					return nil, replayed, fmt.Errorf(
						"replay configured Kconfig stdout command %s: %w: %s",
						expected.ID,
						commandErr,
						detail,
					)
				}
				return nil, replayed, fmt.Errorf(
					"replay configured Kconfig stdout command %s: %w",
					expected.ID,
					commandErr,
				)
			}
			actual := normalizeKconfigCommandOutput(output)
			refreshed[index].Stdout = &actual
		default:
			return nil, replayed, fmt.Errorf(
				"configured Kconfig command %s has unsupported kind %q",
				expected.ID,
				expected.Kind,
			)
		}
	}
	return refreshed, replayed, nil
}

func configuredGraphInputsMatch(inputs map[string]string, sourceRoot string) (bool, error) {
	for path, expected := range inputs {
		clean := filepath.Clean(filepath.FromSlash(path))
		if clean == "." ||
			filepath.IsAbs(clean) ||
			clean == ".." ||
			strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return false, fmt.Errorf("input path %q must stay beneath the source root", path)
		}
		data, err := os.ReadFile(filepath.Join(sourceRoot, clean))
		if os.IsNotExist(err) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("read input %q: %w", path, err)
		}
		actual := fmt.Sprintf("%x", sha256.Sum256(data))
		if actual != expected {
			return false, nil
		}
	}
	return true, nil
}

func replayConfiguredKconfigCommands(
	ctx context.Context,
	commands []KconfigCommand,
	tools KbuildGraphProbeTools,
	requireMatchingInputs bool,
) (int, error) {
	refreshed, replayed, err := refreshConfiguredKconfigCommands(
		ctx,
		commands,
		tools,
		requireMatchingInputs,
	)
	if err != nil {
		return replayed, err
	}
	for index := range commands {
		if sameKconfigCommandResult(commands[index], refreshed[index]) {
			continue
		}
		expected := commands[index]
		actual := refreshed[index]
		switch expected.Kind {
		case KconfigCommandKindSuccess:
			return replayed, fmt.Errorf(
				"configured Kconfig command %s result mismatch: profile recorded success=%t, configured tools returned success=%t",
				expected.ID,
				expected.Success != nil && *expected.Success,
				actual.Success != nil && *actual.Success,
			)
		case KconfigCommandKindStdout:
			return replayed, fmt.Errorf(
				"configured Kconfig command %s stdout mismatch: profile recorded %q, configured tools returned %q",
				expected.ID,
				valueOrEmpty(expected.Stdout),
				valueOrEmpty(actual.Stdout),
			)
		}
	}
	return replayed, nil
}

// ReplayConfiguredKconfigCommands rejects source-input and configured-tool
// drift for a consumed graph projection.
func ReplayConfiguredKconfigCommands(
	ctx context.Context,
	commands []KconfigCommand,
	tools KbuildGraphProbeTools,
) (int, error) {
	return replayConfiguredKconfigCommands(ctx, commands, tools, true)
}

// ReplayMatchingConfiguredKconfigCommands validates the entries in a
// multi-source profile whose recorded inputs match the selected source tree.
func ReplayMatchingConfiguredKconfigCommands(
	ctx context.Context,
	commands []KconfigCommand,
	tools KbuildGraphProbeTools,
) (int, error) {
	return replayConfiguredKconfigCommands(ctx, commands, tools, false)
}

func refreshConfiguredKbuildGraphProbes(
	ctx context.Context,
	probes []KbuildGraphProbe,
	tools KbuildGraphProbeTools,
	requireMatchingInputs bool,
) ([]KbuildGraphProbe, int, error) {
	refreshed := make([]KbuildGraphProbe, len(probes))
	replayed := 0
	for index, expected := range probes {
		refreshed[index] = cloneKbuildGraphProbe(expected)
		inputsMatch, err := configuredGraphInputsMatch(
			expected.Inputs,
			tools.SourceRoot,
		)
		if err != nil {
			return nil, replayed, fmt.Errorf(
				"match Kbuild graph probe %s inputs: %w",
				expected.ID,
				err,
			)
		}
		if !inputsMatch {
			if requireMatchingInputs {
				return nil, replayed, fmt.Errorf(
					"Kbuild graph probe %s inputs do not match the current source tree",
					expected.ID,
				)
			}
			continue
		}
		supported, err := EvaluateKbuildGraphProbe(
			ctx,
			expected.Identity(),
			tools,
		)
		replayed++
		if err != nil {
			return nil, replayed, fmt.Errorf(
				"replay Kbuild graph probe %s: %w",
				expected.ID,
				err,
			)
		}
		refreshed[index].Supported = supported
	}
	return refreshed, replayed, nil
}

func replayConfiguredKbuildGraphProbes(
	ctx context.Context,
	probes []KbuildGraphProbe,
	tools KbuildGraphProbeTools,
	requireMatchingInputs bool,
) (int, error) {
	refreshed, replayed, err := refreshConfiguredKbuildGraphProbes(
		ctx,
		probes,
		tools,
		requireMatchingInputs,
	)
	if err != nil {
		return replayed, err
	}
	for index, expected := range probes {
		if refreshed[index].Supported != expected.Supported {
			return replayed, fmt.Errorf(
				"Kbuild graph probe %s result mismatch: profile recorded supported=%t, configured tools returned supported=%t",
				expected.ID,
				expected.Supported,
				refreshed[index].Supported,
			)
		}
	}
	return replayed, nil
}

// ReplayConfiguredKbuildGraphProbes rejects source-input and configured-tool
// drift for a consumed graph projection.
func ReplayConfiguredKbuildGraphProbes(
	ctx context.Context,
	probes []KbuildGraphProbe,
	tools KbuildGraphProbeTools,
) (int, error) {
	return replayConfiguredKbuildGraphProbes(ctx, probes, tools, true)
}

// ReplayMatchingConfiguredKbuildGraphProbes validates the entries in a
// multi-source profile whose recorded inputs match the selected source tree.
func ReplayMatchingConfiguredKbuildGraphProbes(
	ctx context.Context,
	probes []KbuildGraphProbe,
	tools KbuildGraphProbeTools,
) (int, error) {
	return replayConfiguredKbuildGraphProbes(ctx, probes, tools, false)
}

func sameKconfigCommandResult(left, right KconfigCommand) bool {
	if left.Kind != right.Kind {
		return false
	}
	switch left.Kind {
	case KconfigCommandKindSuccess:
		return left.Success != nil &&
			right.Success != nil &&
			*left.Success == *right.Success
	case KconfigCommandKindStdout:
		return left.Stdout != nil &&
			right.Stdout != nil &&
			*left.Stdout == *right.Stdout
	default:
		return false
	}
}

// RefreshConfiguredGraphProfile replaces deterministic model answers with
// results from the selected compiler/linker while preserving every identity.
func RefreshConfiguredGraphProfile(
	ctx context.Context,
	profile GraphProfile,
	tools KbuildGraphProbeTools,
) (GraphProfile, error) {
	if err := ValidateGraphProfile(profile); err != nil {
		return GraphProfile{}, err
	}
	if !analysisIdentityEqual(
		profile.AnalysisIdentity,
		tools.CommandTemplate.AnalysisIdentity,
	) {
		return GraphProfile{}, fmt.Errorf(
			"configured graph profile analysis identity does not match selected tools",
		)
	}
	commands, _, err := RefreshConfiguredKconfigCommands(
		ctx,
		profile.KconfigCommands,
		tools,
	)
	if err != nil {
		return GraphProfile{}, err
	}
	probes, _, err := refreshConfiguredKbuildGraphProbes(
		ctx,
		profile.KbuildGraphProbes,
		tools,
		false,
	)
	if err != nil {
		return GraphProfile{}, err
	}
	refreshed := GraphProfile{
		Schema:            profile.Schema,
		Architecture:      profile.Architecture,
		DriverContract:    profile.DriverContract,
		AnalysisIdentity:  profile.AnalysisIdentity,
		KconfigCommands:   commands,
		KbuildGraphProbes: probes,
	}
	if err := ValidateGraphProfile(refreshed); err != nil {
		return GraphProfile{}, fmt.Errorf("validate refreshed graph profile: %w", err)
	}
	return refreshed, nil
}

func absolutePathOrOriginal(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return absolute
}

func isConfiguredKconfigCommand(
	command string,
	environment map[string]string,
) bool {
	for _, marker := range []string{
		"$CC",
		"${CC}",
		"$LD",
		"${LD}",
		"$AR",
		"${AR}",
		"$NM",
		"${NM}",
		"$OBJCOPY",
		"${OBJCOPY}",
		"/scripts/cc-",
		"/scripts/as-",
		"/scripts/ld-",
		"/scripts/gcc-",
	} {
		if strings.Contains(command, marker) {
			return true
		}
	}
	for _, name := range []string{"CC", "LD", "AR", "NM", "OBJCOPY"} {
		if containsShellWord(command, strings.TrimSpace(environment[name])) {
			return true
		}
	}
	return false
}

func validateConfiguredKconfigTools(tools KbuildGraphProbeTools) error {
	if err := ValidateCommandTemplate(tools.CommandTemplate); err != nil {
		return fmt.Errorf("command template: %w", err)
	}
	for _, tool := range []struct {
		name string
		path string
	}{
		{name: "archiver", path: tools.Archiver},
		{name: "coreutils", path: tools.Coreutils},
		{name: "grep", path: tools.Grep},
		{name: "linker", path: tools.Linker},
		{name: "nm", path: tools.NM},
		{name: "objcopy", path: tools.Objcopy},
	} {
		if tool.path == "" {
			return fmt.Errorf("%s must be non-empty", tool.name)
		}
	}
	return nil
}

func ensureConfiguredKconfigToolShims(
	shimDir string,
	created map[string]string,
	environment map[string]string,
	tools KbuildGraphProbeTools,
) error {
	for _, binding := range []struct {
		name     string
		target   string
		compiler bool
	}{
		{name: "CC", target: tools.CommandTemplate.Compiler, compiler: true},
		{name: "LD", target: tools.Linker},
		{name: "AR", target: tools.Archiver},
		{name: "NM", target: tools.NM},
		{name: "OBJCOPY", target: tools.Objcopy},
	} {
		alias := strings.TrimSpace(environment[binding.name])
		if alias == "" {
			continue
		}
		var err error
		if binding.compiler {
			err = ensureCompilerShim(shimDir, created, alias, tools)
		} else {
			err = ensureToolShim(shimDir, created, alias, binding.target)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func containsShellWord(command, word string) bool {
	if word == "" {
		return false
	}
	for offset := 0; ; {
		index := strings.Index(command[offset:], word)
		if index < 0 {
			return false
		}
		index += offset
		beforeOK := index == 0 || isShellWordBoundary(command[index-1])
		after := index + len(word)
		afterOK := after == len(command) || isShellWordBoundary(command[after])
		if beforeOK && afterOK {
			return true
		}
		offset = index + len(word)
	}
}

func isShellWordBoundary(value byte) bool {
	return strings.ContainsRune(" \t\r\n;|&()<>\"'=", rune(value))
}

func ensureToolShim(
	shimDir string,
	created map[string]string,
	alias string,
	target string,
) error {
	if alias == "" {
		return fmt.Errorf("configured Kconfig command tool alias must be non-empty")
	}
	if filepath.Base(alias) != alias {
		return fmt.Errorf("configured Kconfig command tool alias %q must be a basename", alias)
	}
	target, err := filepath.Abs(target)
	if err != nil {
		return fmt.Errorf("resolve configured tool %q: %w", target, err)
	}
	if previous, ok := created[alias]; ok {
		if previous != target {
			return fmt.Errorf("configured tool alias %q maps to both %q and %q", alias, previous, target)
		}
		return nil
	}
	if err := os.Symlink(target, filepath.Join(shimDir, alias)); err != nil {
		return fmt.Errorf("create configured tool alias %q: %w", alias, err)
	}
	created[alias] = target
	return nil
}

func ensureCompilerShim(
	shimDir string,
	created map[string]string,
	alias string,
	tools KbuildGraphProbeTools,
) error {
	if alias == "" {
		return fmt.Errorf("configured Kconfig command tool alias must be non-empty")
	}
	if filepath.Base(alias) != alias {
		return fmt.Errorf("configured Kconfig command tool alias %q must be a basename", alias)
	}
	compiler, err := filepath.Abs(tools.CommandTemplate.Compiler)
	if err != nil {
		return fmt.Errorf(
			"resolve configured compiler %q: %w",
			tools.CommandTemplate.Compiler,
			err,
		)
	}
	shell, err := filepath.Abs(tools.Shell)
	if err != nil {
		return fmt.Errorf("resolve configured shell %q: %w", tools.Shell, err)
	}
	executionRoot, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve configured compiler execution root: %w", err)
	}
	prefix, suffix := splitCommandTemplateKbuildArgv(tools.CommandTemplate)
	prefix = absolutizeCompilerTemplatePaths(prefix, executionRoot)
	suffix = absolutizeCompilerTemplatePaths(suffix, executionRoot)
	signatureArgv := make([]string, 0, len(prefix)+len(suffix)+2)
	signatureArgv = append(signatureArgv, compiler)
	signatureArgv = append(signatureArgv, prefix...)
	signatureArgv = append(signatureArgv, "\x1fKBUILD_ARGV\x1f")
	signatureArgv = append(signatureArgv, suffix...)
	signature := strings.Join(signatureArgv, "\x00")
	if previous, ok := created[alias]; ok {
		if previous != signature {
			return fmt.Errorf(
				"configured tool alias %q maps to both %q and %q",
				alias,
				previous,
				signature,
			)
		}
		return nil
	}
	command := make([]string, 0, len(prefix)+1)
	command = append(command, compiler)
	command = append(command, prefix...)
	quoted := make([]string, len(command))
	for index, arg := range command {
		quoted[index] = quoteShellWord(arg)
	}
	suffixQuoted := make([]string, len(suffix))
	for index, arg := range suffix {
		suffixQuoted[index] = quoteShellWord(arg)
	}
	script := "#!" + shell + "\nexec " + strings.Join(quoted, " ") + " \"$@\""
	if len(suffixQuoted) != 0 {
		script += " " + strings.Join(suffixQuoted, " ")
	}
	script += "\n"
	if err := os.WriteFile(filepath.Join(shimDir, alias), []byte(script), 0o755); err != nil {
		return fmt.Errorf("create configured compiler alias %q: %w", alias, err)
	}
	created[alias] = signature
	return nil
}

func quoteShellWord(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func normalizeKconfigCommandOutput(output []byte) string {
	output = bytes.TrimRight(output, "\n")
	output = bytes.ReplaceAll(output, []byte("\n"), []byte(" "))
	return string(output)
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// EvaluateKbuildGraphProbe replays one exact graph-shaping Kbuild decision
// against the configured compiler or linker.
func EvaluateKbuildGraphProbe(
	ctx context.Context,
	request KbuildGraphProbeIdentity,
	tools KbuildGraphProbeTools,
) (bool, error) {
	if err := ValidateCommandTemplate(tools.CommandTemplate); err != nil {
		return false, fmt.Errorf("Kbuild graph probe command template: %w", err)
	}
	if tools.Linker == "" {
		return false, fmt.Errorf("Kbuild graph probe linker must be non-empty")
	}
	if request.ID != KbuildGraphProbeIdentityID(request) {
		return false, fmt.Errorf(
			"Kbuild graph probe request ID %q, want %q",
			request.ID,
			KbuildGraphProbeIdentityID(request),
		)
	}

	contextArgv, err := expandKbuildGraphProbePaths(request.ContextArgv, tools)
	if err != nil {
		return false, err
	}
	candidateArgv, err := expandKbuildGraphProbePaths(request.CandidateArgv, tools)
	if err != nil {
		return false, err
	}

	var executable string
	switch request.Kind {
	case KbuildGraphProbeKindCCOption:
		if request.Language != "c" || len(candidateArgv) == 0 {
			return false, fmt.Errorf(
				"%s Kbuild graph probe requires c language and non-empty candidate argv",
				request.Kind,
			)
		}
		executable = tools.CommandTemplate.Compiler
		for index, arg := range candidateArgv {
			if strings.HasPrefix(arg, "-Wno-") {
				candidateArgv[index] = "-W" + strings.TrimPrefix(arg, "-Wno-")
			}
		}
		candidateArgv = compilerKbuildGraphProbeArgv(
			tools.CommandTemplate,
			append(contextArgv, candidateArgv...),
			"c",
		)
	case KbuildGraphProbeKindASOption:
		if request.Language != "asm" || len(candidateArgv) == 0 {
			return false, fmt.Errorf(
				"as_option Kbuild graph probe requires asm language and non-empty candidate argv",
			)
		}
		executable = tools.CommandTemplate.Compiler
		candidateArgv = compilerKbuildGraphProbeArgv(
			tools.CommandTemplate,
			append(contextArgv, candidateArgv...),
			"assembler-with-cpp",
		)
	case KbuildGraphProbeKindLDOption:
		if request.Language != "link" || len(candidateArgv) == 0 {
			return false, fmt.Errorf(
				"ld_option Kbuild graph probe requires link language and non-empty candidate argv",
			)
		}
		executable = tools.Linker
		candidateArgv = append(append(contextArgv, candidateArgv...), "-v")
	default:
		return false, fmt.Errorf("unsupported Kbuild graph probe kind %q", request.Kind)
	}

	command := exec.CommandContext(ctx, executable, candidateArgv...)
	if tools.CommandTemplate.Environment != nil {
		command.Env = environmentList(tools.CommandTemplate.Environment)
	}
	err = command.Run()
	if err == nil {
		return true, nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return false, nil
	}
	return false, fmt.Errorf("execute %s Kbuild graph probe %s: %w", request.Kind, request.ID, err)
}

func compilerKbuildGraphProbeArgv(
	template CommandTemplate,
	argv []string,
	language string,
) []string {
	prefix, suffix := splitCommandTemplateKbuildArgv(template)
	out := make([]string, 0, len(prefix)+len(argv)+len(suffix)+6)
	out = append(out, prefix...)
	out = append(out, argv...)
	out = append(out, suffix...)
	out = append(out, "-c", "-x", language, os.DevNull)
	out = append(out, "-o", os.DevNull)
	return out
}

func splitCommandTemplateKbuildArgv(
	template CommandTemplate,
) (prefix []string, suffix []string) {
	for index, arg := range template.MutableArgv {
		if arg == template.KbuildFlagsSentinel {
			return append([]string(nil), template.MutableArgv[:index]...),
				append([]string(nil), template.MutableArgv[index+1:]...)
		}
	}
	return append([]string(nil), template.MutableArgv...), nil
}

func absolutizeCompilerTemplatePaths(argv []string, executionRoot string) []string {
	out := append([]string(nil), argv...)
	separate := map[string]bool{
		"-B":              true,
		"-I":              true,
		"-idirafter":      true,
		"-imacros":        true,
		"-include":        true,
		"-iquote":         true,
		"-isystem":        true,
		"-isysroot":       true,
		"-resource-dir":   true,
		"--gcc-toolchain": true,
		"--sysroot":       true,
	}
	joined := []string{
		"--gcc-toolchain=",
		"--sysroot=",
		"-fmodule-map-file=",
		"-idirafter",
		"-iquote",
		"-isystem",
		"-isysroot",
		"-resource-dir=",
		"-B",
		"-I",
	}
	for index := 0; index < len(out); index++ {
		arg := out[index]
		if separate[arg] && index+1 < len(out) {
			index++
			out[index] = absoluteCompilerTemplatePath(out[index], executionRoot)
			continue
		}
		if strings.HasPrefix(arg, "@") && len(arg) > 1 {
			out[index] = "@" + absoluteCompilerTemplatePath(arg[1:], executionRoot)
			continue
		}
		for _, prefix := range joined {
			value, ok := strings.CutPrefix(arg, prefix)
			if !ok || value == "" {
				continue
			}
			out[index] = prefix + absoluteCompilerTemplatePath(value, executionRoot)
			break
		}
	}
	return out
}

func absoluteCompilerTemplatePath(value string, executionRoot string) string {
	path := filepath.FromSlash(value)
	if value == "" || filepath.IsAbs(path) {
		return value
	}
	return filepath.ToSlash(filepath.Join(executionRoot, path))
}

func expandKbuildGraphProbePaths(
	argv []string,
	tools KbuildGraphProbeTools,
) ([]string, error) {
	out := make([]string, len(argv))
	for index, arg := range argv {
		for _, replacement := range []struct {
			token string
			path  string
		}{
			{token: GraphProfileSourceRoot, path: tools.SourceRoot},
			{token: GraphProfileObjectRoot, path: tools.ObjectRoot},
		} {
			if !strings.Contains(arg, replacement.token) {
				continue
			}
			if replacement.path == "" {
				return nil, fmt.Errorf(
					"Kbuild graph probe argument %q requires a path for %s",
					arg,
					replacement.token,
				)
			}
			arg = strings.ReplaceAll(
				arg,
				replacement.token,
				filepath.ToSlash(filepath.Clean(replacement.path)),
			)
		}
		out[index] = arg
	}
	return out, nil
}
