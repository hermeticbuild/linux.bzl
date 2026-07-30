"""Analysis tests for fragment-scoped Linux compiler flags."""

load(
    "@bazel_skylib//lib:unittest.bzl",
    "analysistest",
    "asserts",
)
load("//internal:linux_objects.bzl", "linux_config")

visibility("private")

def _fragment_flags_action_test_impl(ctx):
    env = analysistest.begin(ctx)
    actions = [
        action
        for action in analysistest.target_actions(env)
        if action.mnemonic == "LinuxKernelCFlags"
    ]
    asserts.equals(env, 1, len(actions))
    if actions:
        argv = actions[0].argv
        asserts.true(env, "-arch" in argv)
        asserts.true(env, "x86" in argv)
        asserts.true(env, "-version" in argv)
        asserts.true(env, "6.18.2" in argv)
        asserts.true(env, "-config" in argv)
        asserts.true(env, "-out" in argv)
        asserts.true(env, "-asm_out" in argv)
    return analysistest.end(env)

_fragment_flags_action_test = analysistest.make(_fragment_flags_action_test_impl)

def linux_config_fragment_test_suite(name):
    target = name + "_target"
    linux_config(
        name = target,
        arch = "x86",
        config_flags = {
            "CONFIG_FRAME_POINTER": "y",
        },
        tags = ["manual"],
    )
    _fragment_flags_action_test(
        name = name + "_action",
        target_under_test = ":" + target,
    )
