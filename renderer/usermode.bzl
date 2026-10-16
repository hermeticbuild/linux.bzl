"""User Mode Linux targets, selected by a platform mode constraint."""

load(":image.bzl", "kbuild_resolved_build", "kbuild_resolved_setup")
load(":rules.bzl", "kernel_config")

# Bazel callback signatures include inputs this implementation does not use.
# buildifier: disable=unused-variable
def _platform_impl(settings, attr):
    return {"//command_line_option:platforms": str(Label("//usermode:platform"))}

_platform = transition(implementation = _platform_impl, inputs = [], outputs = ["//command_line_option:platforms"])

def _impl(ctx):
    target = ctx.attr.target[0]
    result = [DefaultInfo(files = target[DefaultInfo].files)]
    if OutputGroupInfo in target:
        result.append(target[OutputGroupInfo])
    return result

_configured = rule(implementation = _impl, attrs = {
    "target": attr.label(mandatory = True, cfg = _platform),
    "_allowlist_function_transition": attr.label(default = "@bazel_tools//tools/allowlists/function_transition_allowlist"),
})

def _test_impl(ctx):
    images = ctx.attr.kernel[OutputGroupInfo].kernel.to_list()
    if len(images) != 1:
        fail("Expected one UML executable")
    script = ctx.actions.declare_file(ctx.label.name + ".sh")
    ctx.actions.write(script, '#!/bin/sh\nexec /usr/bin/python3 "$TEST_SRCDIR/$TEST_WORKSPACE/%s" "$TEST_SRCDIR/$TEST_WORKSPACE/uml-vmlinux"\n' % ctx.file._checker.short_path, is_executable = True)
    return [DefaultInfo(executable = script, runfiles = ctx.runfiles(files = [ctx.file._checker], symlinks = {"uml-vmlinux": images[0]}))]

_boot_test = rule(implementation = _test_impl, test = True, attrs = {
    "kernel": attr.label(mandatory = True),
    "_checker": attr.label(default = "//rules:usermode_test.py", allow_single_file = True),
})

# Fixed package fixtures or action helpers, not target-naming macros.
# buildifier: disable=unnamed-macro
def usermode_targets(platform_parent, requested = {}):
    native.platform(
        name = "platform",
        parents = [platform_parent],
        constraint_values = ["//kernel_modes:usermode"],
    )

    # Only analyze these implementation targets through the UML transition;
    # wildcard builds otherwise request STATIC_LINK in the native kernel.
    kernel_config(
        name = "options",
        options = "//options:options",
        allnoconfig = True,
        requested = requested,
        strict = True,
        tags = ["manual"],
    )
    kbuild_resolved_build(name = "image", config = ":options", catalog = "//:catalog", roots = ["vmlinux"], tags = ["manual"])
    _configured(name = "config", target = ":options", visibility = ["//visibility:public"])
    kbuild_resolved_setup(name = "setup_info", config = ":options", tags = ["manual"])
    _configured(name = "setup", target = ":setup_info", visibility = ["//visibility:public"])
    _configured(name = "kernel", target = ":image", visibility = ["//visibility:public"], tags = ["manual"])
    _boot_test(name = "boot_test", kernel = ":kernel", tags = ["manual", "local"], size = "large")
