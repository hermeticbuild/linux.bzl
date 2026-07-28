package kconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestGenerateRustProfileLegacyLayout(t *testing.T) {
	root := rustProfileFixture(t, false)
	profile, err := GenerateRustProfile(root, "x86")
	if err != nil {
		t.Fatalf("GenerateRustProfile() failed: %v", err)
	}
	if profile.Schema != RustProfileSchema || profile.Architecture != "x86_64" || profile.SourceLayout != "legacy" {
		t.Fatalf("profile identity = %#v", profile)
	}
	if profile.Schema != "linux-rust-profile-v2" {
		t.Fatalf("schema = %q, want linux-rust-profile-v2", profile.Schema)
	}
	if profile.Target.Kind != "generated" ||
		profile.Target.GeneratorSource != "scripts/generate_rust_target.rs" ||
		profile.Target.Stdin != "config_auto_conf" ||
		profile.Target.Output != "scripts/target.json" ||
		profile.Target.BuiltinTriple != "" {
		t.Fatalf("x86 target recipe = %#v", profile.Target)
	}
	if len(profile.CommonFlags.Always) == 0 || profile.CommonFlags.Always[0] != "--edition=2021" {
		t.Fatalf("common flags = %#v", profile.CommonFlags)
	}
	core := rustProfileCrate(profile, "core")
	if got, want := core.VersionPredicates, []RustVersionPredicate{newRustVersionPredicate(
		"1.87.0",
		[]string{"--edition=2024"},
		[]string{"--edition=2021"},
		nil,
		[]string{"--edition=2024"},
	)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("core version predicates = %#v, want %#v", got, want)
	}
	framePointer := rustProfileConditional(profile, "CONFIG_FRAME_POINTER")
	if got, want := framePointer.VersionPredicates, []RustVersionPredicate{newRustVersionPredicate(
		"1.98.0",
		nil,
		[]string{"-Zllvm_module_flag=frame-pointer:u32:2:max"},
		[]string{"-Zllvm_module_flag=frame-pointer:u32:2:max"},
		nil,
	)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("frame-pointer version predicates = %#v, want %#v", got, want)
	}
	ibt := rustProfileConditional(profile, "CONFIG_X86_KERNEL_IBT")
	if got, want := ibt.VersionPredicates, []RustVersionPredicate{newRustVersionPredicate(
		"1.93.0",
		[]string{"-Cjump-tables=n"},
		[]string{"-Zno-jump-tables"},
		[]string{"-Zno-jump-tables"},
		[]string{"-Cjump-tables=n"},
	)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("IBT version predicates = %#v, want %#v", got, want)
	}
	if got, want := profile.Module.AllowedFeatures, []string{"arbitrary_self_types", "lint_reasons", "used_with_arg"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("allowed features = %v, want %v", got, want)
	}
	if len(profile.ProcMacros) != 1 || profile.ProcMacros[0].Name != "macros" || profile.ProcMacros[0].UsesRustcCfg {
		t.Fatalf("legacy proc macros = %#v", profile.ProcMacros)
	}
	if hasRustProfileCrate(profile, "pin_init") {
		t.Fatalf("legacy profile contains pin_init: %#v", profile.Crates)
	}
	if len(profile.GeneratedAssembly) != 0 {
		t.Fatalf("legacy generated assembly = %#v", profile.GeneratedAssembly)
	}
	if !hasRustConditionalFlag(profile.TargetFlags.Conditional, "CONFIG_DEBUG_INFO", "-Cdebuginfo=2") ||
		!hasRustConditionalFlag(profile.TargetFlags.Conditional, "CONFIG_DEBUG_INFO_DWARF5", "-Zdwarf-version=5") {
		t.Fatalf("Rust debug flags are missing: %#v", profile.TargetFlags.Conditional)
	}
	for _, path := range rustRuntimePaths(profile) {
		if path == "rust/pin_init.o" {
			t.Fatalf("legacy runtime contains pin_init: %#v", profile.RuntimeObjects)
		}
	}
	data1, err := profile.JSON()
	if err != nil {
		t.Fatal(err)
	}
	data2, err := profile.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(data1, data2) || !strings.HasSuffix(string(data1), "\n") {
		t.Fatal("Rust profile JSON is not deterministic newline-terminated output")
	}
	for _, forbidden := range []string{
		`"version_predicates": null`,
		`"else_flags": null`,
		`"add": null`,
		`"remove": null`,
		`"else_add": null`,
		`"else_remove": null`,
	} {
		if strings.Contains(string(data1), forbidden) {
			t.Fatalf("Rust profile JSON contains %s:\n%s", forbidden, data1)
		}
	}
}

