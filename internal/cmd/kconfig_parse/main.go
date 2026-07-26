// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
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

func namedValueMap(values []namedPath) map[string]string {
	out := map[string]string{}
	for _, value := range values {
		out[value.Name] = value.Path
	}
	return out
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

func main() {
	var (
		root                     = flag.String("root", "", "Root Kconfig file to parse")
		srctree                  = flag.String("srctree", "", "Source tree used to resolve source statements")
		allowShell               = flag.Bool("allow_shell", false, "Allow $(shell,...) expansion")
		linuxProbeModel          = flag.String("linux_probe_model", "", "Hermetic Linux Kconfig probe model to use for $(shell,...) expansion. Supported: linux_llvm")
		out                      = flag.String("out", "", "Path to write the parsed Kconfig as JSON. Defaults to stdout when no other output is set")
		kbuildPath               = flag.String("kbuild", "", "Kbuild/Makefile path for compact object metadata generation")
		kbuildRecursive          = flag.Bool("kbuild_recursive", false, "Follow static Kbuild include directives when writing -kbuild_out")
		kbuildSrctree            = flag.String("kbuild_srctree", "", "Source tree used to resolve recursive Kbuild includes. Defaults to -srctree")
		kbuildOut                = flag.String("kbuild_out", "", "Path to write the parsed Kbuild/Makefile as JSON")
		kbuildTreeRoot           = flag.String("kbuild_tree_root", "", "Linux source tree root to recursively validate Kbuild/Makefile/*.mk files")
		kbuildTreeOut            = flag.String("kbuild_tree_out", "", "Path to write recursive Kbuild tree validation summary JSON")
		kbuildTreeMinCount       = flag.Int("kbuild_tree_min_count", 0, "Minimum number of Kbuild-like files that must be parsed during -kbuild_tree_root validation")
		resolvedFlagsDir         = flag.String("resolved_flags_dir", "", "Directory to write each compact config's overlay-resolved effective flags as <NAME>.flags")
		compactMetadataOut       = flag.String("compact_metadata_out", "", "Path to write compact fragment-keyed Linux metadata JSON")
		compactBuildfileOut      = flag.String("compact_buildfile_out", "", "Path to write a combined compact object/image BUILD file")
		compactSchemaValue       = flag.String("compact_schema", string(kconfig.CompactSchemaV011), "Compact output schema. Supported: v0.0.11, v0.0.12")
		rustProfileOut           = flag.String("rust_profile_out", "", "Path to write the v0.0.12 source-derived Rust profile JSON")
		compactKbuildTree        = flag.Bool("compact_kbuild_tree", false, "Follow active Kbuild directory descent when generating compact metadata")
		objectBuildfileOut       = flag.String("object_buildfile_out", "", "Path to write compact shared object-variant BUILD file")
		imageBuildfileOut        = flag.String("image_buildfile_out", "", "Path to write compact per-config image BUILD file")
		resolveConfig            = flag.String("resolve_config", "", "Named .config input in NAME=PATH form to resolve through Kconfig defaults and dependencies")
		resolveOverlay           = flag.String("resolve_overlay", "", "Optional .config overlay fragment merged onto -resolve_config before resolution")
		configMode               = flag.String("config_mode", "default", "Config resolver mode. Supported: default, allnoconfig")
		resolvedConfigOut        = flag.String("resolved_config_out", "", "Path to write the resolved .config")
		resolvedAutoConfOut      = flag.String("resolved_auto_conf_out", "", "Path to write the resolved include/config/auto.conf")
		resolvedCmdOut           = flag.String("resolved_auto_conf_cmd_out", "", "Path to write the resolved include/config/auto.conf.cmd")
		resolvedAutoconfOut      = flag.String("resolved_autoconf_out", "", "Path to write the resolved include/generated/autoconf.h")
		resolvedRustcCfgOut      = flag.String("resolved_rustc_cfg_out", "", "Path to write the resolved include/generated/rustc_cfg")
		resolvedReleaseOut       = flag.String("resolved_kernel_release_out", "", "Path to write the resolved include/config/kernel.release")
		resolvedFlagsOut         = flag.String("resolved_flags_out", "", "Path to write resolved effective CONFIG_* flags")
		kernelVersion            = flag.String("kernel_version", "6.18.2", "Base kernel release used when writing resolved config outputs")
		generatedHeaders         = flag.String("generated_headers", "", "Bazel label for generated Linux headers emitted into source-backed compact object rules")
		objectLabelPackage       = flag.String("object_label_package", "", "Bazel package containing the compact object BUILD file. Defaults to -object_buildfile_out package")
		sourceLabelPackage       = flag.String("source_label_package", "", "Bazel package containing Linux source file labels for generated compact object BUILD files")
		sourceASN1Compiler       = flag.String("source_asn1_compiler", "", "Bazel label for the kernel source tree's scripts/asn1_compiler tool emitted into source-backed compact object rules")
		sourceRelacheck          = flag.String("source_relacheck", "", "Bazel label for the kernel source tree's arch/arm64/kernel/pi/relacheck tool emitted into arm64 .pi.o rules")
		sourceConfig             = flag.String("source_config", "", "Bazel label for a full LinuxConfigInfo target emitted into source-backed compact object rules")
		sourceRootLabel          = flag.String("source_root_label", "", "Bazel label for a file in the Linux source root, emitted into source-backed compact object rules")
		linuxObjectsLoad         = flag.String("linux_objects_load", "", "Load label for the compact Linux object/image rules")
		vars                     = stringMapFlag{}
		env                      = stringMapFlag{}
		linuxProbeValues         = stringMapFlag{}
		visibility               = stringSliceFlag{}
		kbuildTreeExcludes       = stringSliceFlag{}
		compactConfigInputs      = namedPathFlag{}
		compactConfigOverlays    = namedPathFlag{}
		compactExports           = stringSliceFlag{}
		sourceRootMaps           = namedPathFlag{}
		kconfigExtras            = namedPathFlag{}
		kbuildExtras             = namedPathFlag{}
		sourceLabelMaps          = namedPathFlag{}
		sourceTreeAllFiles       = stringSliceFlag{}
		sourceTreeArchHeaders    = stringSliceFlag{}
		sourceTreeDtbSources     = stringSliceFlag{}
		sourceTreeGlobalHeaders  = stringSliceFlag{}
		sourceTreeHeaders        = stringSliceFlag{}
		sourceTreeKbuildFiles    = stringSliceFlag{}
		sourceTreeLocalIncludes  = stringSliceFlag{}
		sourceTreeLookupFiles    = stringSliceFlag{}
		sourceTreeScriptsHeaders = stringSliceFlag{}
		sourceTreeUapiHeaders    = stringSliceFlag{}
	)
	flag.Var(vars, "var", "Preprocessor variable in KEY=VALUE form. May be repeated")
	flag.Var(env, "env", "Hermetic environment variable in KEY=VALUE form. May be repeated")
	flag.Var(linuxProbeValues, "linux_probe_value", "Linux probe override in KEY=VALUE form. May be repeated with -linux_probe_model")
	flag.Var(&visibility, "visibility", "Default visibility for the generated BUILD file. May be repeated. Defaults to //visibility:public")
	flag.Var(&kbuildTreeExcludes, "kbuild_tree_exclude", "Source-root-relative subtree to skip during -kbuild_tree_root validation. May be repeated")
	flag.Var(&compactConfigInputs, "config", "Named .config input in NAME=PATH form for compact metadata generation. May be repeated")
	flag.Var(&compactConfigOverlays, "config_overlay", "Optional .config overlay in NAME=PATH form merged onto the matching -config. May be repeated")
	flag.Var(&compactExports, "compact_buildfile_export", "Source filename exported by the generated compact BUILD file. May be repeated")
	flag.Var(&sourceRootMaps, "source_root_map", "Virtual source prefix to filesystem root in PREFIX=PATH form. May be repeated")
	flag.Var(&kconfigExtras, "kconfig_extra", "Extra Kconfig source in PREFIX=PATH form. May be repeated")
	flag.Var(&kbuildExtras, "kbuild_extra", "Extra Kbuild/Makefile source in PREFIX=PATH form. May be repeated")
	flag.Var(&sourceLabelMaps, "source_label_map", "Virtual source prefix to Bazel label package in PREFIX=LABEL_PACKAGE form. May be repeated")
	flag.Var(&sourceTreeAllFiles, "source_tree_all_files_label", "Bazel label for explicit full source tree files emitted into source-backed compact object rules. May be repeated")
	flag.Var(&sourceTreeArchHeaders, "source_tree_arch_headers_label", "Bazel label for architecture source headers emitted into source-backed compact object rules. May be repeated")
	flag.Var(&sourceTreeDtbSources, "source_tree_dtb_sources_label", "Bazel label for devicetree source inputs emitted into source-backed compact object rules. May be repeated")
	flag.Var(&sourceTreeGlobalHeaders, "source_tree_global_headers_label", "Bazel label for global source headers emitted into source-backed compact object rules. May be repeated")
	flag.Var(&sourceTreeHeaders, "source_tree_headers_label", "Bazel label for source headers emitted into source-backed compact object rules. May be repeated")
	flag.Var(&sourceTreeKbuildFiles, "source_tree_kbuild_files_label", "Bazel label for Kbuild/Makefile source inputs emitted into source-backed compact object rules. May be repeated")
	flag.Var(&sourceTreeLocalIncludes, "source_tree_local_include_files_label", "Bazel label for source-like files that may be included from the same directory. May be repeated")
	flag.Var(&sourceTreeLookupFiles, "source_tree_lookup_files_label", "Bazel label for bounded special source inputs looked up by native Linux actions. May be repeated")
	flag.Var(&sourceTreeScriptsHeaders, "source_tree_scripts_headers_label", "Bazel label for scripts headers emitted into source-backed compact object rules. May be repeated")
	flag.Var(&sourceTreeUapiHeaders, "source_tree_uapi_headers_label", "Bazel label for UAPI headers emitted into source-backed compact object rules. May be repeated")
	flag.Parse()

	compactSchema, err := kconfig.ParseCompactSchema(*compactSchemaValue)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	if *root == "" && *kbuildOut == "" && *kbuildTreeOut == "" && *rustProfileOut == "" {
		flag.PrintDefaults()
		os.Exit(2)
	}

	var tree *kconfig.Tree
	if *root != "" {
		var err error
		shell, err := linuxProbeShell(*linuxProbeModel, linuxProbeValues)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to configure Linux probe model: %v\n", err)
			os.Exit(2)
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
				os.Exit(1)
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
			os.Exit(1)
		}
	}

	if *kbuildOut != "" {
		if *kbuildPath == "" {
			fmt.Fprintf(os.Stderr, "-kbuild is required when -kbuild_out is set\n")
			os.Exit(2)
		}
		kb, err := parseKbuild(*kbuildPath, *kbuildRecursive, *kbuildSrctree, *srctree, vars)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to parse Kbuild: %v\n", err)
			os.Exit(1)
		}
		data, err := json.MarshalIndent(kb, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to encode Kbuild dump: %v\n", err)
			os.Exit(1)
		}
		data = append(data, '\n')
		if err := os.WriteFile(workspacePath(*kbuildOut), data, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "failed to write Kbuild dump: %v\n", err)
			os.Exit(1)
		}
	}

	if *kbuildTreeOut != "" {
		if *kbuildTreeRoot == "" {
			fmt.Fprintf(os.Stderr, "-kbuild_tree_root is required when -kbuild_tree_out is set\n")
			os.Exit(2)
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
			os.Exit(1)
		}
		if summary.Count < *kbuildTreeMinCount {
			fmt.Fprintf(os.Stderr, "failed to validate Kbuild tree: parsed %d files, want at least %d\n", summary.Count, *kbuildTreeMinCount)
			os.Exit(1)
		}
		data, err := json.MarshalIndent(summary, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to encode Kbuild tree summary: %v\n", err)
			os.Exit(1)
		}
		data = append(data, '\n')
		if err := os.WriteFile(workspacePath(*kbuildTreeOut), data, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "failed to write Kbuild tree summary: %v\n", err)
			os.Exit(1)
		}
	}

	if *rustProfileOut != "" {
		if compactSchema != kconfig.CompactSchemaV012 {
			fmt.Fprintln(os.Stderr, "-rust_profile_out requires -compact_schema=v0.0.12")
			os.Exit(2)
		}
		if *srctree == "" {
			fmt.Fprintln(os.Stderr, "-srctree is required for -rust_profile_out")
			os.Exit(2)
		}
		profile, err := kconfig.GenerateRustProfile(workspacePath(*srctree), vars["ARCH"])
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to generate Rust profile: %v\n", err)
			os.Exit(1)
		}
		data, err := profile.JSON()
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to encode Rust profile: %v\n", err)
			os.Exit(1)
		}
		if err := os.WriteFile(workspacePath(*rustProfileOut), data, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "failed to write Rust profile: %v\n", err)
			os.Exit(1)
		}
	}

	resolvedConfigRequested := *resolveConfig != "" || *resolvedConfigOut != "" || *resolvedAutoConfOut != "" || *resolvedCmdOut != "" || *resolvedAutoconfOut != "" || *resolvedRustcCfgOut != "" || *resolvedReleaseOut != ""
	if tree == nil && (*compactMetadataOut != "" || *objectBuildfileOut != "" || *imageBuildfileOut != "" || resolvedConfigRequested || *resolvedFlagsOut != "" || *out != "") {
		fmt.Fprintf(os.Stderr, "-root is required for Kconfig outputs\n")
		os.Exit(2)
	}

	if resolvedConfigRequested {
		if err := writeResolvedConfig(tree, *resolveConfig, *resolveOverlay, *configMode, resolvedConfigOutputs{
			config:        *resolvedConfigOut,
			autoConf:      *resolvedAutoConfOut,
			autoConfCmd:   *resolvedCmdOut,
			autoconf:      *resolvedAutoconfOut,
			rustcCfg:      *resolvedRustcCfgOut,
			kernelRelease: *resolvedReleaseOut,
		}, *kernelVersion); err != nil {
			fmt.Fprintf(os.Stderr, "failed to write resolved config: %v\n", err)
			os.Exit(1)
		}
	}

	if *resolvedFlagsOut != "" {
		if err := writeResolvedFlags(tree, *resolveConfig, *resolveOverlay, *configMode, *resolvedFlagsOut); err != nil {
			fmt.Fprintf(os.Stderr, "failed to write resolved flags: %v\n", err)
			os.Exit(1)
		}
	}

	if *compactMetadataOut != "" || *compactBuildfileOut != "" || *objectBuildfileOut != "" || *imageBuildfileOut != "" {
		metadata, err := compactMetadata(tree, *root, *kbuildPath, compactConfigInputs, compactConfigOverlays, *resolvedFlagsDir, *configMode, *compactKbuildTree, vars, kbuildExtras, namedPathMap(sourceRootMaps), compactSchema)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to generate compact metadata: %v\n", err)
			os.Exit(1)
		}
		if *compactMetadataOut != "" {
			data, err := metadata.JSON()
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to encode compact metadata: %v\n", err)
				os.Exit(1)
			}
			if err := os.WriteFile(workspacePath(*compactMetadataOut), data, 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "failed to write compact metadata: %v\n", err)
				os.Exit(1)
			}
		}
		if *objectBuildfileOut != "" {
			data, err := metadata.ObjectBuildFile(kconfig.CompactBuildFileOptions{
				Schema:                   compactSchema,
				Arch:                     vars["ARCH"],
				Visibility:               []string(visibility),
				RuleLoadLabel:            *linuxObjectsLoad,
				SourceLabelPackage:       *sourceLabelPackage,
				SourceLabelPackages:      namedValueMap(sourceLabelMaps),
				SourceASN1Compiler:       *sourceASN1Compiler,
				SourceRelacheck:          *sourceRelacheck,
				SourceConfig:             *sourceConfig,
				SourceRootLabel:          *sourceRootLabel,
				SourceTreeAllFiles:       []string(sourceTreeAllFiles),
				SourceTreeArchHeaders:    []string(sourceTreeArchHeaders),
				SourceTreeDtbSources:     []string(sourceTreeDtbSources),
				SourceTreeGlobalHeaders:  []string(sourceTreeGlobalHeaders),
				SourceTreeHeaders:        []string(sourceTreeHeaders),
				SourceTreeKbuildFiles:    []string(sourceTreeKbuildFiles),
				SourceTreeLocalIncludes:  []string(sourceTreeLocalIncludes),
				SourceTreeLookupFiles:    []string(sourceTreeLookupFiles),
				SourceTreeScriptsHeaders: []string(sourceTreeScriptsHeaders),
				SourceTreeUapiHeaders:    []string(sourceTreeUapiHeaders),
				GeneratedHeaders:         *generatedHeaders,
				Srcarch:                  vars["SRCARCH"],
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to generate compact object BUILD file: %v\n", err)
				os.Exit(1)
			}
			if err := os.WriteFile(workspacePath(*objectBuildfileOut), data, 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "failed to write compact object BUILD file: %v\n", err)
				os.Exit(1)
			}
		}
		if *imageBuildfileOut != "" {
			objectPkg := *objectLabelPackage
			if objectPkg == "" {
				objectPkg = inferLabelPackage(*objectBuildfileOut)
			}
			data, err := metadata.ImageBuildFile(kconfig.CompactImageBuildFileOptions{
				Schema:             compactSchema,
				Arch:               vars["ARCH"],
				Visibility:         []string(visibility),
				ObjectLabelPackage: objectPkg,
				RequireReal:        *sourceConfig != "",
				RuleLoadLabel:      *linuxObjectsLoad,
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to generate compact image BUILD file: %v\n", err)
				os.Exit(1)
			}
			if err := os.WriteFile(workspacePath(*imageBuildfileOut), data, 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "failed to write compact image BUILD file: %v\n", err)
				os.Exit(1)
			}
		}
		if *compactBuildfileOut != "" {
			objectBuild, err := metadata.ObjectBuildFile(kconfig.CompactBuildFileOptions{
				Schema:                   compactSchema,
				Arch:                     vars["ARCH"],
				Visibility:               []string(visibility),
				RuleLoadLabel:            *linuxObjectsLoad,
				SourceLabelPackage:       *sourceLabelPackage,
				SourceLabelPackages:      namedValueMap(sourceLabelMaps),
				SourceASN1Compiler:       *sourceASN1Compiler,
				SourceRelacheck:          *sourceRelacheck,
				SourceConfig:             *sourceConfig,
				SourceRootLabel:          *sourceRootLabel,
				SourceTreeAllFiles:       []string(sourceTreeAllFiles),
				SourceTreeArchHeaders:    []string(sourceTreeArchHeaders),
				SourceTreeDtbSources:     []string(sourceTreeDtbSources),
				SourceTreeGlobalHeaders:  []string(sourceTreeGlobalHeaders),
				SourceTreeHeaders:        []string(sourceTreeHeaders),
				SourceTreeKbuildFiles:    []string(sourceTreeKbuildFiles),
				SourceTreeLocalIncludes:  []string(sourceTreeLocalIncludes),
				SourceTreeLookupFiles:    []string(sourceTreeLookupFiles),
				SourceTreeScriptsHeaders: []string(sourceTreeScriptsHeaders),
				SourceTreeUapiHeaders:    []string(sourceTreeUapiHeaders),
				GeneratedHeaders:         *generatedHeaders,
				Srcarch:                  vars["SRCARCH"],
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to generate compact object BUILD file: %v\n", err)
				os.Exit(1)
			}
			objectPkg := *objectLabelPackage
			if objectPkg == "" {
				objectPkg = inferLabelPackage(*compactBuildfileOut)
			}
			imageBuild, err := metadata.ImageBuildFile(kconfig.CompactImageBuildFileOptions{
				Schema:             compactSchema,
				Arch:               vars["ARCH"],
				Visibility:         []string(visibility),
				ObjectLabelPackage: objectPkg,
				RequireReal:        *sourceConfig != "",
				RuleLoadLabel:      *linuxObjectsLoad,
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to generate compact image BUILD file: %v\n", err)
				os.Exit(1)
			}
			exports := []string(compactExports)
			if len(exports) == 0 {
				exports = compactBuildfileExports(*compactBuildfileOut, *compactMetadataOut)
			}
			data, err := kconfig.MergeBuildFiles("compact.BUILD.bazel", exports, objectBuild, imageBuild)
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to merge compact BUILD file: %v\n", err)
				os.Exit(1)
			}
			if err := os.WriteFile(workspacePath(*compactBuildfileOut), data, 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "failed to write compact BUILD file: %v\n", err)
				os.Exit(1)
			}
		}
	}

	// Only emit the JSON dump when explicitly requested, or when no output was
	// requested at all (preserves the prior default of dumping to stdout).
	if *out == "" && (*kbuildOut != "" || *kbuildTreeOut != "" || *compactMetadataOut != "" || *compactBuildfileOut != "" || *objectBuildfileOut != "" || *imageBuildfileOut != "" || *rustProfileOut != "" || resolvedConfigRequested || *resolvedFlagsOut != "") {
		return
	}

	data, err := json.MarshalIndent(tree.Dump(), "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to encode Kconfig dump: %v\n", err)
		os.Exit(1)
	}
	data = append(data, '\n')

	if *out == "" {
		if _, err := os.Stdout.Write(data); err != nil {
			fmt.Fprintf(os.Stderr, "failed to write stdout: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if err := os.WriteFile(workspacePath(*out), data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "failed to write output: %v\n", err)
		os.Exit(1)
	}
}

func linuxProbeShell(model string, values map[string]string) (func(context.Context, string) (string, error), error) {
	if model == "" {
		if len(values) > 0 {
			return nil, fmt.Errorf("-linux_probe_value requires -linux_probe_model")
		}
		return nil, nil
	}
	config, err := kconfig.LinuxProbeConfigForModel(model)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := kconfig.ApplyLinuxProbeValue(&config, key, values[key]); err != nil {
			return nil, err
		}
	}
	return kconfig.LinuxProbeShellFromConfig(config), nil
}

