"""Analysis-time identity of a resolved execution platform."""

visibility("//internal/...")

LinuxExecutionPlatformInfo = provider(
    doc = "The platform label selected by an owning rule's execution group.",
    fields = {"label": "Resolved execution platform Label."},
)

def _linux_execution_platform_impl(ctx):
    return [LinuxExecutionPlatformInfo(label = ctx.fragments.platform.platform)]

linux_execution_platform = rule(
    implementation = _linux_execution_platform_impl,
    fragments = ["platform"],
)

def linux_execution_platform_attr(exec_group = None):
    return attr.label(
        cfg = "exec" if exec_group == None else config.exec(exec_group = exec_group),
        default = Label("//internal:execution_platform"),
        providers = [LinuxExecutionPlatformInfo],
    )

def linux_execution_platform_label(target):
    return target[LinuxExecutionPlatformInfo].label
