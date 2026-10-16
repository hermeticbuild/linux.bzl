"""Examples built from generated Kconfig/Make data and selected Bazel platforms."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("@kernel//rules:execution_tools.bzl", "EXECUTION_TOOLS_TYPE")
load("@kernel//rules:image.bzl", "kbuild_resolved_build")
load("@kernel//rules:rules.bzl", "KernelConfigInfo", "kernel_config")

# Bazel supplies this fixed transition callback signature.
# buildifier: disable=unused-variable
def _platform_impl(settings, attr):
    return {"//command_line_option:platforms": str(attr.platform)}

_platform = transition(implementation = _platform_impl, inputs = [], outputs = ["//command_line_option:platforms"])

def _configured_impl(ctx):
    target = ctx.attr.target[0]
    files = target[OutputGroupInfo].kernel if OutputGroupInfo in target and hasattr(target[OutputGroupInfo], "kernel") else target[DefaultInfo].files
    result = [DefaultInfo(files = files)]
    if KernelConfigInfo in target:
        result.append(target[KernelConfigInfo])
    if OutputGroupInfo in target:
        result.append(target[OutputGroupInfo])
    return result

_configured = rule(implementation = _configured_impl, attrs = {
    "target": attr.label(mandatory = True, cfg = _platform),
    "platform": attr.label(mandatory = True),
    "_allowlist_function_transition": attr.label(default = "@bazel_tools//tools/allowlists/function_transition_allowlist"),
})

def kernel_example(name, requested, platform, overrides = {}, required_options = {}, tags = []):
    kernel_config(
        name = name + "_requested",
        options = "@kernel//options:options",
        base = requested,
        requested = overrides,
        allnoconfig = True,
        tags = ["manual"],
    )
    kbuild_resolved_build(
        name = name + "_unconfigured",
        config = ":" + name + "_requested",
        catalog = "@kernel//:catalog",
        roots = ["vmlinux"],
        required_options = required_options,
        tags = ["manual"],
    )
    _configured(name = name, target = ":" + name + "_unconfigured", platform = platform, tags = tags)
    _configured(name = name + "_config", target = ":" + name + "_requested", platform = platform)

def _config_test_impl(ctx):
    values = ctx.attr.config[KernelConfigInfo].values
    for name, expected in ctx.attr.expected.items():
        actual = values.get(name, "n")
        if actual != expected:
            fail("%s expected %s=%s, resolved %s" % (ctx.label, name, expected, actual))
    script = ctx.actions.declare_file(ctx.label.name + ".sh")
    ctx.actions.write(script, "#!/bin/sh\nexit 0\n", is_executable = True)
    return [DefaultInfo(executable = script)]

example_config_test = rule(implementation = _config_test_impl, test = True, attrs = {
    "config": attr.label(mandatory = True, providers = [KernelConfigInfo]),
    "expected": attr.string_dict(mandatory = True),
})

def _initramfs_impl(ctx):
    execution = ctx.toolchains[EXECUTION_TOOLS_TYPE].execution
    output = ctx.actions.declare_file(ctx.label.name + ".cpio")
    ctx.actions.run(
        executable = execution.python,
        arguments = [ctx.file._writer.path, ctx.file.motd.path, output.path],
        inputs = depset([ctx.file._writer, ctx.file.motd], transitive = [execution.files]),
        tools = execution.tools,
        outputs = [output],
        mnemonic = "ExampleInitramfs",
        env = {"PYTHONNOUSERSITE": "1"},
    )
    return [DefaultInfo(files = depset([output]))]

example_initramfs = rule(implementation = _initramfs_impl, toolchains = [EXECUTION_TOOLS_TYPE], attrs = {
    "motd": attr.label(mandatory = True, allow_single_file = True),
    "_writer": attr.label(default = "//:initramfs.py", allow_single_file = True),
})

def _btf_capability_test_impl(ctx):
    env = analysistest.begin(ctx)
    asserts.expect_failure(env, "Required kernel option DEBUG_INFO_BTF=y resolved to n")
    return analysistest.end(env)

btf_capability_test = analysistest.make(
    _btf_capability_test_impl,
    expect_failure = True,
)
