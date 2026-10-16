"""Resolve compiler flags in analysis, using Bazel's registered LLVM toolchains."""

load("@rules_cc//cc:action_names.bzl", "ACTION_NAMES")
load("@rules_cc//cc:find_cc_toolchain.bzl", "find_cc_toolchain", "use_cc_toolchain")
load("@rules_cc//cc/common:cc_common.bzl", "cc_common")
load(":compiler_capabilities.bzl", "KernelCompilerCapabilitiesInfo")
load(":compiler_metadata.bzl", "KernelCompilerMetadataInfo")
load(":execution_tools.bzl", "EXECUTION_TOOLS_TYPE", "execution_tools")
load(":expressions.bzl", "evaluate", "program", "source_index")
load(":flag_program.bzl", "FLAG_PROGRAM")
load(":host_toolchain.bzl", "KernelHostToolchainInfo")
load(":rules.bzl", "KernelConfigInfo")
load(":source_tree.bzl", "KernelSourceTreeInfo")

KernelToolchainInfo = provider(doc = "Kernel Toolchain Info.", fields = ["file", "files", "identity", "tools", "variables", "sources", "host_cc", "host_features", "architecture", "host_architecture", "driver_tools"])

def _impl(ctx):
    config = ctx.attr._config[KernelConfigInfo]
    cc = find_cc_toolchain(ctx)
    host_info = ctx.attr._host_toolchain[KernelHostToolchainInfo]
    host = host_info.cc
    features = cc_common.configure_features(
        ctx = ctx,
        cc_toolchain = cc,
        requested_features = ctx.features,
        unsupported_features = ctx.disabled_features,
    )
    host_features = host_info.features
    metadata = ctx.attr._metadata[KernelCompilerMetadataInfo]
    if metadata.toolchain_id != cc.toolchain_id:
        fail("Compiler metadata and image actions must use the same selected toolchain")
    tools = dict(metadata.tools)
    driver_tools = []
    for name in metadata.driver_linker_aliases:
        alias = ctx.actions.declare_file(ctx.label.name + ".driver-tools/" + name)
        ctx.actions.symlink(output = alias, target_file = metadata.tool_files["LD"], is_executable = True)
        driver_tools.append(alias)
    if not driver_tools:
        fail("Compiler metadata must declare the linker's driver lookup names")
    tools.update({
        "AR": cc_common.get_tool_for_action(feature_configuration = features, action_name = ACTION_NAMES.cpp_link_static_library),
        "HOSTCC": cc_common.get_tool_for_action(feature_configuration = host_features, action_name = ACTION_NAMES.c_compile),
        "HOSTCXX": cc_common.get_tool_for_action(feature_configuration = host_features, action_name = ACTION_NAMES.cpp_compile),
    })
    execution = execution_tools(ctx)
    for name, command in {"CONFIG_SHELL": "sh", "BASH": "bash", "PYTHON3": "python3", "PERL": "perl", "AWK": "awk", "LEX": "flex", "YACC": "bison", "KGZIP": "gzip", "KBZIP2": "bzip2", "KLZMA": "lzma", "LZOP": "lzop", "XZ": "xz", "ZSTD": "zstd"}.items():
        if command in execution.commands:
            tools[name] = execution.commands[command].path
    prepared = ctx.attr._prepared_source[KernelSourceTreeInfo]
    source = prepared.root.path
    sources = source_index([source + "/" + p for p in prepared.logical])
    sources["__execution_tools__"] = {n: f.path for n, f in execution.commands.items()}
    compile_variables = cc_common.create_compile_variables(cc_toolchain = cc, feature_configuration = features)
    compile_flags = cc_common.get_memory_inefficient_command_line(feature_configuration = features, action_name = ACTION_NAMES.c_compile, variables = compile_variables)
    sources["__compiler_include__"] = metadata.builtin_include
    sources["__host_machine__"] = host_info.architecture.cpu
    sources["__capabilities__"] = dict(FLAG_PROGRAM["capabilities"])
    sources["__capabilities__"].update(ctx.attr._capabilities[KernelCompilerCapabilitiesInfo].make)
    sources["__includes__"] = FLAG_PROGRAM["image_flag_programs"]
    sources["__source_root__"] = source
    initial = {
        "__config__": config.make_values,
        "ARCH": config.arch,
        "SRCARCH": config.architecture.srcarch,
        "SUBARCH": config.architecture.subarch,
        "HEADER_ARCH": config.architecture.header_arch,
        "srctree": source,
        "srcroot": source,
        "objtree": "__object__",
        "LLVM": "1" if config.values.get("CC_IS_CLANG") == "y" else "",
        "LLVM_IAS": "1" if config.values.get("AS_IS_LLVM") == "y" else "0",
        "building_out_of_srctree": "1",
        "need-config": "1",
        "need-compiler": "1",
        "KCONFIG_CONFIG": "__config__",
    }
    initial.update(tools)
    setup_sources = dict(sources)
    setup_sources["__includes__"] = FLAG_PROGRAM["image_setup_programs"]
    variables = program(FLAG_PROGRAM["global"], initial, setup_sources).variables

    # Selected tools are command-line inputs to native Kbuild, whose ordinary
    # Makefile assignments cannot replace them. Bind before derived exports.
    for name, tool in tools.items():
        variables[name] = {"value": tool}
    variables = program(FLAG_PROGRAM["image_utilities"], variables, sources, cells = True).variables
    variables = program(FLAG_PROGRAM["image_version"], variables, sources, cells = True).variables
    variables = program(FLAG_PROGRAM["image_cpp"], variables, sources, cells = True).variables

    link_sources = dict(sources)
    link_sources["__includes__"] = FLAG_PROGRAM["image_link_programs"]
    variables = program(FLAG_PROGRAM["image_link_global"], variables, link_sources, cells = True).variables
    if config.values.get("UML") == "y":
        # UML's USER_OBJS and final executable consume the target libc. Take
        # its sysroot, headers and runtime flags from the selected toolchain.
        link_variables = cc_common.create_link_variables(
            cc_toolchain = cc,
            feature_configuration = features,
            is_static_linking_mode = config.values.get("STATIC_LINK") == "y",
        )
        link_flags = cc_common.get_memory_inefficient_command_line(
            feature_configuration = features,
            action_name = ACTION_NAMES.cpp_link_executable,
            variables = link_variables,
        )
        for name, flags in [("USER_CFLAGS", compile_flags), ("CFLAGS_vmlinux", link_flags)]:
            native = evaluate([{"op": "reference", "name": [{"op": "literal", "value": name}]}], variables, sources)
            if name == "CFLAGS_vmlinux":
                # link-vmlinux.sh word-splits this environment variable. Shell
                # quotes embedded in a variable are literal characters there.
                if any([not flag or any([c in flag for c in [" ", "\t", "\n", "'", '"']]) for flag in flags]):
                    fail("Native UML link script cannot word-split this toolchain argument list: %r" % flags)
                rendered = " ".join(flags)
            else:
                rendered = " ".join(["'" + flag.replace("'", "'\\''") + "'" for flag in flags])
            variables[name] = {"value": native + " " + rendered}
    release = ".".join([evaluate([{"op": "reference", "name": [{"op": "literal", "value": name}]}], variables, sources) for name in ["VERSION", "PATCHLEVEL", "SUBLEVEL"]]) + evaluate([{"op": "reference", "name": [{"op": "literal", "value": "EXTRAVERSION"}]}], variables, sources) + config.make_values.get("CONFIG_LOCALVERSION", "")
    for name, val in {"KERNELRELEASE": release, "KBUILD_BUILD_VERSION": "1", "KBUILD_BUILD_TIMESTAMP": "Thu Jan 1 00:00:00 UTC 1970"}.items():
        variables[name] = {"value": val}
    variables = program(FLAG_PROGRAM["image_exports"], variables, sources, cells = True).variables
    for name in list(tools) + ["LLVM", "LLVM_IAS", "KBUILD_BUILD_VERSION", "KBUILD_BUILD_TIMESTAMP"]:
        variables["__exports__"][name] = True
    observed = {
        name: evaluate([{"op": "reference", "name": [{"op": "literal", "value": name}]}], variables, sources)
        for name in ["KBUILD_CFLAGS", "KBUILD_AFLAGS", "KBUILD_CPPFLAGS", "KBUILD_LDFLAGS", "KBUILD_HOSTCFLAGS", "KBUILD_HOSTLDFLAGS", "LDFLAGS_vmlinux"]
    }
    out = ctx.actions.declare_file(ctx.label.name + ".setup.json")
    ctx.actions.write(out, json.encode({
        "variables": observed,
        "tools": tools,
        "compiler_include": sources["__compiler_include__"],
        "compile_flags": compile_flags,
        "execution_bin": execution.bin_dir,
        "architecture": {"cpu": config.architecture.cpu, "arch": config.architecture.arch, "srcarch": config.architecture.srcarch, "bits": config.architecture.bits},
    }))
    return [KernelToolchainInfo(
        file = out,
        files = depset(driver_tools, transitive = [cc.all_files, host.all_files, execution.files, prepared.files, metadata.files]),
        identity = config.identity,
        tools = tools,
        variables = variables,
        sources = sources,
        host_cc = host,
        host_features = host_features,
        architecture = config.architecture,
        host_architecture = host_info.architecture,
        driver_tools = driver_tools[0].dirname,
    ), DefaultInfo(files = depset([out]))]

kbuild_toolchain_setup = rule(
    implementation = _impl,
    fragments = ["cpp"],
    toolchains = use_cc_toolchain() + [EXECUTION_TOOLS_TYPE],
    attrs = {
        "_capabilities": attr.label(default = "//:compiler_capabilities", providers = [KernelCompilerCapabilitiesInfo]),
        "_host_toolchain": attr.label(default = "//:host_toolchain", providers = [KernelHostToolchainInfo], cfg = "exec"),
        "_metadata": attr.label(default = "//:compiler_metadata", providers = [KernelCompilerMetadataInfo]),
        "_config": attr.label(default = "//:active_config", providers = [KernelConfigInfo]),
        "_anchor": attr.label(default = "//:source/Makefile", allow_single_file = True),
        "_source_tree": attr.label(default = "//:source_tree"),
        "_prepared_source": attr.label(default = "//:prepared_source", providers = [KernelSourceTreeInfo], cfg = "exec"),
    },
)
