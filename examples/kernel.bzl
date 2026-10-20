"""Examples select a platform/toolchain and pass fragments to the measured kernel rule."""

load("@bazel_skylib//lib:shell.bzl", "shell")
load("@rules_python//python:defs.bzl", "py_test")
load("//rules:config.bzl", "kernel_config")
load("//rules:kernel.bzl", "KernelBuildInfo")

# Fixed Bazel transition callback signature.
# buildifier: disable=unused-variable
def _platform_impl(settings, attr):
    return {
        "//command_line_option:platforms": str(attr.platform),
        "//command_line_option:extra_toolchains": [str(attr.cc_toolchain)],
        "//examples:configuration": str(attr.configuration),
    }

_platform = transition(
    implementation = _platform_impl,
    inputs = [],
    outputs = ["//command_line_option:platforms", "//command_line_option:extra_toolchains", "//examples:configuration"],
)

def _configured_impl(ctx):
    target = ctx.attr.target[0]
    result = [DefaultInfo(files = target[DefaultInfo].files)]
    if KernelBuildInfo in target:
        result.extend([target[KernelBuildInfo], target[OutputGroupInfo]])
    return result

_configured = rule(implementation = _configured_impl, attrs = {
    "target": attr.label(mandatory = True, cfg = _platform),
    "platform": attr.label(mandatory = True),
    "cc_toolchain": attr.label(mandatory = True),
    "configuration": attr.string(mandatory = True),
    "_allowlist_function_transition": attr.label(default = "@bazel_tools//tools/allowlists/function_transition_allowlist"),
})

def kernel_example(name, fragments, platform, cc_toolchain, requested = {}, build_target = "kernel"):
    kernel_config(
        name = name + "_configuration",
        source = "//native:source",
        fragments = fragments,
        requested = requested,
        pahole = "@pahole//:pahole",
        tags = ["manual"],
    )
    for public, target in [(name + "_config", name + "_configuration"), (name, build_target)]:
        _configured(
            name = public,
            target = ":" + target,
            platform = platform,
            configuration = str(native.package_relative_label(":" + name + "_configuration")),
            cc_toolchain = cc_toolchain,
            tags = ["manual"],
        )

def example_config_test(name, config, expected):
    py_test(
        name = name,
        srcs = ["//:test_config_values.py"],
        main = "//:test_config_values.py",
        args = ["$(location " + config + ")", shell.quote(json.encode(expected))],
        data = [config],
        tags = ["manual"],
    )

def _initramfs_impl(ctx):
    output = ctx.actions.declare_file(ctx.label.name + ".cpio")
    ctx.actions.run(
        executable = ctx.attr._writer[DefaultInfo].files_to_run,
        arguments = [ctx.file.motd.path, output.path],
        inputs = [ctx.file.motd],
        outputs = [output],
        mnemonic = "ExampleInitramfs",
    )
    return [DefaultInfo(files = depset([output]))]

example_initramfs = rule(implementation = _initramfs_impl, attrs = {
    "motd": attr.label(mandatory = True, allow_single_file = True),
    "_writer": attr.label(default = "//examples:initramfs_writer", cfg = "exec", executable = True),
})
