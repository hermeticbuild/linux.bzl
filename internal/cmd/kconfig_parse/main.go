package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
	"github.com/hermeticbuild/linux.bzl/internal/rusttoolchain"
)

type stringMapFlag map[string]string

func (f stringMapFlag) String() string {
	return fmt.Sprint(map[string]string(f))
}

func (f stringMapFlag) Set(value string) error {
	key, val, ok := strings.Cut(value, "=")
	if !ok || key == "" {
		return fmt.Errorf("expected KEY=VALUE")
	}
	f[key] = val
	return nil
}

func applyRustToolchainProbe(
	vars stringMapFlag,
	probe rusttoolchain.Probe,
) (int, int) {
	vars["RUSTC_VERSION_TEXT"] = probe.VersionText
	return probe.VersionCode, probe.LLVMVersionCode
}

var fixedLinuxProbeEnvironment = map[string]string{
	"AR":              "llvm-ar",
	"BINDGEN":         "bindgen",
	"CC":              "clang",
	"CC_VERSION_TEXT": "clang version 22.1.8None",
	"CLANG_FLAGS":     "-fintegrated-as",
	"LD":              "ld.lld",
	"NM":              "llvm-nm",
	"OBJCOPY":         "llvm-objcopy",
	"PAHOLE":          "pahole",
	"PYTHON3":         "python3",
	"RUSTC":           "rustc",
}

func fixedLinuxProbeShell(
	architecture string,
	rustcVersion int,
	rustcLLVMVersion int,
	env stringMapFlag,
) (func(context.Context, string) (string, error), error) {
	if architecture == "" {
		return nil, nil
	}
	for name, value := range fixedLinuxProbeEnvironment {
		if configured, ok := env[name]; ok && configured != value {
			return nil, fmt.Errorf(
				"fixed Linux probe requires %s=%q, got %q",
				name,
				value,
				configured,
			)
		}
		env[name] = value
	}
	return kconfig.LinuxProbeShell(architecture, rustcVersion, rustcLLVMVersion)
}

type stringSliceFlag []string

func (f *stringSliceFlag) String() string {
	return strings.Join(*f, ",")
}

func (f *stringSliceFlag) Set(value string) error {
	if value == "" {
		return fmt.Errorf("empty value")
	}
	*f = append(*f, value)
	return nil
}

type namedPath struct {
	Name string
	Path string
}

type namedPathFlag []namedPath

func (f *namedPathFlag) String() string {
	parts := make([]string, len(*f))
	for i, value := range *f {
		parts[i] = value.Name + "=" + value.Path
	}
	return strings.Join(parts, ",")
}

func (f *namedPathFlag) Set(value string) error {
	name, path, ok := strings.Cut(value, "=")
	if !ok || name == "" || path == "" {
		return fmt.Errorf("expected NAME=PATH")
	}
	*f = append(*f, namedPath{Name: name, Path: path})
	return nil
}

func namedPathMap(values []namedPath) map[string]string {
	out := map[string]string{}
	for _, value := range values {
		out[value.Name] = workspacePath(value.Path)
	}
	return out
}

