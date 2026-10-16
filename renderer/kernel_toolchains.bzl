"""Compiler-only kernel toolchains using hermetic-llvm's source bootstrap."""

load("@llvm//toolchain/args:resource_directory_args.bzl", "resource_directory_args")
load("@llvm//toolchain/bootstrap:bootstrap_binary.bzl", "bootstrap_binary")
load("@llvm//toolchain/bootstrap:declare_toolchains.bzl", "declare_tool_map")
load("@rules_cc//cc/toolchains:args.bzl", "cc_args")
load("@rules_cc//cc/toolchains:toolchain.bzl", "cc_toolchain")
load(":llvm_backends.bzl", "LLVM_BACKENDS")

# Fixed package fixtures or action helpers, not target-naming macros.
# buildifier: disable=unnamed-macro
def kernel_toolchains():
    native.constraint_setting(name = "purpose")
    native.constraint_value(name = "kernel", constraint_setting = ":purpose")
    native.platform(name = "linux_x86_64_platform", constraint_values = [
        "@platforms//cpu:x86_64",
        "@platforms//os:linux",
        "@llvm//constraints/libc:musl",
    ])
    prefix = "stage1_linux_x86_64"
    declare_tool_map("linux", "x86_64", prefix = prefix)
    for tool in ["llvm-objdump", "llvm-readelf"]:
        bootstrap_binary(
            name = prefix + "/bin/" + tool,
            actual = "@llvm-project//llvm:llvm.stripped",
            platform = ":linux_x86_64_platform",
        )
    resource_directory_args(
        name = "resources",
        compile_directory = prefix + "/clang_resource_directory",
        link_directory = prefix + "/clang_resource_directory",
    )
    cc_args(
        name = "target",
        actions = ["@rules_cc//cc/toolchains/actions:compile_actions"],
        args = select({
            ":" + cpu: ["--target=" + entry["triple"]]
            for cpu, entry in LLVM_BACKENDS.items()
        }),
    )
    cc_toolchain(
        name = "compiler",
        compiler = "clang",
        target_libc = "none",
        tool_map = prefix + "/default_tools",
        args = [":resources", ":target"],
        known_features = ["@rules_cc//cc/toolchains/args:experimental_replace_legacy_action_config_features"],
        enabled_features = ["@rules_cc//cc/toolchains/args:experimental_replace_legacy_action_config_features"],
    )
    for cpu in LLVM_BACKENDS:
        native.config_setting(name = cpu, constraint_values = ["@platforms//cpu:" + cpu])
        native.toolchain(
            name = cpu + "_toolchain",
            toolchain = ":compiler",
            toolchain_type = "@bazel_tools//tools/cpp:toolchain_type",
            exec_compatible_with = ["@platforms//cpu:x86_64", "@platforms//os:linux"],
            target_compatible_with = ["@platforms//cpu:" + cpu, ":kernel", "@platforms//os:linux"],
        )
