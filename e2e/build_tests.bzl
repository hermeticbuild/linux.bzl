"""Build-test helpers for consumer targets with fixed target architectures."""

def _arm64_transition_impl(_settings, _attr):
    return {"//command_line_option:platforms": "@llvm//platforms:linux_arm64"}

_arm64_transition = transition(
    implementation = _arm64_transition_impl,
    inputs = [],
    outputs = ["//command_line_option:platforms"],
)

def _arm64_build_target_impl(ctx):
    return [DefaultInfo(files = depset(transitive = [
        target[DefaultInfo].files
        for target in ctx.attr.target
    ]))]

arm64_build_target = rule(
    implementation = _arm64_build_target_impl,
    attrs = {
        "target": attr.label(
            cfg = _arm64_transition,
            mandatory = True,
        ),
        "_allowlist_function_transition": attr.label(
            default = "@bazel_tools//tools/allowlists/function_transition_allowlist",
        ),
    },
)
