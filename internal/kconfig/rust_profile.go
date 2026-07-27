package kconfig

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const RustProfileSchema = "linux-rust-profile-v1"

type RustProfile struct {
	Schema            string                  `json:"schema"`
	Architecture      string                  `json:"architecture"`
	SourceLayout      string                  `json:"source_layout"`
	Target            RustTargetRecipe        `json:"target"`
	CommonFlags       []string                `json:"common_flags"`
	TargetFlags       RustFlagSet             `json:"target_flags"`
	Module            RustModuleProfile       `json:"module"`
	Bindgen           RustBindgenProfile      `json:"bindgen"`
	ProcMacros        []RustProcMacroProfile  `json:"proc_macros"`
	Crates            []RustCrateProfile      `json:"crates"`
	GeneratedAssembly []RustGeneratedAssembly `json:"generated_assembly,omitempty"`
	Exports           RustExportsProfile      `json:"exports"`
	RuntimeObjects    []RustRuntimeObject     `json:"runtime_objects"`
}

type RustTargetRecipe struct {
	GeneratorSource string `json:"generator_source"`
	Stdin           string `json:"stdin"`
	Output          string `json:"output"`
}

type RustFlagSet struct {
	Always      []string               `json:"always"`
	Conditional []RustConditionalFlags `json:"conditional"`
}

type RustConditionalFlags struct {
	Config    string   `json:"config"`
	Equals    string   `json:"equals,omitempty"`
	Flags     []string `json:"flags"`
	ElseFlags []string `json:"else_flags,omitempty"`
}

type RustModuleProfile struct {
	AllowedFeatures []string `json:"allowed_features"`
	Flags           []string `json:"flags"`
}

type RustBindgenProfile struct {
	Parameters     string   `json:"parameters"`
	BindingsHeader string   `json:"bindings_header"`
	UAPIHeader     string   `json:"uapi_header"`
	HelpersSource  string   `json:"helpers_source"`
	CommonFlags    []string `json:"common_flags"`
}

type RustProcMacroProfile struct {
	Name           string   `json:"name"`
	Source         string   `json:"source"`
	SourcePrefixes []string `json:"source_prefixes"`
	SourceFiles    []string `json:"source_files"`
	Flags          []string `json:"flags"`
	UsesRustcCfg   bool     `json:"uses_rustc_cfg"`
}

type RustCrateProfile struct {
	Name            string   `json:"name"`
	Source          string   `json:"source"`
	SourcePrefixes  []string `json:"source_prefixes"`
	SourceFiles     []string `json:"source_files"`
	GeneratedInputs []string `json:"generated_inputs"`
	Deps            []string `json:"deps"`
	Externs         []string `json:"externs"`
	Flags           []string `json:"flags"`
	SkipFlags       []string `json:"skip_flags"`
	ObjcopyFlags    []string `json:"objcopy_flags"`
}

type RustGeneratedAssembly struct {
	Config string `json:"config"`
	Equals string `json:"equals,omitempty"`
	Source string `json:"source"`
	Output string `json:"output"`
}

type RustExportsProfile struct {
	Source string   `json:"source"`
	Crates []string `json:"crates"`
}

type RustRuntimeObject struct {
	Path   string `json:"path"`
	Config string `json:"config,omitempty"`
	Equals string `json:"equals,omitempty"`
}