func compactGeneratedHeaderLabels(
	configs []namedPath,
	values []namedPath,
) (map[string]string, error) {
	configNames := map[string]bool{}
	for _, config := range configs {
		configNames[config.Name] = true
	}
	out := map[string]string{}
	for _, value := range values {
		if !configNames[value.Name] {
			return nil, fmt.Errorf("generated headers config %q has no matching -config", value.Name)
		}
		if _, ok := out[value.Name]; ok {
			return nil, fmt.Errorf("duplicate generated headers binding for config %q", value.Name)
		}
		out[value.Name] = value.Path
	}
	var missing []string
	for name := range configNames {
		if out[name] == "" {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) != 0 {
		return nil, fmt.Errorf("missing -generated_headers_for_config for %s", strings.Join(missing, ", "))
	}
	return out, nil
}

func syntheticKconfigRoot(root string, extras []namedPath) (string, func(), error) {
	file, err := os.CreateTemp("", "linux-bzl-kconfig-root-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() {
		_ = os.Remove(file.Name())
	}
	if _, err := fmt.Fprintf(file, "source %q\n", workspacePath(root)); err != nil {
		file.Close()
		cleanup()
		return "", nil, err
	}
	for _, extra := range extras {
		path := filepath.ToSlash(filepath.Join(extra.Name, filepath.Base(extra.Path)))
		if _, err := fmt.Fprintf(file, "source %q\n", path); err != nil {
			file.Close()
			cleanup()
			return "", nil, err
		}
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return file.Name(), cleanup, nil
}

func startRuntimeProfiles(cpuPath, heapPath, allocsPath string) (func() error, error) {
	var cpuFile *os.File
	if cpuPath != "" {
		var err error
		cpuFile, err = os.Create(cpuPath)
		if err != nil {
			return nil, fmt.Errorf("create CPU profile: %w", err)
		}
		if err := pprof.StartCPUProfile(cpuFile); err != nil {
			cpuFile.Close()
			return nil, fmt.Errorf("start CPU profile: %w", err)
		}
	}

	return func() error {
		var profileErrors []error
		if cpuFile != nil {
			pprof.StopCPUProfile()
			if err := cpuFile.Close(); err != nil {
				profileErrors = append(profileErrors, fmt.Errorf("close CPU profile: %w", err))
			}
		}
		for _, profile := range []struct {
			name string
			path string
			gc   bool
		}{
			{name: "allocs", path: allocsPath},
			{name: "heap", path: heapPath, gc: true},
		} {
			name, path := profile.name, profile.path
			if path == "" {
				continue
			}
			if profile.gc {
				runtime.GC()
			}
			file, err := os.Create(path)
			if err != nil {
				profileErrors = append(profileErrors, fmt.Errorf("create %s profile: %w", name, err))
				continue
			}
			if err := pprof.Lookup(name).WriteTo(file, 0); err != nil {
				profileErrors = append(profileErrors, fmt.Errorf("write %s profile: %w", name, err))
			}
			if err := file.Close(); err != nil {
				profileErrors = append(profileErrors, fmt.Errorf("close %s profile: %w", name, err))
			}
		}
		return errors.Join(profileErrors...)
	}, nil
}

func main() {
	os.Exit(run())
}

func run() (exitCode int) {
	var (
		root                     = flag.String("root", "", "Root Kconfig file to parse")
		srctree                  = flag.String("srctree", "", "Source tree used to resolve source statements")
		allowShell               = flag.Bool("allow_shell", false, "Allow $(shell,...) expansion")
		linuxProbeArch           = flag.String("linux_probe_arch", "", "Linux architecture for the fixed Clang 22.1.8 Kconfig probe policy")
		linuxProbeRustcVersion   = flag.Int("linux_probe_rustc_version", kconfig.LinuxProbeDefaultRustcVersion, "Linux-encoded Rust compiler version for repository-time Kconfig resolution")
		linuxProbeRustcLLVM      = flag.Int("linux_probe_rustc_llvm_version", kconfig.LinuxProbeDefaultRustcLLVMVersion, "Linux-encoded Rust LLVM version for repository-time Kconfig resolution")
		rustToolchainProbe       = flag.String("rust_toolchain_probe", "", "JSON identity produced from the selected rustc -vV output")
		validateConfigEquivalent = flag.Bool("validate_config_equivalence", false, "Require action-time config to match the repository-generated structural snapshot")
		out                      = flag.String("out", "", "Path to write the parsed Kconfig as JSON. Defaults to stdout when no other output is set")
		kbuildPath               = flag.String("kbuild", "", "Kbuild/Makefile path for compact object metadata generation")
		kbuildRecursive          = flag.Bool("kbuild_recursive", false, "Follow static Kbuild include directives when writing -kbuild_out")
		kbuildSrctree            = flag.String("kbuild_srctree", "", "Source tree used to resolve recursive Kbuild includes. Defaults to -srctree")
		kbuildOut                = flag.String("kbuild_out", "", "Path to write the parsed Kbuild/Makefile as JSON")
		kbuildTreeRoot           = flag.String("kbuild_tree_root", "", "Linux source tree root to recursively validate Kbuild/Makefile/*.mk files")
		kbuildTreeOut            = flag.String("kbuild_tree_out", "", "Path to write recursive Kbuild tree validation summary JSON")
		kbuildTreeMinCount       = flag.Int("kbuild_tree_min_count", 0, "Minimum number of Kbuild-like files that must be parsed during -kbuild_tree_root validation")
		compactMetadataOut       = flag.String("compact_metadata_out", "", "Path to write compact fragment-keyed Linux metadata JSON")
		compactBuildfileOut      = flag.String("compact_buildfile_out", "", "Path to write a combined compact object/image BUILD file")
		compactBaseConfig        = flag.String("compact_base_config", "", "Base config name for delta image BUILD emission")
		compileEnvironmentABI    = flag.String("compile_environment_abi", "", "Toolchain/action ABI bound into compile environment content IDs")
		rustProfileOut           = flag.String("rust_profile_out", "", "Path to write the source-derived Rust profile JSON")
		compactKbuildTree        = flag.Bool("compact_kbuild_tree", false, "Follow active Kbuild directory descent when generating compact metadata")
		resolveConfig            = flag.String("resolve_config", "", "Named .config input in NAME=PATH form to resolve through Kconfig defaults and dependencies")
		configMode               = flag.String("config_mode", "default", "Config resolver mode. Supported: default, allnoconfig")
		resolvedConfigOut        = flag.String("resolved_config_out", "", "Path to write the resolved .config")
		resolvedAutoConfOut      = flag.String("resolved_auto_conf_out", "", "Path to write the resolved include/config/auto.conf")
		resolvedCmdOut           = flag.String("resolved_auto_conf_cmd_out", "", "Path to write the resolved include/config/auto.conf.cmd")
		resolvedAutoconfOut      = flag.String("resolved_autoconf_out", "", "Path to write the resolved include/generated/autoconf.h")
		resolvedRustcCfgOut      = flag.String("resolved_rustc_cfg_out", "", "Path to write the resolved include/generated/rustc_cfg")
		resolvedReleaseOut       = flag.String("resolved_kernel_release_out", "", "Path to write the resolved include/config/kernel.release")
		kernelVersion            = flag.String("kernel_version", "6.18.2", "Base kernel release used when writing resolved config outputs")
		objectLabelPackage       = flag.String("object_label_package", "", "Bazel package containing the compact object targets. Defaults to the -compact_buildfile_out package")
		sourceLabelPackage       = flag.String("source_label_package", "", "Bazel package containing Linux source file labels for generated compact object BUILD files")
		sourceASN1Compiler       = flag.String("source_asn1_compiler", "", "Bazel label for the kernel source tree's scripts/asn1_compiler tool emitted into source-backed compact object rules")
		sourceObjtool            = flag.String("source_objtool", "", "Bazel label for the kernel source tree's objtool executable emitted into x86 source-backed compact object rules")
		sourceRelacheck          = flag.String("source_relacheck", "", "Bazel label for the kernel source tree's arch/arm64/kernel/pi/relacheck tool emitted into arm64 .pi.o rules")
		sourceRootLabel          = flag.String("source_root_label", "", "Bazel label for a file in the Linux source root, emitted into source-backed compact object rules")
		linuxObjectsLoad         = flag.String("linux_objects_load", "", "Load label for the compact Linux object/image rules")
		vars                     = stringMapFlag{}
		env                      = stringMapFlag{}
		visibility               = stringSliceFlag{}
		kbuildTreeExcludes       = stringSliceFlag{}
		compactConfigInputs      = namedPathFlag{}
		compactExports           = stringSliceFlag{}
		sourceRootMaps           = namedPathFlag{}
		kconfigExtras            = namedPathFlag{}
		generatedHeadersByConfig = namedPathFlag{}
		cpuProfile               = flag.String("cpu_profile", "", "Optional path to write a Go CPU profile")
		heapProfile              = flag.String("heap_profile", "", "Optional path to write a Go live-heap profile")
		allocsProfile            = flag.String("allocs_profile", "", "Optional path to write a Go cumulative-allocation profile")
	)
	flag.Var(vars, "var", "Preprocessor variable in KEY=VALUE form. May be repeated")
	flag.Var(env, "env", "Hermetic environment variable in KEY=VALUE form. May be repeated")
	flag.Var(&visibility, "visibility", "Default visibility for the generated BUILD file. May be repeated. Defaults to //visibility:public")
	flag.Var(&kbuildTreeExcludes, "kbuild_tree_exclude", "Source-root-relative subtree to skip during -kbuild_tree_root validation. May be repeated")
	flag.Var(&compactConfigInputs, "config", "Named .config input in NAME=PATH form for compact metadata generation. May be repeated")
	flag.Var(&compactExports, "compact_buildfile_export", "Source filename exported by the generated compact BUILD file. May be repeated")
	flag.Var(&sourceRootMaps, "source_root_map", "Virtual source prefix to filesystem root in PREFIX=PATH form. May be repeated")
	flag.Var(&kconfigExtras, "kconfig_extra", "Extra Kconfig source in PREFIX=PATH form. May be repeated")
	flag.Var(&generatedHeadersByConfig, "generated_headers_for_config", "Generated headers binding in NAME=LABEL form. May be repeated once per compact config")
	flag.Parse()

	stopProfiles, err := startRuntimeProfiles(*cpuProfile, *heapProfile, *allocsProfile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start runtime profiles: %v\n", err)
		return 2
	}
	defer func() {
		if err := stopProfiles(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to finish runtime profiles: %v\n", err)
			if exitCode == 0 {
				exitCode = 1
			}
		}
	}()

	if *root == "" && *kbuildOut == "" && *kbuildTreeOut == "" && *rustProfileOut == "" {
		flag.PrintDefaults()
		return 2
	}

	var tree *kconfig.Tree
	if *root != "" {
		var err error
		if *rustToolchainProbe != "" {
			if *linuxProbeArch == "" {
				fmt.Fprintln(os.Stderr, "-rust_toolchain_probe requires -linux_probe_arch")
				return 2
			}
			probeFile, openErr := os.Open(workspacePath(*rustToolchainProbe))
			if openErr != nil {
				fmt.Fprintf(os.Stderr, "failed to open Rust toolchain probe: %v\n", openErr)
				return 1
			}
			probe, decodeErr := rusttoolchain.Decode(probeFile)
			closeErr := probeFile.Close()
			if decodeErr != nil {
				fmt.Fprintf(os.Stderr, "failed to decode Rust toolchain probe: %v\n", decodeErr)
				return 1
			}
			if closeErr != nil {
				fmt.Fprintf(os.Stderr, "failed to close Rust toolchain probe: %v\n", closeErr)
				return 1
			}
			*linuxProbeRustcVersion, *linuxProbeRustcLLVM = applyRustToolchainProbe(vars, probe)
		}
		shell, err := fixedLinuxProbeShell(
			*linuxProbeArch,
			*linuxProbeRustcVersion,
			*linuxProbeRustcLLVM,
			env,
		)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to configure fixed Linux probe: %v\n", err)
			return 2
		}
		sourceRoots := namedPathMap(sourceRootMaps)
		resolvedRoot := *root
		if *srctree == "" {
			resolvedRoot = workspacePath(*root)
		}
		resolvedSrctree := workspacePath(*srctree)
		if len(kconfigExtras) != 0 {
			synthetic, cleanup, err := syntheticKconfigRoot(resolvedRoot, kconfigExtras)
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to write synthetic Kconfig root: %v\n", err)
				return 1
			}
			defer cleanup()
			resolvedRoot = synthetic
		}
		tree, err = kconfig.ParseFile(context.Background(), resolvedRoot, kconfig.Options{
			RootDir:     resolvedSrctree,
			SourceRoots: sourceRoots,
			Variables:   vars,
			Env:         env,
			AllowShell:  *allowShell || shell != nil,
			Shell:       shell,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to parse Kconfig: %v\n", err)
			return 1
		}
	}

	if *kbuildOut != "" {
		if *kbuildPath == "" {
			fmt.Fprintf(os.Stderr, "-kbuild is required when -kbuild_out is set\n")
			return 2
		}
		kb, err := parseKbuild(*kbuildPath, *kbuildRecursive, *kbuildSrctree, *srctree, vars)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to parse Kbuild: %v\n", err)
			return 1
		}
		data, err := json.MarshalIndent(kb, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to encode Kbuild dump: %v\n", err)
			return 1
		}
		data = append(data, '\n')
		if err := os.WriteFile(workspacePath(*kbuildOut), data, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "failed to write Kbuild dump: %v\n", err)
			return 1
		}
	}

	if *kbuildTreeOut != "" {
		if *kbuildTreeRoot == "" {
			fmt.Fprintf(os.Stderr, "-kbuild_tree_root is required when -kbuild_tree_out is set\n")
			return 2
		}
		resolvedRoot := workspacePath(*kbuildTreeRoot)
		treeVars := map[string]string{}
		for key, value := range vars {
			treeVars[key] = value
		}
		if _, ok := treeVars["srctree"]; !ok {
			treeVars["srctree"] = resolvedRoot
		}
		summary, err := validateKbuildTree(resolvedRoot, kbuildTreeExcludes, treeVars)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to validate Kbuild tree: %v\n", err)
			return 1
		}
		if summary.Count < *kbuildTreeMinCount {
			fmt.Fprintf(os.Stderr, "failed to validate Kbuild tree: parsed %d files, want at least %d\n", summary.Count, *kbuildTreeMinCount)
			return 1
		}
		data, err := json.MarshalIndent(summary, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to encode Kbuild tree summary: %v\n", err)
			return 1
		}
		data = append(data, '\n')
		if err := os.WriteFile(workspacePath(*kbuildTreeOut), data, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "failed to write Kbuild tree summary: %v\n", err)
			return 1
		}
	}

	if *rustProfileOut != "" {
		if *srctree == "" {
			fmt.Fprintln(os.Stderr, "-srctree is required for -rust_profile_out")
			return 2
		}
		profile, err := kconfig.GenerateRustProfile(workspacePath(*srctree), vars["ARCH"])
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to generate Rust profile: %v\n", err)
			return 1
		}
		data, err := profile.JSON()
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to encode Rust profile: %v\n", err)
			return 1
		}
		if err := os.WriteFile(workspacePath(*rustProfileOut), data, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "failed to write Rust profile: %v\n", err)
			return 1
		}
	}

	resolvedConfigRequested := *resolveConfig != "" || *resolvedConfigOut != "" || *resolvedAutoConfOut != "" || *resolvedCmdOut != "" || *resolvedAutoconfOut != "" || *resolvedRustcCfgOut != "" || *resolvedReleaseOut != ""
	if tree == nil && (*compactMetadataOut != "" || *compactBuildfileOut != "" || resolvedConfigRequested || *out != "") {
		fmt.Fprintf(os.Stderr, "-root is required for Kconfig outputs\n")
		return 2
	}

	if resolvedConfigRequested {
		if err := writeResolvedConfig(tree, *resolveConfig, *configMode, resolvedConfigOutputs{
			config:        *resolvedConfigOut,
			autoConf:      *resolvedAutoConfOut,
			autoConfCmd:   *resolvedCmdOut,
			autoconf:      *resolvedAutoconfOut,
			rustcCfg:      *resolvedRustcCfgOut,
			kernelRelease: *resolvedReleaseOut,
		}, *kernelVersion, *validateConfigEquivalent); err != nil {
			fmt.Fprintf(os.Stderr, "failed to write resolved config: %v\n", err)
			return 1
		}
	}

	if *compactMetadataOut != "" || *compactBuildfileOut != "" {
		headerLabels, err := compactGeneratedHeaderLabels(
			compactConfigInputs,
			generatedHeadersByConfig,
		)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to configure compact generated headers: %v\n", err)
			return 2
		}
		metadata, err := compactMetadata(
			tree,
			*root,
			*kbuildPath,
			compactConfigInputs,
			*configMode,
			*compactKbuildTree,
			vars,
			namedPathMap(sourceRootMaps),
			headerLabels,
			*kernelVersion,
			*compileEnvironmentABI,
		)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to generate compact metadata: %v\n", err)
			return 1
		}
		if *compactMetadataOut != "" {
			data, err := metadata.JSON()
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to encode compact metadata: %v\n", err)
				return 1
			}
			if err := os.WriteFile(workspacePath(*compactMetadataOut), data, 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "failed to write compact metadata: %v\n", err)
				return 1
			}
		}
		if *compactBuildfileOut != "" {
			exports := []string(compactExports)
			if len(exports) == 0 {
				exports = compactBuildfileExports(*compactBuildfileOut, *compactMetadataOut)
			}
			objectPkg := *objectLabelPackage
			if objectPkg == "" {
				objectPkg = inferLabelPackage(*compactBuildfileOut)
			}
			data, err := metadata.BuildFile(kconfig.CompactBuildFileOptions{
				Arch:               vars["ARCH"],
				Version:            *kernelVersion,
				Visibility:         []string(visibility),
				RuleLoadLabel:      *linuxObjectsLoad,
				BaseConfig:         *compactBaseConfig,
				ObjectLabelPackage: objectPkg,
				Exports:            exports,
				SourceLabelPackage: *sourceLabelPackage,
				SourceASN1Compiler: *sourceASN1Compiler,
				SourceObjtool:      *sourceObjtool,
				SourceRelacheck:    *sourceRelacheck,
				SourceRootLabel:    *sourceRootLabel,
				Srcarch:            vars["SRCARCH"],
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to generate compact BUILD file: %v\n", err)
				return 1
			}
			if err := os.WriteFile(workspacePath(*compactBuildfileOut), data, 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "failed to write compact BUILD file: %v\n", err)
				return 1
			}
		}
	}

	// Only emit the JSON dump when explicitly requested, or when no output was
	// requested at all (preserves the prior default of dumping to stdout).
	if *out == "" && (*kbuildOut != "" || *kbuildTreeOut != "" || *compactMetadataOut != "" || *compactBuildfileOut != "" || *rustProfileOut != "" || resolvedConfigRequested) {
		return 0
	}

	data, err := json.MarshalIndent(tree.Dump(), "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to encode Kconfig dump: %v\n", err)
		return 1
	}
	data = append(data, '\n')

	if *out == "" {
		if _, err := os.Stdout.Write(data); err != nil {
			fmt.Fprintf(os.Stderr, "failed to write stdout: %v\n", err)
			return 1
		}
		return 0
	}
	if err := os.WriteFile(workspacePath(*out), data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "failed to write output: %v\n", err)
		return 1
	}
	return 0
}

