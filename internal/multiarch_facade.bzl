"""Platform-first lazy selection for generated multi-architecture facades."""

load(":architecture_profiles.bzl", "linux_architecture_profiles")
load(":platform_transition_gateway.bzl", "linux_platform_transition")
load(":providers.bzl", "LinuxKernelInfo", "LinuxModuleSdkInfo")

visibility("public")

_KERNEL_FIELDS = [
    "config",
    "image",
    "kernel_release",
    "system_map",
    "vmlinux",
]

_MODULE_FIELDS = [
    "module_symvers",
    "modules",
    "modules_builtin",
    "modules_builtin_modinfo",
    "modules_order",
]

def _forward(target):
    providers = [target[DefaultInfo]]
    if LinuxKernelInfo in target:
        providers.append(target[LinuxKernelInfo])
    if LinuxModuleSdkInfo in target:
        providers.append(target[LinuxModuleSdkInfo])
    if OutputGroupInfo in target:
        providers.append(target[OutputGroupInfo])
    return providers

def _selector_impl(ctx):
    return _forward(ctx.attr.target)

_selector = rule(
    implementation = _selector_impl,
    attrs = {"target": attr.label(mandatory = True)},
)

def _profile_select(name, targets, visibility):
    conditions = {}
    for profile in linux_architecture_profiles():
        setting = name + "__" + profile.name
        native.config_setting(
            name = setting,
            constraint_values = [
                Label("@platforms//os:linux"),
                profile.cpu,
            ],
            visibility = ["//visibility:private"],
        )
        conditions[":" + setting] = targets[profile.name]
    _selector(
        name = name,
        target = select(conditions, no_match_error = (
            "Linux image platform must have @platforms//os:linux and exactly one " +
            "supported CPU constraint: %s" % [profile.cpu for profile in linux_architecture_profiles()]
        )),
        visibility = visibility,
    )

def _kernel_projection_impl(ctx):
    info = ctx.attr.kernel[LinuxKernelInfo]
    return [DefaultInfo(files = depset([getattr(info, ctx.attr.field)]))]

_kernel_projection = rule(
    implementation = _kernel_projection_impl,
    attrs = {
        "field": attr.string(mandatory = True, values = _KERNEL_FIELDS),
        "kernel": attr.label(mandatory = True, providers = [LinuxKernelInfo]),
    },
)

def _module_projection_impl(ctx):
    info = ctx.attr.kernel[LinuxModuleSdkInfo]
    value = getattr(info, ctx.attr.field)
    return [DefaultInfo(files = value if type(value) == "depset" else depset([value]))]

_module_projection = rule(
    implementation = _module_projection_impl,
    attrs = {
        "field": attr.string(mandatory = True, values = _MODULE_FIELDS),
        "kernel": attr.label(mandatory = True, providers = [LinuxModuleSdkInfo]),
    },
)

def _validate_profiles(values, what):
    expected = [profile.name for profile in linux_architecture_profiles()]
    if sorted(values.keys()) != sorted(expected):
        fail("%s must contain exactly %s, got %s" % (what, expected, sorted(values.keys())))

def linux_multiarch_kernel_exports(
        name,
        graphs,
        platform,
        visibility = ["//visibility:public"]):
    """Selects a lazy kernel graph after transitioning to the image platform."""
    _validate_profiles(graphs, "multiarch graphs")
    selector = name + "__architecture"
    _profile_select(selector, graphs, ["//visibility:private"])
    linux_platform_transition(
        name = name,
        graph = ":" + selector,
        platform = platform,
        visibility = visibility,
    )
    for field in _KERNEL_FIELDS:
        _kernel_projection(
            name = field,
            field = field,
            kernel = ":" + name,
            visibility = visibility,
        )
    for field in _MODULE_FIELDS:
        _module_projection(
            name = field,
            field = field,
            kernel = ":" + name,
            visibility = visibility,
        )

def linux_multiarch_file(name, files, platform, visibility = ["//visibility:public"]):
    """Selects one source file after transitioning to the image platform."""
    _validate_profiles(files, "multiarch files")
    selector = name + "__architecture"
    _profile_select(selector, files, ["//visibility:private"])
    linux_platform_transition(
        name = name,
        graph = ":" + selector,
        platform = platform,
        visibility = visibility,
    )
