"""Common source-tree-specific host tools for Linux builds."""

load("@rules_bison//bison:bison.bzl", "bison")
load("@rules_cc//cc:defs.bzl", "cc_binary")
load("@rules_cc//cc/common:cc_common.bzl", "cc_common")
load("@rules_cc//cc/common:cc_info.bzl", "CcInfo")
load("@rules_flex//flex:flex.bzl", "flex")
load(":kconfig.bzl", "KconfigInfo")
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
        visibility = None):
    """Defines host tools shared by all native Linux architecture builds."""
    if target_prefix == None:
        target_prefix = name
    if source_root == None:
        source_root = source_label(source_repo, "Kconfig")
    if source_tree == None:
        source_tree = [source_label(source_repo, "all_files")]
    asn1_compiler = target_prefix + "_asn1_compiler_tool"
    genksyms_tool = target_prefix + "_genksyms_tool"
    kallsyms_tool = target_prefix + "_kallsyms_tool"
    resolve_btfids_tool = target_prefix + "_resolve_btfids_tool"

    genksyms_dir = target_prefix + ".genksyms"
    genksyms_parser = genksyms_dir + "/parse.tab"
    genksyms_lexer = genksyms_dir + "/lex.lex"
    parse_y = source_label(source_repo, "scripts/genksyms/parse.y")
    lex_l = source_label(source_repo, "scripts/genksyms/lex.l")

    bison(
        name = genksyms_parser,
        src = parse_y,
        bison_options = [
            "-t",
            "-l",
        ],
        visibility = visibility,
    )

    flex(
        name = genksyms_lexer,
        src = lex_l,
        flex_options = [
            "-L",
        ],
        visibility = visibility,
    )

    cc_binary(
        name = genksyms_tool,
        includes = [genksyms_dir],
        srcs = [
            source_label(source_repo, "scripts/genksyms/genksyms.c"),
            package_label(genksyms_lexer),
            package_label(genksyms_parser),
        ],
        visibility = visibility,
        deps = [source_label(source_repo, "genksyms_headers_cc")],
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
        genksyms_tool = package_label(genksyms_tool),
        kallsyms_tool = package_label(kallsyms_tool),
        resolve_btfids_tool = package_label(resolve_btfids_tool),
        source_asn1_compiler = package_label(asn1_compiler),
        source_label_package = source_label_package(source_repo),
        source_repo = source_repo,
        source_root = source_root,
        source_tree = source_tree,
    )

def _linux_sorttable_config_impl(ctx):
    config = ctx.attr.config[KconfigInfo].config_flags

    # ftrace skips runtime sorting when this option is enabled, so sorttable
    # must sort __mcount_loc before the kernel boots.
    defines = ["MCOUNT_SORT_ENABLED"] if config.get("CONFIG_BUILDTIME_MCOUNT_SORT") == "y" else []
    return [CcInfo(compilation_context = cc_common.create_compilation_context(
        defines = depset(defines),
    ))]

linux_sorttable_config = rule(
    implementation = _linux_sorttable_config_impl,
    attrs = {
        "config": attr.label(mandatory = True, providers = [KconfigInfo]),
    },
    provides = [CcInfo],
    doc = "Derives sorttable host compiler defines from a resolved kernel config.",
)

def linux_sorttable(name, config, source_repo, visibility = None):
    """Builds sorttable for one kernel config, including overlay variants."""
    config_target = name + "_config"
    linux_sorttable_config(
        name = config_target,
        config = config,
        visibility = visibility,
    )
    cc_binary(
        name = name,
        srcs = [source_label(source_repo, "scripts/sorttable.c")],
        visibility = visibility,
        deps = [
            ":" + config_target,
            Label("@elfutils//:elf"),
            source_label(source_repo, "scripts_headers_cc"),
            source_label(source_repo, "tools_headers_cc"),
        ],
    )
