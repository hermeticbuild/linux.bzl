"""Analysis tests for sanitizer-specific compile action inputs."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load(
    "//internal:linux_objects.bzl",
    "linux_config",
    "linux_object",
    "linux_source_tree",
)

visibility("private")

def _has_input_suffix(action, suffix):
    for file in action.inputs.to_list():
        if ("/" + file.short_path.replace("\\", "/")).endswith(suffix):
            return True
    return False

def _sanitizer_compile_action_test_impl(ctx):
    env = analysistest.begin(ctx)
    actions = [
        action
        for action in analysistest.target_actions(env)
        if action.mnemonic == "LinuxObjectCompile"
    ]
    asserts.equals(env, 1, len(actions))
    if actions:
        action = actions[0]
        ignorelist_flags = [
            arg
            for arg in action.argv
            if arg.startswith("-fsanitize-ignorelist=")
        ]
        asserts.equals(env, 1, len(ignorelist_flags))
        if ignorelist_flags:
            asserts.true(
                env,
                ignorelist_flags[0].endswith("/scripts/integer-wrap-ignore.scl"),
                "unexpected integer-wrap ignorelist flag %s" % ignorelist_flags[0],
            )
        asserts.true(
            env,
            _has_input_suffix(action, "/scripts/integer-wrap-ignore.scl"),
            "integer-wrap ignorelist is not a compile action input",
        )
        asserts.true(
            env,
            _has_input_suffix(action, "/include/generated/integer-wrap.h"),
            "integer-wrap rebuild marker is not a compile action input",
        )
    return analysistest.end(env)

_sanitizer_compile_action_test = analysistest.make(_sanitizer_compile_action_test_impl)

def sanitizer_actions_test(name):
    config = name + "_config"
    source_tree = name + "_source_tree"
    object = name + "_object"
    fixture_tags = ["manual"]

    linux_config(
        name = config,
        arch = "x86",
        config_flags = {
            "CONFIG_UBSAN": "y",
            "CONFIG_UBSAN_INTEGER_WRAP": "y",
        },
        tags = fixture_tags,
    )
    linux_source_tree(
        name = source_tree,
        lookup_files = ["sanitizer_test_tree/scripts/integer-wrap-ignore.scl"],
        root = "sanitizer_test_tree/Kconfig",
        tags = fixture_tags,
    )
    linux_object(
        name = object,
        config = ":" + config,
        flags = [
            "-DINTEGER_WRAP",
            "-fsanitize-ignorelist=$(srctree)/scripts/integer-wrap-ignore.scl",
        ],
        mode = "y",
        object = "test.o",
        source_tree_info = ":" + source_tree,
        src = "sanitizer_test_tree/test.c",
        tags = fixture_tags,
    )
    _sanitizer_compile_action_test(
        name = name,
        target_under_test = ":" + object,
    )