func TestRustProfileCoreEditionBoundaryAfterSkipFlags(t *testing.T) {
	profile, err := GenerateRustProfile(rustProfileFixture(t, false), "x86")
	if err != nil {
		t.Fatalf("GenerateRustProfile() failed: %v", err)
	}
	core := rustProfileCrate(profile, "core")
	for _, test := range []struct {
		version string
		want    string
		absent  string
	}{
		{version: "1.78.0", want: "--edition=2021", absent: "--edition=2024"},
		{version: "1.86.0", want: "--edition=2021", absent: "--edition=2024"},
		{version: "1.87.0", want: "--edition=2024", absent: "--edition=2021"},
	} {
		t.Run(test.version, func(t *testing.T) {
			flags := evaluateRustCrateFlags(t, profile, core, nil, test.version)
			count := countRustProfileValue(flags, test.want)
			if count != 1 {
				t.Fatalf("flags %v contain %q %d times, want once", flags, test.want, count)
			}
			if containsRustProfileValue(flags, test.absent) {
				t.Fatalf("flags %v unexpectedly contain %q", flags, test.absent)
			}
		})
	}
}

func TestRustProfileVersionGateBoundaries(t *testing.T) {
	x86, err := GenerateRustProfile(rustProfileFixture(t, false), "x86")
	if err != nil {
		t.Fatalf("GenerateRustProfile(x86) failed: %v", err)
	}
	arm64, err := GenerateRustProfile(rustProfileFixture(t, false), "arm64")
	if err != nil {
		t.Fatalf("GenerateRustProfile(arm64) failed: %v", err)
	}

	for _, test := range []struct {
		name    string
		profile *RustProfile
		config  map[string]string
		version string
		want    string
		absent  string
	}{
		{
			name:    "arm64-target-before-1.85",
			profile: arm64,
			version: "1.84.0",
			want:    "--target=aarch64-unknown-none",
			absent:  "--target=aarch64-unknown-none-softfloat",
		},
		{
			name:    "arm64-target-at-1.85",
			profile: arm64,
			version: "1.85.0",
			want:    "--target=aarch64-unknown-none-softfloat",
			absent:  "--target=aarch64-unknown-none",
		},
		{
			name:    "x86-ibt-before-1.93",
			profile: x86,
			config:  map[string]string{"CONFIG_X86_KERNEL_IBT": "y"},
			version: "1.92.0",
			want:    "-Zno-jump-tables",
			absent:  "-Cjump-tables=n",
		},
		{
			name:    "x86-ibt-at-1.93",
			profile: x86,
			config:  map[string]string{"CONFIG_X86_KERNEL_IBT": "y"},
			version: "1.93.0",
			want:    "-Cjump-tables=n",
			absent:  "-Zno-jump-tables",
		},
		{
			name:    "frame-pointer-before-1.98",
			profile: x86,
			config:  map[string]string{"CONFIG_FRAME_POINTER": "y"},
			version: "1.97.0",
			want:    "-Zllvm_module_flag=frame-pointer:u32:2:max",
		},
		{
			name:    "frame-pointer-at-1.98",
			profile: x86,
			config:  map[string]string{"CONFIG_FRAME_POINTER": "y"},
			version: "1.98.0",
			absent:  "-Zllvm_module_flag=frame-pointer:u32:2:max",
		},
		{
			name:    "arm64-unwind-before-1.98",
			profile: arm64,
			config:  map[string]string{"CONFIG_UNWIND_TABLES": "y"},
			version: "1.97.0",
			want:    "-Zllvm_module_flag=uwtable:u32:2:max",
		},
		{
			name:    "arm64-unwind-at-1.98",
			profile: arm64,
			config:  map[string]string{"CONFIG_UNWIND_TABLES": "y"},
			version: "1.98.0",
			absent:  "-Zllvm_module_flag=uwtable:u32:2:max",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			flags, predicates := evaluateRustTargetFlags(test.profile, test.config)
			flags = applyRustProfilePredicates(t, flags, predicates, test.version)
			if test.want != "" && !containsRustProfileValue(flags, test.want) {
				t.Fatalf("flags %v do not contain %q", flags, test.want)
			}
			if test.absent != "" && containsRustProfileValue(flags, test.absent) {
				t.Fatalf("flags %v unexpectedly contain %q", flags, test.absent)
			}
		})
	}
}

