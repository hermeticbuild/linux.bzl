package ccprofile

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type KbuildGraphProbeTools struct {
	Compiler    string
	Linker      string
	Shell       string
	SourceRoot  string
	ObjectRoot  string
	Environment map[string]string
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
	if tools.Compiler == "" {
		return "", fmt.Errorf("configured Kconfig command compiler must be non-empty")
	}
	if tools.Linker == "" {
		return "", fmt.Errorf("configured Kconfig command linker must be non-empty")
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
	for _, tool := range []struct {
		alias  string
		target string
	}{
		{alias: strings.TrimSpace(environment["CC"]), target: tools.Compiler},
		{alias: strings.TrimSpace(environment["LD"]), target: tools.Linker},
	} {
		if tool.alias == "" {
			continue
		}
		if err := ensureToolShim(shimDir, createdShims, tool.alias, tool.target); err != nil {
			return "", err
		}
	}
	commandText = strings.ReplaceAll(
		commandText,
		GraphProfileSourceRoot,
		filepath.ToSlash(filepath.Clean(tools.SourceRoot)),
	)
	command := exec.CommandContext(ctx, tools.Shell, "-c", commandText)
	command.Dir = tools.SourceRoot
	runtimeEnvironment := cloneStringMap(tools.Environment)
	for name, value := range environment {
		runtimeEnvironment[name] = value
	}
	searchPath := []string{
		shimDir,
		filepath.Dir(absolutePathOrOriginal(tools.Compiler)),
		filepath.Dir(absolutePathOrOriginal(tools.Linker)),
	}
	if hostPath := os.Getenv("PATH"); hostPath != "" {
		searchPath = append(searchPath, hostPath)
	}
	runtimeEnvironment["PATH"] = strings.Join(searchPath, string(os.PathListSeparator))
	command.Env = environmentList(runtimeEnvironment)
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
	if tools.Compiler == "" {
		return nil, 0, fmt.Errorf("configured Kconfig replay compiler must be non-empty")
	}
	if tools.Linker == "" {
		return nil, 0, fmt.Errorf("configured Kconfig replay linker must be non-empty")
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
	refreshed := make([]KconfigCommand, len(commands))
	replayed := 0
	for index, expected := range commands {
		refreshed[index] = cloneKconfigCommand(expected)
		ccAlias := strings.TrimSpace(expected.Environment["CC"])
		ldAlias := strings.TrimSpace(expected.Environment["LD"])
		if !isConfiguredKconfigCommand(expected.Command, ccAlias, ldAlias) {
			continue
		}
		for _, tool := range []struct {
			alias  string
			target string
		}{
			{alias: ccAlias, target: tools.Compiler},
			{alias: ldAlias, target: tools.Linker},
		} {
			if err := ensureToolShim(shimDir, createdShims, tool.alias, tool.target); err != nil {
				return nil, replayed, err
			}
		}
		commandText := strings.ReplaceAll(
			expected.Command,
			GraphProfileSourceRoot,
			filepath.ToSlash(filepath.Clean(tools.SourceRoot)),
		)
		command := exec.CommandContext(ctx, tools.Shell, "-c", commandText)
		command.Dir = workDir
		environment := cloneStringMap(expected.Environment)
		environment["CC"] = ccAlias
		environment["LD"] = ldAlias
		searchPath := []string{
			shimDir,
			filepath.Dir(absolutePathOrOriginal(tools.Compiler)),
			filepath.Dir(absolutePathOrOriginal(tools.Linker)),
		}
		if hostPath := os.Getenv("PATH"); hostPath != "" {
			searchPath = append(searchPath, hostPath)
		}
		environment["PATH"] = strings.Join(searchPath, string(os.PathListSeparator))
		command.Env = environmentList(environment)
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

// ReplayConfiguredKconfigCommands rejects stale configured-tool results.
func ReplayConfiguredKconfigCommands(
	ctx context.Context,
	commands []KconfigCommand,
	tools KbuildGraphProbeTools,
) (int, error) {
	refreshed, replayed, err := RefreshConfiguredKconfigCommands(ctx, commands, tools)
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
	commands, _, err := RefreshConfiguredKconfigCommands(
		ctx,
		profile.KconfigCommands,
		tools,
	)
	if err != nil {
		return GraphProfile{}, err
	}
	probes := make([]KbuildGraphProbe, len(profile.KbuildGraphProbes))
	for index, expected := range profile.KbuildGraphProbes {
		probes[index] = cloneKbuildGraphProbe(expected)
		supported, err := EvaluateKbuildGraphProbe(
			ctx,
			profile.AnalysisIdentity,
			expected.Identity(),
			tools,
		)
		if err != nil {
			return GraphProfile{}, fmt.Errorf(
				"refresh Kbuild graph probe %s: %w",
				expected.ID,
				err,
			)
		}
		probes[index].Supported = supported
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

func isConfiguredKconfigCommand(command, ccAlias, ldAlias string) bool {
	for _, marker := range []string{
		"$CC",
		"${CC}",
		"$LD",
		"${LD}",
		"/scripts/cc-",
		"/scripts/as-",
		"/scripts/ld-",
		"/scripts/gcc-",
	} {
		if strings.Contains(command, marker) {
			return true
		}
	}
	return containsShellWord(command, ccAlias) || containsShellWord(command, ldAlias)
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
	analysisIdentity AnalysisIdentity,
	request KbuildGraphProbeIdentity,
	tools KbuildGraphProbeTools,
) (bool, error) {
	if tools.Compiler == "" {
		return false, fmt.Errorf("Kbuild graph probe compiler must be non-empty")
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
		executable = tools.Compiler
		for index, arg := range candidateArgv {
			if strings.HasPrefix(arg, "-Wno-") {
				candidateArgv[index] = "-W" + strings.TrimPrefix(arg, "-Wno-")
			}
		}
		candidateArgv = compilerKbuildGraphProbeArgv(
			analysisIdentity,
			append(contextArgv, candidateArgv...),
			"c",
		)
	case KbuildGraphProbeKindASOption:
		if request.Language != "asm" || len(candidateArgv) == 0 {
			return false, fmt.Errorf(
				"as_option Kbuild graph probe requires asm language and non-empty candidate argv",
			)
		}
		executable = tools.Compiler
		candidateArgv = compilerKbuildGraphProbeArgv(
			analysisIdentity,
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
	if tools.Environment != nil {
		command.Env = environmentList(tools.Environment)
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
	analysisIdentity AnalysisIdentity,
	argv []string,
	language string,
) []string {
	out := make([]string, 0, len(argv)+8)
	if target := analysisIdentity.TargetGNUSystemName; target != "" {
		out = append(out, "--target="+target)
	}
	out = append(out, argv...)
	out = append(out, "-c", "-x", language, os.DevNull)
	out = append(out, "-o", os.DevNull)
	return out
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