type resolvedConfigOutputs struct {
	config        string
	autoConf      string
	autoConfCmd   string
	autoconf      string
	rustcCfg      string
	kernelRelease string
}

func writeResolvedConfig(tree *kconfig.Tree, input, configMode string, outputs resolvedConfigOutputs, kernelVersion string, validateEquivalent bool) error {
	if input == "" {
		return fmt.Errorf("-resolve_config is required when resolved config outputs are requested")
	}
	missing := map[string]string{
		"-resolved_config_out":         outputs.config,
		"-resolved_auto_conf_out":      outputs.autoConf,
		"-resolved_auto_conf_cmd_out":  outputs.autoConfCmd,
		"-resolved_autoconf_out":       outputs.autoconf,
		"-resolved_rustc_cfg_out":      outputs.rustcCfg,
		"-resolved_kernel_release_out": outputs.kernelRelease,
	}
	var missingFlags []string
	for flagName, path := range missing {
		if path == "" {
			missingFlags = append(missingFlags, flagName)
		}
	}
	sort.Strings(missingFlags)
	if len(missingFlags) != 0 {
		return fmt.Errorf("missing required output flags: %s", strings.Join(missingFlags, ", "))
	}

	name, path, ok := strings.Cut(input, "=")
	if !ok || name == "" || path == "" {
		return fmt.Errorf("-resolve_config expects NAME=PATH")
	}
	file, err := os.Open(workspacePath(path))
	if err != nil {
		return err
	}
	defer file.Close()
	raw, err := kconfig.ParseConfig(file)
	if err != nil {
		return err
	}
	resolveOpts, err := resolveConfigOptions(configMode)
	if err != nil {
		return err
	}
	resolved, err := tree.ResolveConfigWithOptions(name, raw, resolveOpts)
	if err != nil {
		return err
	}
	if validateEquivalent {
		if err := kconfig.ValidateRustToolchainEquivalence(raw, resolved); err != nil {
			return err
		}
	}
	return writeResolvedConfigOutputs(tree, resolved, outputs, kernelVersion)
}

