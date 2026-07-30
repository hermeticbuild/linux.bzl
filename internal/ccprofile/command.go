package ccprofile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"
)

const (
	CommandTemplateSchema  = "linux.bzl/cc-command-template-v1"
	CompilerIdentitySchema = "linux.bzl/cc-compiler-identity-v1"
)

type CompileSentinels struct {
	Source      string
	Output      string
	KbuildFlags string
}

type CommandTemplate struct {
	Schema              string            `json:"schema"`
	Architecture        string            `json:"architecture"`
	DriverContract      string            `json:"driver_contract"`
	AnalysisIdentity    AnalysisIdentity  `json:"analysis_identity"`
	Compiler            string            `json:"compiler"`
	MutableArgv         []string          `json:"mutable_argv"`
	Environment         map[string]string `json:"environment"`
	KbuildFlagsSentinel string            `json:"kbuild_flags_sentinel"`
}

type CompilerIdentity struct {
	Schema           string            `json:"schema"`
	Architecture     string            `json:"architecture"`
	DriverContract   string            `json:"driver_contract"`
	AnalysisIdentity AnalysisIdentity  `json:"analysis_identity"`
	CCName           string            `json:"cc_name"`
	CCVersion        int               `json:"cc_version"`
	CCVersionText    string            `json:"cc_version_text"`
	BuiltinMacros    map[string]string `json:"builtin_macros"`
}

func NewCommandTemplate(
	architecture string,
	analysisIdentity AnalysisIdentity,
	compiler string,
	argv []string,
	environment map[string]string,
	sentinels CompileSentinels,
) (CommandTemplate, error) {
	mutableArgv, err := ExtractMutableCompileArgv(argv, sentinels)
	if err != nil {
		return CommandTemplate{}, err
	}
	mutableArgv, err = normalizeLinuxCompileArgv(
		architecture,
		analysisIdentity,
		mutableArgv,
		sentinels.KbuildFlags,
	)
	if err != nil {
		return CommandTemplate{}, err
	}
	template := CommandTemplate{
		Schema:              CommandTemplateSchema,
		Architecture:        architecture,
		DriverContract:      DriverContract,
		AnalysisIdentity:    analysisIdentity,
		Compiler:            compiler,
		MutableArgv:         mutableArgv,
		Environment:         cloneStringMap(environment),
		KbuildFlagsSentinel: sentinels.KbuildFlags,
	}
	if err := ValidateCommandTemplate(template); err != nil {
		return CommandTemplate{}, err
	}
	return template, nil
}

func ExtractMutableCompileArgv(argv []string, sentinels CompileSentinels) ([]string, error) {
	sentinelValues := []struct {
		name  string
		value string
	}{
		{"source", sentinels.Source},
		{"output", sentinels.Output},
		{"Kbuild flags", sentinels.KbuildFlags},
	}
	for _, sentinel := range sentinelValues {
		name := sentinel.name
		value := sentinel.value
		if value == "" || strings.ContainsRune(value, '\x00') {
			return nil, fmt.Errorf("%s sentinel must be non-empty and contain no NUL", name)
		}
	}
	if sentinels.Source == sentinels.Output ||
		sentinels.Source == sentinels.KbuildFlags ||
		sentinels.Output == sentinels.KbuildFlags {
		return nil, fmt.Errorf("compile sentinels must be distinct")
	}

	mutableCapacity := len(argv) - 3
	if mutableCapacity < 0 {
		mutableCapacity = 0
	}
	mutable := make([]string, 0, mutableCapacity)
	sourceCount := 0
	outputCount := 0
	outputSeedCount := 0
	compileCount := 0
	kbuildFlagsCount := 0
	for index := 0; index < len(argv); index++ {
		arg := argv[index]
		if arg == "" || strings.ContainsRune(arg, '\x00') {
			return nil, fmt.Errorf("compile argv[%d] must be non-empty and contain no NUL", index)
		}
		if arg == "-frandom-seed="+sentinels.Output {
			outputSeedCount++
			if outputSeedCount > 1 {
				return nil, fmt.Errorf("compile argv contains more than one output-derived -frandom-seed")
			}
			continue
		}
		for _, sentinel := range sentinelValues {
			if strings.Contains(arg, sentinel.value) && arg != sentinel.value && arg != "-o"+sentinel.value {
				return nil, fmt.Errorf("%s sentinel is embedded in compile argv[%d] %q", sentinel.name, index, arg)
			}
		}
		switch arg {
		case "-c":
			compileCount++
			continue
		case "-E", "-S", "--":
			return nil, fmt.Errorf("compile argv contains incompatible structural argument %q", arg)
		case sentinels.Source:
			sourceCount++
			continue
		case sentinels.KbuildFlags:
			kbuildFlagsCount++
			mutable = append(mutable, arg)
			continue
		case "-o":
			if index+1 >= len(argv) || argv[index+1] != sentinels.Output {
				return nil, fmt.Errorf("compile argv -o must be followed by the output sentinel")
			}
			outputCount++
			index++
			continue
		case "-o" + sentinels.Output:
			outputCount++
			continue
		case sentinels.Output:
			return nil, fmt.Errorf("output sentinel is not attached to -o")
		}
		mutable = append(mutable, arg)
	}
	counts := []struct {
		name  string
		count int
	}{
		{"-c", compileCount},
		{"source", sourceCount},
		{"output", outputCount},
		{"Kbuild flags", kbuildFlagsCount},
	}
	for _, value := range counts {
		if value.count != 1 {
			return nil, fmt.Errorf(
				"compile argv must contain exactly one %s sentinel/argument, got %d",
				value.name,
				value.count,
			)
		}
	}
	return mutable, nil
}

