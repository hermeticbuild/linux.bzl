"""Macros for source-tree-specific arm64 Linux host tools."""

load("@rules_cc//cc:defs.bzl", "cc_binary")
load(":common_host_tools.bzl", "linux_common_host_tools")
load(":linux_objects.bzl", "linux_arm64_generated_headers")
load(":source_utils.bzl", "package_label", "source_label")

def linux_arm64_host_tools(
        name,
        config,
        source_repo,
        target_prefix = None,
        source_root = None,
        source_tree = None,
        env = None,
        visibility = None):
    """Defines the source-tree-specific host tools required by arm64 native builds."""
    if env == None:
        env = {
            "ARCH": "arm64",
            "SRCARCH": "arm64",
        }
    common = linux_common_host_tools(
        name = name,
        env = env,
        source_repo = source_repo,
        source_root = source_root,
        source_tree = source_tree,
        target_prefix = target_prefix,
        visibility = visibility,
    )
    if target_prefix == None:
        target_prefix = name

    vdsomunge_tool = target_prefix + "_vdsomunge_tool"
    relacheck_tool = target_prefix + "_relacheck_tool"
    generated_headers = target_prefix + "_arm64_generated_headers"

    cc_binary(
        name = vdsomunge_tool,
        srcs = [source_label(source_repo, "arch/arm/vdso/vdsomunge.c")],
        visibility = visibility,
    )

    cc_binary(
        name = relacheck_tool,
        srcs = [source_label(source_repo, "arch/arm64/kernel/pi/relacheck.c")],
        visibility = visibility,
    )

    linux_arm64_generated_headers(
        name = generated_headers,
        asm_offsets_c = source_label(source_repo, "arch/arm64/kernel/asm-offsets.c"),
        bounds_c = source_label(source_repo, "kernel/bounds.c"),
        config = config,
        cpucaps = source_label(source_repo, "arch/arm64/tools/cpucaps"),
        hyp_constants_c = source_label(source_repo, "arch/arm64/kvm/hyp/hyp-constants.c"),
        rq_offsets_c = source_label(source_repo, "kernel/sched/rq-offsets.c"),
        source_root = common.source_root,
        source_tree = common.source_tree,
        syscall_32_tbl = source_label(source_repo, "arch/arm64/tools/syscall_32.tbl"),
        syscall_64_tbl = source_label(source_repo, "arch/arm64/tools/syscall_64.tbl"),
        sysreg = source_label(source_repo, "arch/arm64/tools/sysreg"),
        vdsomunge = ":" + vdsomunge_tool,
        visibility = visibility,
    )

    return struct(
        asn1_compiler = common.asn1_compiler,
        generated_headers = package_label(generated_headers),
        kallsyms_tool = common.kallsyms_tool,
        probe_config = common.probe_config,
        relacheck_tool = package_label(relacheck_tool),
        source_asn1_compiler = common.source_asn1_compiler,
        source_label_package = common.source_label_package,
        source_repo = source_repo,
        source_root = common.source_root,
        source_tree = common.source_tree,
        sorttable_tool = common.sorttable_tool,
        vdsomunge_tool = package_label(vdsomunge_tool),
    )
