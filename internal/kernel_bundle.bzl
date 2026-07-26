"""Private aggregate and projection rules for generated kernel repositories."""

load(":linux_objects.bzl", "LinuxConfigInfo")
load(":platform_transition_gateway.bzl", "linux_platform_gateway")
load(":providers.bzl", "LinuxKernelInfo", "LinuxModuleSdkInfo")

visibility("public")

_PROJECTION_FIELDS = [
    "config",
    "image",
    "kernel_release",
    "system_map",
    "vmlinux",
]

_MODULE_PROJECTION_FIELDS = [
    "module_symvers",
    "modules",
    "modules_builtin",
    "modules_builtin_modinfo",
    "modules_order",
]

def _one_file(files, description):
    values = files.to_list()
    if len(values) != 1:
        fail("%s must contain exactly one file, got %d" % (description, len(values)))
    return values[0]

def _linux_kernel_bundle_impl(ctx):
    image = _one_file(ctx.attr.image[DefaultInfo].files, "%s image" % ctx.label)
    vmlinux_groups = ctx.attr.vmlinux[OutputGroupInfo]
    vmlinux = _one_file(vmlinux_groups.vmlinux, "%s vmlinux" % ctx.label)
    system_map = _one_file(vmlinux_groups.system_map, "%s System.map" % ctx.label)
    config = ctx.attr.config[LinuxConfigInfo]
    module_sdk = ctx.attr.module_sdk[LinuxModuleSdkInfo]

    info = LinuxKernelInfo(
        arch = ctx.attr.arch,
        version = ctx.attr.version,
        kernel_release = config.kernel_release,
        image = image,
        vmlinux = vmlinux,
        config = config.config,
        system_map = system_map,
    )
    return [
        DefaultInfo(files = depset([image])),
        info,
        module_sdk,
        OutputGroupInfo(
            config = depset([config.config]),
            image = depset([image]),
            kernel_release = depset([config.kernel_release]),
            module_symvers = depset([module_sdk.module_symvers]),
            modules = module_sdk.modules,
            modules_builtin = depset([module_sdk.modules_builtin]),
            modules_builtin_modinfo = depset([module_sdk.modules_builtin_modinfo]),
            modules_order = depset([module_sdk.modules_order]),
            system_map = depset([system_map]),
            vmlinux = depset([vmlinux]),
        ),
    ]

linux_kernel_bundle = rule(
    implementation = _linux_kernel_bundle_impl,
    attrs = {
        "arch": attr.string(
            mandatory = True,
            values = ["aarch64", "x86_64"],
        ),
        "config": attr.label(
            mandatory = True,
            providers = [LinuxConfigInfo],
        ),
        "image": attr.label(mandatory = True),
        "module_sdk": attr.label(
            mandatory = True,
            providers = [LinuxModuleSdkInfo],
        ),
        "version": attr.string(mandatory = True),
        "vmlinux": attr.label(mandatory = True),
    },
)

def _linux_kernel_projection_impl(ctx):
    info = ctx.attr.kernel[LinuxKernelInfo]
    return [DefaultInfo(files = depset([getattr(info, ctx.attr.field)]))]

_linux_kernel_projection = rule(
    implementation = _linux_kernel_projection_impl,
    attrs = {
        "field": attr.string(
            mandatory = True,
            values = _PROJECTION_FIELDS,
        ),
        "kernel": attr.label(
            mandatory = True,
            providers = [LinuxKernelInfo],
        ),
    },
)

def _linux_module_projection_impl(ctx):
    info = ctx.attr.kernel[LinuxModuleSdkInfo]
    value = getattr(info, ctx.attr.field)
    files = value if type(value) == "depset" else depset([value])
    return [DefaultInfo(files = files)]

_linux_module_projection = rule(
    implementation = _linux_module_projection_impl,
    attrs = {
        "field": attr.string(
            mandatory = True,
            values = _MODULE_PROJECTION_FIELDS,
        ),
        "kernel": attr.label(
            mandatory = True,
            providers = [LinuxModuleSdkInfo],
        ),
    },
)

def linux_kernel_exports(
        name,
        graph,
        platform,
        arch,
        visibility = ["//visibility:public"]):
    """Exports the supported target surface for one generated kernel variant."""
    linux_platform_gateway(
        name = name,
        arch = arch,
        graph = graph,
        platform = platform,
        visibility = visibility,
    )
    for field in _PROJECTION_FIELDS:
        _linux_kernel_projection(
            name = field,
            field = field,
            kernel = ":" + name,
            visibility = visibility,
        )
    for field in _MODULE_PROJECTION_FIELDS:
        _linux_module_projection(
            name = field,
            field = field,
            kernel = ":" + name,
            visibility = visibility,
        )
