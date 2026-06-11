"""Build-test helpers for consumer targets with fixed target architectures."""

def _platform_transition_impl(_settings, attr):
    return {"//command_line_option:platforms": attr.target_platform}

_platform_transition = transition(
    implementation = _platform_transition_impl,
    inputs = [],
    outputs = ["//command_line_option:platforms"],
)

def _platform_build_target_impl(ctx):
    return [DefaultInfo(files = depset(transitive = [
        target[DefaultInfo].files
        for target in ctx.attr.target
    ]))]

platform_build_target = rule(
    implementation = _platform_build_target_impl,
    attrs = {
        "target": attr.label(
            cfg = _platform_transition,
            mandatory = True,
        ),
        "target_platform": attr.string(mandatory = True),
        "_allowlist_function_transition": attr.label(
            default = "@bazel_tools//tools/allowlists/function_transition_allowlist",
        ),
    },
)
