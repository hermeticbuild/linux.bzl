"""Resolve compiler flags in analysis, using Bazel's registered LLVM toolchains."""

load("@rules_cc//cc:action_names.bzl", "ACTION_NAMES")
load("@rules_cc//cc:find_cc_toolchain.bzl", "CC_TOOLCHAIN_TYPE", "find_cc_toolchain", "use_cc_toolchain")
load("@rules_cc//cc/common:cc_common.bzl", "cc_common")
load(":expressions.bzl", "evaluate", "program", "source_index")
load(":flag_program.bzl", "FLAG_PROGRAM")
load(":rules.bzl", "KernelConfigInfo")

KernelToolchainInfo = provider(doc = "Kernel Toolchain Info.", fields = ["file", "files", "identity", "tools", "variables", "sources", "host_cc", "host_features"])

def _impl(ctx):
    config = ctx.attr._config[KernelConfigInfo]
    cc = find_cc_toolchain(ctx)
    host_info = ctx.exec_groups["host"].toolchains[CC_TOOLCHAIN_TYPE]
    host = host_info.cc if hasattr(host_info, "cc") else host_info
    features = cc_common.configure_features(
        ctx = ctx,
        cc_toolchain = cc,
        requested_features = ctx.features,
        unsupported_features = ctx.disabled_features,
    )
    host_features = cc_common.configure_features(
        ctx = ctx,
        cc_toolchain = host,
        requested_features = ctx.features,
        unsupported_features = ctx.disabled_features,
    )
    tools = {
        "CC": cc_common.get_tool_for_action(feature_configuration = features, action_name = ACTION_NAMES.c_compile),
        "HOSTCC": cc_common.get_tool_for_action(feature_configuration = host_features, action_name = ACTION_NAMES.c_compile),
        "HOSTCXX": cc_common.get_tool_for_action(feature_configuration = host_features, action_name = ACTION_NAMES.cpp_compile),
    }
    sources = source_index([f.path for f in ctx.attr._source_tree[DefaultInfo].files.to_list()])
    sources["__capabilities__"] = FLAG_PROGRAM["capabilities"]
    source = ctx.file._anchor.dirname
    initial = {
        "__config__": config.make_values,
        "ARCH": config.arch,
        "SRCARCH": config.architecture.srcarch,
        "srctree": source,
        "srcroot": source,
        "objtree": "__object__",
        "LLVM": "1" if config.values.get("CC_IS_CLANG") == "y" else "",
        "LLVM_IAS": "1" if config.values.get("AS_IS_LLVM") == "y" else "0",
        "building_out_of_srctree": "1",
        "need-config": "1",
        "need-compiler": "1",
        "KCONFIG_CONFIG": "__config__",
        "CONFIG_SHELL": "/bin/sh",
    }
    variables = program(FLAG_PROGRAM["global"], initial, sources).variables
    observed = {
        name: evaluate([{"op": "reference", "name": [{"op": "literal", "value": name}]}], variables, sources)
        for name in ["KBUILD_CFLAGS", "KBUILD_AFLAGS", "KBUILD_CPPFLAGS", "KBUILD_LDFLAGS", "KBUILD_HOSTCFLAGS", "KBUILD_HOSTLDFLAGS"]
    }
    out = ctx.actions.declare_file(ctx.label.name + ".setup.json")
    ctx.actions.write(out, json.encode({"variables": observed, "tools": tools}))
    return [KernelToolchainInfo(
        file = out,
        files = depset(transitive = [cc.all_files, host.all_files]),
        identity = config.identity,
        tools = tools,
        variables = variables,
        sources = sources,
        host_cc = host,
        host_features = host_features,
    ), DefaultInfo(files = depset([out]))]

kbuild_toolchain_setup = rule(
    implementation = _impl,
    fragments = ["cpp"],
    toolchains = use_cc_toolchain(),
    exec_groups = {"host": exec_group(toolchains = use_cc_toolchain())},
    attrs = {
        "_config": attr.label(default = "//:active_config", providers = [KernelConfigInfo]),
        "_anchor": attr.label(default = "//:source/Makefile", allow_single_file = True),
        "_source_tree": attr.label(default = "//:source_tree"),
    },
)
