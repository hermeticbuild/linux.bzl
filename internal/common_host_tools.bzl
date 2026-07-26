"""Common source-tree-specific host tools for Linux builds."""

load("@rules_cc//cc:defs.bzl", "cc_binary")
load(":compact_generator.bzl", "linux_probe_config")
load(":source_utils.bzl", "package_label", "source_label", "source_label_package", "source_labels")

visibility("//...")

_RESOLVE_BTFIDS_SRCS = [
    "tools/bpf/resolve_btfids/main.c",
    "tools/lib/rbtree.c",
    "tools/lib/str_error_r.c",
    "tools/lib/zalloc.c",
    "tools/lib/subcmd/exec-cmd.c",
    "tools/lib/subcmd/help.c",
    "tools/lib/subcmd/pager.c",
    "tools/lib/subcmd/parse-options.c",
    "tools/lib/subcmd/run-command.c",
    "tools/lib/subcmd/sigchain.c",
    "tools/lib/subcmd/subcmd-config.c",
]

_RESOLVE_BTFIDS_COPTS = [
    "-D_GNU_SOURCE",
    "-D_LARGEFILE64_SOURCE",
    "-D_FILE_OFFSET_BITS=64",
    "-ffunction-sections",
    "-fdata-sections",
    "-Wno-deprecated-declarations",
    "-Wno-implicit-function-declaration",
    "-Wno-pointer-sign",
    "-Wno-unused-function",
    "-Wno-unused-parameter",
    "-Wno-unused-variable",
]

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
    resolve_btfids_tool = target_prefix + "_resolve_btfids_tool"
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
            Label("@elfutils//:elf"),
            source_label(source_repo, "scripts_headers_cc"),
            source_label(source_repo, "tools_headers_cc"),
        ],
    )

    cc_binary(
        name = resolve_btfids_tool,
        srcs = source_labels(source_repo, _RESOLVE_BTFIDS_SRCS),
        copts = _RESOLVE_BTFIDS_COPTS,
        linkopts = ["-Wl,--gc-sections"],
        linkstatic = True,
        visibility = visibility,
        deps = [
            Label("@elfutils//:elf"),
            Label("@libbpf//:libbpf"),
            source_label(source_repo, "tools_headers_cc"),
        ],
    )

    return struct(
        asn1_compiler = package_label(asn1_compiler),
        kallsyms_tool = package_label(kallsyms_tool),
        probe_config = package_label(probe_config),
        resolve_btfids_tool = package_label(resolve_btfids_tool),
        sorttable_tool = package_label(sorttable_tool),
        source_asn1_compiler = package_label(asn1_compiler),
        source_label_package = source_label_package(source_repo),
        source_repo = source_repo,
        source_root = source_root,
        source_tree = source_tree,
    )