func ValidateCommandTemplate(template CommandTemplate) error {
	if template.Schema != CommandTemplateSchema {
		return fmt.Errorf("command template schema %q, want %q", template.Schema, CommandTemplateSchema)
	}
	if err := validateArchitecture(template.Architecture); err != nil {
		return err
	}
	if template.DriverContract != DriverContract {
		return fmt.Errorf("driver_contract %q, want %q", template.DriverContract, DriverContract)
	}
	if err := validateAnalysisIdentity(template.AnalysisIdentity); err != nil {
		return err
	}
	if err := validateText(template.Compiler, "compiler"); err != nil {
		return err
	}
	if err := validateText(template.KbuildFlagsSentinel, "kbuild_flags_sentinel"); err != nil {
		return err
	}
	if template.MutableArgv == nil {
		return fmt.Errorf("mutable_argv is required")
	}
	sentinelCount := 0
	for index, arg := range template.MutableArgv {
		if arg == "" || strings.ContainsRune(arg, '\x00') {
			return fmt.Errorf("mutable_argv[%d] must be non-empty and contain no NUL", index)
		}
		if arg == template.KbuildFlagsSentinel {
			sentinelCount++
		} else if strings.Contains(arg, template.KbuildFlagsSentinel) {
			return fmt.Errorf("Kbuild flags sentinel is embedded in mutable_argv[%d] %q", index, arg)
		}
		if arg == "-c" || arg == "-E" || arg == "-S" || arg == "--" || isGNUOutputOption(arg) {
			return fmt.Errorf("mutable_argv[%d] contains structural argument %q", index, arg)
		}
	}
	if sentinelCount != 1 {
		return fmt.Errorf("mutable_argv must contain exactly one Kbuild flags sentinel, got %d", sentinelCount)
	}
	normalized, err := normalizeLinuxCompileArgv(
		template.Architecture,
		template.AnalysisIdentity,
		template.MutableArgv,
		template.KbuildFlagsSentinel,
	)
	if err != nil {
		return err
	}
	if !slices.Equal(template.MutableArgv, normalized) {
		return fmt.Errorf("mutable_argv is not a canonical Linux compile command")
	}
	if template.Environment == nil {
		return fmt.Errorf("environment is required")
	}
	for name, value := range template.Environment {
		if name == "" || strings.ContainsAny(name, "=\x00") {
			return fmt.Errorf("invalid environment variable name %q", name)
		}
		if strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("environment variable %s contains NUL", name)
		}
	}
	return nil
}