func (p *RustProfile) JSON() ([]byte, error) {
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func GenerateRustProfile(sourceRoot, arch string) (*RustProfile, error) {
	sourceRoot = filepath.Clean(sourceRoot)
	if arch != "x86" && arch != "x86_64" {
		return nil, fmt.Errorf("Rust profile generation supports only x86_64, got ARCH=%q", arch)
	}

	rootMake, err := readRustProfileFile(sourceRoot, "Makefile")
	if err != nil {
		return nil, err
	}
	archMake, err := readRustProfileFile(sourceRoot, "arch/x86/Makefile")
	if err != nil {
		return nil, err
	}
	buildMake, err := readRustProfileFile(sourceRoot, "scripts/Makefile.build")
	if err != nil {
		return nil, err
	}
	rustMake, err := readRustProfileFile(sourceRoot, "rust/Makefile")
	if err != nil {
		return nil, err
	}

	required := []string{
		"scripts/generate_rust_target.rs",
		"rust/bindgen_parameters",
		"rust/bindings/bindings_helper.h",
		"rust/uapi/uapi_helper.h",
		"rust/helpers/helpers.c",
		"rust/exports.c",
		"rust/macros/lib.rs",
		"rust/compiler_builtins.rs",
		"rust/ffi.rs",
		"rust/build_error.rs",
		"rust/bindings/lib.rs",
		"rust/uapi/lib.rs",
		"rust/kernel/lib.rs",
	}
	for _, path := range required {
		if err := requireRustProfileFile(sourceRoot, path); err != nil {
			return nil, err
		}
	}

	commonFlags, err := makeWords(rootMake, "rust_common_flags")
	if err != nil {
		return nil, err
	}
	if err := rejectMakeWords("rust_common_flags", commonFlags); err != nil {
		return nil, err
	}
	baseFlags, err := makeWords(rootMake, "KBUILD_RUSTFLAGS")
	if err != nil {
		return nil, err
	}
	baseFlags = removeMakeWords(baseFlags, "$(rust_common_flags)")
	if err := rejectMakeWords("KBUILD_RUSTFLAGS", baseFlags); err != nil {
		return nil, err
	}
	allowedFeatures, err := makeWords(buildMake, "rust_allowed_features")
	if err != nil {
		return nil, err
	}
	if len(allowedFeatures) != 1 {
		return nil, fmt.Errorf("rust_allowed_features must contain one comma-separated value, got %q", allowedFeatures)
	}
	allowedFeatures = strings.Split(allowedFeatures[0], ",")
	for _, feature := range allowedFeatures {
		if feature == "" {
			return nil, fmt.Errorf("rust_allowed_features contains an empty feature")
		}
	}

	layout := "legacy"
	hasPinInit := rustProfileFileExists(sourceRoot, "rust/pin-init/src/lib.rs")
	hasPinInitInternal := rustProfileFileExists(sourceRoot, "rust/pin-init/internal/src/lib.rs")
	if hasPinInit != hasPinInitInternal {
		return nil, fmt.Errorf("unrecognized mixed Rust pin-init layout")
	}
	if hasPinInit {
		layout = "pin-init"
		for _, path := range []string{
			"rust/macros/quote.rs",
			"rust/pin-init/src/lib.rs",
			"rust/pin-init/internal/src/lib.rs",
		} {
			if err := requireRustProfileFile(sourceRoot, path); err != nil {
				return nil, err
			}
		}
		if !strings.Contains(rustMake, "obj-$(CONFIG_RUST) += bindings.o pin_init.o kernel.o") {
			return nil, fmt.Errorf("pin-init layout does not declare pin_init.o in rust/Makefile")
		}
		for _, marker := range []string{"$(obj)/pin_init.o:", "libpin_init_internal"} {
			if !strings.Contains(rustMake, marker) {
				return nil, fmt.Errorf("pin-init rust/Makefile is missing %q", marker)
			}
		}
	} else if strings.Contains(rustMake, "pin_init") {
		return nil, fmt.Errorf("legacy Rust layout references missing pin-init sources")
	}
	for _, marker := range []string{
		"obj-$(CONFIG_RUST) += core.o compiler_builtins.o ffi.o",
		"obj-$(CONFIG_RUST) += helpers/helpers.o",
		"obj-$(CONFIG_RUST) += uapi.o",
		"obj-$(CONFIG_RUST) += exports.o",
		"$(obj)/core.o:",
		"$(obj)/compiler_builtins.o:",
		"$(obj)/ffi.o:",
		"$(obj)/bindings.o:",
		"$(obj)/uapi.o:",
		"$(obj)/kernel.o:",
	} {
		if !strings.Contains(rustMake, marker) {
			return nil, fmt.Errorf("unsupported rust/Makefile crate graph: missing %q", marker)
		}
	}
	targetGenerator, err := readRustProfileFile(sourceRoot, "scripts/generate_rust_target.rs")
	if err != nil {
		return nil, err
	}
	for _, marker := range []string{"KernelConfig::from_stdin()", `cfg.has("X86_64")`} {
		if !strings.Contains(targetGenerator, marker) {
			return nil, fmt.Errorf("unsupported Rust target generator: missing %q", marker)
		}
	}
	procMacroUsesRustcCfg, err := rustProcMacroUsesRustcCfg(rustMake)
	if err != nil {
		return nil, err
	}
	if layout == "pin-init" && !procMacroUsesRustcCfg {
		return nil, fmt.Errorf("pin-init Rust layout requires proc macros to consume rustc_cfg")
	}

	redirects, err := makeWords(rustMake, "redirect-intrinsics")
	if err != nil {
		return nil, err
	}
	var coreObjcopy []string
	for _, symbol := range redirects {
		if strings.Contains(symbol, "$(") || strings.ContainsAny(symbol, " \t") {
			return nil, fmt.Errorf("unsupported redirect-intrinsics entry %q", symbol)
		}
		coreObjcopy = append(coreObjcopy, "--redefine-sym", symbol+"=__rust"+symbol)
	}

	alwaysTargetFlags := append([]string(nil), baseFlags...)
	for _, requiredFlag := range []string{
		"--target=$(objtree)/scripts/target.json",
		"-Ctarget-feature=-sse,-sse2,-sse3,-ssse3,-sse4.1,-sse4.2,-avx,-avx2",
		"-Cno-redzone=y",
		"-Ccode-model=kernel",
	} {
		if !strings.Contains(archMake, "KBUILD_RUSTFLAGS += "+requiredFlag) {
			return nil, fmt.Errorf("arch/x86/Makefile does not contain supported Rust flag %q", requiredFlag)
		}
		alwaysTargetFlags = append(alwaysTargetFlags, strings.ReplaceAll(requiredFlag, "$(objtree)/scripts/target.json", "{target_spec}"))
	}
	alwaysTargetFlags = append(alwaysTargetFlags, "@{rustc_cfg}")

	conditional, err := rustTargetConditionalFlags(rootMake, archMake)
	if err != nil {
		return nil, err
	}

	procMacros := []RustProcMacroProfile{{
		Name:           "macros",
		Source:         "rust/macros/lib.rs",
		SourcePrefixes: []string{"rust/macros/"},
		SourceFiles:    []string{},
		Flags:          []string{},
		UsesRustcCfg:   procMacroUsesRustcCfg,
	}}
	if layout == "pin-init" {
		procMacros = append(procMacros, RustProcMacroProfile{
			Name:           "pin_init_internal",
			Source:         "rust/pin-init/internal/src/lib.rs",
			SourcePrefixes: []string{"rust/pin-init/internal/src/"},
			SourceFiles:    []string{"rust/macros/quote.rs"},
			Flags:          []string{"--cfg", "kernel"},
			UsesRustcCfg:   true,
		})
	}

	crates := rustCrateGraph(layout, coreObjcopy)
	generatedAssembly, err := rustGeneratedAssembly(sourceRoot, rustMake, layout)
	if err != nil {
		return nil, err
	}
	for i := range crates {
		if crates[i].Name == "kernel" {
			for _, generated := range generatedAssembly {
				crates[i].GeneratedInputs = append(crates[i].GeneratedInputs, generated.Output)
			}
		}
	}

	moduleExterns := []string{"kernel"}
	if layout == "pin-init" {
		moduleExterns = append([]string{"pin_init"}, moduleExterns...)
	}
	moduleFlags := []string{
		"--cfg", "MODULE",
		"-Zallow-features={allowed_features_csv}",
		"-Zcrate-attr=no_std",
		"-Zcrate-attr=feature({allowed_features_csv})",
		"-Zunstable-options",
	}
	for _, name := range moduleExterns {
		moduleFlags = append(moduleFlags, "--extern", name)
	}
	moduleFlags = append(moduleFlags, "--crate-type", "rlib", "-L{rust_dir}", "--sysroot=/dev/null")

	runtime := []RustRuntimeObject{
		{Path: "rust/core.o"},
		{Path: "rust/compiler_builtins.o"},
		{Path: "rust/ffi.o"},
		{Path: "rust/helpers/helpers.o"},
		{Path: "rust/bindings.o"},
	}
	if layout == "pin-init" {
		runtime = append(runtime, RustRuntimeObject{Path: "rust/pin_init.o"})
	}
	runtime = append(runtime,
		RustRuntimeObject{Path: "rust/kernel.o"},
		RustRuntimeObject{Path: "rust/uapi.o"},
		RustRuntimeObject{Path: "rust/build_error.o", Config: "CONFIG_RUST_BUILD_ASSERT_ALLOW", Equals: "y"},
		RustRuntimeObject{Path: "rust/exports.o"},
	)

	return &RustProfile{
		Schema:       RustProfileSchema,
		Architecture: "x86_64",
		SourceLayout: layout,
		Target: RustTargetRecipe{
			GeneratorSource: "scripts/generate_rust_target.rs",
			Stdin:           "config_auto_conf",
			Output:          "scripts/target.json",
		},
		CommonFlags: commonFlags,
		TargetFlags: RustFlagSet{
			Always:      alwaysTargetFlags,
			Conditional: conditional,
		},
		Module: RustModuleProfile{
			AllowedFeatures: allowedFeatures,
			Flags:           moduleFlags,
		},
		Bindgen: RustBindgenProfile{
			Parameters:     "rust/bindgen_parameters",
			BindingsHeader: "rust/bindings/bindings_helper.h",
			UAPIHeader:     "rust/uapi/uapi_helper.h",
			HelpersSource:  "rust/helpers/helpers.c",
			CommonFlags: []string{
				"--rust-target", "1.68",
				"--use-core",
				"--with-derive-default",
				"--ctypes-prefix", "ffi",
				"--no-layout-tests",
				"--no-debug", ".*",
				"--enable-function-attribute-detection",
			},
		},
		ProcMacros:        procMacros,
		Crates:            crates,
		GeneratedAssembly: generatedAssembly,
		Exports: RustExportsProfile{
			Source: "rust/exports.c",
			Crates: []string{"core", "helpers", "bindings", "kernel"},
		},
		RuntimeObjects: runtime,
	}, nil
}

func rustCrateGraph(layout string, coreObjcopy []string) []RustCrateProfile {
	empty := []string{}
	crates := []RustCrateProfile{
		{
			Name:            "core",
			Source:          "rustc://library/core/src/lib.rs",
			SourcePrefixes:  []string{"rustc://library/core/"},
			SourceFiles:     empty,
			GeneratedInputs: empty,
			Deps:            empty,
			Externs:         empty,
			Flags:           []string{"--edition=2024", "--cfg", "no_fp_fmt_parse"},
			SkipFlags:       []string{"--edition=2021", "-Wunreachable_pub"},
			ObjcopyFlags:    coreObjcopy,
		},
		{
			Name:            "compiler_builtins",
			Source:          "rust/compiler_builtins.rs",
			SourcePrefixes:  empty,
			SourceFiles:     []string{"rust/compiler_builtins.rs"},
			GeneratedInputs: empty,
			Deps:            []string{"core"},
			Externs:         empty,
			Flags:           empty,
			SkipFlags:       empty,
			ObjcopyFlags:    []string{"-w", "-W", "__*"},
		},
	}
	if layout == "pin-init" {
		crates = append(crates, RustCrateProfile{
			Name:            "pin_init",
			Source:          "rust/pin-init/src/lib.rs",
			SourcePrefixes:  []string{"rust/pin-init/src/"},
			SourceFiles:     empty,
			GeneratedInputs: empty,
			Deps:            []string{"core", "compiler_builtins", "macros", "pin_init_internal"},
			Externs:         []string{"pin_init_internal", "macros"},
			Flags:           []string{"--cfg", "kernel"},
			SkipFlags:       empty,
			ObjcopyFlags:    empty,
		})
	}
	baseDeps := []string{"core", "compiler_builtins"}
	pinDeps := []string{}
	pinExterns := []string{}
	if layout == "pin-init" {
		pinDeps = []string{"pin_init", "macros", "pin_init_internal"}
		pinExterns = []string{"pin_init"}
	}
	crates = append(crates,
		RustCrateProfile{
			Name: "ffi", Source: "rust/ffi.rs", SourcePrefixes: empty,
			SourceFiles: []string{"rust/ffi.rs"}, GeneratedInputs: empty,
			Deps: append([]string(nil), baseDeps...), Externs: empty,
			Flags: empty, SkipFlags: empty, ObjcopyFlags: empty,
		},
		RustCrateProfile{
			Name: "build_error", Source: "rust/build_error.rs", SourcePrefixes: empty,
			SourceFiles: []string{"rust/build_error.rs"}, GeneratedInputs: empty,
			Deps: append([]string(nil), baseDeps...), Externs: empty,
			Flags: empty, SkipFlags: empty, ObjcopyFlags: empty,
		},
		RustCrateProfile{
			Name: "bindings", Source: "rust/bindings/lib.rs", SourcePrefixes: []string{"rust/bindings/"},
			SourceFiles: empty, GeneratedInputs: []string{"bindings_generated", "bindings_helpers_generated"},
			Deps: append(append(append([]string{}, baseDeps...), "ffi"), pinDeps...), Externs: append([]string{"ffi"}, pinExterns...),
			Flags: empty, SkipFlags: empty, ObjcopyFlags: empty,
		},
		RustCrateProfile{
			Name: "uapi", Source: "rust/uapi/lib.rs", SourcePrefixes: []string{"rust/uapi/"},
			SourceFiles: empty, GeneratedInputs: []string{"uapi_generated"},
			Deps: append(append(append([]string{}, baseDeps...), "ffi"), pinDeps...), Externs: append([]string{"ffi"}, pinExterns...),
			Flags: empty, SkipFlags: empty, ObjcopyFlags: empty,
		},
	)
	kernelDeps := append([]string{}, baseDeps...)
	kernelDeps = append(kernelDeps, "ffi", "build_error")
	if layout == "pin-init" {
		kernelDeps = append(kernelDeps, "pin_init", "bindings", "uapi", "macros", "pin_init_internal")
	} else {
		kernelDeps = append(kernelDeps, "bindings", "uapi", "macros")
	}
	kernelExterns := append([]string{"ffi"}, pinExterns...)
	kernelExterns = append(kernelExterns, "build_error", "macros", "bindings", "uapi")
	crates = append(crates, RustCrateProfile{
		Name: "kernel", Source: "rust/kernel/lib.rs", SourcePrefixes: []string{"rust/kernel/"},
		SourceFiles: empty, GeneratedInputs: empty,
		Deps: kernelDeps, Externs: kernelExterns,
		Flags: empty, SkipFlags: empty, ObjcopyFlags: empty,
	})
	return crates
}

func rustGeneratedAssembly(sourceRoot, rustMake, layout string) ([]RustGeneratedAssembly, error) {
	if layout == "legacy" {
		return nil, nil
	}
	entries := []RustGeneratedAssembly{
		{Config: "CONFIG_JUMP_LABEL", Equals: "y", Source: "rust/kernel/generated_arch_static_branch_asm.rs.S", Output: "rust/kernel/generated_arch_static_branch_asm.rs"},
		{Config: "CONFIG_BUG", Equals: "y", Source: "rust/kernel/generated_arch_warn_asm.rs.S", Output: "rust/kernel/generated_arch_warn_asm.rs"},
		{Config: "CONFIG_BUG", Equals: "y", Source: "rust/kernel/generated_arch_reachable_asm.rs.S", Output: "rust/kernel/generated_arch_reachable_asm.rs"},
	}
	for _, entry := range entries {
		if err := requireRustProfileFile(sourceRoot, entry.Source); err != nil {
			return nil, err
		}
		if !strings.Contains(rustMake, filepath.Base(entry.Output)) {
			return nil, fmt.Errorf("rust/Makefile does not declare generated assembly %q", entry.Output)
		}
	}
	return entries, nil
}

func rustTargetConditionalFlags(rootMake, archMake string) ([]RustConditionalFlags, error) {
	required := []struct {
		content string
		marker  string
		group   RustConditionalFlags
	}{
		{rootMake, "KBUILD_RUSTFLAGS += -Copt-level=s", RustConditionalFlags{Config: "CONFIG_CC_OPTIMIZE_FOR_SIZE", Equals: "y", Flags: []string{"-Copt-level=s"}, ElseFlags: []string{"-Copt-level=2"}}},
		{rootMake, "CONFIG_RUST_DEBUG_ASSERTIONS", RustConditionalFlags{Config: "CONFIG_RUST_DEBUG_ASSERTIONS", Equals: "y", Flags: []string{"-Cdebug-assertions=y"}, ElseFlags: []string{"-Cdebug-assertions=n"}}},
		{rootMake, "CONFIG_RUST_OVERFLOW_CHECKS", RustConditionalFlags{Config: "CONFIG_RUST_OVERFLOW_CHECKS", Equals: "y", Flags: []string{"-Coverflow-checks=y"}, ElseFlags: []string{"-Coverflow-checks=n"}}},
		{rootMake, "ifdef CONFIG_FRAME_POINTER", RustConditionalFlags{Config: "CONFIG_FRAME_POINTER", Equals: "y", Flags: []string{"-Cforce-frame-pointers=y", "-Zllvm_module_flag=frame-pointer:u32:2:max"}}},
		{archMake, "RETHUNK_RUSTFLAGS", RustConditionalFlags{Config: "CONFIG_MITIGATION_RETHUNK", Equals: "y", Flags: []string{"-Zfunction-return=thunk-extern"}}},
		{archMake, "CONFIG_X86_KERNEL_IBT", RustConditionalFlags{Config: "CONFIG_X86_KERNEL_IBT", Equals: "y", Flags: []string{"-Zcf-protection=branch", "-Cjump-tables=n"}}},
		{archMake, "PADDING_RUSTFLAGS", RustConditionalFlags{Config: "CONFIG_CALL_PADDING", Equals: "y", Flags: []string{"-Zpatchable-function-entry={CONFIG_FUNCTION_PADDING_BYTES},{CONFIG_FUNCTION_PADDING_BYTES}"}}},
	}
	out := make([]RustConditionalFlags, 0, len(required)+5)
	for _, item := range required {
		if !strings.Contains(item.content, item.marker) {
			return nil, fmt.Errorf("unsupported Rust flag layout: missing %q", item.marker)
		}
		out = append(out, item.group)
	}
	if strings.Contains(rootMake, "KBUILD_RUSTFLAGS-$(CONFIG_WERROR) += -Dwarnings") {
		out = append(out, RustConditionalFlags{
			Config: "CONFIG_WERROR",
			Equals: "y",
			Flags:  []string{"-Dwarnings"},
		})
	}

	if strings.Contains(archMake, "ifdef CONFIG_X86_NATIVE_CPU") {
		if !strings.Contains(archMake, "KBUILD_RUSTFLAGS += -Ctarget-cpu=native") ||
			!strings.Contains(archMake, "KBUILD_RUSTFLAGS += -Ctarget-cpu=x86-64 -Ztune-cpu=generic") {
			return nil, fmt.Errorf("unsupported CONFIG_X86_NATIVE_CPU Rust flag layout")
		}
		out = append(out, RustConditionalFlags{
			Config: "CONFIG_X86_NATIVE_CPU", Equals: "y",
			Flags:     []string{"-Ctarget-cpu=native"},
			ElseFlags: []string{"-Ctarget-cpu=x86-64", "-Ztune-cpu=generic"},
		})
		return out, nil
	}

	re := regexp.MustCompile(`(?m)^[ \t]*rustflags-\$\(CONFIG_([A-Za-z0-9_]+)\)[ \t]*\+=[ \t]*(\S(?:.*\S)?)?[ \t]*$`)
	matches := re.FindAllStringSubmatch(archMake, -1)
	if len(matches) == 0 {
		return nil, fmt.Errorf("arch/x86/Makefile has no supported Rust CPU flag mapping")
	}
	for _, match := range matches {
		flags := strings.Fields(match[2])
		if err := rejectMakeWords("x86 Rust CPU flags", flags); err != nil {
			return nil, err
		}
		out = append(out, RustConditionalFlags{
			Config: "CONFIG_" + match[1],
			Equals: "y",
			Flags:  flags,
		})
	}
	return out, nil
}

func rustProcMacroUsesRustcCfg(rustMake string) (bool, error) {
	start := strings.Index(rustMake, "cmd_rustc_procmacro =")
	if start < 0 {
		return false, fmt.Errorf("unsupported rust/Makefile: missing cmd_rustc_procmacro")
	}
	block := rustMake[start:]
	if end := strings.Index(block, "\n\n"); end >= 0 {
		block = block[:end]
	}
	return strings.Contains(block, "@$(objtree)/include/generated/rustc_cfg"), nil
}

func readRustProfileFile(root, path string) (string, error) {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil {
		return "", fmt.Errorf("read Rust profile input %s: %w", path, err)
	}
	return string(data), nil
}

func requireRustProfileFile(root, path string) error {
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil {
		return fmt.Errorf("Rust profile requires %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("Rust profile input %s is not a regular file", path)
	}
	return nil
}

func rustProfileFileExists(root, path string) bool {
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(path)))
	return err == nil && info.Mode().IsRegular()
}