func TestGenerateRustProfilePinInitLayout(t *testing.T) {
	root := rustProfileFixture(t, true)
	profile, err := GenerateRustProfile(root, "x86_64")
	if err != nil {
		t.Fatalf("GenerateRustProfile() failed: %v", err)
	}
	if profile.SourceLayout != "pin-init" {
		t.Fatalf("source layout = %q, want pin-init", profile.SourceLayout)
	}
	if len(profile.ProcMacros) != 2 || profile.ProcMacros[1].Name != "pin_init_internal" {
		t.Fatalf("pin-init proc macros = %#v", profile.ProcMacros)
	}
	for _, proc := range profile.ProcMacros {
		if !proc.UsesRustcCfg {
			t.Fatalf("pin-init proc macro does not consume rustc_cfg: %#v", proc)
		}
	}
	if !hasRustProfileCrate(profile, "pin_init") {
		t.Fatalf("pin-init crate graph = %#v", profile.Crates)
	}
	if len(profile.GeneratedAssembly) != 3 {
		t.Fatalf("generated assembly = %#v", profile.GeneratedAssembly)
	}
	kernel := rustProfileCrate(profile, "kernel")
	if got, want := kernel.GeneratedInputs, []string{
		"rust/kernel/generated_arch_static_branch_asm.rs",
		"rust/kernel/generated_arch_warn_asm.rs",
		"rust/kernel/generated_arch_reachable_asm.rs",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("kernel generated inputs = %v, want %v", got, want)
	}
}

func TestGenerateRustProfileArm64Layouts(t *testing.T) {
	for _, test := range []struct {
		name       string
		arch       string
		pinInit    bool
		wantLayout string
	}{
		{name: "linux-6.12-legacy", arch: "arm64", wantLayout: "legacy"},
		{name: "linux-6.18-pin-init", arch: "aarch64", pinInit: true, wantLayout: "pin-init"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := rustProfileFixture(t, test.pinInit)
			profile, err := GenerateRustProfile(root, test.arch)
			if err != nil {
				t.Fatalf("GenerateRustProfile() failed: %v", err)
			}
			if profile.Architecture != "aarch64" || profile.SourceLayout != test.wantLayout {
				t.Fatalf("profile identity = %#v", profile)
			}
			if profile.Target.Kind != "builtin" ||
				profile.Target.BuiltinTriple != "aarch64-unknown-none" ||
				profile.Target.GeneratorSource != "" ||
				profile.Target.Stdin != "" ||
				profile.Target.Output != "" {
				t.Fatalf("arm64 target recipe = %#v", profile.Target)
			}
			for _, flag := range []string{"--target=aarch64-unknown-none", "-Ctarget-feature=-neon"} {
				if !containsRustProfileValue(profile.TargetFlags.Always, flag) {
					t.Fatalf("arm64 target flags %v do not contain %q", profile.TargetFlags.Always, flag)
				}
			}
			if got, want := profile.TargetFlags.VersionPredicates, []RustVersionPredicate{newRustVersionPredicate(
				"1.85.0",
				[]string{"--target=aarch64-unknown-none-softfloat"},
				[]string{"--target=aarch64-unknown-none", "-Ctarget-feature=-neon"},
				nil,
				[]string{"--target=aarch64-unknown-none-softfloat"},
			)}; !reflect.DeepEqual(got, want) {
				t.Fatalf("arm64 target version predicates = %#v, want %#v", got, want)
			}

			unwind := rustProfileConditional(profile, "CONFIG_UNWIND_TABLES")
			if got, want := unwind.Flags, []string{"-Cforce-unwind-tables=y", "-Zuse-sync-unwind=n"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("arm64 unwind flags = %#v, want %#v", got, want)
			}
			if got, want := unwind.ElseFlags, []string{"-Cforce-unwind-tables=n"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("arm64 no-unwind flags = %#v, want %#v", got, want)
			}
			if len(unwind.VersionPredicates) != 1 || unwind.VersionPredicates[0].AtLeast != "1.98.0" {
				t.Fatalf("arm64 unwind version predicates = %#v", unwind.VersionPredicates)
			}
			ptrAuthIndex := rustProfileConditionalIndex(profile, "CONFIG_ARM64_PTR_AUTH_KERNEL")
			btiIndex := rustProfileConditionalIndex(profile, "CONFIG_ARM64_BTI_KERNEL")
			if btiIndex < 0 || ptrAuthIndex <= btiIndex {
				t.Fatalf("arm64 branch-protection condition order = ptr-auth %d, BTI %d", ptrAuthIndex, btiIndex)
			}
			ptrAuth := profile.TargetFlags.Conditional[ptrAuthIndex]
			if ptrAuth.UnlessConfig != "CONFIG_ARM64_BTI_KERNEL" {
				t.Fatalf("arm64 pointer-auth condition guard = %q", ptrAuth.UnlessConfig)
			}
			if got := rustProfileConditional(profile, "CONFIG_SHADOW_CALL_STACK").Flags; !reflect.DeepEqual(got, []string{"-Zfixed-x18"}) {
				t.Fatalf("arm64 shadow-call-stack flags = %#v", got)
			}

			core := rustProfileCrate(profile, "core")
			for _, flag := range []string{
				"__ashrti3=__rust__ashrti3",
				"__ashlti3=__rust__ashlti3",
				"__lshrti3=__rust__lshrti3",
			} {
				if !containsRustProfileValue(core.ObjcopyFlags, flag) {
					t.Fatalf("arm64 core objcopy flags %v do not contain %q", core.ObjcopyFlags, flag)
				}
			}
			if got, want := profile.UnsupportedConfigs, []string{
				"CONFIG_CPU_BIG_ENDIAN",
				"CONFIG_CFI",
				"CONFIG_CFI_CLANG",
				"CONFIG_KASAN",
				"CONFIG_KCSAN",
				"CONFIG_UBSAN",
			}; !reflect.DeepEqual(got, want) {
				t.Fatalf("arm64 unsupported configs = %#v, want %#v", got, want)
			}
		})
	}
}

