"""Build one symbolic kernel graph in every Linux/LLVM target configuration."""

load(":kernel_platform_data.bzl", "KERNEL_CONSTRAINTS", _SUPPORTED_KERNEL_CPUS = "SUPPORTED_KERNEL_CPUS")

SUPPORTED_KERNEL_CPUS = _SUPPORTED_KERNEL_CPUS
KernelPlatformsInfo = provider(doc = "Kernel Platforms Info.", fields = {"files": "CPU to configured target output depset.", "targets": "CPU to configured target."})

# Fixed package fixtures or action helpers, not target-naming macros.
# buildifier: disable=unnamed-macro
def kernel_platforms():
    for cpu in SUPPORTED_KERNEL_CPUS:
        native.platform(
            name = cpu,
            constraint_values = ["@platforms//os:linux", "@platforms//cpu:" + cpu] + KERNEL_CONSTRAINTS,
            visibility = ["//visibility:public"],
        )

# Bazel callback signatures include inputs this implementation does not use.
# buildifier: disable=unused-variable
def _split_impl(settings, attr):
    return {cpu: {"//command_line_option:platforms": str(Label("//kernel_platforms:" + cpu))} for cpu in SUPPORTED_KERNEL_CPUS}

_split = transition(implementation = _split_impl, inputs = [], outputs = ["//command_line_option:platforms"])

def _impl(ctx):
    files = {}
    for cpu, target in ctx.split_attr.target.items():
        files[cpu] = target[DefaultInfo].files
    manifest = ctx.actions.declare_file(ctx.label.name + ".platforms.json")
    ctx.actions.write(manifest, json.encode({cpu: [f.path for f in outputs.to_list()] for cpu, outputs in files.items()}))
    return [
        DefaultInfo(files = depset([manifest], transitive = files.values())),
        KernelPlatformsInfo(files = files, targets = ctx.split_attr.target),
        OutputGroupInfo(**files),
    ]

kernel_multi_platform = rule(
    implementation = _impl,
    attrs = {
        "target": attr.label(mandatory = True, cfg = _split),
        "_allowlist_function_transition": attr.label(default = "@bazel_tools//tools/allowlists/function_transition_allowlist"),
    },
)

def _test_impl(ctx):
    info = ctx.attr.kernels[KernelPlatformsInfo]
    kernels, boot, aliases = {}, {}, {}
    for cpu in info.files:
        images = info.targets[cpu][OutputGroupInfo].kernel.to_list()
        if len(images) != 1:
            fail("Expected one vmlinux for %s, found %s" % (cpu, images))
        kernels[cpu] = images[0]
        aliases["kernel-platforms/" + cpu + "/vmlinux"] = images[0]
        boot[cpu] = []
        for i, image in enumerate(info.targets[cpu][OutputGroupInfo].boot.to_list()):
            path = "kernel-platforms/" + cpu + "/boot/%d" % i
            aliases[path] = image
            boot[cpu].append(path)
    spec = ctx.actions.declare_file(ctx.label.name + ".json")
    ctx.actions.write(spec, json.encode({"kernels": {cpu: "kernel-platforms/" + cpu + "/vmlinux" for cpu in kernels}, "boot": boot}))
    script = ctx.actions.declare_file(ctx.label.name + ".sh")
    ctx.actions.write(script, '#!/bin/sh\nexec /usr/bin/python3 "$TEST_SRCDIR/$TEST_WORKSPACE/%s" "$TEST_SRCDIR/$TEST_WORKSPACE/%s"\n' % (ctx.file._checker.short_path, spec.short_path), is_executable = True)
    return [DefaultInfo(executable = script, runfiles = ctx.runfiles(files = [spec, ctx.file._checker], symlinks = aliases))]

kernel_multi_platform_test = rule(implementation = _test_impl, test = True, attrs = {
    "kernels": attr.label(mandatory = True, providers = [KernelPlatformsInfo]),
    "_checker": attr.label(default = "//rules:multi_platform_test.py", allow_single_file = True),
})
