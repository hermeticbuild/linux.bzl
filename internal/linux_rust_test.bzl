"""Focused analysis tests for Rust-for-Linux SDK selection."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts", "unittest")
load(":linux_rust.bzl", "linux_disabled_rust_kernel_sdk", "linux_rust_test_helpers")
load(":providers.bzl", "LinuxRustSdkInfo")
load(":repositories.bzl", "repositories_test_helpers")

visibility("//...")

def _disabled_sdk_test_impl(ctx):
    env = analysistest.begin(ctx)
    sdk = analysistest.target_under_test(env)[LinuxRustSdkInfo]

    asserts.false(env, sdk.enabled)
    asserts.equals(env, [], sdk.compile_inputs.to_list())
    asserts.equals(env, [], sdk.module_flags)
    asserts.equals(env, None, sdk.rustc)
    asserts.equals(env, {}, sdk.rustc_env)
    asserts.equals(env, [], sdk.rustc_files.to_list())
    asserts.equals(env, "", sdk.rustc_version)
    asserts.equals(env, None, sdk.rustc_version_runner)
    asserts.equals(env, [], sdk.runtime_objects)
    asserts.equals(env, None, sdk.target_spec)
    return analysistest.end(env)

_disabled_sdk_test = analysistest.make(_disabled_sdk_test_impl)

def _repository_protocol_test_impl(ctx):
    env = unittest.begin(ctx)
    generated = repositories_test_helpers.kernel_root_build(
        arch = "x86_64",
        version = "6.18.39",
        source_repo = "@@linux_sources",
        platform = "@@llvm//platforms:linux_x86_64",
        base_config = "//configs:x86_64",
        base_rust_enabled = False,
        config_mode = "default",
        graph_image = "//graph/base:x86_64_image",
        variant_configs = {"rust": "//configs:rust"},
        variant_graph_images = {"rust": "//graph/rust:rust_image"},
        variant_rust_enabled = {"rust": True},
        rules_repo = "@@linux_bzl",
    )

    asserts.equals(
        env,
        "compact-v2-rust-sdk-state",
        repositories_test_helpers.generator_protocol,
    )
    asserts.true(env, "base_rust_enabled = False," in generated)
    asserts.true(
        env,
        'variant_rust_enabled = {\n        "rust": True,\n    },' in generated,
    )
    return unittest.end(env)

_repository_protocol_test = unittest.make(_repository_protocol_test_impl)

def _unsupported_dead_code_elimination_test_impl(ctx):
    env = unittest.begin(ctx)
    asserts.equals(
        env,
        ["CONFIG_LD_DEAD_CODE_DATA_ELIMINATION"],
        linux_rust_test_helpers.unsupported_config_symbols(struct(
            config_flags = {
                "CONFIG_LD_DEAD_CODE_DATA_ELIMINATION": "y",
            },
        )),
    )
    return unittest.end(env)

_unsupported_dead_code_elimination_test = unittest.make(
    _unsupported_dead_code_elimination_test_impl,
)

def linux_rust_test_suite(name):
    disabled_sdk = name + "_disabled_sdk"
    linux_disabled_rust_kernel_sdk(
        name = disabled_sdk,
        tags = ["manual"],
    )

    disabled_test = disabled_sdk + "_test"
    _disabled_sdk_test(
        name = disabled_test,
        target_under_test = ":" + disabled_sdk,
    )

    protocol_test = name + "_repository_protocol_test"
    _repository_protocol_test(name = protocol_test)
    dead_code_test = name + "_dead_code_elimination_test"
    _unsupported_dead_code_elimination_test(name = dead_code_test)

    native.test_suite(
        name = name,
        tests = [
            ":" + disabled_test,
            ":" + protocol_test,
            ":" + dead_code_test,
        ],
    )
