"""Baseline assembler semantics supplied by the selected compiler provider."""

load("@rules_cc//cc:action_names.bzl", "ACTION_NAMES")
load("@rules_cc//cc:find_cc_toolchain.bzl", "find_cc_toolchain", "use_cc_toolchain")
load("@rules_cc//cc/common:cc_common.bzl", "cc_common")
load(":architecture.bzl", "KernelArchitectureInfo")
load(":compiler_capabilities_data.bzl", "COMPILER_CAPABILITIES")
load(":compiler_metadata.bzl", "KernelCompilerMetadataInfo")

KernelCompilerCapabilitiesInfo = provider(doc = "Kernel Compiler Capabilities Info.", fields = ["inputs", "checks", "toolchain_id", "make"])

def _impl(ctx):
    cc = find_cc_toolchain(ctx)
    compiler = ctx.attr._compiler[config_common.FeatureFlagInfo].value
    architecture = ctx.attr._architecture[KernelArchitectureInfo]
    metadata = ctx.attr._metadata[KernelCompilerMetadataInfo]
    inputs, checks, make = {}, [], {}
    for fact in COMPILER_CAPABILITIES:
        supplied = (compiler in fact["compilers"] if metadata.capabilities == None else fact.get("input", fact["probe"] + ":" + fact["argument"]) in metadata.capabilities)
        supported = supplied and architecture.srcarch == fact["srcarch"]
        if fact.get("kbuild"):
            if supported:
                make[fact["probe"] + ":" + fact["argument"]] = True
        else:
            inputs[fact["input"]] = ["y" if supported else "n", "tristate"]
        if supported:
            checks.append(fact)
        elif architecture.srcarch == fact["srcarch"]:
            # buildifier: disable=print
            print("WARNING: unresolved assembler capability %s in %s; using fallback n" % (fact["description"], cc.toolchain_id))
    return [KernelCompilerCapabilitiesInfo(inputs = inputs, checks = checks, toolchain_id = cc.toolchain_id, make = make)]

kernel_compiler_capabilities = rule(implementation = _impl, fragments = ["cpp"], toolchains = use_cc_toolchain(), attrs = {
    "_compiler": attr.label(default = "@rules_cc//cc/compiler:compiler", providers = [config_common.FeatureFlagInfo]),
    "_architecture": attr.label(default = "//:architecture", providers = [KernelArchitectureInfo]),
    "_metadata": attr.label(default = "//:compiler_metadata", providers = [KernelCompilerMetadataInfo]),
})

def _check_impl(ctx):
    info = ctx.attr._capabilities[KernelCompilerCapabilitiesInfo]
    cc = find_cc_toolchain(ctx)
    if cc.toolchain_id != info.toolchain_id:
        fail("Capability checks must use the same selected C++ toolchain")
    features = cc_common.configure_features(ctx = ctx, cc_toolchain = cc, requested_features = ctx.features, unsupported_features = ctx.disabled_features)
    variables = cc_common.create_compile_variables(cc_toolchain = cc, feature_configuration = features)
    flags = cc_common.get_memory_inefficient_command_line(feature_configuration = features, action_name = ACTION_NAMES.c_compile, variables = variables)
    objects = []
    for i, fact in enumerate(info.checks):
        source = ctx.actions.declare_file(ctx.label.name + ".checks/%d.S" % i)
        out = ctx.actions.declare_file(ctx.label.name + ".checks/%d.o" % i)
        ctx.actions.write(source, fact.get("source", fact["argument"].replace("\\n", "\n").replace("\\$", "$")))
        ctx.actions.run(
            executable = cc_common.get_tool_for_action(feature_configuration = features, action_name = ACTION_NAMES.c_compile),
            arguments = flags + fact.get("flags", []) + ["-c", "-x", fact.get("language", "assembler"), source.path, "-o", out.path],
            inputs = depset([source], transitive = [cc.all_files]),
            outputs = [out],
            mnemonic = "KernelCapabilityCheck",
        )
        objects.append(out)
    return [DefaultInfo(files = depset(objects))]

kernel_compiler_capabilities_check = rule(implementation = _check_impl, fragments = ["cpp"], toolchains = use_cc_toolchain(), attrs = {
    "_capabilities": attr.label(default = "//:compiler_capabilities", providers = [KernelCompilerCapabilitiesInfo]),
})

def _test_impl(ctx):
    objects = [f for f in ctx.attr.checks[DefaultInfo].files.to_list() if f.extension == "o"]
    aliases = {"compiler-capabilities/%d.o" % i: f for i, f in enumerate(objects)}
    script = ctx.actions.declare_file(ctx.label.name + ".sh")
    ctx.actions.write(script, "#!/bin/sh\nset -eu\n" + "\n".join(['test -s "$TEST_SRCDIR/$TEST_WORKSPACE/%s"' % path for path in aliases]), is_executable = True)
    return [DefaultInfo(executable = script, runfiles = ctx.runfiles(symlinks = aliases))]

kernel_compiler_capabilities_test = rule(implementation = _test_impl, test = True, attrs = {
    "checks": attr.label(mandatory = True),
})
