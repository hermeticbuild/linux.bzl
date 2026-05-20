"""Common source-tree-specific host tools for Linux builds."""

load("@rules_cc//cc:defs.bzl", "cc_binary")
load(":compact_generator.bzl", "linux_probe_config")
load(":source_utils.bzl", "package_label", "source_label", "source_label_package")

def linux_common_host_tools(
        name,
        source_repo,
        target_prefix = None,
        source_root = None,
        source_tree = None,
        env = None,
        visibility = None):
    """Defines host tools shared by all native Linux architecture builds."""
    if target_prefix == None:
        target_prefix = name
    if source_root == None:
        source_root = source_label(source_repo, "Kconfig")
    if source_tree == None:
        source_tree = [source_label(source_repo, "all_files")]
    if env == None:
        env = {}

    probe_config = target_prefix + "_linux_probe"
    asn1_compiler = target_prefix + "_asn1_compiler_tool"
    kallsyms_tool = target_prefix + "_kallsyms_tool"
    sorttable_tool = target_prefix + "_sorttable_tool"

    linux_probe_config(
        name = probe_config,
        env = env,
        visibility = visibility,
    )

    cc_binary(
        name = asn1_compiler,
        srcs = [source_label(source_repo, "scripts/asn1_compiler.c")],
        visibility = visibility,
        deps = [source_label(source_repo, "linux_headers_cc")],
    )

    cc_binary(
        name = kallsyms_tool,
        srcs = [source_label(source_repo, "scripts/kallsyms.c")],
        visibility = visibility,
        deps = [source_label(source_repo, "scripts_headers_cc")],
    )

    cc_binary(
        name = sorttable_tool,
        srcs = [source_label(source_repo, "scripts/sorttable.c")],
        visibility = visibility,
        deps = [
            source_label(source_repo, "scripts_headers_cc"),
            source_label(source_repo, "tools_headers_cc"),
        ],
    )

    return struct(
        asn1_compiler = package_label(asn1_compiler),
        kallsyms_tool = package_label(kallsyms_tool),
        probe_config = package_label(probe_config),
        sorttable_tool = package_label(sorttable_tool),
        source_asn1_compiler = package_label(asn1_compiler),
        source_label_package = source_label_package(source_repo),
        source_repo = source_repo,
        source_root = source_root,
        source_tree = source_tree,
    )