func TestGenerateRustProfileRejectsMixedLayout(t *testing.T) {
	root := rustProfileFixture(t, false)
	writeRustProfileFixture(t, root, "rust/pin-init/src/lib.rs", "")
	_, err := GenerateRustProfile(root, "x86")
	if err == nil || !strings.Contains(err.Error(), "mixed Rust pin-init layout") {
		t.Fatalf("GenerateRustProfile() error = %v, want mixed-layout error", err)
	}
}

func TestGenerateRustProfileRejectsUnresolvedCommonFlag(t *testing.T) {
	root := rustProfileFixture(t, false)
	path := filepath.Join(root, "Makefile")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content = []byte(strings.Replace(string(content), "-Wmissing_docs", "$(UNRESOLVED)", 1))
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = GenerateRustProfile(root, "x86")
	if err == nil || !strings.Contains(err.Error(), "unsupported Make expression") {
		t.Fatalf("GenerateRustProfile() error = %v, want unresolved-expression error", err)
	}
}

func TestGenerateRustProfileRejectsOtherArchitectures(t *testing.T) {
	root := rustProfileFixture(t, false)
	_, err := GenerateRustProfile(root, "riscv")
	if err == nil || !strings.Contains(err.Error(), "only x86_64 and arm64") {
		t.Fatalf("GenerateRustProfile() error = %v, want architecture error", err)
	}
}

