"""Analysis tests for sanitizer-specific compile action inputs."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load(
    "//internal:linux_objects.bzl",
    "linux_compile_environment_index",
    "linux_object",
    "linux_source_input_index",
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
    compile_environment_index = name + "_compile_environment_index"
    source_tree = name + "_source_tree"
    source_input_index = name + "_source_input_index"
    object = name + "_object"
    fixture_tags = ["manual"]
    config_payload_id = "1111111111111111111111111111111111111111111111111111111111111111"
    compile_environment_id = "2222222222222222222222222222222222222222222222222222222222222222"
    object_id = "3333333333333333333333333333333333333333333333333333333333333333"

    linux_compile_environment_index(
        name = compile_environment_index,
        arch = "x86",
        compile_environments = {
            compile_environment_id: json.encode({
                "abi": "tests/sanitizer/x86",
                "config_payload": config_payload_id,
                "generated_header_families": [],
            }),
        },
        config_payloads = {
            config_payload_id: "CONFIG_UBSAN=y\nCONFIG_UBSAN_INTEGER_WRAP=y\n",
        },
        expected_abi = "tests/sanitizer/x86",
        tags = fixture_tags,
    )
    linux_source_tree(
        name = source_tree,
        root = "sanitizer_test_tree/Kconfig",
        tags = fixture_tags,
    )
    linux_source_input_index(
        name = source_input_index,
        groups = ["1,2"],
        source_tree_info = ":" + source_tree,
        srcs = [
            "sanitizer_test_tree/scripts/integer-wrap-ignore.scl",
            "sanitizer_test_tree/test.c",
        ],
        tags = fixture_tags,
    )
    linux_object(
        name = object,
        compile_environment_id = compile_environment_id,
        compile_environment_index = ":" + compile_environment_index,
        content_id = object_id,
        flags = [
            "-DINTEGER_WRAP",
            "-fsanitize-ignorelist=$(srctree)/scripts/integer-wrap-ignore.scl",
        ],
        mode = "y",
        object = "test.o",
        source_input_file = 2,
        source_input_group = 1,
        source_input_index = ":" + source_input_index,
        tags = fixture_tags,
    )
    _sanitizer_compile_action_test(
        name = name,
        target_under_test = ":" + object,
    )
