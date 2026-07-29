package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/ccprofile"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: ccprofile <check|compare|compile|inspect|probe|validate> [flags]")
	}
	switch args[0] {
	case "check":
		return runCheck(args[1:])
	case "compare":
		return runCompare(args[1:])
	case "compile":
		return runCompile(args[1:])
	case "inspect":
		return runInspect(args[1:])
	case "probe":
		return runProbe(args[1:])
	case "validate":
		return runValidate(args[1:])
	default:
		return fmt.Errorf("unsupported ccprofile command %q", args[0])
	}
}

const structuralProbeRequestsSchema = "linux.bzl/cc-structural-probe-requests-v1"

type structuralProbeRequestManifest struct {
	Schema           string                   `json:"schema"`
	StructuralProbes []structuralProbeRequest `json:"structural_probes"`
}

type structuralProbeRequest struct {
	ID         string   `json:"id"`
	Kind       string   `json:"kind"`
	Language   string   `json:"language"`
	PrefixArgv []string `json:"prefix_argv"`
	Argv       []string `json:"argv"`
}

type repeatedStringFlag []string

func (values *repeatedStringFlag) String() string {
	return strings.Join(*values, ",")
}

func (values *repeatedStringFlag) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func runCheck(args []string) error {
	flags := flag.NewFlagSet("ccprofile check", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	profilePath := flags.String("profile", "", "CC capability profile")
	canonicalOut := flags.String("canonical_out", "", "canonical JSON output")
	digestOut := flags.String("digest_out", "", "profile digest output")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *profilePath == "" {
		return fmt.Errorf("ccprofile check requires -profile and no positional arguments")
	}
	profile, err := readProfile(*profilePath)
	if err != nil {
		return err
	}
	if *canonicalOut != "" {
		data, err := ccprofile.CanonicalJSON(profile)
		if err != nil {
			return err
		}
		if err := os.WriteFile(*canonicalOut, data, 0o644); err != nil {
			return fmt.Errorf("write canonical profile: %w", err)
		}
	}
	if *digestOut != "" {
		digest, err := ccprofile.Digest(profile)
		if err != nil {
			return err
		}
		if err := os.WriteFile(*digestOut, []byte(digest+"\n"), 0o644); err != nil {
			return fmt.Errorf("write profile digest: %w", err)
		}
	}
	return nil
}

func runCompare(args []string) error {
	flags := flag.NewFlagSet("ccprofile compare", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	expectedPath := flags.String("expected", "", "expected CC capability profile")
	actualPath := flags.String("actual", "", "actual CC capability profile")
	out := flags.String("out", "", "validation stamp")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *expectedPath == "" || *actualPath == "" || *out == "" {
		return fmt.Errorf("ccprofile compare requires -expected, -actual, -out and no positional arguments")
	}
	expected, err := readProfile(*expectedPath)
	if err != nil {
		return fmt.Errorf("expected profile: %w", err)
	}
	actual, err := readProfile(*actualPath)
	if err != nil {
		return fmt.Errorf("actual profile: %w", err)
	}
	if err := ccprofile.Compare(expected, actual); err != nil {
		return err
	}
	digest, err := ccprofile.Digest(expected)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, []byte("profile_digest="+digest+"\n"), 0o644); err != nil {
		return fmt.Errorf("write validation stamp: %w", err)
	}
	return nil
}

func runCompile(args []string) error {
	flags := flag.NewFlagSet("ccprofile compile", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	templatePath := flags.String("template", "", "validated CC command template")
	validationPath := flags.String("validation", "", "CC profile validation stamp")
	source := flags.String("source", "", "compile source")
	output := flags.String("output", "", "compile output")
	var objectArgs repeatedStringFlag
	var removals repeatedStringFlag
	flags.Var(&objectArgs, "arg", "mutable object compile argument; repeat in command-line order")
	flags.Var(&removals, "remove", "exact mutable argument to remove; repeat as needed")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 ||
		*templatePath == "" ||
		*validationPath == "" ||
		*source == "" ||
		*output == "" {
		return fmt.Errorf(
			"ccprofile compile requires -template, -validation, -source, -output and no positional arguments",
		)
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

func runProbe(args []string) error {
	flags := flag.NewFlagSet("ccprofile probe", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	profilePath := flags.String("profile", "", "base CC capability profile")
	requestsPath := flags.String("requests", "", "canonical structural-probe requests")
	compiler := flags.String("compiler", "", "selected compiler executable")
	linker := flags.String("linker", "", "selected linker executable")
	sourceRoot := flags.String("source_root", "", "Linux source root for canonical request path expansion")
	objectRoot := flags.String("object_root", "", "Linux object root for canonical request path expansion")
	replace := flags.Bool("replace", false, "replace existing structural probes instead of merging requests")
	out := flags.String("out", "", "canonical populated CC capability profile")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 ||
		*profilePath == "" ||
		*requestsPath == "" ||
		*compiler == "" ||
		*linker == "" ||
		*out == "" {
		return fmt.Errorf(
			"ccprofile probe requires profile, requests, compiler, linker, out, and no positional arguments",
		)
	}
	profile, err := readProfile(*profilePath)
	if err != nil {
		return fmt.Errorf("base profile: %w", err)
	}
	requests, err := readStructuralProbeRequests(*requestsPath)
	if err != nil {
		return err
	}

	probesByID := make(map[string]ccprofile.StructuralProbe, len(profile.StructuralProbes)+len(requests))
	if !*replace {
		for _, probe := range profile.StructuralProbes {
			probesByID[probe.ID] = probe
		}
	}
	for index := range requests {
		supported, err := ccprofile.EvaluateStructuralProbe(
			context.Background(),
			profile,
			requests[index],
			ccprofile.StructuralProbeTools{
				Compiler:   *compiler,
				Linker:     *linker,
				SourceRoot: *sourceRoot,
				ObjectRoot: *objectRoot,
			},
		)
		if err != nil {
			return fmt.Errorf("evaluate structural_probes[%d]: %w", index, err)
		}
		requests[index].Supported = supported
		probesByID[requests[index].ID] = requests[index]
	}
	probes := make([]ccprofile.StructuralProbe, 0, len(probesByID))
	for _, probe := range probesByID {
		probes = append(probes, probe)
	}
	slices.SortFunc(probes, func(left, right ccprofile.StructuralProbe) int {
		return strings.Compare(left.ID, right.ID)
	})
	profile.StructuralProbes = probes
	data, err := ccprofile.CanonicalJSON(profile)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		return fmt.Errorf("write populated CC profile: %w", err)
	}
	return nil
}

func readStructuralProbeRequests(path string) ([]ccprofile.StructuralProbe, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open structural probe requests: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var manifest structuralProbeRequestManifest
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decode structural probe requests: %w", err)
	}
	if manifest.Schema != structuralProbeRequestsSchema {
		return nil, fmt.Errorf(
			"structural probe requests schema %q, want %q",
			manifest.Schema,
			structuralProbeRequestsSchema,
		)
	}
	if manifest.StructuralProbes == nil {
		return nil, fmt.Errorf("structural probe requests must contain structural_probes")
	}
	requests := make([]ccprofile.StructuralProbe, len(manifest.StructuralProbes))
	previousID := ""
	for index, request := range manifest.StructuralProbes {
		requests[index] = ccprofile.StructuralProbe{
			ID:         request.ID,
			Kind:       request.Kind,
			Language:   request.Language,
			PrefixArgv: request.PrefixArgv,
			Argv:       request.Argv,
		}
		if request.ID != ccprofile.StructuralProbeID(requests[index]) {
			return nil, fmt.Errorf(
				"structural_probes[%d] ID %q, want %q",
				index,
				request.ID,
				ccprofile.StructuralProbeID(requests[index]),
			)
		}
		if request.ID <= previousID {
			return nil, fmt.Errorf("structural probe requests must be sorted by unique ID")
		}
		previousID = request.ID
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, fmt.Errorf("structural probe requests contain trailing JSON")
	}
	return requests, nil
}

func runValidate(args []string) error {
	flags := flag.NewFlagSet("ccprofile validate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	profilePath := flags.String("profile", "", "expected CC capability profile")
	identityPath := flags.String("identity", "", "inspected compiler identity")
	out := flags.String("out", "", "validation stamp")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *profilePath == "" || *identityPath == "" || *out == "" {
		return fmt.Errorf("ccprofile validate requires -profile, -identity, -out and no positional arguments")
	}
	profile, err := readProfile(*profilePath)
	if err != nil {
		return fmt.Errorf("expected profile: %w", err)
	}
	identity, err := readCompilerIdentity(*identityPath)
	if err != nil {
		return err
	}
	if err := ccprofile.ValidateProfileIdentity(profile, identity); err != nil {
		return fmt.Errorf("validate compiler identity: %w", err)
	}
	profileDigest, err := ccprofile.Digest(profile)
	if err != nil {
		return err
	}
	identityDigest, err := ccprofile.CompilerIdentityDigest(identity)
	if err != nil {
		return err
	}
	stamp := "profile_digest=" + profileDigest + "\n" +
		"compiler_identity_digest=" + identityDigest + "\n" +
		"validation_scope=compiler\n"
	if err := os.WriteFile(*out, []byte(stamp), 0o644); err != nil {
		return fmt.Errorf("write validation stamp: %w", err)
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

func readProfile(path string) (ccprofile.Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ccprofile.Profile{}, fmt.Errorf("read %s: %w", path, err)
	}
	profile, err := ccprofile.Decode(data)
	if err != nil {
		return ccprofile.Profile{}, fmt.Errorf("decode %s: %w", path, err)
	}
	return profile, nil
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
