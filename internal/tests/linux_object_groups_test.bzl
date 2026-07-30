"""Analysis tests for compact-v7 lazy object action groups."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("@bazel_skylib//rules:build_test.bzl", "build_test")
load("//internal:flag_programs.bzl", "linux_flag_programs")
load("//internal:graph_profile.bzl", "linux_graph_profile_context")
load(
    "//internal:linux_object_groups.bzl",
    "LinuxObjectActionGroupInfo",
    "LinuxObjectProjectionInfo",
    "linux_arm64_nvhe_object_action_group",
    "linux_composite_object_action_group",
    "linux_grouped_image",
    "linux_grouped_modules",
    "linux_image_object_projection",
    "linux_module_object_projection",
    "linux_object_action_group",
    "linux_object_action_group_import",
)
load(
    "//internal:linux_objects.bzl",
    "LinuxGeneratedHeadersInfo",
    "LinuxImageInfo",
    "LinuxModuleObjectsInfo",
    "LinuxObjectInfo",
    "linux_compile_environment_index",
)

visibility("private")

_COMPILE_ENVIRONMENT_ID = "1111111111111111111111111111111111111111111111111111111111111111"
_CONFIG_PAYLOAD_ID = "2222222222222222222222222222222222222222222222222222222222222222"
_SECOND_COMPILE_ENVIRONMENT_ID = "1818181818181818181818181818181818181818181818181818181818181818"
_SECOND_CONFIG_PAYLOAD_ID = "1919191919191919191919191919191919191919191919191919191919191919"
_RECIPE_ID = "3333333333333333333333333333333333333333333333333333333333333333"
_REACHABILITY_ID = "4444444444444444444444444444444444444444444444444444444444444444"
_ACTION_SOURCE_GROUP_ID = "5555555555555555555555555555555555555555555555555555555555555555"
_SECOND_ACTION_SOURCE_GROUP_ID = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
_FIRST_CONTENT_ID = "6666666666666666666666666666666666666666666666666666666666666666"
_SECOND_CONTENT_ID = "7777777777777777777777777777777777777777777777777777777777777777"
_MODULE_RECIPE_ID = "8888888888888888888888888888888888888888888888888888888888888888"
_MODULE_CONTENT_ID = "9999999999999999999999999999999999999999999999999999999999999999"
_COMPOSITE_RECIPE_ID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
_COMPOSITE_CONTENT_ID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
_NVHE_COMPILE_ENVIRONMENT_ID = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
_NVHE_CONFIG_PAYLOAD_ID = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
_NVHE_FAMILY_ID = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
_NVHE_MEMBER_RECIPE_ID = "1212121212121212121212121212121212121212121212121212121212121212"
_NVHE_MEMBER_CONTENT_ID = "1313131313131313131313131313131313131313131313131313131313131313"
_NVHE_RECIPE_ID = "1414141414141414141414141414141414141414141414141414141414141414"
_NVHE_CONTENT_ID = "1515151515151515151515151515151515151515151515151515151515151515"
_NVHE_REACHABILITY_ID = "1616161616161616161616161616161616161616161616161616161616161616"
_NVHE_ACTION_SOURCE_GROUP_ID = "1717171717171717171717171717171717171717171717171717171717171717"
_FDT_RECIPE_ID = "2020202020202020202020202020202020202020202020202020202020202020"
_FDT_CONTENT_ID = "2121212121212121212121212121212121212121212121212121212121212121"
_IMPORT_RECIPE_ID = "2323232323232323232323232323232323232323232323232323232323232323"
_IMPORT_FIRST_CONTENT_ID = "2424242424242424242424242424242424242424242424242424242424242424"
_IMPORT_SECOND_CONTENT_ID = "2525252525252525252525252525252525252525252525252525252525252525"
_IMPORT_MODULE_CONTENT_ID = "2626262626262626262626262626262626262626262626262626262626262626"
_FLAGS_TERMINAL_ID = "2727272727272727272727272727272727272727272727272727272727272727"
_REMOVE_TERMINAL_ID = "2828282828282828282828282828282828282828282828282828282828282828"
_EMPTY_TERMINAL_ID = "2929292929292929292929292929292929292929292929292929292929292929"
_FLAGS_PROGRAM_ID = "3030303030303030303030303030303030303030303030303030303030303030"
_REMOVE_PROGRAM_ID = "3131313131313131313131313131313131313131313131313131313131313131"
_EMPTY_PROGRAM_ID = "3232323232323232323232323232323232323232323232323232323232323232"

_SOURCE_PATHS = [
    "cross_tree/leaf.c",
    "cross_tree/second.c",
    "cross_tree/unrelated.inc",
    "include/linux/compiler-version.h",
    "include/linux/compiler_types.h",
    "include/linux/kconfig.h",
    "shared/first.inc",
    "shared/nested/second.inc",
]

_SRCS = [
    "//tests/compile:source/cross_tree/leaf.c",
    "//tests/compile:source/cross_tree/second.c",
    "//tests/compile:source/cross_tree/unrelated.inc",
    "//tests/compile:source/include/linux/compiler-version.h",
    "//tests/compile:source/include/linux/compiler_types.h",
    "//tests/compile:source/include/linux/kconfig.h",
    "//tests/compile:source/shared/first.inc",
    "//tests/compile:source/shared/nested/second.inc",
]

_NVHE_MEMBER_SOURCE_PATHS = [
    "cross_tree/second.c",
    "include/linux/compiler-version.h",
    "include/linux/compiler_types.h",
    "include/linux/kconfig.h",
]

_NVHE_MEMBER_SRCS = [
    "//tests/compile:source/cross_tree/second.c",
    "//tests/compile:source/include/linux/compiler-version.h",
    "//tests/compile:source/include/linux/compiler_types.h",
    "//tests/compile:source/include/linux/kconfig.h",
]

_NVHE_SOURCE_PATHS = [
    "arch/arm64/kvm/hyp/nvhe/hyp.lds.S",
    "include/linux/compiler-version.h",
    "include/linux/kconfig.h",
]

_NVHE_SRCS = [
    "//tests/compile:source/arch/arm64/kvm/hyp/nvhe/hyp.lds.S",
    "//tests/compile:source/include/linux/compiler-version.h",
    "//tests/compile:source/include/linux/kconfig.h",
]

def _fake_arm64_generated_headers_impl(ctx):
    header = ctx.actions.declare_file(ctx.label.name + "/include/generated/fake.h")
    ctx.actions.write(header, "")
    family = struct(
        arch = "arm64",
        cflags = None,
        content_id = _NVHE_FAMILY_ID,
        files = depset([header]),
        include_dir_anchors = {},
        include_dirs = [],
        name = "all",
        srcarch = "arm64",
        vdsomunge = None,
    )
    return [
        DefaultInfo(files = depset([header])),
        LinuxGeneratedHeadersInfo(
            arch = "arm64",
            cflags = None,
            families = {"all": family},
            files = depset([header]),
            include_dir_anchors = {},
            include_dirs = [],
            srcarch = "arm64",
            vdsomunge = None,
        ),
    ]

_fake_arm64_generated_headers = rule(
    implementation = _fake_arm64_generated_headers_impl,
)

def _fake_legacy_object_impl(ctx):
    output = ctx.actions.declare_file(ctx.label.name + ".o")
    ctx.actions.write(output, "")
    info = LinuxObjectInfo(
        content_id = ctx.attr.content_id,
        generated_headers = depset([]),
        generated_include_dir_anchors = {},
        generated_include_dirs = [],
        mode = ctx.attr.mode,
        module_root_kind = "",
        object = ctx.attr.object,
        objtool_args = [],
        objtool_force = False,
        output = output,
    )
    return [
        DefaultInfo(files = depset([output])),
        info,
    ]

_fake_legacy_object = rule(
    implementation = _fake_legacy_object_impl,
    attrs = {
        "content_id": attr.string(mandatory = True),
        "mode": attr.string(mandatory = True, values = ["m", "y"]),
        "object": attr.string(mandatory = True),
    },
)

def _object_spec(
        content_id,
        object_path,
        source_files,
        primary_source = 1,
        action_source_group = _ACTION_SOURCE_GROUP_ID,
        compile_environment = _COMPILE_ENVIRONMENT_ID):
    return json.encode({
        "action_source_group": action_source_group,
        "compile_environment": compile_environment,
        "content_id": content_id,
        "deps": [],
        "members": [],
        "object": object_path,
        "primary_source": primary_source,
        "source_files": source_files,
    })

def _actions_with_mnemonic(actions, mnemonic):
    return [action for action in actions if action.mnemonic == mnemonic]

def _argument_after(argv, flag):
    for index in range(len(argv) - 1):
        if argv[index] == flag:
            return argv[index + 1]
    return ""

def _aggregate_spec(content_id, object_path, members):
    return json.encode({
        "content_id": content_id,
        "deps": [],
        "members": members,
        "object": object_path,
    })

def _group_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    info = target[LinuxObjectActionGroupInfo]
    actions = analysistest.target_actions(env)
    compiles = _actions_with_mnemonic(actions, "LinuxObjectCompile")
    objtools = _actions_with_mnemonic(actions, "LinuxObjectObjtool")

    asserts.equals(env, "y", info.mode)
    asserts.equals(env, _RECIPE_ID, info.recipe_id)
    asserts.equals(env, _REACHABILITY_ID, info.reachability_id)
    asserts.equals(env, ["base", "debug"], info.reachable_configs)
    asserts.equals(env, ["first", "second"], info.object_targets)
    asserts.equals(env, 2, len(info.objects))
    asserts.equals(env, 2, len(compiles))
    asserts.equals(env, 2, len(objtools))
    asserts.equals(env, 0, len(_actions_with_mnemonic(actions, "LinuxFlagFilter")))

    outputs = {}
    for action in compiles:
        asserts.equals(env, 1, len(action.outputs.to_list()))
        if action.outputs.to_list():
            outputs[action.outputs.to_list()[0].path] = action
        asserts.true(env, "compile" in action.argv)
        asserts.true(env, "-flags_file" in action.argv)
        asserts.true(env, "-remove_flags_file" in action.argv)
        asserts.true(env, "-remove=-fcolor-diagnostics" in action.argv)
    asserts.equals(env, 2, len(outputs))
    objtool_inputs = {}
    objtool_outputs = {}
    for action in objtools:
        asserts.true(env, "-force" in action.argv)
        asserts.true(env, "-objtool_arg=--noabs" in action.argv)
        asserts.equals(env, "builtin", _argument_after(action.argv, "-mode"))
        for file in action.inputs.to_list():
            objtool_inputs[file.path] = True
        for file in action.outputs.to_list():
            objtool_outputs[file.path] = True
    for path in outputs.keys():
        asserts.true(env, path in objtool_inputs)
    for object_info in info.objects.values():
        asserts.true(env, object_info.output.path in objtool_outputs)

    first_action = None
    second_action = None
    for path, action in outputs.items():
        if _FIRST_CONTENT_ID in path:
            first_action = action
        if _SECOND_CONTENT_ID in path:
            second_action = action
    asserts.true(env, first_action != None)
    asserts.true(env, second_action != None)
    if first_action != None and second_action != None:
        first_inputs = [file.basename for file in first_action.inputs.to_list()]
        second_inputs = [file.basename for file in second_action.inputs.to_list()]
        asserts.false(env, "unrelated.inc" in first_inputs)
        asserts.true(env, "unrelated.inc" in second_inputs)
        asserts.true(env, "second.inc" in first_inputs)
        asserts.false(env, "second.inc" in second_inputs)
        first_input_paths = [file.path for file in first_action.inputs.to_list()]
        second_input_paths = [file.path for file in second_action.inputs.to_list()]
        asserts.true(env, _CONFIG_PAYLOAD_ID in "\n".join(first_input_paths))
        asserts.false(env, _SECOND_CONFIG_PAYLOAD_ID in "\n".join(first_input_paths))
        asserts.true(env, _SECOND_CONFIG_PAYLOAD_ID in "\n".join(second_input_paths))
        asserts.false(env, _CONFIG_PAYLOAD_ID in "\n".join(second_input_paths))
    return analysistest.end(env)

_group_test = analysistest.make(_group_test_impl)

def _disabled_objtool_group_test_impl(ctx):
    env = analysistest.begin(ctx)
    actions = analysistest.target_actions(env)
    asserts.equals(env, 1, len(_actions_with_mnemonic(actions, "LinuxObjectCompile")))
    asserts.equals(env, 0, len(_actions_with_mnemonic(actions, "LinuxObjectObjtool")))
    return analysistest.end(env)

_disabled_objtool_group_test = analysistest.make(_disabled_objtool_group_test_impl)

def _import_group_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    info = target[LinuxObjectActionGroupInfo]
    asserts.equals(env, "y", info.mode)
    asserts.equals(env, _IMPORT_RECIPE_ID, info.recipe_id)
    asserts.equals(env, _REACHABILITY_ID, info.reachability_id)
    asserts.equals(env, ["base", "debug"], info.reachable_configs)
    asserts.equals(env, ["legacy_first", "legacy_second"], info.object_targets)
    asserts.equals(env, 2, len(info.objects))
    asserts.equals(env, 0, len(analysistest.target_actions(env)))
    return analysistest.end(env)

_import_group_test = analysistest.make(_import_group_test_impl)

def _composite_group_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    info = target[LinuxObjectActionGroupInfo]
    actions = analysistest.target_actions(env)
    links = _actions_with_mnemonic(actions, "LinuxCompositeObject")

    asserts.equals(env, ["composite"], info.object_targets)
    asserts.equals(env, 1, len(links))
    asserts.equals(env, 0, len(_actions_with_mnemonic(actions, "LinuxObjectCompile")))
    if links:
        inputs = [file.basename for file in links[0].inputs.to_list()]
        asserts.true(env, "first.o" in inputs)
        asserts.true(env, "second.o" in inputs)
        asserts.true(env, "-base_arg" in links[0].argv)
        asserts.true(env, "-r" in links[0].argv)
    return analysistest.end(env)

_composite_group_test = analysistest.make(_composite_group_test_impl)

def _nvhe_group_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    info = target[LinuxObjectActionGroupInfo]
    actions = analysistest.target_actions(env)

    asserts.equals(env, ["nvhe"], info.object_targets)
    asserts.equals(env, 0, len(_actions_with_mnemonic(actions, "LinuxObjectCompile")))
    asserts.equals(env, 1, len(_actions_with_mnemonic(actions, "LinuxArm64NvheLinkerScript")))
    asserts.equals(env, 2, len(_actions_with_mnemonic(actions, "LinuxRelocatableLink")))
    asserts.equals(env, 1, len(_actions_with_mnemonic(actions, "LinuxArm64NvheHypReloc")))
    asserts.equals(env, 1, len(_actions_with_mnemonic(actions, "LinuxVmlinuxCompile")))
    asserts.equals(env, 1, len(_actions_with_mnemonic(actions, "LinuxArm64NvheObjcopy")))

    linker_script = _actions_with_mnemonic(actions, "LinuxArm64NvheLinkerScript")
    if linker_script:
        inputs = [file.basename for file in linker_script[0].inputs.to_list()]
        asserts.true(env, "hyp.lds.S" in inputs)
        asserts.true(env, "compiler-version.h" in inputs)
        asserts.true(env, "kconfig.h" in inputs)
    member_links = [
        action
        for action in _actions_with_mnemonic(actions, "LinuxRelocatableLink")
        if "arm_member.o" in [file.basename for file in action.inputs.to_list()]
    ]
    asserts.equals(env, 1, len(member_links))
    return analysistest.end(env)

_nvhe_group_test = analysistest.make(
    _nvhe_group_test_impl,
    config_settings = {
        "//command_line_option:platforms": str(Label("@llvm//platforms:linux_arm64")),
    },
)

def _projection_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    info = target[LinuxObjectProjectionInfo]

    asserts.equals(env, ctx.attr.expected_config, info.config)
    asserts.equals(env, ctx.attr.expected_mode, info.mode)
    asserts.equals(env, ctx.attr.expected_targets, info.object_targets)
    asserts.equals(env, len(ctx.attr.expected_targets), len(info.objects))
    asserts.equals(env, 0, len(analysistest.target_actions(env)))
    for index in range(len(ctx.attr.expected_targets)):
        target_name = ctx.attr.expected_targets[index]
        asserts.equals(env, info.objects_by_target[target_name], info.objects[index])
        asserts.equals(env, ctx.attr.expected_mode, info.objects[index].mode)
    return analysistest.end(env)

_projection_test = analysistest.make(
    _projection_test_impl,
    attrs = {
        "expected_config": attr.string(mandatory = True),
        "expected_mode": attr.string(mandatory = True),
        "expected_targets": attr.string_list(),
    },
)

_arm64_projection_test = analysistest.make(
    _projection_test_impl,
    attrs = {
        "expected_config": attr.string(mandatory = True),
        "expected_mode": attr.string(mandatory = True),
        "expected_targets": attr.string_list(),
    },
    config_settings = {
        "//command_line_option:platforms": str(Label("@llvm//platforms:linux_arm64")),
    },
)

def _grouped_image_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    info = target[LinuxImageInfo]
    actions = analysistest.target_actions(env)
    archives = _actions_with_mnemonic(actions, "LinuxCompactImageArchive")

    asserts.equals(env, 2, len(info.objects))
    asserts.equals(env, 0, len(info.module_objects))
    asserts.equals(env, 1, len(archives))
    asserts.equals(env, info.output, target[DefaultInfo].files.to_list()[0])
    return analysistest.end(env)

_grouped_image_test = analysistest.make(_grouped_image_test_impl)

def _grouped_modules_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    info = target[LinuxModuleObjectsInfo]

    asserts.equals(env, 1, len(info.objects))
    asserts.equals(env, "m", info.objects[0].mode)
    asserts.equals(env, 0, len(analysistest.target_actions(env)))
    return analysistest.end(env)

_grouped_modules_test = analysistest.make(_grouped_modules_test_impl)

def _failure_test_impl(ctx):
    env = analysistest.begin(ctx)
    asserts.expect_failure(env, ctx.attr.expected_error)
    return analysistest.end(env)

_failure_test = analysistest.make(
    _failure_test_impl,
    attrs = {
        "expected_error": attr.string(mandatory = True),
    },
    expect_failure = True,
)

def _static_flag_programs(name, graph_profile):
    linux_flag_programs(
        name = name,
        graph_profile = graph_profile,
        nodes = {},
        probes = {},
        programs = {
            _EMPTY_PROGRAM_ID: _EMPTY_TERMINAL_ID,
            _FLAGS_PROGRAM_ID: _FLAGS_TERMINAL_ID,
            _REMOVE_PROGRAM_ID: _REMOVE_TERMINAL_ID,
        },
        source_root = "//tests/compile:source/Kconfig",
        terminals = {
            _EMPTY_TERMINAL_ID: json.encode([]),
            _FLAGS_TERMINAL_ID: json.encode([
                "-DNO_GCSE=$(cflags-nogcse-yy)",
                "-Wall",
                "-Werror",
            ]),
            _REMOVE_TERMINAL_ID: json.encode(["-Werror"]),
        },
        tags = ["manual"],
    )

def linux_object_groups_test_suite(name):
    fake_objtool = name + "_fake_objtool"
    native.genrule(
        name = fake_objtool,
        srcs = ["fake_objtool.sh"],
        outs = [fake_objtool + ".tool"],
        cmd = "cp $(location fake_objtool.sh) $@",
        executable = True,
        tags = ["manual"],
    )

    profile = name + "_graph_profile"
    linux_graph_profile_context(
        name = profile,
        arch = "x86_64",
        graph_projection = ":graph_projection.json",
        kbuild_linker = "@llvm//tools:ld.lld",
        source_root = "//tests/compile:source/Kconfig",
        tags = ["manual"],
    )
    flag_programs = name + "_flag_programs"
    _static_flag_programs(
        name = flag_programs,
        graph_profile = ":" + profile,
    )

    environments = name + "_compile_environments"
    linux_compile_environment_index(
        name = environments,
        arch = "x86",
        compile_environments = {
            _COMPILE_ENVIRONMENT_ID: json.encode({
                "abi": "tests/object-groups/x86",
                "config_payload": _CONFIG_PAYLOAD_ID,
                "generated_header_families": [],
            }),
            _SECOND_COMPILE_ENVIRONMENT_ID: json.encode({
                "abi": "tests/object-groups/x86",
                "config_payload": _SECOND_CONFIG_PAYLOAD_ID,
                "generated_header_families": [],
            }),
        },
        config_payload_files = {
            ":object_groups_base.config": _CONFIG_PAYLOAD_ID,
            ":object_groups_expert.config": _SECOND_CONFIG_PAYLOAD_ID,
        },
        expected_abi = "tests/object-groups/x86",
        tags = ["manual"],
    )

    group = name + "_group"
    linux_object_action_group(
        name = group,
        arch = "x86",
        compile_environment_index = ":" + environments,
        flag_program = _FLAGS_PROGRAM_ID,
        flag_programs = ":" + flag_programs,
        flag_effects = ["argv"],
        graph_profile = ":" + profile,
        language = "c",
        mode = "y",
        objtool = ":" + fake_objtool,
        objtool_args = ["--noabs"],
        objtool_force = True,
        objects = {
            "first": _object_spec(
                _FIRST_CONTENT_ID,
                "cross_tree/first.o",
                [1, 4, 5, 6, 7, 8],
            ),
            "second": _object_spec(
                _SECOND_CONTENT_ID,
                "cross_tree/second.o",
                [2, 3, 4, 5, 6],
                primary_source = 2,
                action_source_group = _SECOND_ACTION_SOURCE_GROUP_ID,
                compile_environment = _SECOND_COMPILE_ENVIRONMENT_ID,
            ),
        },
        reachable_configs = [
            "base",
            "debug",
        ],
        reachability_id = _REACHABILITY_ID,
        recipe_id = _RECIPE_ID,
        remove_flag_program = _REMOVE_PROGRAM_ID,
        remove_flag_effects = ["argv"],
        source_paths = _SOURCE_PATHS,
        source_root = "//tests/compile:source/Kconfig",
        srcarch = "x86",
        srcs = _SRCS,
        tags = ["manual"],
        toolchain_remove_flags = ["-fcolor-diagnostics"],
    )

    group_test = name + "_group_test"
    _group_test(
        name = group_test,
        target_under_test = ":" + group,
    )
    group_build_test = name + "_group_build_test"
    build_test(
        name = group_build_test,
        targets = [":" + group],
    )

    fdt_group = name + "_fdt_group"
    linux_object_action_group(
        name = fdt_group,
        arch = "x86",
        compile_environment_index = ":" + environments,
        flag_program = _EMPTY_PROGRAM_ID,
        flag_programs = ":" + flag_programs,
        flag_effects = ["argv"],
        graph_profile = ":" + profile,
        language = "c",
        mode = "y",
        objtool = ":" + fake_objtool,
        objects = {
            "fdt": _object_spec(
                _FDT_CONTENT_ID,
                "lib/fdt_ro.o",
                [1, 2, 3, 4],
                primary_source = 4,
            ),
        },
        objtool_disabled = True,
        reachable_configs = ["base"],
        reachability_id = _REACHABILITY_ID,
        recipe_id = _FDT_RECIPE_ID,
        remove_flag_program = _EMPTY_PROGRAM_ID,
        remove_flag_effects = ["argv"],
        source_paths = [
            "include/linux/compiler-version.h",
            "include/linux/compiler_types.h",
            "include/linux/kconfig.h",
            "lib/fdt_ro.c",
        ],
        source_root = "//tests/compile:source/Kconfig",
        srcarch = "x86",
        srcs = [
            "//tests/compile:source/include/linux/compiler-version.h",
            "//tests/compile:source/include/linux/compiler_types.h",
            "//tests/compile:source/include/linux/kconfig.h",
            "//tests/compile:source/lib/fdt_ro.c",
        ],
        tags = ["manual"],
    )
    fdt_build_test = name + "_fdt_build_test"
    build_test(
        name = fdt_build_test,
        targets = [":" + fdt_group],
    )
    fdt_group_test = name + "_fdt_group_test"
    _disabled_objtool_group_test(
        name = fdt_group_test,
        target_under_test = ":" + fdt_group,
    )

    legacy_first = name + "_legacy_first"
    _fake_legacy_object(
        name = legacy_first,
        content_id = _IMPORT_FIRST_CONTENT_ID,
        mode = "y",
        object = "legacy/first.o",
        tags = ["manual"],
    )
    legacy_second = name + "_legacy_second"
    _fake_legacy_object(
        name = legacy_second,
        content_id = _IMPORT_SECOND_CONTENT_ID,
        mode = "y",
        object = "legacy/second.o",
        tags = ["manual"],
    )
    imported_group = name + "_imported_group"
    linux_object_action_group_import(
        name = imported_group,
        object_targets = [
            "legacy_first",
            "legacy_second",
        ],
        objects = [
            ":" + legacy_first,
            ":" + legacy_second,
        ],
        reachable_configs = [
            "base",
            "debug",
        ],
        reachability_id = _REACHABILITY_ID,
        recipe_id = _IMPORT_RECIPE_ID,
        tags = ["manual"],
    )
    imported_group_test = name + "_imported_group_test"
    _import_group_test(
        name = imported_group_test,
        target_under_test = ":" + imported_group,
    )
    mixed_projection = name + "_mixed_projection"
    linux_image_object_projection(
        name = mixed_projection,
        config = "base",
        groups = [
            ":" + group,
            ":" + imported_group,
        ],
        object_targets = [
            "legacy_second",
            "first",
        ],
        tags = ["manual"],
    )
    mixed_projection_test = name + "_mixed_projection_test"
    _projection_test(
        name = mixed_projection_test,
        expected_config = "base",
        expected_mode = "y",
        expected_targets = [
            "legacy_second",
            "first",
        ],
        target_under_test = ":" + mixed_projection,
    )

    duplicate_legacy = name + "_duplicate_legacy"
    _fake_legacy_object(
        name = duplicate_legacy,
        content_id = _IMPORT_FIRST_CONTENT_ID,
        mode = "y",
        object = "legacy/duplicate.o",
        tags = ["manual"],
    )
    duplicate_import = name + "_duplicate_import"
    linux_object_action_group_import(
        name = duplicate_import,
        object_targets = [
            "legacy_duplicate",
            "legacy_first",
        ],
        objects = [
            ":" + duplicate_legacy,
            ":" + legacy_first,
        ],
        reachable_configs = ["base"],
        reachability_id = _REACHABILITY_ID,
        recipe_id = _IMPORT_RECIPE_ID,
        tags = ["manual"],
    )
    duplicate_import_test = name + "_duplicate_import_test"
    _failure_test(
        name = duplicate_import_test,
        expected_error = "share content ID",
        target_under_test = ":" + duplicate_import,
    )

    module_legacy = name + "_module_legacy"
    _fake_legacy_object(
        name = module_legacy,
        content_id = _IMPORT_MODULE_CONTENT_ID,
        mode = "m",
        object = "legacy/module.o",
        tags = ["manual"],
    )
    mixed_mode_import = name + "_mixed_mode_import"
    linux_object_action_group_import(
        name = mixed_mode_import,
        object_targets = [
            "legacy_first",
            "legacy_module",
        ],
        objects = [
            ":" + legacy_first,
            ":" + module_legacy,
        ],
        reachable_configs = ["base"],
        reachability_id = _REACHABILITY_ID,
        recipe_id = _IMPORT_RECIPE_ID,
        tags = ["manual"],
    )
    mixed_mode_import_test = name + "_mixed_mode_import_test"
    _failure_test(
        name = mixed_mode_import_test,
        expected_error = "one shared object mode",
        target_under_test = ":" + mixed_mode_import,
    )

    image_projection = name + "_image_projection"
    linux_image_object_projection(
        name = image_projection,
        config = "base",
        groups = [":" + group],
        object_targets = [
            "second",
            "first",
        ],
        tags = ["manual"],
    )
    image_projection_test = name + "_image_projection_test"
    _projection_test(
        name = image_projection_test,
        expected_config = "base",
        expected_mode = "y",
        expected_targets = [
            "second",
            "first",
        ],
        target_under_test = ":" + image_projection,
    )
    grouped_image = name + "_grouped_image"
    linux_grouped_image(
        name = grouped_image,
        graph_profile = ":" + profile,
        objects = ":" + image_projection,
        tags = ["manual"],
    )
    grouped_image_test = name + "_grouped_image_test"
    _grouped_image_test(
        name = grouped_image_test,
        target_under_test = ":" + grouped_image,
    )

    composite_group = name + "_composite_group"
    linux_composite_object_action_group(
        name = composite_group,
        arch = "x86",
        flag_program = _EMPTY_PROGRAM_ID,
        flag_programs = ":" + flag_programs,
        graph_profile = ":" + profile,
        member_groups = [":" + group],
        mode = "y",
        objects = {
            "composite": _aggregate_spec(
                _COMPOSITE_CONTENT_ID,
                "cross_tree/composite.o",
                [
                    "first",
                    "second",
                ],
            ),
        },
        reachable_configs = [
            "base",
            "debug",
        ],
        reachability_id = _REACHABILITY_ID,
        recipe_id = _COMPOSITE_RECIPE_ID,
        remove_flag_program = _EMPTY_PROGRAM_ID,
        relocatable_link_flags = ["-r"],
        tags = ["manual"],
    )
    composite_group_test = name + "_composite_group_test"
    _composite_group_test(
        name = composite_group_test,
        target_under_test = ":" + composite_group,
    )
    composite_build_test = name + "_composite_build_test"
    build_test(
        name = composite_build_test,
        targets = [":" + composite_group],
    )
    composite_projection = name + "_composite_projection"
    linux_image_object_projection(
        name = composite_projection,
        config = "base",
        groups = [":" + composite_group],
        object_targets = ["composite"],
        tags = ["manual"],
    )
    composite_projection_test = name + "_composite_projection_test"
    _projection_test(
        name = composite_projection_test,
        expected_config = "base",
        expected_mode = "y",
        expected_targets = ["composite"],
        target_under_test = ":" + composite_projection,
    )

    module_group = name + "_module_group"
    linux_object_action_group(
        name = module_group,
        arch = "x86",
        compile_environment_index = ":" + environments,
        flag_program = _EMPTY_PROGRAM_ID,
        flag_programs = ":" + flag_programs,
        flag_effects = ["argv"],
        graph_profile = ":" + profile,
        language = "c",
        mode = "m",
        module_root = True,
        objtool = ":" + fake_objtool,
        objects = {
            "module": _object_spec(
                _MODULE_CONTENT_ID,
                "cross_tree/module.o",
                [1, 4, 5, 6, 7, 8],
            ),
        },
        reachable_configs = ["base"],
        reachability_id = _REACHABILITY_ID,
        recipe_id = _MODULE_RECIPE_ID,
        remove_flag_program = _EMPTY_PROGRAM_ID,
        remove_flag_effects = ["argv"],
        source_paths = _SOURCE_PATHS,
        source_root = "//tests/compile:source/Kconfig",
        srcarch = "x86",
        srcs = _SRCS,
        tags = ["manual"],
    )
    module_projection = name + "_module_projection"
    linux_module_object_projection(
        name = module_projection,
        config = "base",
        groups = [":" + module_group],
        object_targets = ["module"],
        tags = ["manual"],
    )
    module_projection_test = name + "_module_projection_test"
    _projection_test(
        name = module_projection_test,
        expected_config = "base",
        expected_mode = "m",
        expected_targets = ["module"],
        target_under_test = ":" + module_projection,
    )
    grouped_modules = name + "_grouped_modules"
    linux_grouped_modules(
        name = grouped_modules,
        objects = ":" + module_projection,
        tags = ["manual"],
    )
    grouped_modules_test = name + "_grouped_modules_test"
    _grouped_modules_test(
        name = grouped_modules_test,
        target_under_test = ":" + grouped_modules,
    )

    nvhe_profile = name + "_nvhe_graph_profile"
    linux_graph_profile_context(
        name = nvhe_profile,
        arch = "aarch64",
        graph_projection = ":graph_projection_aarch64.json",
        kbuild_linker = "@llvm//tools:ld.lld",
        source_root = "//tests/compile:source/Kconfig",
        tags = ["manual"],
    )
    nvhe_flag_programs = name + "_nvhe_flag_programs"
    _static_flag_programs(
        name = nvhe_flag_programs,
        graph_profile = ":" + nvhe_profile,
    )
    nvhe_headers = name + "_nvhe_generated_headers"
    _fake_arm64_generated_headers(
        name = nvhe_headers,
        tags = ["manual"],
    )
    nvhe_environments = name + "_nvhe_compile_environments"
    linux_compile_environment_index(
        name = nvhe_environments,
        arch = "arm64",
        compile_environments = {
            _NVHE_COMPILE_ENVIRONMENT_ID: json.encode({
                "abi": "tests/object-groups/arm64",
                "config_payload": _NVHE_CONFIG_PAYLOAD_ID,
                "generated_header_families": [_NVHE_FAMILY_ID],
            }),
        },
        config_payloads = {
            _NVHE_CONFIG_PAYLOAD_ID: "CONFIG_CC_IS_CLANG=y\n",
        },
        expected_abi = "tests/object-groups/arm64",
        generated_headers = [":" + nvhe_headers],
        tags = ["manual"],
    )
    nvhe_member_group = name + "_nvhe_member_group"
    linux_object_action_group(
        name = nvhe_member_group,
        arch = "arm64",
        compile_environment_index = ":" + nvhe_environments,
        flag_program = _EMPTY_PROGRAM_ID,
        flag_programs = ":" + nvhe_flag_programs,
        flag_effects = ["argv"],
        graph_profile = ":" + nvhe_profile,
        language = "c",
        mode = "y",
        objects = {
            "arm_member": _object_spec(
                _NVHE_MEMBER_CONTENT_ID,
                "arch/arm64/kvm/hyp/nvhe/arm_member.o",
                [1, 2, 3, 4],
                compile_environment = _NVHE_COMPILE_ENVIRONMENT_ID,
            ),
        },
        reachable_configs = ["base"],
        reachability_id = _NVHE_REACHABILITY_ID,
        recipe_id = _NVHE_MEMBER_RECIPE_ID,
        remove_flag_program = _EMPTY_PROGRAM_ID,
        remove_flag_effects = ["argv"],
        source_paths = _NVHE_MEMBER_SOURCE_PATHS,
        source_root = "//tests/compile:source/Kconfig",
        srcarch = "arm64",
        srcs = _NVHE_MEMBER_SRCS,
        tags = ["manual"],
    )
    nvhe_group = name + "_nvhe_group"
    linux_arm64_nvhe_object_action_group(
        name = nvhe_group,
        compile_environment_index = ":" + nvhe_environments,
        flag_program = _EMPTY_PROGRAM_ID,
        flag_programs = ":" + nvhe_flag_programs,
        graph_profile = ":" + nvhe_profile,
        member_groups = [":" + nvhe_member_group],
        objcopy = "@llvm//tools:llvm-objcopy",
        objects = {
            "nvhe": json.encode({
                "action_source_group": _NVHE_ACTION_SOURCE_GROUP_ID,
                "compile_environment": _NVHE_COMPILE_ENVIRONMENT_ID,
                "content_id": _NVHE_CONTENT_ID,
                "deps": [],
                "members": ["arm_member"],
                "object": "arch/arm64/kvm/hyp/nvhe/kvm_nvhe.o",
                "primary_source": 1,
                "source_files": [1, 2, 3],
            }),
        },
        reachable_configs = ["base"],
        reachability_id = _NVHE_REACHABILITY_ID,
        recipe_id = _NVHE_RECIPE_ID,
        remove_flag_program = _EMPTY_PROGRAM_ID,
        relocatable_link_flags = ["-r"],
        source_paths = _NVHE_SOURCE_PATHS,
        source_root = "//tests/compile:source/Kconfig",
        srcs = _NVHE_SRCS,
        tags = ["manual"],
    )
    nvhe_group_test = name + "_nvhe_group_test"
    _nvhe_group_test(
        name = nvhe_group_test,
        target_under_test = ":" + nvhe_group,
    )
    nvhe_projection = name + "_nvhe_projection"
    linux_image_object_projection(
        name = nvhe_projection,
        config = "base",
        groups = [":" + nvhe_group],
        object_targets = ["nvhe"],
        tags = ["manual"],
    )
    nvhe_projection_test = name + "_nvhe_projection_test"
    _arm64_projection_test(
        name = nvhe_projection_test,
        expected_config = "base",
        expected_mode = "y",
        expected_targets = ["nvhe"],
        target_under_test = ":" + nvhe_projection,
    )

    unreachable = name + "_unreachable"
    linux_image_object_projection(
        name = unreachable,
        config = "btf",
        groups = [":" + group],
        object_targets = ["first"],
        tags = ["manual"],
    )
    unreachable_test = name + "_unreachable_test"
    _failure_test(
        name = unreachable_test,
        expected_error = "is outside group",
        target_under_test = ":" + unreachable,
    )

    native.test_suite(
        name = name,
        tests = [
            ":" + group_test,
            ":" + group_build_test,
            ":" + fdt_build_test,
            ":" + fdt_group_test,
            ":" + imported_group_test,
            ":" + mixed_projection_test,
            ":" + duplicate_import_test,
            ":" + mixed_mode_import_test,
            ":" + image_projection_test,
            ":" + grouped_image_test,
            ":" + composite_group_test,
            ":" + composite_build_test,
            ":" + composite_projection_test,
            ":" + module_projection_test,
            ":" + grouped_modules_test,
            ":" + nvhe_group_test,
            ":" + nvhe_projection_test,
            ":" + unreachable_test,
        ],
    )
