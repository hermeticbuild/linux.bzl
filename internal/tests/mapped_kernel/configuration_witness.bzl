"""Independent execution-config projection for the real family fixture."""

load("//internal:providers.bzl", "LinuxKernelInfo", "LinuxModuleSdkInfo")

def _execution_config_impl(ctx):
    kernel = ctx.attr.kernel[LinuxKernelInfo].config
    sdk = ctx.attr.kernel[LinuxModuleSdkInfo].config
    if kernel != sdk:
        fail("kernel and SDK use different configuration artifacts")
    tree = ctx.attr.kernel[LinuxModuleSdkInfo].sdk
    output = ctx.actions.declare_file(ctx.label.name + ".config")
    args = ctx.actions.args()
    args.add("-copy_tree_file")
    args.add_all("-tree", [tree], expand_directories = False)
    args.add("-path", ".config")
    args.add("-output", output)
    ctx.actions.run(
        executable = ctx.executable._recipe_runner,
        inputs = [tree],
        outputs = [output],
        arguments = [args],
    )
    return [DefaultInfo(files = depset([output]), runfiles = ctx.runfiles(files = [output]))]

execution_config = rule(
    implementation = _execution_config_impl,
    attrs = {
        "kernel": attr.label(mandatory = True, providers = [LinuxKernelInfo, LinuxModuleSdkInfo]),
        "_recipe_runner": attr.label(default = "//internal/cmd/mapdirectoryrecipe", executable = True, cfg = "exec"),
    },
)