func normalizeLinuxCompileArgv(
	architecture string,
	analysisIdentity AnalysisIdentity,
	argv []string,
	kbuildFlagsSentinel string,
) ([]string, error) {
	if err := validateArchitecture(architecture); err != nil {
		return nil, err
	}
	if err := validateAnalysisIdentity(analysisIdentity); err != nil {
		return nil, err
	}
	base := make([]string, 0, len(argv))
	sentinelCount := 0
	for _, arg := range argv {
		if arg == kbuildFlagsSentinel {
			sentinelCount++
			continue
		}
		base = append(base, arg)
	}
	if sentinelCount != 1 {
		return nil, fmt.Errorf(
			"mutable argv must contain exactly one Kbuild flags sentinel, got %d",
			sentinelCount,
		)
	}
	if analysisIdentity.Compiler == "clang" {
		base = rewriteLinuxClangTargetArgv(base, linuxKbuildTargetTriple(architecture))
	}

	out := make([]string, 0, len(base)+3)
	for index := 0; index < len(base); index++ {
		arg := base[index]
		if arg == "-Xclang" && index+1 < len(base) {
			switch base[index+1] {
			case "-internal-isystem":
				index += 3
				continue
			case "-fno-cxx-modules":
				index++
				continue
			}
		}
		if arg == "-I" || arg == "-iquote" || arg == "-isystem" {
			if index+1 < len(base) && linuxDropToolchainInclude(base[index+1]) {
				index++
				continue
			}
		}
		if linuxDropToolchainCompileArg(arg) {
			continue
		}
		if arg == "--sysroot" {
			if index+1 < len(base) {
				index++
			}
			continue
		}
		if strings.HasPrefix(arg, "--sysroot=") {
			continue
		}
		if strings.HasPrefix(arg, "-I") &&
			linuxDropToolchainInclude(strings.TrimPrefix(arg, "-I")) {
			continue
		}
		if strings.HasPrefix(arg, "-iquote") &&
			linuxDropToolchainInclude(strings.TrimPrefix(arg, "-iquote")) {
			continue
		}
		if strings.HasPrefix(arg, "-isystem") &&
			linuxDropToolchainInclude(strings.TrimPrefix(arg, "-isystem")) {
			continue
		}
		out = append(out, arg)
	}
	if !slices.Contains(out, "-nostdinc") {
		out = append(out, "-nostdinc")
	}
	if analysisIdentity.Compiler == "clang" &&
		!slices.Contains(out, "-fintegrated-as") {
		out = append(out, "-fintegrated-as")
	}
	out = append(out, kbuildFlagsSentinel)
	return out, nil
}

func rewriteLinuxClangTargetArgv(argv []string, target string) []string {
	out := make([]string, 0, len(argv)+1)
	inserted := false
	for index := 0; index < len(argv); index++ {
		arg := argv[index]
		if arg == "-target" || arg == "--target" {
			if index+1 < len(argv) {
				index++
			}
			if !inserted {
				out = append(out, "--target="+target)
				inserted = true
			}
			continue
		}
		if strings.HasPrefix(arg, "-target=") ||
			strings.HasPrefix(arg, "--target=") {
			if !inserted {
				out = append(out, "--target="+target)
				inserted = true
			}
			continue
		}
		out = append(out, arg)
	}
	if !inserted {
		out = append([]string{"--target=" + target}, out...)
	}
	return out
}

func linuxKbuildTargetTriple(architecture string) string {
	if architecture == "aarch64" {
		return "aarch64-linux-gnu"
	}
	return "x86_64-linux-gnu"
}

func linuxDropToolchainCompileArg(arg string) bool {
	switch arg {
	case "-fcolor-diagnostics",
		"-fstack-protector",
		"-no-canonical-prefixes",
		"-nostdlibinc",
		"-Werror=incomplete-umbrella",
		"-Wall",
		"-Wno-free-nonheap-object",
		"-Wno-module-import-in-extern-c",
		"-Wno-modules-import-nested-redundant",
		"-Wself-assign",
		"-Wthread-safety",
		"-Wunused-but-set-parameter":
		return true
	default:
		return false
	}
}

