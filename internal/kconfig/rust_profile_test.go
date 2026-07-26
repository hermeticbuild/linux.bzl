// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package kconfig

import (
	"os"
	"path/filepath"
	"reflect"
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
	_, err := GenerateRustProfile(root, "arm64")
	if err == nil || !strings.Contains(err.Error(), "only x86_64") {
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
KBUILD_RUSTFLAGS += -Zllvm_module_flag=frame-pointer:u32:2:max
endif
`)
	archMake := `
KBUILD_RUSTFLAGS += --target=$(objtree)/scripts/target.json
KBUILD_RUSTFLAGS += -Ctarget-feature=-sse,-sse2,-sse3,-ssse3,-sse4.1,-sse4.2,-avx,-avx2
RETHUNK_RUSTFLAGS := -Zfunction-return=thunk-extern
ifdef CONFIG_X86_KERNEL_IBT
KBUILD_RUSTFLAGS += -Zcf-protection=branch -Cjump-tables=n
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
	features := "arbitrary_self_types,lint_reasons,used_with_arg"
	if pinInit {
		features = "asm_const,asm_goto,arbitrary_self_types,lint_reasons,offset_of_nested,raw_ref_op,used_with_arg"
	}
	writeRustProfileFixture(t, root, "scripts/Makefile.build", "rust_allowed_features := "+features+"\n")
	writeRustProfileFixture(t, root, "scripts/generate_rust_target.rs", `
fn main() {
    let cfg = KernelConfig::from_stdin();
    if cfg.has("X86_64") {}
}
`)
	rustMake := `
obj-$(CONFIG_RUST) += core.o compiler_builtins.o ffi.o
obj-$(CONFIG_RUST) += helpers/helpers.o
obj-$(CONFIG_RUST) += bindings.o kernel.o
obj-$(CONFIG_RUST) += uapi.o
obj-$(CONFIG_RUST) += exports.o
redirect-intrinsics := __addsf3 __multi3
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
