"""Private platform transition boundary for execution-time Linux graphs."""

load(":providers.bzl", "LinuxKernelInfo", "LinuxModuleSdkInfo", "LinuxModuleTreeInfo")

visibility("//...")

def _platform_transition_impl(_settings, attr):
    return {
        "//command_line_option:platforms": str(attr.platform),
    }

_platform_transition = transition(
    implementation = _platform_transition_impl,
    inputs = [],
    outputs = ["//command_line_option:platforms"],
)

def _forwarded_providers(target):
    providers = [target[DefaultInfo]]
    if LinuxKernelInfo in target:
        providers.append(target[LinuxKernelInfo])
    if LinuxModuleSdkInfo in target:
        providers.append(target[LinuxModuleSdkInfo])
    if LinuxModuleTreeInfo in target:
        providers.append(target[LinuxModuleTreeInfo])
    if OutputGroupInfo in target:
        providers.append(target[OutputGroupInfo])
    return providers

def _platform_gateway_impl(ctx):
    graph = ctx.attr.graph
    if len(graph) != 1:
        fail("Linux platform transition produced %d graph configurations, want 1" % len(graph))
    return _forwarded_providers(graph[0])

_platform_gateway = rule(
    implementation = _platform_gateway_impl,
    attrs = {
        "graph": attr.label(
            cfg = _platform_transition,
            mandatory = True,
        ),
        "platform": attr.label(mandatory = True),
        "_allowlist_function_transition": attr.label(
            default = Label("@bazel_tools//tools/allowlists/function_transition_allowlist"),
        ),
    },
)

def linux_platform_transition(
        name,
        graph,
        platform,
        visibility = None,
        tags = None):
    """Transitions one Linux graph to the consumer-selected target platform."""
    _platform_gateway(
        name = name,
        graph = graph,
        platform = platform,
        tags = tags,
        visibility = visibility,
    )