func resolveConfigOptions(mode string) (kconfig.ResolveConfigOptions, error) {
	switch mode {
	case "", "default":
		return kconfig.ResolveConfigOptions{}, nil
	case "allnoconfig":
		return kconfig.ResolveConfigOptions{AllNoConfig: true}, nil
	default:
		return kconfig.ResolveConfigOptions{}, fmt.Errorf("unsupported -config_mode %q", mode)
	}
}

func rustcCfgLines(tree *kconfig.Tree, resolved *kconfig.ResolvedConfig) []string {
	keys := make([]string, 0, len(resolved.Effective))
	for key := range resolved.Effective {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var lines []string
	for _, key := range keys {
		if !resolved.ShouldWrite(key) {
			continue
		}
		symbol := tree.Symbols[strings.TrimPrefix(key, "CONFIG_")]
		if symbol == nil || symbol.Type == kconfig.SymbolUnknown {
			continue
		}
		value := resolved.Effective[key]
		switch symbol.Type {
		case kconfig.SymbolBool, kconfig.SymbolTristate:
			if value == "n" {
				continue
			}
			lines = append(lines, "--cfg="+key)
		}
		if symbol.Type == kconfig.SymbolHex &&
			!strings.HasPrefix(value, "0x") &&
			!strings.HasPrefix(value, "0X") {
			value = "0x" + value
		}
		if symbol.Type == kconfig.SymbolString {
			value = unquoteKconfigString(value)
		}
		lines = append(lines, "--cfg="+key+"="+strconv.Quote(value))
	}
	return lines
}

func unquoteKconfigString(value string) string {
	if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
		return value
	}

	value = value[1 : len(value)-1]
	var out strings.Builder
	out.Grow(len(value))
	for i := 0; i < len(value); i++ {
		if value[i] == '\\' && i+1 < len(value) &&
			(value[i+1] == '\\' || value[i+1] == '"') {
			i++
		}
		out.WriteByte(value[i])
	}
	return out.String()
}