func linuxDropToolchainInclude(path string) bool {
	return strings.Contains(path, "llvm++musl+musl_libc/") ||
		strings.Contains(path, `llvm++musl+musl_libc\`) ||
		strings.Contains(path, "llvm++kernel_headers+linux_kernel_headers_")
}

func CanonicalCommandTemplateJSON(template CommandTemplate) ([]byte, error) {
	if err := ValidateCommandTemplate(template); err != nil {
		return nil, err
	}
	return marshalCanonical(template, "CC command template")
}

func InspectCompiler(ctx context.Context, template CommandTemplate) (CompilerIdentity, error) {
	if err := ValidateCommandTemplate(template); err != nil {
		return CompilerIdentity{}, err
	}
	versionOutput, err := runCompiler(ctx, template, []string{"--version"}, nil)
	if err != nil {
		return CompilerIdentity{}, fmt.Errorf("probe compiler version: %w", err)
	}
	versionText := firstNonemptyLine(versionOutput)
	if versionText == "" {
		return CompilerIdentity{}, fmt.Errorf("probe compiler version: compiler produced no version text")
	}

	probeArgv := make([]string, 0, len(template.MutableArgv)+4)
	for _, arg := range template.MutableArgv {
		if arg != template.KbuildFlagsSentinel {
			probeArgv = append(probeArgv, arg)
		}
	}
	probeArgv = append(probeArgv, "-dM", "-E", "-x", "c", "-")
	macroOutput, err := runCompiler(ctx, template, probeArgv, []byte{})
	if err != nil {
		return CompilerIdentity{}, fmt.Errorf("probe compiler predefined macros: %w", err)
	}
	macros, err := ParseBuiltinMacros(macroOutput)
	if err != nil {
		return CompilerIdentity{}, err
	}
	ccName, ccVersion, err := compilerVersion(macros)
	if err != nil {
		return CompilerIdentity{}, err
	}
	expectedCCName := "Clang"
	if template.AnalysisIdentity.Compiler == "gcc" {
		expectedCCName = "GCC"
	}
	if ccName != expectedCCName {
		return CompilerIdentity{}, fmt.Errorf(
			"compiler macros identify %s but analysis selected %s",
			ccName,
			template.AnalysisIdentity.Compiler,
		)
	}

	identity := CompilerIdentity{
		Schema:           CompilerIdentitySchema,
		Architecture:     template.Architecture,
		DriverContract:   template.DriverContract,
		AnalysisIdentity: template.AnalysisIdentity,
		CCName:           ccName,
		CCVersion:        ccVersion,
		CCVersionText:    versionText,
		BuiltinMacros:    macros,
	}
	if err := ValidateCompilerIdentity(identity); err != nil {
		return CompilerIdentity{}, err
	}
	return identity, nil
}

func runCompiler(
	ctx context.Context,
	template CommandTemplate,
	argv []string,
	stdin []byte,
) ([]byte, error) {
	command := exec.CommandContext(ctx, template.Compiler, argv...)
	command.Env = environmentList(template.Environment)
	if stdin != nil {
		command.Stdin = bytes.NewReader(stdin)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		if detail != "" {
			return nil, fmt.Errorf("%w: %s", err, detail)
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

func ParseBuiltinMacros(data []byte) (map[string]string, error) {
	macros := map[string]string{}
	for lineNumber, rawLine := range strings.Split(string(data), "\n") {
		line := strings.TrimSuffix(rawLine, "\r")
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "#define ") {
			return nil, fmt.Errorf("compiler macro output line %d is malformed: %q", lineNumber+1, line)
		}
		definition := strings.TrimPrefix(line, "#define ")
		name := definition
		value := ""
		if index := strings.IndexAny(definition, " \t"); index >= 0 {
			name = definition[:index]
			value = strings.TrimSpace(definition[index:])
		}
		if !macroNamePattern.MatchString(name) {
			// Function-like macros are not Kconfig builtin-macro inputs.
			if strings.Contains(name, "(") {
				continue
			}
			return nil, fmt.Errorf("compiler macro output line %d has invalid name %q", lineNumber+1, name)
		}
		if _, exists := macros[name]; exists {
			return nil, fmt.Errorf("compiler macro output defines %s more than once", name)
		}
		macros[name] = value
	}
	if len(macros) == 0 {
		return nil, fmt.Errorf("compiler produced no predefined macros")
	}
	return macros, nil
}

func compilerVersion(macros map[string]string) (string, int, error) {
	ccName := "GCC"
	if _, ok := macros["__clang_major__"]; ok {
		ccName = "Clang"
	}
	var names []string
	if ccName == "Clang" {
		names = []string{"__clang_major__", "__clang_minor__", "__clang_patchlevel__"}
	} else {
		names = []string{"__GNUC__", "__GNUC_MINOR__", "__GNUC_PATCHLEVEL__"}
	}
	values := make([]int, len(names))
	for index, name := range names {
		raw, ok := macros[name]
		if !ok {
			return "", 0, fmt.Errorf("%s compiler macros omit %s", ccName, name)
		}
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			return "", 0, fmt.Errorf("%s compiler macro %s has invalid integer %q", ccName, name, raw)
		}
		values[index] = value
	}
	return ccName, values[0]*10000 + values[1]*100 + values[2], nil
}

func ValidateCompilerIdentity(identity CompilerIdentity) error {
	if identity.Schema != CompilerIdentitySchema {
		return fmt.Errorf("compiler identity schema %q, want %q", identity.Schema, CompilerIdentitySchema)
	}
	if err := validateArchitecture(identity.Architecture); err != nil {
		return err
	}
	if identity.DriverContract != DriverContract {
		return fmt.Errorf("driver_contract %q, want %q", identity.DriverContract, DriverContract)
	}
	if err := validateAnalysisIdentity(identity.AnalysisIdentity); err != nil {
		return err
	}
	expectedCCName := "Clang"
	if identity.AnalysisIdentity.Compiler == "gcc" {
		expectedCCName = "GCC"
	}
	if identity.CCName != expectedCCName {
		return fmt.Errorf("cc_name %q does not match compiler %q", identity.CCName, identity.AnalysisIdentity.Compiler)
	}
	if identity.CCVersion <= 0 {
		return fmt.Errorf("cc_version must be positive")
	}
	if err := validateText(identity.CCVersionText, "cc_version_text"); err != nil {
		return err
	}
	if identity.BuiltinMacros == nil {
		return fmt.Errorf("builtin_macros is required")
	}
	for name, value := range identity.BuiltinMacros {
		if !macroNamePattern.MatchString(name) {
			return fmt.Errorf("invalid builtin macro name %q", name)
		}
		if strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("builtin macro %s contains NUL", name)
		}
	}
	return nil
}

func CanonicalCompilerIdentityJSON(identity CompilerIdentity) ([]byte, error) {
	if err := ValidateCompilerIdentity(identity); err != nil {
		return nil, err
	}
	return marshalCanonical(identity, "compiler identity")
}

func DecodeCompilerIdentity(data []byte) (CompilerIdentity, error) {
	if err := rejectDuplicateKeys(data); err != nil {
		return CompilerIdentity{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var identity CompilerIdentity
	if err := decoder.Decode(&identity); err != nil {
		return CompilerIdentity{}, fmt.Errorf("decode compiler identity: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return CompilerIdentity{}, err
	}
	if err := ValidateCompilerIdentity(identity); err != nil {
		return CompilerIdentity{}, fmt.Errorf("validate compiler identity: %w", err)
	}
	return identity, nil
}

func CompilerIdentityDigest(identity CompilerIdentity) (string, error) {
	data, err := CanonicalCompilerIdentityJSON(identity)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte("linux.bzl/cc-compiler-identity/v1\x00"), data...))
	return hex.EncodeToString(sum[:]), nil
}

func validateArchitecture(architecture string) error {
	if architecture != "x86_64" && architecture != "aarch64" {
		return fmt.Errorf("unsupported architecture %q", architecture)
	}
	return nil
}

func validateAnalysisIdentity(identity AnalysisIdentity) error {
	if identity.Compiler != "clang" && identity.Compiler != "gcc" {
		return fmt.Errorf("analysis_identity.compiler %q must be clang or gcc", identity.Compiler)
	}
	return validateText(identity.TargetGNUSystemName, "analysis_identity.target_gnu_system_name")
}

func analysisIdentityEqual(left, right AnalysisIdentity) bool {
	return left.Compiler == right.Compiler &&
		left.TargetGNUSystemName == right.TargetGNUSystemName
}

func firstNonemptyLine(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line != "" {
			return line
		}
	}
	return ""
}

func cloneStringMap(values map[string]string) map[string]string {
	cloned := make(map[string]string, len(values))
	for name, value := range values {
		cloned[name] = value
	}
	return cloned
}

func environmentList(environment map[string]string) []string {
	names := make([]string, 0, len(environment))
	for name := range environment {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]string, 0, len(names))
	for _, name := range names {
		result = append(result, name+"="+environment[name])
	}
	return result
}

func marshalCanonical(value any, context string) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", context, err)
	}
	return append(data, '\n'), nil
}