type resolvedConfigOutputs struct {
	config        string
	autoConf      string
	autoConfCmd   string
	autoconf      string
	rustcCfg      string
	kernelRelease string
}

func writeResolvedConfig(tree *kconfig.Tree, input, overlay, configMode string, outputs resolvedConfigOutputs, kernelVersion string) error {
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
	if err := applyConfigOverlay(raw, overlay); err != nil {
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
	return writeResolvedConfigOutputs(tree, resolved, outputs, kernelVersion)
}

func writeResolvedFlags(tree *kconfig.Tree, input, overlay, configMode, out string) error {
	if input == "" {
		return fmt.Errorf("-resolve_config is required for -resolved_flags_out")
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
	if err := applyConfigOverlay(raw, overlay); err != nil {
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
	return os.WriteFile(workspacePath(out), []byte(strings.Join(resolvedFlagLines(resolved), "\n")+"\n"), 0o644)
}

func resolvedFlagLines(resolved *kconfig.ResolvedConfig) []string {
	keys := make([]string, 0, len(resolved.Effective))
	for key := range resolved.Effective {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		if !resolved.ShouldWrite(key) {
			continue
		}
		value := resolved.Effective[key]
		if value == "" || value == "n" {
			continue
		}
		lines = append(lines, key+"="+value)
	}
	return lines
}

func applyConfigOverlay(raw map[string]string, overlayPath string) error {
	if overlayPath == "" {
		return nil
	}
	file, err := os.Open(workspacePath(overlayPath))
	if err != nil {
		return err
	}
	defer file.Close()
	overlay, err := kconfig.ParseConfigOverlay(file)
	if err != nil {
		return err
	}
	kconfig.MergeConfigOverlay(raw, overlay)
	return nil
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

func compactMetadata(tree *kconfig.Tree, rootPath string, kbuildPath string, configInputs, configOverlays []namedPath, resolvedFlagsDir string, configMode string, compactKbuildTree bool, vars map[string]string, kbuildExtras []namedPath, sourceRoots map[string]string, schema kconfig.CompactSchema) (*kconfig.CompactMetadata, error) {
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
	overlayPaths := map[string]string{}
	for _, overlay := range configOverlays {
		if !configNames[overlay.Name] {
			return nil, fmt.Errorf("config overlay %q has no matching -config", overlay.Name)
		}
		if _, ok := overlayPaths[overlay.Name]; ok {
			return nil, fmt.Errorf("duplicate config overlay name %q", overlay.Name)
		}
		overlayPaths[overlay.Name] = overlay.Path
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
		if err := applyConfigOverlay(flags, overlayPaths[input.Name]); err != nil {
			return nil, fmt.Errorf("%s overlay: %w", input.Name, err)
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
		Schema:      schema,
		ObjectDir:   objectDir,
		SourceRoot:  sourceRoot,
		SourceRoots: sourceRoots,
		LibraryDirs: kbuildLibraryDirs(vars),
		Srcarch:     vars["SRCARCH"],
	}
	rootDir := sourceRoot
	if rootDir == "" {
		rootDir = filepath.Dir(workspacePath(kbuildPath))
	}
	if resolvedFlagsDir != "" {
		if err := os.MkdirAll(workspacePath(resolvedFlagsDir), 0o755); err != nil {
			return nil, fmt.Errorf("create resolved flags dir: %w", err)
		}
	}
	parts := make([]*kconfig.CompactMetadata, 0, len(configs))
	for _, config := range configs {
		resolved, err := tree.ResolveConfigWithOptions(config.Name, config.Flags, kconfig.ResolveConfigOptions{
			AllNoConfig: config.AllNoConfig,
		})
		if err != nil {
			return nil, err
		}
		if resolvedFlagsDir != "" {
			flagsPath := filepath.Join(workspacePath(resolvedFlagsDir), config.Name+".flags")
			if err := os.WriteFile(flagsPath, []byte(strings.Join(resolvedFlagLines(resolved), "\n")+"\n"), 0o644); err != nil {
				return nil, fmt.Errorf("write resolved flags for %s: %w", config.Name, err)
			}
		}
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
			return nil, parseErr
		}
		for _, extra := range kbuildExtras {
			extraRoot := sourceRoots[extra.Name]
			if extraRoot == "" {
				extraRoot = filepath.Dir(workspacePath(extra.Path))
			}
			extraKb, err := kconfig.ParseKbuildDirectoryTree(workspacePath(extra.Path), kconfig.KbuildOptions{
				RootDir:         extraRoot,
				Variables:       kbuildVariablesForConfig(vars, resolved),
				MaxIncludeDepth: 64,
			})
			if err != nil {
				return nil, err
			}
			kb = kconfig.MergeKbuildFileAtDirectory(kb, extra.Name, kconfig.PrefixKbuildFile(extraKb, extra.Name))
		}
		part, err := tree.CompactMetadataWithOptions(kb, []kconfig.NamedConfig{config}, opts)
		if err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}
	return kconfig.MergeCompactMetadata(parts...)
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
	vars := map[string]string{}
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