func makeWords(content, name string) ([]string, error) {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	prefix := regexp.MustCompile(`^(?:export[ \t]+)?` + regexp.QuoteMeta(name) + `[ \t]*(?::=|\+=|=)[ \t]*(.*)$`)
	for i, line := range lines {
		match := prefix.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		value := match[1]
		for strings.HasSuffix(strings.TrimSpace(value), `\`) {
			value = strings.TrimSpace(value)
			value = strings.TrimSpace(strings.TrimSuffix(value, `\`))
			i++
			if i >= len(lines) {
				return nil, fmt.Errorf("unterminated %s assignment", name)
			}
			value += " " + strings.TrimSpace(lines[i])
		}
		words := strings.Fields(value)
		if len(words) == 0 {
			return nil, fmt.Errorf("%s assignment is empty", name)
		}
		return words, nil
	}
	return nil, fmt.Errorf("missing %s assignment", name)
}

func removeMakeWords(words []string, removed string) []string {
	out := make([]string, 0, len(words))
	for _, word := range words {
		if word != removed {
			out = append(out, word)
		}
	}
	return out
}

func rejectMakeWords(name string, words []string) error {
	for _, word := range words {
		if strings.Contains(word, "$(") || strings.Contains(word, "${") || strings.ContainsAny(word, "\r\n") {
			return fmt.Errorf("%s contains unsupported Make expression %q", name, word)
		}
	}
	return nil
}
