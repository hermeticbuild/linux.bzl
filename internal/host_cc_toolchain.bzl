"""Exec-configured C toolchains for Linux host binaries."""

load("@rules_cc//cc/common:cc_common.bzl", "cc_common")

visibility("//internal/...")

def host_cc_toolchain(ctx):
    return ctx.attr._host_cc_toolchain[cc_common.CcToolchainInfo]

def host_cc_toolchain_attr(exec_group = None):
    return attr.label(
        cfg = config.exec(exec_group = exec_group),
        default = Label("@rules_cc//cc:current_cc_toolchain"),
        providers = [cc_common.CcToolchainInfo],
    )