func writeResolvedConfigOutputs(tree *kconfig.Tree, resolved *kconfig.ResolvedConfig, outputs resolvedConfigOutputs, kernelVersion string) error {
	keys := make([]string, 0, len(resolved.Effective))
	for key := range resolved.Effective {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	configLines := make([]string, 0, len(keys))
	headerLines := []string{
		"/* Generated by Bazel kconfig_parse. */",
		"#ifndef __GENERATED_AUTOCONF_H__",
		"#define __GENERATED_AUTOCONF_H__",
	}
	for _, key := range keys {
		if !resolved.ShouldWrite(key) {
			continue
		}
		value := resolved.Effective[key]
		if value == "" || value == "n" {
			continue
		}
		configLines = append(configLines, key+"="+value)
		if header := configValueToHeaderLine(key, value); header != "" {
			headerLines = append(headerLines, header)
		}
	}
	headerLines = append(headerLines, "#endif")

	localVersion := strings.Trim(resolved.Effective["CONFIG_LOCALVERSION"], `"`)
	files := map[string]string{
		outputs.config:        strings.Join(configLines, "\n") + "\n",
		outputs.autoConf:      strings.Join(configLines, "\n") + "\n",
		outputs.autoConfCmd:   "cmd_" + filepath.ToSlash(outputs.autoConf) + " := bazel kconfig_parse -resolve_config\n",
		outputs.autoconf:      strings.Join(headerLines, "\n") + "\n",
		outputs.rustcCfg:      strings.Join(rustcCfgLines(tree, resolved), "\n") + "\n",
		outputs.kernelRelease: kernelVersion + localVersion + "\n",
	}
	for path, content := range files {
		if err := os.WriteFile(workspacePath(path), []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func configValueToHeaderLine(key, value string) string {
	switch value {
	case "y":
		return "#define " + key + " 1"
	case "m":
		return "#define " + key + "_MODULE 1"
	case "", "n":
		return ""
	default:
		return "#define " + key + " " + value
	}
}

func parseKbuild(path string, recursive bool, kbuildSrctree string, srctree string, vars map[string]string) (*kconfig.KbuildFile, error) {
	path = workspacePath(path)
	if !recursive {
		return kconfig.ParseKbuildFile(path)
	}
	rootDir := kbuildSrctree
	if rootDir == "" {
		rootDir = srctree
	}
	return kconfig.ParseKbuildFileTree(path, kconfig.KbuildOptions{
		RootDir:   workspacePath(rootDir),
		Variables: vars,
	})
}

type kbuildTreeSummary struct {
	Count int      `json:"count"`
	Files []string `json:"files"`
}

func validateKbuildTree(root string, excludes []string, vars map[string]string) (*kbuildTreeSummary, error) {
	var files []string
	var failures []string
	normalizedExcludes := normalizeTreeExcludes(excludes)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			if rel != "." && treePathExcluded(rel, normalizedExcludes) {
				return filepath.SkipDir
			}
			return nil
		}
		if !isKbuildValidationFile(entry.Name()) || treePathExcluded(rel, normalizedExcludes) {
			return nil
		}

		fileVars := map[string]string{}
		for key, value := range vars {
			fileVars[key] = value
		}
		dir := filepath.ToSlash(filepath.Dir(rel))
		fileVars["src"] = dir
		fileVars["obj"] = dir
		if _, err := kconfig.ParseKbuildFileWithOptions(path, kconfig.KbuildOptions{Variables: fileVars}); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", rel, err))
			return nil
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	if len(failures) > 0 {
		sort.Strings(failures)
		return nil, fmt.Errorf("%d Kbuild files failed:\n%s", len(failures), strings.Join(failures, "\n"))
	}
	return &kbuildTreeSummary{
		Count: len(files),
		Files: files,
	}, nil
}

func normalizeTreeExcludes(excludes []string) []string {
	normalized := make([]string, 0, len(excludes))
	for _, exclude := range excludes {
		exclude = filepath.ToSlash(strings.Trim(exclude, "/"))
		if exclude != "" && exclude != "." {
			normalized = append(normalized, exclude)
		}
	}
	return normalized
}

func treePathExcluded(path string, excludes []string) bool {
	for _, exclude := range excludes {
		if path == exclude || strings.HasPrefix(path, exclude+"/") {
			return true
		}
	}
	return false
}

func isKbuildValidationFile(name string) bool {
	return name == "Kbuild" || name == "Makefile" || strings.HasSuffix(name, ".mk")
}

func compactMetadata(
	tree *kconfig.Tree,
	rootPath string,
	kbuildPath string,
	configInputs []namedPath,
	configMode string,
	compactKbuildTree bool,
	vars map[string]string,
	sourceRoots map[string]string,
	generatedHeadersByConfig map[string]string,
	kernelVersion string,
	compileEnvironmentABI string,
) (*kconfig.CompactMetadata, error) {
	if kbuildPath == "" {
		return nil, fmt.Errorf("-kbuild is required")
	}
	if len(configInputs) == 0 {
		return nil, fmt.Errorf("at least one -config NAME=PATH is required")
	}
	configNames := map[string]bool{}
	for _, input := range configInputs {
		if configNames[input.Name] {
			return nil, fmt.Errorf("duplicate compact config name %q", input.Name)
		}
		configNames[input.Name] = true
	}
	resolveOpts, err := resolveConfigOptions(configMode)
	if err != nil {
		return nil, err
	}
	configs := make([]kconfig.NamedConfig, 0, len(configInputs))
	for _, input := range configInputs {
		file, err := os.Open(workspacePath(input.Path))
		if err != nil {
			return nil, err
		}
		flags, parseErr := kconfig.ParseConfig(file)
		closeErr := file.Close()
		if parseErr != nil {
			return nil, fmt.Errorf("%s: %w", input.Path, parseErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("%s: %w", input.Path, closeErr)
		}
		configs = append(configs, kconfig.NamedConfig{Name: input.Name, Flags: flags, AllNoConfig: resolveOpts.AllNoConfig})
	}
	sourceRoot := ""
	objectDir := ""
	if rootPath != "" {
		sourceRoot = filepath.Dir(workspacePath(rootPath))
	}
	if sourceRoot != "" {
		if rel, err := filepath.Rel(sourceRoot, filepath.Dir(workspacePath(kbuildPath))); err == nil && rel != "." {
			rel = filepath.ToSlash(rel)
			if rel != ".." && !strings.HasPrefix(rel, "../") {
				objectDir = rel
			}
		}
	}
	opts := kconfig.CompactMetadataOptions{
		ObjectDir:             objectDir,
		SourceRoot:            sourceRoot,
		SourceRoots:           sourceRoots,
		LibraryDirs:           kbuildLibraryDirs(vars),
		CompileEnvironmentABI: compileEnvironmentABI,
		KernelVersion:         kernelVersion,
		Srcarch:               vars["SRCARCH"],
	}
	rootDir := sourceRoot
	if rootDir == "" {
		rootDir = filepath.Dir(workspacePath(kbuildPath))
	}
	return tree.CompactMetadataBatchWithOptions(configs, opts, func(resolved *kconfig.ResolvedConfig) (kconfig.CompactConfigGraph, error) {
		kbuildOpts := kconfig.KbuildOptions{
			Variables: kbuildVariablesForConfig(vars, resolved),
		}
		var kb *kconfig.KbuildFile
		var parseErr error
		if compactKbuildTree {
			kbuildOpts.RootDir = rootDir
			kbuildOpts.RootMakefiles = linuxRootMakefiles(rootDir, vars)
			kb, parseErr = kconfig.ParseKbuildDirectoryTree(workspacePath(kbuildPath), kbuildOpts)
		} else {
			kb, parseErr = kconfig.ParseKbuildFileWithOptions(workspacePath(kbuildPath), kbuildOpts)
		}
		if parseErr != nil {
			return kconfig.CompactConfigGraph{}, parseErr
		}
		return kconfig.CompactConfigGraph{
			Kbuild:                kb,
			GeneratedHeadersLabel: generatedHeadersByConfig[resolved.Name],
		}, nil
	})
}

func kbuildLibraryDirs(vars map[string]string) []string {
	var out []string
	seen := map[string]bool{}
	for _, value := range strings.Fields(vars["ARCH_LIB"]) {
		if !strings.HasSuffix(value, "/") {
			continue
		}
		dir := strings.Trim(filepath.ToSlash(value), "/")
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		out = append(out, dir)
	}
	return out
}

func linuxRootMakefiles(rootDir string, vars map[string]string) []string {
	srcarch := vars["SRCARCH"]
	if srcarch == "" {
		srcarch = vars["ARCH"]
	}
	if rootDir == "" || srcarch == "" {
		return nil
	}
	rel := filepath.Join("arch", filepath.FromSlash(srcarch), "Makefile")
	path := filepath.Join(rootDir, rel)
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		return []string{rel}
	}
	return nil
}

func compactBuildfileExports(buildfileOut, metadataOut string) []string {
	if buildfileOut == "" || metadataOut == "" {
		return nil
	}
	if filepath.Dir(buildfileOut) != filepath.Dir(metadataOut) {
		return nil
	}
	return []string{filepath.Base(metadataOut)}
}

func kbuildVariablesForConfig(base map[string]string, config *kconfig.ResolvedConfig) map[string]string {
	// scripts/Kbuild.include defines this before the architecture Makefile is
	// evaluated by Kbuild. Compact graph generation starts at that Makefile.
	vars := map[string]string{"comma": ","}
	for key, value := range base {
		vars[key] = value
	}
	for key, value := range config.Effective {
		if !config.ShouldWrite(key) || value == "n" {
			vars[key] = ""
			continue
		}
		vars[key] = value
	}
	return vars
}

func workspacePath(path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	workspace := os.Getenv("BUILD_WORKSPACE_DIRECTORY")
	if workspace == "" {
		return path
	}
	return filepath.Join(workspace, path)
}

func inferLabelPackage(path string) string {
	if path == "" || filepath.IsAbs(path) {
		return ""
	}
	dir := filepath.Dir(path)
	if dir == "." {
		return ""
	}
	return filepath.ToSlash(dir)
}
