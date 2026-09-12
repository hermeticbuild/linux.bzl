"""Public providers returned by Bazel-native Linux build rules."""

visibility("//...")

LinuxKernelInfo = provider(
    doc = "Outputs and metadata for one configured Linux kernel.",
    fields = {
        "arch": "File containing the exact source-derived Linux ARCH followed by a newline.",
        "version": "Upstream Linux source version.",
        "kernel_release": "File containing the resolved kernel release.",
        "image": "Architecture boot image File.",
        "vmlinux": "Uncompressed vmlinux File.",
        "config": "Resolved kernel configuration File.",
        "system_map": "System.map File.",
    },
)

LinuxModuleSdkInfo = provider(
    doc = "Configured Kbuild source, object tree, probes, and toolsets consumed by external modules.",
    fields = {
        "auto_conf": "Resolved include/config/auto.conf File.",
        "auto_conf_cmd": "Resolved include/config/auto.conf.cmd File.",
        "autoconf": "Resolved include/generated/autoconf.h File.",
        "config": "Resolved kernel .config File.",
        "host_action_args": "Configured linux-kbuild-* host action argv by role.",
        "host_action_environments": "Exact configured host action environment by tool role.",
        "host_action_requirements": "Exact configured host execution requirements by tool role.",
        "host_companion_tools": "Typed FilesToRunProvider companions required by each host tool role.",
        "host_deps": "Configured host dependency TreeArtifact used by Kbuild actions.",
        "host_execution_platform": "Execution platform label that selected the host compiler toolchain.",
        "host_kconfig_probe_results": "Replayed host-scoped source-derived Kconfig capability results.",
        "host_probe_results": "Replayed host compiler capability results.",
        "host_probe_runner": "Exact host-configured proberun FilesToRunProvider selected by this kernel.",
        "host_recipe_runner": "Exact host-configured mapdirectoryrecipe FilesToRunProvider selected by this kernel.",
        "host_tool_files": "Exact configured host tools, mapped-action helpers, and script runtime by role.",
        "host_toolchain_files": "Complete host compiler, generator, helper, and runtime closure.",
        "host_toolset_identity": "TreeArtifact containing the exact host toolset identity marker.",
        "host_toolset_anchors": "Stable root IDs mapped to typed host toolset anchor Files.",
        "host_toolset_manifest": "Identity-bound host Kbuild toolset manifest File.",
        "kbuild": "Root Linux Kbuild/Makefile File.",
        "kernel_key": "Configured kernel/toolset identity used to reject cross-kernel module dependencies.",
        "kernel_release": "File containing the configured kernel release.",
        "libelf_compile_flags": "Kbuild LIBELF_FLAGS using the configured host dependency tree.",
        "libelf_link_flags": "Kbuild LIBELF_LIBS using the configured host dependency tree.",
        "make_vars": "Configured module Make command-line variables.",
        "rust_source_files": "Selected Rust standard-library source depset.",
        "rust_source_root": "Canonical Rust standard-library source root, or an empty string.",
        "rustc_cfg": "Resolved include/generated/rustc_cfg File.",
        "sdk": "Execution-time kernel SDK TreeArtifact.",
        "source": "Depset of declared kernel source Files available to mapped external actions.",
        "source_root": "Root Kconfig File anchoring source paths.",
        "target_action_args": "Configured linux-kbuild-* target action argv by role.",
        "target_action_environments": "Exact configured target action environment by tool role.",
        "target_action_requirements": "Exact configured target execution requirements by tool role.",
        "target_companion_tools": "Typed FilesToRunProvider companions required by each target tool role.",
        "target_execution_platform": "Execution platform label that selected the target compiler toolchain.",
        "target_kconfig_probe_results": "Replayed target-scoped source-derived Kconfig capability results.",
        "target_probe_results": "Replayed target compiler capability results.",
        "target_probe_runner": "Exact target-configured proberun FilesToRunProvider selected by this kernel.",
        "target_recipe_runner": "Exact target-configured mapdirectoryrecipe FilesToRunProvider selected by this kernel.",
        "target_tool_files": "Exact configured target tools, mapped-action helpers, and script runtime by role.",
        "target_toolchain_files": "Complete target compiler, helper, and runtime closure.",
        "target_toolset_identity": "TreeArtifact containing the exact target toolset identity marker.",
        "target_toolset_anchors": "Stable root IDs mapped to typed target toolset anchor Files.",
        "target_toolset_manifest": "Identity-bound target Kbuild toolset manifest File.",
        "version": "Upstream Linux source version.",
    },
)

LinuxModuleInfo = provider(
    doc = "Private metadata for one externally built Linux module.",
    fields = {
        "kernel_key": "Identity of the LinuxModuleSdkInfo used for this module.",
        "module_symvers": "Module.symvers File containing this module's exports.",
    },
)

LinuxModuleTreeInfo = provider(
    doc = "Private manifest-backed view of configured in-tree Linux modules.",
    fields = {
        "manifest": "Newline-delimited canonical paths for the .ko files in tree.",
        "tree": "TreeArtifact containing configured in-tree .ko files.",
    },
)

# Private carrier used by the repository facade.  Keeping the complete family
# behind one configured target is what gives equivalent nodes one Bazel action
# owner; the public root and variant labels only select an existing payload.
LinuxMappedKernelFamilyInfo = provider(
    doc = "Private map from image-family variant names to public kernel provider payloads.",
    fields = {
        "variants": "Dictionary from variant name to a struct containing DefaultInfo, LinuxKernelInfo, LinuxModuleSdkInfo, LinuxModuleTreeInfo, and OutputGroupInfo.",
    },
)
