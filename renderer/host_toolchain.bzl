"""C++ toolchain metadata consumed through an execution transition."""

load("@rules_cc//cc:find_cc_toolchain.bzl", "find_cc_toolchain", "use_cc_toolchain")
load("@rules_cc//cc/common:cc_common.bzl", "cc_common")
load(":architecture.bzl", "KernelArchitectureInfo")

KernelHostToolchainInfo = provider(doc = "Kernel Host Toolchain Info.", fields = ["cc", "features", "architecture"])

def _impl(ctx):
    cc = find_cc_toolchain(ctx)
    features = cc_common.configure_features(ctx = ctx, cc_toolchain = cc, requested_features = ctx.features, unsupported_features = ctx.disabled_features)
    return [KernelHostToolchainInfo(cc = cc, features = features, architecture = ctx.attr._architecture[KernelArchitectureInfo])]

kernel_host_toolchain = rule(implementation = _impl, fragments = ["cpp"], toolchains = use_cc_toolchain(), attrs = {
    "_architecture": attr.label(default = "//:architecture", providers = [KernelArchitectureInfo]),
})
