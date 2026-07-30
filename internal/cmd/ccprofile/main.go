package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/ccprofile"
)

func main() {
	if filepath.Base(os.Args[0]) == "grep" {
		os.Exit(runGrep(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: ccprofile <compile|concat-node|inspect|link|merge-graph|refresh-graph|resolve-node|validate-graph> [flags]")
	}
	switch args[0] {
	case "compile":
		return runCompile(args[1:])
	case "concat-node":
		return runConcatNode(args[1:])
	case "inspect":
		return runInspect(args[1:])
	case "link":
		return runLink(args[1:])
	case "merge-graph":
		return runMergeGraph(args[1:])
	case "refresh-graph":
		return runRefreshGraph(args[1:])
	case "resolve-node":
		return runResolveNode(args[1:])
	case "validate-graph":
		return runValidateGraph(args[1:])
	default:
		return fmt.Errorf("unsupported ccprofile command %q", args[0])
	}
}

func runRefreshGraph(args []string) error {
	flags := flag.NewFlagSet("ccprofile refresh-graph", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	profilePath := flags.String("profile", "", "recorded toolchain graph profile")
	identityPath := flags.String("identity", "", "inspected compiler identity")
	templatePath := flags.String("template", "", "configured compiler command template")
	archiver := flags.String("archiver", "", "configured archiver")
	linker := flags.String("linker", "", "configured raw linker")
	nm := flags.String("nm", "", "configured symbol table tool")
	objcopy := flags.String("objcopy", "", "configured object copy tool")
	coreutils := flags.String("coreutils", "", "hermetic coreutils multicall binary")
	grep := flags.String("grep", "", "hermetic grep executable")
	shell := flags.String("shell", "", "execution-platform shell")
	sourceRoot := flags.String("source_root", "", "Linux source root")
	objectRoot := flags.String("object_root", "", "optional Linux object root")
	out := flags.String("out", "", "canonical refreshed graph profile")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 ||
		*profilePath == "" ||
		*identityPath == "" ||
		*templatePath == "" ||
		*archiver == "" ||
		*linker == "" ||
		*nm == "" ||
		*objcopy == "" ||
		*coreutils == "" ||
		*grep == "" ||
		*shell == "" ||
		*sourceRoot == "" ||
		*out == "" {
		return fmt.Errorf(
			"ccprofile refresh-graph requires -profile, -identity, -template, -archiver, -linker, -nm, -objcopy, -coreutils, -grep, -shell, -source_root, -out, and no positional arguments",
		)
	}
	identity, err := readCompilerIdentity(*identityPath)
	if err != nil {
		return err
	}
	template, err := readCommandTemplate(*templatePath)
	if err != nil {
		return err
	}
	if template.Architecture != identity.Architecture ||
		template.DriverContract != identity.DriverContract ||
		template.AnalysisIdentity != identity.AnalysisIdentity {
		return fmt.Errorf("compiler template and inspected identity disagree")
	}
	data, err := os.ReadFile(*profilePath)
	if err != nil {
		return fmt.Errorf("read graph profile %s: %w", *profilePath, err)
	}
	profile, err := ccprofile.DecodeGraphProfile(data)
	if err != nil {
		return fmt.Errorf("decode graph profile %s: %w", *profilePath, err)
	}
	if profile.Architecture != identity.Architecture ||
		profile.DriverContract != identity.DriverContract ||
		profile.AnalysisIdentity != identity.AnalysisIdentity {
		return fmt.Errorf("recorded graph profile and inspected compiler identity disagree")
	}
	refreshed, err := ccprofile.RefreshConfiguredGraphProfile(
		context.Background(),
		profile,
		ccprofile.KbuildGraphProbeTools{
			CommandTemplate: template,
			Archiver:        *archiver,
			Coreutils:       *coreutils,
			Grep:            *grep,
			Linker:          *linker,
			NM:              *nm,
			Objcopy:         *objcopy,
			Shell:           *shell,
			SourceRoot:      *sourceRoot,
			ObjectRoot:      *objectRoot,
		},
	)
	if err != nil {
		return fmt.Errorf("refresh configured graph profile: %w", err)
	}
	refreshedData, err := ccprofile.CanonicalGraphProfileJSON(refreshed)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, refreshedData, 0o644); err != nil {
		return fmt.Errorf("write refreshed graph profile: %w", err)
	}
	return nil
}

func runMergeGraph(args []string) error {
	flags := flag.NewFlagSet("ccprofile merge-graph", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	out := flags.String("out", "", "canonical merged toolchain graph profile")
	var inputs repeatedStringFlag
	flags.Var(&inputs, "input", "canonical toolchain graph profile; repeat in merge order")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || len(inputs) == 0 || *out == "" {
		return fmt.Errorf(
			"ccprofile merge-graph requires at least one -input, -out, and no positional arguments",
		)
	}
	profiles := make([]ccprofile.GraphProfile, len(inputs))
	for index, path := range inputs {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read graph profile input %d %s: %w", index, path, err)
		}
		profiles[index], err = ccprofile.DecodeGraphProfile(data)
		if err != nil {
			return fmt.Errorf("decode graph profile input %d %s: %w", index, path, err)
		}
	}
	merged, err := ccprofile.MergeGraphProfiles(profiles...)
	if err != nil {
		return fmt.Errorf("merge graph profiles: %w", err)
	}
	data, err := ccprofile.CanonicalGraphProfileJSON(merged)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		return fmt.Errorf("write merged graph profile: %w", err)
	}
	return nil
}

type repeatedStringFlag []string

func (values *repeatedStringFlag) String() string {
	return strings.Join(*values, ",")
}

func (values *repeatedStringFlag) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func runCompile(args []string) error {
	flags := flag.NewFlagSet("ccprofile compile", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	templatePath := flags.String("template", "", "validated CC command template")
	validationPath := flags.String("validation", "", "configured graph validation stamp")
	source := flags.String("source", "", "compile source")
	kbuildSource := flags.String("kbuild_source", "", "original Kbuild source used for $(src) expansion")
	output := flags.String("output", "", "compile output")
	flagsFile := flags.String("flags_file", "", "newline-delimited object arguments")
	removeFlagsFile := flags.String("remove_flags_file", "", "newline-delimited exact removals")
	configPath := flags.String("config", "", "resolved Linux .config used for CONFIG_* Make references")
	sourceRoot := flags.String("source_root", "", "Linux source directory for Kbuild reference expansion")
	objectPath := flags.String("object_path", "", "source-root-relative object output path")
	objectRoot := flags.String("object_root", "", "execution-time object directory for Kbuild $(obj) references")
	utsversionTmp := flags.String("utsversion_tmp", "", "generated object-local utsversion-tmp.h")
	var objectArgs repeatedStringFlag
	var removals repeatedStringFlag
	flags.Var(&objectArgs, "arg", "mutable object compile argument; repeat in command-line order")
	flags.Var(&removals, "remove", "exact mutable argument to remove; repeat as needed")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *templatePath == "" ||
		*validationPath == "" ||
		*source == "" ||
		*configPath == "" ||
		*output == "" {
		return fmt.Errorf(
			"ccprofile compile requires -template, -validation, -source, -config, and -output",
		)
	}
	objectArgs = append(objectArgs, flags.Args()...)
	expansionSource := *source
	if *kbuildSource != "" {
		expansionSource = *kbuildSource
	}
	validationData, err := os.ReadFile(*validationPath)
	if err != nil {
		return fmt.Errorf("read validation stamp %s: %w", *validationPath, err)
	}
	if _, err := ccprofile.DecodeValidationStamp(validationData); err != nil {
		return fmt.Errorf("decode validation stamp %s: %w", *validationPath, err)
	}
	templateData, err := os.ReadFile(*templatePath)
	if err != nil {
		return fmt.Errorf("read command template %s: %w", *templatePath, err)
	}
	template, err := ccprofile.DecodeCommandTemplate(templateData)
	if err != nil {
		return fmt.Errorf("decode command template %s: %w", *templatePath, err)
	}
	configValues, err := readKbuildConfig(*configPath)
	if err != nil {
		return fmt.Errorf("read compile config: %w", err)
	}
	objectArgs, err = expandKbuildResponseFiles(
		objectArgs,
		configValues,
		expansionSource,
		*sourceRoot,
		*objectPath,
		*objectRoot,
		*utsversionTmp,
	)
	if err != nil {
		return fmt.Errorf("expand compile flags and response files: %w", err)
	}
	if *flagsFile != "" {
		fileArgv, err := readArgvFile(*flagsFile)
		if err != nil {
			return fmt.Errorf("read compile flags file: %w", err)
		}
		fileArgv, err = expandKbuildProgramArgv(
			fileArgv,
			configValues,
			expansionSource,
			*sourceRoot,
			*objectPath,
			*objectRoot,
			*utsversionTmp,
		)
		if err != nil {
			return fmt.Errorf("expand compile flag program: %w", err)
		}
		objectArgs = append(objectArgs, fileArgv...)
	}
	if *removeFlagsFile != "" {
		fileRemovals, err := readArgvFile(*removeFlagsFile)
		if err != nil {
			return fmt.Errorf("read compile remove-flags file: %w", err)
		}
		fileRemovals, err = expandKbuildProgramArgv(
			fileRemovals,
			configValues,
			expansionSource,
			*sourceRoot,
			*objectPath,
			*objectRoot,
			*utsversionTmp,
		)
		if err != nil {
			return fmt.Errorf("expand compile remove-flag program: %w", err)
		}
		removals = append(removals, fileRemovals...)
	}
	if err := ccprofile.Compile(
		context.Background(),
		template,
		*source,
		*output,
		objectArgs,
		removals,
		os.Stdout,
		os.Stderr,
	); err != nil {
		return err
	}
	return nil
}

func runInspect(args []string) error {
	flags := flag.NewFlagSet("ccprofile inspect", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	architecture := flags.String("architecture", "", "profile architecture")
	analysisCompiler := flags.String("analysis_compiler", "", "analysis compiler family")
	analysisTarget := flags.String("analysis_target_gnu_system_name", "", "analysis GNU target")
	compiler := flags.String("compiler", "", "selected compiler executable")
	sourceSentinel := flags.String("source_sentinel", "", "compile source sentinel")
	outputSentinel := flags.String("output_sentinel", "", "compile output sentinel")
	kbuildFlagsSentinel := flags.String("kbuild_flags_sentinel", "", "Kbuild flags sentinel")
	templateOut := flags.String("template_out", "", "canonical command template output")
	identityOut := flags.String("identity_out", "", "canonical compiler identity output")
	var compileArgv repeatedStringFlag
	var compileEnvironment repeatedStringFlag
	flags.Var(&compileArgv, "compile_arg", "compile argument; repeat in command-line order")
	flags.Var(&compileEnvironment, "compile_env", "compile environment NAME=VALUE; repeat by name")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 ||
		*architecture == "" ||
		*analysisCompiler == "" ||
		*analysisTarget == "" ||
		*compiler == "" ||
		*sourceSentinel == "" ||
		*outputSentinel == "" ||
		*kbuildFlagsSentinel == "" ||
		*templateOut == "" ||
		*identityOut == "" {
		return fmt.Errorf(
			"ccprofile inspect requires architecture, analysis identity, compiler, all sentinels, template_out, identity_out, and no positional arguments",
		)
	}
	environment, err := parseEnvironment(compileEnvironment)
	if err != nil {
		return err
	}
	template, err := ccprofile.NewCommandTemplate(
		*architecture,
		ccprofile.AnalysisIdentity{
			Compiler:            *analysisCompiler,
			TargetGNUSystemName: *analysisTarget,
		},
		*compiler,
		compileArgv,
		environment,
		ccprofile.CompileSentinels{
			Source:      *sourceSentinel,
			Output:      *outputSentinel,
			KbuildFlags: *kbuildFlagsSentinel,
		},
	)
	if err != nil {
		return fmt.Errorf("extract compile command template: %w", err)
	}
	identity, err := ccprofile.InspectCompiler(context.Background(), template)
	if err != nil {
		return err
	}
	templateData, err := ccprofile.CanonicalCommandTemplateJSON(template)
	if err != nil {
		return err
	}
	identityData, err := ccprofile.CanonicalCompilerIdentityJSON(identity)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*templateOut, templateData, 0o644); err != nil {
		return fmt.Errorf("write command template: %w", err)
	}
	if err := os.WriteFile(*identityOut, identityData, 0o644); err != nil {
		return fmt.Errorf("write compiler identity: %w", err)
	}
	return nil
}

func runValidateGraph(args []string) error {
	flags := flag.NewFlagSet("ccprofile validate-graph", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	profilePath := flags.String("profile", "", "checked-in toolchain graph profile")
	projectionPath := flags.String("projection", "", "consumed graph projection")
	identityPath := flags.String("identity", "", "inspected compiler identity")
	templatePath := flags.String("template", "", "configured compiler command template")
	archiver := flags.String("archiver", "", "configured archiver")
	linker := flags.String("linker", "", "configured raw linker")
	nm := flags.String("nm", "", "configured symbol table tool")
	objcopy := flags.String("objcopy", "", "configured object copy tool")
	coreutils := flags.String("coreutils", "", "hermetic coreutils multicall binary")
	grep := flags.String("grep", "", "hermetic grep executable")
	shell := flags.String("shell", "", "execution-platform shell")
	sourceRoot := flags.String("source_root", "", "Linux source root")
	objectRoot := flags.String("object_root", "", "optional Linux object root")
	out := flags.String("out", "", "validation stamp")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 ||
		(*profilePath == "") == (*projectionPath == "") ||
		*identityPath == "" ||
		*templatePath == "" ||
		*archiver == "" ||
		*linker == "" ||
		*nm == "" ||
		*objcopy == "" ||
		*coreutils == "" ||
		*grep == "" ||
		*shell == "" ||
		*sourceRoot == "" ||
		*out == "" {
		return fmt.Errorf(
			"ccprofile validate-graph requires exactly one of -profile or -projection, plus -identity, -template, -archiver, -linker, -nm, -objcopy, -coreutils, -grep, -shell, -source_root, -out, and no positional arguments",
		)
	}
	identity, err := readCompilerIdentity(*identityPath)
	if err != nil {
		return err
	}
	template, err := readCommandTemplate(*templatePath)
	if err != nil {
		return err
	}
	if template.Architecture != identity.Architecture ||
		template.DriverContract != identity.DriverContract ||
		template.AnalysisIdentity != identity.AnalysisIdentity {
		return fmt.Errorf("compiler template and inspected identity disagree")
	}
	graphPath := *profilePath
	if graphPath == "" {
		graphPath = *projectionPath
	}
	graphData, err := os.ReadFile(graphPath)
	if err != nil {
		return fmt.Errorf("read graph identity input %s: %w", graphPath, err)
	}
	var schemaHeader struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(graphData, &schemaHeader); err != nil {
		return fmt.Errorf("decode graph identity input %s: %w", graphPath, err)
	}
	var digest string
	var kconfigCommands []ccprofile.KconfigCommand
	var graphProbes []ccprofile.KbuildGraphProbe
	replayKconfigCommands := ccprofile.ReplayConfiguredKconfigCommands
	replayKbuildProbes := ccprofile.ReplayConfiguredKbuildGraphProbes
	switch schemaHeader.Schema {
	case ccprofile.GraphProfileSchema:
		if *projectionPath != "" {
			return fmt.Errorf(
				"-projection input has graph profile schema %q",
				schemaHeader.Schema,
			)
		}
		profile, err := ccprofile.DecodeGraphProfile(graphData)
		if err != nil {
			return fmt.Errorf("decode graph profile %s: %w", graphPath, err)
		}
		if err := ccprofile.ValidateGraphProfileCompilerIdentity(profile, identity); err != nil {
			return fmt.Errorf("validate graph profile compiler identity: %w", err)
		}
		digest, err = ccprofile.GraphProfileDigest(profile)
		if err != nil {
			return err
		}
		kconfigCommands = profile.KconfigCommands
		graphProbes = profile.KbuildGraphProbes
		replayKconfigCommands = ccprofile.ReplayMatchingConfiguredKconfigCommands
		replayKbuildProbes = ccprofile.ReplayMatchingConfiguredKbuildGraphProbes
	case ccprofile.GraphProjectionSchema:
		projection, err := ccprofile.DecodeGraphProjection(graphData)
		if err != nil {
			return fmt.Errorf("decode graph projection %s: %w", graphPath, err)
		}
		if err := ccprofile.ValidateGraphProjectionCompilerIdentity(projection, identity); err != nil {
			return fmt.Errorf("validate graph projection compiler identity: %w", err)
		}
		digest, err = ccprofile.GraphProjectionDigest(projection)
		if err != nil {
			return err
		}
		kconfigCommands = projection.KconfigCommands
		graphProbes = projection.KbuildGraphProbes
	default:
		return fmt.Errorf(
			"graph identity input schema %q is unsupported",
			schemaHeader.Schema,
		)
	}
	if _, err := replayKconfigCommands(
		context.Background(),
		kconfigCommands,
		ccprofile.KbuildGraphProbeTools{
			CommandTemplate: template,
			Archiver:        *archiver,
			Coreutils:       *coreutils,
			Grep:            *grep,
			Linker:          *linker,
			NM:              *nm,
			Objcopy:         *objcopy,
			Shell:           *shell,
			SourceRoot:      *sourceRoot,
		},
	); err != nil {
		return fmt.Errorf("replay configured Kconfig commands: %w", err)
	}
	if _, err := replayKbuildProbes(
		context.Background(),
		graphProbes,
		ccprofile.KbuildGraphProbeTools{
			CommandTemplate: template,
			Linker:          *linker,
			SourceRoot:      *sourceRoot,
			ObjectRoot:      *objectRoot,
		},
	); err != nil {
		return fmt.Errorf("replay Kbuild graph probes: %w", err)
	}
	identityDigest, err := ccprofile.CompilerIdentityDigest(identity)
	if err != nil {
		return err
	}
	stamp := "profile_digest=" + digest + "\n" +
		"compiler_identity_digest=" + identityDigest + "\n" +
		"validation_scope=configured-graph\n"
	if err := os.WriteFile(*out, []byte(stamp), 0o644); err != nil {
		return fmt.Errorf("write graph validation stamp: %w", err)
	}
	return nil
}

func parseEnvironment(entries []string) (map[string]string, error) {
	environment := make(map[string]string, len(entries))
	for _, entry := range entries {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || name == "" || strings.ContainsAny(name, "=\x00") || strings.ContainsRune(value, '\x00') {
			return nil, fmt.Errorf("invalid compile environment entry %q", entry)
		}
		if _, exists := environment[name]; exists {
			return nil, fmt.Errorf("duplicate compile environment variable %q", name)
		}
		environment[name] = value
	}
	return environment, nil
}

func readCompilerIdentity(path string) (ccprofile.CompilerIdentity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ccprofile.CompilerIdentity{}, fmt.Errorf("read %s: %w", path, err)
	}
	identity, err := ccprofile.DecodeCompilerIdentity(data)
	if err != nil {
		return ccprofile.CompilerIdentity{}, fmt.Errorf("decode %s: %w", path, err)
	}
	return identity, nil
}
