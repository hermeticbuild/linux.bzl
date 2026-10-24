"""Select a target platform for a native configuration report."""

# Fixed transition callback signature.
# buildifier: disable=unused-variable
def _platform_transition_impl(settings, attr):
    return {"//command_line_option:platforms": str(attr.platform)}

_platform_transition = transition(
    implementation = _platform_transition_impl,
    inputs = [],
    outputs = ["//command_line_option:platforms"],
)

def _configured_impl(ctx):
    return [DefaultInfo(files = ctx.attr.report[0][DefaultInfo].files)]

configured_report = rule(implementation = _configured_impl, attrs = {
    "report": attr.label(mandatory = True, cfg = _platform_transition),
    "platform": attr.label(mandatory = True),
    "_allowlist_function_transition": attr.label(default = "@bazel_tools//tools/allowlists/function_transition_allowlist"),
})