func rustProfileFixture(t *testing.T, pinInit bool) string {
	t.Helper()
	root := t.TempDir()
	writeRustProfileFixture(t, root, "Makefile", `
export rust_common_flags := --edition=2021 \
 -Zbinary_dep_depinfo=y \
 -Wmissing_docs
KBUILD_RUSTFLAGS := $(rust_common_flags) \
 -Cpanic=abort -Cembed-bitcode=n -Clto=n \
 -Cforce-unwind-tables=n -Ccodegen-units=1 \
 -Csymbol-mangling-version=v0 -Crelocation-model=static \
 -Zfunction-sections=n -Wclippy::float_arithmetic
KBUILD_RUSTFLAGS += -Copt-level=2
KBUILD_RUSTFLAGS += -Copt-level=s
KBUILD_RUSTFLAGS += -Cdebug-assertions=$(if $(CONFIG_RUST_DEBUG_ASSERTIONS),y,n)
KBUILD_RUSTFLAGS += -Coverflow-checks=$(if $(CONFIG_RUST_OVERFLOW_CHECKS),y,n)
KBUILD_RUSTFLAGS-$(CONFIG_WERROR) += -Dwarnings
ifdef CONFIG_FRAME_POINTER
KBUILD_RUSTFLAGS += -Cforce-frame-pointers=y
KBUILD_RUSTFLAGS += $(if $(call rustc-min-version,109800),,-Zllvm_module_flag=frame-pointer:u32:2:max)
endif
`)
	archMake := `
KBUILD_RUSTFLAGS += --target=$(objtree)/scripts/target.json
KBUILD_RUSTFLAGS += -Ctarget-feature=-sse,-sse2,-sse3,-ssse3,-sse4.1,-sse4.2,-avx,-avx2
RETHUNK_RUSTFLAGS := -Zfunction-return=thunk-extern
ifdef CONFIG_X86_KERNEL_IBT
KBUILD_RUSTFLAGS += -Zcf-protection=branch $(if $(call rustc-min-version,109300),-Cjump-tables=n,-Zno-jump-tables)
endif
ifdef CONFIG_CALL_PADDING
PADDING_RUSTFLAGS := -Zpatchable-function-entry=$(CONFIG_FUNCTION_PADDING_BYTES),$(CONFIG_FUNCTION_PADDING_BYTES)
endif
KBUILD_RUSTFLAGS += -Cno-redzone=y
KBUILD_RUSTFLAGS += -Ccode-model=kernel
`
	if pinInit {
		archMake += `
ifdef CONFIG_X86_NATIVE_CPU
KBUILD_RUSTFLAGS += -Ctarget-cpu=native
else
KBUILD_RUSTFLAGS += -Ctarget-cpu=x86-64 -Ztune-cpu=generic
endif
`
	} else {
		archMake += `
rustflags-$(CONFIG_MK8) += -Ctarget-cpu=k8
rustflags-$(CONFIG_MPSC) += -Ctarget-cpu=nocona
rustflags-$(CONFIG_MCORE2) += -Ctarget-cpu=core2
rustflags-$(CONFIG_MATOM) += -Ctarget-cpu=atom
rustflags-$(CONFIG_GENERIC_CPU) += -Ztune-cpu=generic
`
	}
	writeRustProfileFixture(t, root, "arch/x86/Makefile", archMake)
	writeRustProfileFixture(t, root, "arch/arm64/Makefile", `
ifeq ($(call rustc-min-version, 108500),y)
KBUILD_RUSTFLAGS += --target=aarch64-unknown-none-softfloat
else
KBUILD_RUSTFLAGS += --target=aarch64-unknown-none -Ctarget-feature="-neon"
endif
ifneq ($(CONFIG_UNWIND_TABLES),y)
KBUILD_RUSTFLAGS += -Cforce-unwind-tables=n
else
KBUILD_RUSTFLAGS += -Cforce-unwind-tables=y -Zuse-sync-unwind=n
KBUILD_RUSTFLAGS += $(if $(call rustc-min-version,109800),,-Zllvm_module_flag=uwtable:u32:2:max)
endif
ifeq ($(CONFIG_ARM64_BTI_KERNEL),y)
KBUILD_RUSTFLAGS += -Zbranch-protection=bti,pac-ret
else ifeq ($(CONFIG_ARM64_PTR_AUTH_KERNEL),y)
KBUILD_RUSTFLAGS += -Zbranch-protection=pac-ret
endif
ifeq ($(CONFIG_SHADOW_CALL_STACK), y)
KBUILD_RUSTFLAGS += -Zfixed-x18
endif
`)
	features := "arbitrary_self_types,lint_reasons,used_with_arg"
	if pinInit {
		features = "asm_const,asm_goto,arbitrary_self_types,lint_reasons,offset_of_nested,raw_ref_op,used_with_arg"
	}
	writeRustProfileFixture(t, root, "scripts/Makefile.build", "rust_allowed_features := "+features+"\n")
	writeRustProfileFixture(t, root, "scripts/Makefile.debug", `
ifdef CONFIG_DEBUG_INFO_DWARF5
DEBUG_RUSTFLAGS += -Zdwarf-version=5
endif
ifdef CONFIG_DEBUG_INFO_REDUCED
DEBUG_RUSTFLAGS += -Cdebuginfo=1
else
DEBUG_RUSTFLAGS += -Cdebuginfo=2
endif
`)
	writeRustProfileFixture(t, root, "scripts/generate_rust_target.rs", `
const RUSTC_VERSION: &str = "CONFIG_RUSTC_VERSION";
fn main() {
    let cfg = KernelConfig::from_stdin();
    if cfg.has("ARM64") {
        panic!("arm64 uses the builtin rustc aarch64-unknown-none target");
    } else if cfg.has("X86_64") {
        if cfg.rustc_version_atleast(1, 98, 0) {
        } else if cfg.rustc_version_atleast(1, 86, 0) {
        }
        if cfg.rustc_version_atleast(1, 91, 0) {}
    }
}
`)
	rustMake := `
obj-$(CONFIG_RUST) += core.o compiler_builtins.o ffi.o
obj-$(CONFIG_RUST) += helpers/helpers.o
obj-$(CONFIG_RUST) += bindings.o kernel.o
obj-$(CONFIG_RUST) += uapi.o
obj-$(CONFIG_RUST) += exports.o
redirect-intrinsics := __addsf3 __multi3
ifneq ($(or $(CONFIG_ARM64),$(and $(CONFIG_RISCV),$(CONFIG_64BIT))),)
redirect-intrinsics += __ashrti3 __ashlti3 __lshrti3
endif
core-edition := $(if $(call rustc-min-version,108700),2024,2021)
cmd_rustc_procmacro = $(RUSTC) --crate-type proc-macro
$(obj)/core.o: FORCE
$(obj)/compiler_builtins.o: FORCE
$(obj)/ffi.o: FORCE
$(obj)/bindings.o: FORCE
$(obj)/uapi.o: FORCE
$(obj)/kernel.o: FORCE
`
	if pinInit {
		rustMake = strings.Replace(rustMake, "bindings.o kernel.o", "bindings.o pin_init.o kernel.o", 1)
		rustMake = strings.Replace(rustMake,
			"cmd_rustc_procmacro = $(RUSTC) --crate-type proc-macro",
			"cmd_rustc_procmacro = $(RUSTC) --crate-type proc-macro \\\n @$(objtree)/include/generated/rustc_cfg",
			1,
		)
		rustMake += `
libpin_init_internal_name := libpin_init_internal.so
$(obj)/pin_init.o: FORCE
always-$(subst y,$(CONFIG_RUST),$(CONFIG_JUMP_LABEL)) += kernel/generated_arch_static_branch_asm.rs
always-$(subst y,$(CONFIG_RUST),$(CONFIG_BUG)) += kernel/generated_arch_warn_asm.rs kernel/generated_arch_reachable_asm.rs
`
	}
	writeRustProfileFixture(t, root, "rust/Makefile", rustMake)
	for _, path := range []string{
		"rust/bindgen_parameters",
		"rust/bindings/bindings_helper.h",
		"rust/uapi/uapi_helper.h",
		"rust/helpers/helpers.c",
		"rust/exports.c",
		"rust/macros/lib.rs",
		"rust/macros/quote.rs",
		"rust/compiler_builtins.rs",
		"rust/ffi.rs",
		"rust/build_error.rs",
		"rust/bindings/lib.rs",
		"rust/uapi/lib.rs",
		"rust/kernel/lib.rs",
	} {
		writeRustProfileFixture(t, root, path, "")
	}
	if pinInit {
		for _, path := range []string{
			"rust/pin-init/src/lib.rs",
			"rust/pin-init/internal/src/lib.rs",
			"rust/kernel/generated_arch_static_branch_asm.rs.S",
			"rust/kernel/generated_arch_warn_asm.rs.S",
			"rust/kernel/generated_arch_reachable_asm.rs.S",
		} {
			writeRustProfileFixture(t, root, path, "")
		}
	}
	return root
}

