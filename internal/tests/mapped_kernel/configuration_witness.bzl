"""Independent execution-config projection for the real family fixture."""

load("//internal:providers.bzl", "LinuxKernelInfo", "LinuxModuleSdkInfo")

def _execution_config_impl(ctx):
    kernel = ctx.attr.kernel[LinuxKernelInfo].config
    sdk = ctx.attr.kernel[LinuxModuleSdkInfo].config
    if kernel != sdk:
        fail("kernel and SDK use different configuration artifacts")
    return [DefaultInfo(files = depset([kernel]), runfiles = ctx.runfiles(files = [kernel]))]

execution_config = rule(
    implementation = _execution_config_impl,
    attrs = {"kernel": attr.label(mandatory = True, providers = [LinuxKernelInfo, LinuxModuleSdkInfo])},
)
