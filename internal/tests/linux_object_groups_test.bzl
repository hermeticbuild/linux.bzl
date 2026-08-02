"""Analysis and build tests for concrete compact object groups."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("@bazel_skylib//rules:build_test.bzl", "build_test")
load(
    "//internal:linux_object_groups.bzl",
    "LinuxObjectActionGroupInfo",
    "linux_composite_object_action_group",
    "linux_grouped_compact_image",
    "linux_object_action_group",
    "linux_object_action_group_import",
)
load(
    "//internal:linux_objects.bzl",
    "LinuxGeneratedHeadersInfo",
    "LinuxImageInfo",
    "LinuxObjectInfo",
    "linux_compile_environment_index",
    "linux_source_input_index",
    "linux_source_tree",
)

visibility("private")

_COMPILE_ENVIRONMENT_ID = "1111111111111111111111111111111111111111111111111111111111111111"
_CONFIG_PAYLOAD_ID = "2222222222222222222222222222222222222222222222222222222222222222"
_RECIPE_ID = "3333333333333333333333333333333333333333333333333333333333333333"
_REACHABILITY_ID = "4444444444444444444444444444444444444444444444444444444444444444"
_FIRST_CONTENT_ID = "5555555555555555555555555555555555555555555555555555555555555555"
_SECOND_CONTENT_ID = "6666666666666666666666666666666666666666666666666666666666666666"
_MODULE_CONTENT_ID = "7777777777777777777777777777777777777777777777777777777777777777"
_MODULE_RECIPE_ID = "8888888888888888888888888888888888888888888888888888888888888888"
_COMPOSITE_CONTENT_ID = "9999999999999999999999999999999999999999999999999999999999999999"
_COMPOSITE_RECIPE_ID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
_HEADER_FAMILY_ID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
_ASM_RECIPE_ID = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
_ASM_CONTENT_ID = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"

def _fake_generated_headers_impl(ctx):
    header = ctx.actions.declare_file(ctx.label.name + ".h")
    cflags = ctx.actions.declare_file(ctx.label.name + ".cflags.rsp")
    ctx.actions.write(header, "")
    ctx.actions.write(cflags, "-mstack-protector-guard=tls\n")
    files = depset([header, cflags])
    family = struct(
        arch = "x86",
        cflags = cflags,
        content_id = _HEADER_FAMILY_ID,
        files = files,
        include_dir_anchors = {},
        include_dirs = [],
        name = "all",
        srcarch = "x86",
        vdsomunge = None,
    )
    return [
        DefaultInfo(files = files),
        LinuxGeneratedHeadersInfo(
            arch = "x86",
            cflags = cflags,
            families = {"all": family},
            files = files,
            include_dir_anchors = {},
            include_dirs = [],
            srcarch = "x86",
            vdsomunge = None,
        ),
    ]

_fake_generated_headers = rule(implementation = _fake_generated_headers_impl)

def _fake_object_impl(ctx):
    out = ctx.actions.declare_file(ctx.label.name + ".o")
    ctx.actions.write(out, "")
    return [
        DefaultInfo(files = depset([out])),
        LinuxObjectInfo(
            content_id = ctx.attr.content_id,
            generated_headers = depset([]),
            generated_include_dir_anchors = {},
            generated_include_dirs = [],
            mode = ctx.attr.mode,
            module_root_kind = "single" if ctx.attr.mode == "m" else "",
            object = ctx.attr.object,
            objtool_args = [],
            objtool_force = False,
            output = out,
        ),
    ]

_fake_object = rule(
    implementation = _fake_object_impl,
    attrs = {
        "content_id": attr.string(mandatory = True),
        "mode": attr.string(mandatory = True, values = ["m", "y"]),
        "object": attr.string(mandatory = True),
    },
)

def _actions_with_mnemonic(actions, mnemonic):
    return [action for action in actions if action.mnemonic == mnemonic]

def _compile_group_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    info = target[LinuxObjectActionGroupInfo]
    actions = analysistest.target_actions(env)
    asserts.equals(env, ["first", "second"], info.object_targets)
    asserts.equals(env, ["base", "lz4"], info.reachable_configs)
    asserts.equals(env, 2, len(_actions_with_mnemonic(actions, "LinuxObjectCompile")))
    asserts.equals(env, 0, len(_actions_with_mnemonic(actions, "LinuxFlagFilter")))
    for action in _actions_with_mnemonic(actions, "LinuxObjectCompile"):
        generated_cflags = [arg for arg in action.argv if arg.endswith(".cflags.rsp")]
        asserts.equals(env, 1, len(generated_cflags))
    return analysistest.end(env)

_compile_group_test = analysistest.make(_compile_group_test_impl)

def _asm_compile_group_test_impl(ctx):
    env = analysistest.begin(ctx)
    actions = _actions_with_mnemonic(analysistest.target_actions(env), "LinuxObjectCompile")
    asserts.equals(env, 1, len(actions))
    if actions:
        generated_cflags = [arg for arg in actions[0].argv if arg.endswith(".cflags.rsp")]
        assembler_config = [arg for arg in actions[0].argv if arg.endswith("/bazel_kbuild_aflags.rsp")]
        asserts.equals(env, 0, len(generated_cflags))
        asserts.equals(env, 1, len(assembler_config))
    return analysistest.end(env)

_asm_compile_group_test = analysistest.make(_asm_compile_group_test_impl)

def _composite_group_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    info = target[LinuxObjectActionGroupInfo]
    actions = analysistest.target_actions(env)
    asserts.equals(env, ["composite"], info.object_targets)
    asserts.equals(env, "composite.o", info.objects["composite"].object)
    asserts.equals(env, 1, len(_actions_with_mnemonic(actions, "LinuxCompositeObject")))
    return analysistest.end(env)

_composite_group_test = analysistest.make(_composite_group_test_impl)

def _image_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    info = target[LinuxImageInfo]
    actions = analysistest.target_actions(env)
    asserts.equals(env, ["second.o", "first.o"], [obj.object for obj in info.objects])
    asserts.equals(env, ["fallback.ko.o"], [obj.object for obj in info.module_objects])
    asserts.equals(env, 1, len(_actions_with_mnemonic(actions, "LinuxCompactImageArchive")))
    return analysistest.end(env)

_image_test = analysistest.make(_image_test_impl)

def linux_object_groups_test_suite(name):
    """Defines concrete action-group analysis and build tests.

    Args:
      name: Target-name prefix for the test suite.
    """
    _fake_generated_headers(name = name + "_generated_headers")
    linux_compile_environment_index(
        name = name + "_compile_environments",
        compile_environments = {
            _COMPILE_ENVIRONMENT_ID: json.encode({
                "abi": "tests/grouped/x86",
                "config_payload": _CONFIG_PAYLOAD_ID,
                "generated_header_families": [_HEADER_FAMILY_ID],
            }),
        },
        config_payloads = {
            _CONFIG_PAYLOAD_ID: "CONFIG_TEST=y\n",
        },
        expected_abi = "tests/grouped/x86",
        generated_headers = [":" + name + "_generated_headers"],
    )
    linux_source_tree(
        name = name + "_source_tree",
        root = "//tests/compile:source/Kconfig",
    )
    linux_source_input_index(
        name = name + "_source_inputs",
        groups = ["1,2,3,4,5,6,7"],
        source_tree_info = ":" + name + "_source_tree",
        srcs = [
            "//tests/compile:source/cross_tree/leaf.c",
            "//tests/compile:source/include/linux/compiler-version.h",
            "//tests/compile:source/include/linux/compiler_types.h",
            "//tests/compile:source/include/linux/kconfig.h",
            "//tests/compile:source/shared/first.inc",
            "//tests/compile:source/shared/nested/second.inc",
            "//tests/compile:source/cross_tree/leaf.S",
        ],
    )
    linux_object_action_group(
        name = name + "_compile_group",
        arch = "x86",
        compile_environment_index = ":" + name + "_compile_environments",
        flags = ["-Werror"],
        language = "c",
        mode = "y",
        objects = {
            "first": json.encode({
                "compile_environment": _COMPILE_ENVIRONMENT_ID,
                "content_id": _FIRST_CONTENT_ID,
                "object": "first.o",
                "source_input_file": 2,
                "source_input_group": 1,
            }),
            "second": json.encode({
                "compile_environment": _COMPILE_ENVIRONMENT_ID,
                "content_id": _SECOND_CONTENT_ID,
                "object": "second.o",
                "source_input_file": 2,
                "source_input_group": 1,
            }),
        },
        reachable_configs = ["base", "lz4"],
        reachability_id = _REACHABILITY_ID,
        recipe_id = _RECIPE_ID,
        source_input_index = ":" + name + "_source_inputs",
        srcarch = "x86",
    )
    linux_object_action_group(
        name = name + "_asm_compile_group",
        arch = "x86",
        compile_environment_index = ":" + name + "_compile_environments",
        language = "asm",
        mode = "y",
        objects = {
            "assembly": json.encode({
                "compile_environment": _COMPILE_ENVIRONMENT_ID,
                "content_id": _ASM_CONTENT_ID,
                "object": "assembly.o",
                "source_input_file": 1,
                "source_input_group": 1,
            }),
        },
        reachable_configs = ["base", "lz4"],
        reachability_id = _REACHABILITY_ID,
        recipe_id = _ASM_RECIPE_ID,
        source_input_index = ":" + name + "_source_inputs",
        srcarch = "x86",
    )
    _fake_object(
        name = name + "_module_fallback",
        content_id = _MODULE_CONTENT_ID,
        mode = "m",
        object = "fallback.ko.o",
    )
    linux_object_action_group_import(
        name = name + "_module_group",
        mode = "m",
        object_targets = ["fallback_module"],
        objects = [":" + name + "_module_fallback"],
        reachable_configs = ["base", "lz4"],
        reachability_id = _REACHABILITY_ID,
        recipe_id = _MODULE_RECIPE_ID,
    )
    linux_composite_object_action_group(
        name = name + "_composite_group",
        arch = "x86",
        member_groups = [":" + name + "_compile_group"],
        mode = "y",
        objects = {
            "composite": json.encode({
                "content_id": _COMPOSITE_CONTENT_ID,
                "members": ["first"],
                "object": "composite.o",
            }),
        },
        reachable_configs = ["base", "lz4"],
        reachability_id = _REACHABILITY_ID,
        recipe_id = _COMPOSITE_RECIPE_ID,
    )
    linux_grouped_compact_image(
        name = name + "_image",
        config = "base",
        groups = [
            ":" + name + "_compile_group",
            ":" + name + "_module_group",
        ],
        module_object_targets = ["fallback_module"],
        object_targets = ["second", "first"],
    )
    linux_grouped_compact_image(
        name = name + "_composite_image",
        config = "base",
        groups = [":" + name + "_composite_group"],
        object_targets = ["composite"],
    )
    _compile_group_test(
        name = name + "_compile_group_test",
        target_under_test = ":" + name + "_compile_group",
    )
    _asm_compile_group_test(
        name = name + "_asm_compile_group_test",
        target_under_test = ":" + name + "_asm_compile_group",
    )
    _composite_group_test(
        name = name + "_composite_group_test",
        target_under_test = ":" + name + "_composite_group",
    )
    _image_test(
        name = name + "_image_test",
        target_under_test = ":" + name + "_image",
    )
    build_test(
        name = name + "_build_test",
        targets = [
            ":" + name + "_composite_image",
            ":" + name + "_image",
        ],
    )
