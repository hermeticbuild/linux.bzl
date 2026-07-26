"""Public providers returned by Bazel-native Linux build rules."""

visibility("//...")

LinuxKernelInfo = provider(
    doc = "Outputs and metadata for one configured Linux kernel.",
    fields = {
        "arch": "Canonical Linux target architecture: x86_64 or aarch64.",
        "version": "Upstream Linux source version.",
        "kernel_release": "File containing the resolved kernel release.",
        "image": "Architecture boot image File.",
        "vmlinux": "Uncompressed vmlinux File.",
        "config": "Resolved kernel configuration File.",
        "system_map": "System.map File.",
    },
)

LinuxRustSdkInfo = provider(
    doc = "Private Rust-for-Linux SDK produced for one configured kernel.",
    fields = {
        "compile_inputs": "Depset of Rust crate metadata, generated sources, and toolchain inputs.",
        "enabled": "Whether CONFIG_RUST is enabled for this kernel.",
        "module_flags": "Rust compiler flags shared by external modules.",
        "objtool": "Configured objtool executable File, or None when objtool is disabled.",
        "objtree": "Execroot-relative object-tree directory used by Rust source includes.",
        "rust_dir": "Execroot-relative directory containing the configured Rust crates.",
        "rustc": "Exact rustc File used to build the SDK, or None when disabled.",
        "rustc_env": "Hermetic environment used to invoke rustc.",
        "rustc_files": "Depset of rustc runtime/toolchain input Files.",
        "rustc_version_runner": "Hermetic rustc version-checking executable File, or None when disabled.",
        "rustc_version": "Exact rustc version used to build the SDK, or an empty string when disabled.",
        "runtime_objects": "Ordered list of Rust runtime object Files folded into vmlinux.o.",
        "target_spec": "Rust target specification File, or None for a built-in target.",
    },
)

LinuxModuleSdkInfo = provider(
    doc = "Private, configuration-specific inputs required to build Linux modules.",
    fields = {
        "arch": "Canonical Linux target architecture.",
        "config": "LinuxConfigInfo for the configured kernel.",
        "generated_headers": "LinuxGeneratedHeadersInfo for the configured kernel.",
        "kernel_key": "Stable identity used to reject cross-kernel module dependencies.",
        "kernel_release": "File containing the configured kernel release.",
        "module_common": "Target module-common.o File.",
        "module_lds": "Preprocessed module linker script File.",
        "module_symvers": "Kernel and in-tree module Module.symvers File.",
        "modules": "Depset of configured in-tree .ko Files.",
        "modules_builtin": "Deterministic modules.builtin File.",
        "modules_builtin_modinfo": "Deterministic modules.builtin.modinfo File.",
        "modules_order": "Deterministic modules.order File.",
        "modpost": "Hermetic host modpost executable File.",
        "source_root": "Kernel source root marker File.",
        "source_tree": "Depset of source inputs needed by external module actions.",
        "srcarch": "Linux SRCARCH value.",
        "rust": "LinuxRustSdkInfo, disabled SDK, or None for kernels without Rust support.",
        "target": "Configured target C compiler/linker context used for external module actions.",
        "target_c_flags": "Configured C flags used to compile external module metadata.",
        "target_link_flags": "Configured target flags used to link external modules.",
        "version": "Upstream Linux source version.",
        "vmlinux": "Uncompressed configured vmlinux File.",
        "vmlinux_object": "Relocatable vmlinux.o consumed by modpost.",
    },
)

LinuxModuleInfo = provider(
    doc = "Private metadata for one externally built Linux module.",
    fields = {
        "kernel_key": "Identity of the LinuxModuleSdkInfo used for this module.",
        "ko": "Final .ko File.",
        "module_symvers": "Module.symvers File containing this module's exports.",
    },
)

LinuxVmlinuxInfo = provider(
    doc = "Private configured-vmlinux inputs used to construct a module SDK.",
    fields = {
        "arch": "Canonical Linux target architecture.",
        "config": "LinuxConfigInfo for the configured kernel.",
        "generated_headers": "LinuxGeneratedHeadersInfo for the configured kernel.",
        "module_common": "Target module-common.o File, or None when modules are disabled.",
        "module_lds": "Preprocessed module linker script File, or None when modules are disabled.",
        "module_objects": "Configured module-root LinuxObjectInfo values.",
        "module_outputs": "Dictionary of module object paths to objtool-processed root object Files.",
        "module_sources": "Dictionary of module object paths to modpost-generated .mod.c Files.",
        "module_symvers": "Module.symvers generated from vmlinux.o and configured modules, or None.",
        "modules_order": "Configured modules.order File, or None when modules are disabled.",
        "modpost": "Hermetic host modpost executable File, or None when modules are disabled.",
        "source_root": "Kernel source root marker File.",
        "source_tree": "Depset of source inputs needed by module preparation actions.",
        "srcarch": "Linux SRCARCH value.",
        "rust": "LinuxRustSdkInfo or None.",
        "vmlinux": "Uncompressed configured vmlinux File.",
        "vmlinux_unstripped": "Unstripped configured vmlinux File used for metadata extraction.",
        "vmlinux_object": "Relocatable vmlinux.o consumed by modpost.",
    },
)
