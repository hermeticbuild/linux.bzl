"""Host tools shared by arm, RISC-V, and PowerPC Linux builds."""

load("@rules_cc//cc:defs.bzl", "cc_binary")
load(":common_host_tools.bzl", "linux_common_host_tools")
load(":linux_objects.bzl", "linux_generic_generated_headers")
load(":source_utils.bzl", "package_label", "source_label")

visibility("//...")

_ARCH_INPUTS = {
    "arm": struct(
        asm_offsets = "arch/arm/kernel/asm-offsets.c",
        mach_types = "arch/arm/tools/mach-types",
        syscall_table = "arch/arm/tools/syscall.tbl",
        uts_machine = "armv7l",
    ),
    "powerpc": struct(
        asm_offsets = "arch/powerpc/kernel/asm-offsets.c",
        mach_types = None,
        syscall_table = "arch/powerpc/kernel/syscalls/syscall.tbl",
        uts_machine = "ppc64le",
    ),
    "riscv": struct(
        asm_offsets = "arch/riscv/kernel/asm-offsets.c",
        mach_types = None,
        syscall_table = "scripts/syscall.tbl",
        uts_machine = "riscv64",
    ),
}

def _generated_headers(
        name,
        arch,
        config,
        source_repo,
        generated_header_family_ids,
        source_root,
        source_tree,
        vdsomunge,
        visibility):
    inputs = _ARCH_INPUTS[arch]
    kwargs = {
        "name": name,
        "arch": arch,
        "asm_offsets_c": source_label(source_repo, inputs.asm_offsets),
        "bounds_c": source_label(source_repo, "kernel/bounds.c"),
        "config": config,
        "family_content_ids": generated_header_family_ids,
        "rq_offsets_c": source_label(source_repo, "rq_offsets_c"),
        "source_root": source_root,
        "source_tree": source_tree,
        "syscall_tbl": source_label(source_repo, inputs.syscall_table),
        "uts_machine": inputs.uts_machine,
        "visibility": visibility,
    }
    if vdsomunge:
        kwargs["vdsomunge"] = vdsomunge
    if inputs.mach_types:
        kwargs["mach_types"] = source_label(source_repo, inputs.mach_types)
    linux_generic_generated_headers(**kwargs)

def _linux_generic_host_tools(
        name,
        arch,
        config,
        source_repo,
        generated_header_family_ids,
        target_prefix = None,
        source_root = None,
        source_tree = None,
        visibility = None):
    common = linux_common_host_tools(
        name = name,
        source_repo = source_repo,
        source_root = source_root,
        source_tree = source_tree,
        target_prefix = target_prefix,
        visibility = visibility,
    )
    if target_prefix == None:
        target_prefix = name
    generated_headers = target_prefix + "_" + arch + "_generated_headers"
    vdsomunge_tool = None
    if arch == "arm":
        vdsomunge_tool = target_prefix + "_arm_vdsomunge_tool"
        cc_binary(
            name = vdsomunge_tool,
            srcs = [source_label(source_repo, "arch/arm/vdso/vdsomunge.c")],
            deps = [Label("@elfutils//:elf")],
            visibility = visibility,
        )
    _generated_headers(
        name = generated_headers,
        arch = arch,
        config = config,
        source_repo = source_repo,
        generated_header_family_ids = generated_header_family_ids,
        source_root = common.source_root,
        source_tree = common.source_tree,
        vdsomunge = ":" + vdsomunge_tool if vdsomunge_tool else None,
        visibility = visibility,
    )
    result = {
        "asn1_compiler": common.asn1_compiler,
        "generated_headers": package_label(generated_headers),
        "kallsyms_tool": common.kallsyms_tool,
        "resolve_btfids_tool": common.resolve_btfids_tool,
        "source_asn1_compiler": common.source_asn1_compiler,
        "source_label_package": common.source_label_package,
        "source_repo": source_repo,
        "source_root": common.source_root,
        "source_tree": common.source_tree,
        "sorttable_tool": common.sorttable_tool,
    }
    if vdsomunge_tool:
        result["vdsomunge_tool"] = package_label(vdsomunge_tool)
    return struct(**result)

def _linux_generic_configured_host_tools(
        name,
        arch,
        config,
        shared,
        source_repo,
        generated_header_family_ids,
        source_root = None,
        source_tree = None,
        visibility = None):
    if source_root == None:
        source_root = shared.source_root
    if source_tree == None:
        source_tree = shared.source_tree
    generated_headers = name + "_" + arch + "_generated_headers"
    _generated_headers(
        name = generated_headers,
        arch = arch,
        config = config,
        source_repo = source_repo,
        generated_header_family_ids = generated_header_family_ids,
        source_root = source_root,
        source_tree = source_tree,
        vdsomunge = shared.vdsomunge_tool if arch == "arm" else None,
        visibility = visibility,
    )
    result = {
        "asn1_compiler": shared.asn1_compiler,
        "generated_headers": package_label(generated_headers),
        "kallsyms_tool": shared.kallsyms_tool,
        "resolve_btfids_tool": shared.resolve_btfids_tool,
        "source_asn1_compiler": shared.source_asn1_compiler,
        "source_label_package": shared.source_label_package,
        "source_repo": shared.source_repo,
        "source_root": source_root,
        "source_tree": source_tree,
        "sorttable_tool": shared.sorttable_tool,
    }
    if arch == "arm":
        result["vdsomunge_tool"] = shared.vdsomunge_tool
    return struct(**result)

def linux_arm_host_tools(**kwargs):
    return _linux_generic_host_tools(arch = "arm", **kwargs)

def linux_arm_configured_host_tools(**kwargs):
    return _linux_generic_configured_host_tools(arch = "arm", **kwargs)

def linux_powerpc_host_tools(**kwargs):
    return _linux_generic_host_tools(arch = "powerpc", **kwargs)

def linux_powerpc_configured_host_tools(**kwargs):
    return _linux_generic_configured_host_tools(arch = "powerpc", **kwargs)

def linux_riscv_host_tools(**kwargs):
    return _linux_generic_host_tools(arch = "riscv", **kwargs)

def linux_riscv_configured_host_tools(**kwargs):
    return _linux_generic_configured_host_tools(arch = "riscv", **kwargs)