func writeRustProfileFixture(t *testing.T, root, path, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(strings.TrimLeft(content, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
}

func hasRustProfileCrate(profile *RustProfile, name string) bool {
	return rustProfileCrate(profile, name).Name != ""
}

func rustProfileCrate(profile *RustProfile, name string) RustCrateProfile {
	for _, crate := range profile.Crates {
		if crate.Name == name {
			return crate
		}
	}
	return RustCrateProfile{}
}

func rustRuntimePaths(profile *RustProfile) []string {
	paths := make([]string, len(profile.RuntimeObjects))
	for i, object := range profile.RuntimeObjects {
		paths[i] = object.Path
	}
	return paths
}

func hasRustConditionalFlag(conditions []RustConditionalFlags, config, flag string) bool {
	for _, condition := range conditions {
		if condition.Config != config {
			continue
		}
		for _, got := range condition.Flags {
			if got == flag {
				return true
			}
		}
	}
	return false
}

func rustProfileConditional(profile *RustProfile, config string) RustConditionalFlags {
	index := rustProfileConditionalIndex(profile, config)
	if index < 0 {
		return RustConditionalFlags{}
	}
	return profile.TargetFlags.Conditional[index]
}

func rustProfileConditionalIndex(profile *RustProfile, config string) int {
	for i, condition := range profile.TargetFlags.Conditional {
		if condition.Config == config {
			return i
		}
	}
	return -1
}

func containsRustProfileValue(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func countRustProfileValue(values []string, wanted string) int {
	count := 0
	for _, value := range values {
		if value == wanted {
			count++
		}
	}
	return count
}

func evaluateRustCrateFlags(t *testing.T, profile *RustProfile, crate RustCrateProfile, config map[string]string, version string) []string {
	t.Helper()
	flags, predicates := evaluateRustTargetFlags(profile, config)
	filtered := flags[:0]
	for _, flag := range flags {
		if !containsRustProfileValue(crate.SkipFlags, flag) {
			filtered = append(filtered, flag)
		}
	}
	filtered = append(filtered, crate.Flags...)
	predicates = append(predicates, crate.VersionPredicates...)
	return applyRustProfilePredicates(t, filtered, predicates, version)
}

func evaluateRustTargetFlags(profile *RustProfile, config map[string]string) ([]string, []RustVersionPredicate) {
	flags := append([]string(nil), profile.CommonFlags.Always...)
	flags = append(flags, profile.TargetFlags.Always...)
	predicates := append([]RustVersionPredicate(nil), profile.CommonFlags.VersionPredicates...)
	predicates = append(predicates, profile.TargetFlags.VersionPredicates...)
	for _, condition := range profile.TargetFlags.Conditional {
		if config[condition.Config] == condition.Equals {
			flags = append(flags, condition.Flags...)
			predicates = append(predicates, condition.VersionPredicates...)
		} else {
			flags = append(flags, condition.ElseFlags...)
		}
	}
	return flags, predicates
}

func applyRustProfilePredicates(t *testing.T, flags []string, predicates []RustVersionPredicate, version string) []string {
	t.Helper()
	out := append([]string(nil), flags...)
	for _, predicate := range predicates {
		matches := rustProfileVersionAtLeast(t, version, predicate.AtLeast)
		add, remove := predicate.ElseAdd, predicate.ElseRemove
		if matches {
			add, remove = predicate.Add, predicate.Remove
		}
		filtered := out[:0]
		for _, flag := range out {
			if !containsRustProfileValue(remove, flag) {
				filtered = append(filtered, flag)
			}
		}
		out = append(filtered, add...)
	}
	return out
}

func rustProfileVersionAtLeast(t *testing.T, version, minimum string) bool {
	t.Helper()
	parse := func(raw string) [3]int {
		parts := strings.Split(raw, ".")
		if len(parts) != 3 {
			t.Fatalf("version %q does not use MAJOR.MINOR.PATCH", raw)
		}
		var out [3]int
		for i, part := range parts {
			value, err := strconv.Atoi(part)
			if err != nil {
				t.Fatalf("parse version %q: %v", raw, err)
			}
			out[i] = value
		}
		return out
	}
	got := parse(version)
	want := parse(minimum)
	for i := range got {
		if got[i] != want[i] {
			return got[i] > want[i]
		}
	}
	return true
}
