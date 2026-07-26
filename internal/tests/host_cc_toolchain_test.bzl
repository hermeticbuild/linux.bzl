"""Analysis test for exec-configured Linux host C toolchains."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("@rules_cc//cc:find_cc_toolchain.bzl", "find_cpp_toolchain", "use_cc_toolchain")
load("//internal:host_cc_toolchain.bzl", "host_cc_toolchain", "host_cc_toolchain_attr")

visibility("private")

_HostCcToolchainInfo = provider(
    doc = "Target identities selected for kernel and host C actions.",
    fields = {
        "host_target": "GNU system name selected for host binaries.",
        "kernel_target": "GNU system name selected for kernel binaries.",
    },
)

def _host_cc_toolchain_probe_impl(ctx):
    return [_HostCcToolchainInfo(
        host_target = host_cc_toolchain(ctx).target_gnu_system_name,
        kernel_target = find_cpp_toolchain(ctx).target_gnu_system_name,
    )]

_host_cc_toolchain_probe = rule(
    implementation = _host_cc_toolchain_probe_impl,
    attrs = {
        "_host_cc_toolchain": host_cc_toolchain_attr(exec_group = "host_cc"),
    },
    exec_groups = {
        "host_cc": exec_group(toolchains = use_cc_toolchain()),
    },
    toolchains = use_cc_toolchain(),
)

def _host_cc_toolchain_test_impl(ctx):
    env = analysistest.begin(ctx)
    info = analysistest.target_under_test(env)[_HostCcToolchainInfo]
    asserts.equals(env, "aarch64-unknown-linux-gnu", info.kernel_target)
    asserts.equals(env, "x86_64-unknown-linux-gnu", info.host_target)
    return analysistest.end(env)

_host_cc_toolchain_test = analysistest.make(
    _host_cc_toolchain_test_impl,
    config_settings = {
        "//command_line_option:platforms": str(Label("@llvm//platforms:linux_arm64")),
    },
)

def host_cc_toolchain_test(name):
    target = name + "_target"
    _host_cc_toolchain_probe(
        name = target,
        tags = ["manual"],
    )
    _host_cc_toolchain_test(
        name = name,
        target_under_test = ":" + target,
    )
