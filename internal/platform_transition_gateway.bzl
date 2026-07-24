"""Private platform transition boundary for generated Linux build graphs."""

load(":providers.bzl", "LinuxKernelInfo")

_ARCH_CPU_CONSTRAINTS = {
    "aarch64": Label("@platforms//cpu:aarch64"),
    "x86_64": Label("@platforms//cpu:x86_64"),
}

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
    if OutputGroupInfo in target:
        providers.append(target[OutputGroupInfo])
    return providers

def _platform_guard_impl(ctx):
    if not ctx.attr.platform_matches:
        fail(
            "Linux kernel architecture %s requires a target platform with " % ctx.attr.arch +
            "@platforms//os:linux and %s; configured platform is %s" % (
                _ARCH_CPU_CONSTRAINTS[ctx.attr.arch],
                ctx.attr.platform.label,
            ),
        )
    return _forwarded_providers(ctx.attr.graph)

_platform_guard = rule(
    implementation = _platform_guard_impl,
    attrs = {
        "arch": attr.string(
            mandatory = True,
            values = sorted(_ARCH_CPU_CONSTRAINTS.keys()),
        ),
        "graph": attr.label(mandatory = True),
        "platform": attr.label(mandatory = True),
        "platform_matches": attr.bool(mandatory = True),
    },
)

def _platform_gateway_impl(ctx):
    graph = ctx.attr.graph

    # Bazel represents an outgoing transitioned label as a singleton list in
    # versions where transitioned attributes use split-attribute semantics.
    if type(graph) == "list":
        if len(graph) != 1:
            fail("Linux platform transition produced %d graph configurations, want 1" % len(graph))
        graph = graph[0]
    return _forwarded_providers(graph)

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

def linux_platform_gateway(
        name,
        graph,
        platform,
        arch,
        visibility = None,
        tags = None):
    """Creates one platform transition around a generated Linux build graph.

    This is a private integration macro for generated repositories. The graph
    itself and every normal target below it remain transition-free.

    Args:
      name: Public gateway target name.
      graph: Private aggregate kernel graph target.
      platform: Target platform label used for toolchain resolution.
      arch: Canonical Linux architecture, x86_64 or aarch64.
      visibility: Optional public gateway visibility.
      tags: Optional public gateway tags.
    """
    cpu_constraint = _ARCH_CPU_CONSTRAINTS.get(arch)
    if cpu_constraint == None:
        fail("unsupported Linux platform gateway architecture %r" % arch)

    platform_match = name + "__platform_match"
    guarded_graph = name + "__guarded_graph"
    native.config_setting(
        name = platform_match,
        constraint_values = [
            Label("@platforms//os:linux"),
            cpu_constraint,
        ],
        visibility = ["//visibility:private"],
    )
    _platform_guard(
        name = guarded_graph,
        arch = arch,
        graph = graph,
        platform = platform,
        platform_matches = select({
            ":" + platform_match: True,
            "//conditions:default": False,
        }),
        tags = ["manual"],
        visibility = ["//visibility:private"],
    )
    _platform_gateway(
        name = name,
        graph = ":" + guarded_graph,
        platform = platform,
        tags = tags,
        visibility = visibility,
    )
