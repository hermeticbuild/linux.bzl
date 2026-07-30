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

def _has_input_basename_containing(action, value):
    for file in action.inputs.to_list():
        if value in file.basename:
            return True
    return False

def _has_tool_input(action, name):
    for file in action.inputs.to_list():
        if file.basename in (name, name + ".exe"):
            return True
    return False

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
        asserts.false(env, _has_argument(resolved[0], "-allow_shell"))
        asserts.false(env, _has_argument(resolved[0], "-linux_probe_model"))
        asserts.false(env, _has_argument(resolved[0], "-linux_probe_value"))
        asserts.false(
            env,
            _has_argument(resolved[0], "-rust_toolchain_probe"),
            "C-only config action must not consume a Rust compiler probe",
        )
        asserts.true(
            env,
            _has_argument(resolved[0], "-graph_profile_projection"),
            "C-only config action must consume the repository C graph projection",
        )
        asserts.true(
            env,
            _has_input_basename_containing(resolved[0], "graph_projection"),
            "C-only config action must consume the repository graph projection",
        )
        asserts.true(
            env,
            _has_input_basename_containing(resolved[0], "graph_profile.validated"),
            "C-only config action must depend on configured C graph validation",
        )
        asserts.true(
            env,
            _has_input_basename_containing(resolved[0], "compiler-version.h"),
            "C-only config action must declare graph-profile source inputs",
        )
        for value in [
            "OBJCOPY=llvm-objcopy",
            "PYTHON3=python3",
            "RUSTC=rustc",
        ]:
            asserts.true(
                env,
                _has_argument(resolved[0], value),
                "config replay must preserve graph-profile identity environment %s" % value,
            )
    kernel_flags = _actions_with_mnemonic(actions, "LinuxKernelCFlags")
    asserts.equals(env, 1, len(kernel_flags))
    if kernel_flags:
        asserts.true(
            env,
            _has_argument(kernel_flags[0], "-version"),
            "kernel flag generation must receive the source version",
        )
        asserts.true(
            env,
            _has_argument(kernel_flags[0], "6.18.2"),
            "kernel flag generation must receive the exact source version",
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
        asserts.true(
            env,
            _has_input_basename_containing(probes[0], "rustc_driver"),
            "the compiler probe requires rustc runtime libraries",
        )
        for name in [
            "cargo",
            "cargo-clippy",
            "clang",
            "clippy-driver",
            "ld.lld",
            "llvm-ar",
            "rustdoc",
            "rustfmt",
        ]:
            asserts.false(
                env,
                _has_tool_input(probes[0], name),
                "%s should not be an input to the compiler identity probe" % name,
            )

    resolved = _actions_with_mnemonic(actions, "LinuxResolvedConfig")
    asserts.equals(env, 1, len(resolved))
    if resolved:
        asserts.true(env, _has_argument(resolved[0], "-rust_toolchain_probe"))
        asserts.true(env, _has_argument(resolved[0], "-graph_profile_projection"))
        asserts.true(env, _has_argument(resolved[0], "-validate_config_equivalence"))
        asserts.false(env, _has_argument(resolved[0], "-linux_probe_model"))
        asserts.false(env, _has_argument(resolved[0], "-linux_probe_value"))
        asserts.true(env, _has_argument(resolved[0], "RUSTC=rustc"))
        asserts.true(env, _has_input(resolved[0], info.rustc_probe))
        asserts.true(
            env,
            _has_input_basename_containing(resolved[0], "graph_projection"),
            "Rust config action must consume the repository graph projection",
        )
        asserts.true(
            env,
            _has_input_basename_containing(resolved[0], "graph_profile.validated"),
            "Rust config action must depend on configured C graph validation",
        )
        asserts.true(
            env,
            _has_input_basename_containing(resolved[0], "compiler-version.h"),
            "Rust config action must declare graph-profile source inputs",
        )
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
