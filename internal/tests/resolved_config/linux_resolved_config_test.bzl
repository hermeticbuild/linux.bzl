"""Analysis tests for C-only and Rust-enabled resolved Linux configs."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("//internal:linux_objects.bzl", "LinuxConfigInfo")

visibility("private")

def _actions_with_mnemonic(actions, mnemonic):
    return [
        action
        for action in actions
        if action.mnemonic == mnemonic
    ]

def _has_argument(action, value):
    return value in action.argv

def _has_input(action, file):
    return file.path in {
        input_file.path: True
        for input_file in action.inputs.to_list()
    }

def _c_config_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    info = target[LinuxConfigInfo]
    actions = analysistest.target_actions(env)

    asserts.equals(env, None, info.rustc_probe)
    asserts.equals(
        env,
        0,
        len(_actions_with_mnemonic(actions, "LinuxRustToolchainProbe")),
    )
    resolved = _actions_with_mnemonic(actions, "LinuxResolvedConfig")
    asserts.equals(env, 1, len(resolved))
    if resolved:
        asserts.false(
            env,
            _has_argument(resolved[0], "-rust_toolchain_probe"),
            "C-only config action must not consume a Rust compiler probe",
        )
    return analysistest.end(env)

_c_config_test = analysistest.make(_c_config_test_impl)

def _rust_config_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    info = target[LinuxConfigInfo]
    actions = analysistest.target_actions(env)

    asserts.true(env, info.rustc_probe != None)
    probes = _actions_with_mnemonic(actions, "LinuxRustToolchainProbe")
    asserts.equals(env, 1, len(probes))
    if probes:
        asserts.true(env, _has_argument(probes[0], "-rustc"))
        asserts.true(env, _has_argument(probes[0], "-host-rustc"))
        asserts.true(env, _has_argument(probes[0], "-minimum"))
        asserts.true(env, _has_argument(probes[0], "1.78.0"))

    resolved = _actions_with_mnemonic(actions, "LinuxResolvedConfig")
    asserts.equals(env, 1, len(resolved))
    if resolved:
        asserts.true(env, _has_argument(resolved[0], "-rust_toolchain_probe"))
        asserts.true(env, _has_argument(resolved[0], "-validate_config_equivalence"))
        asserts.true(env, _has_input(resolved[0], info.rustc_probe))
    return analysistest.end(env)

_rust_config_test = analysistest.make(_rust_config_test_impl)

def linux_resolved_config_test_suite(name):
    c_test = name + "_c"
    _c_config_test(
        name = c_test,
        target_under_test = ":resolved_c_config",
    )
    rust_test = name + "_rust"
    _rust_config_test(
        name = rust_test,
        target_under_test = ":resolved_rust_config",
    )
    native.test_suite(
        name = name,
        tests = [
            ":" + c_test,
            ":" + rust_test,
        ],
    )
