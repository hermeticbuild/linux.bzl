"""Analysis tests for per-translation-unit Linux objtool actions."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load(
    "//internal:linux_objects.bzl",
    "linux_config",
    "linux_object",
    "linux_source_tree",
)

visibility("private")

def _argument_after(argv, flag):
    for index in range(len(argv) - 1):
        if argv[index] == flag:
            return argv[index + 1]
    return ""

def _linux_object_objtool_action_test_impl(ctx):
    env = analysistest.begin(ctx)
    actions = analysistest.target_actions(env)
    compile_actions = [
        action
        for action in actions
        if action.mnemonic == "LinuxObjectCompile"
    ]
    objtool_actions = [
        action
        for action in actions
        if action.mnemonic == "LinuxObjectObjtool"
    ]
    objcopy_actions = [
        action
        for action in actions
        if action.mnemonic == "LinuxObjectObjcopy"
    ]
    link_actions = [
        action
        for action in actions
        if action.mnemonic == "LinuxRelocatableLink"
    ]
    asserts.equals(env, 1, len(compile_actions))
    asserts.equals(env, 1 if ctx.attr.expect_objtool else 0, len(objtool_actions))
    asserts.equals(env, 1 if ctx.attr.expect_objcopy else 0, len(objcopy_actions))
    asserts.equals(env, 1 if ctx.attr.expect_link else 0, len(link_actions))
    compile_outputs = []
    if compile_actions:
        compile_outputs = [file.path for file in compile_actions[0].outputs.to_list()]
        asserts.equals(env, 1, len(compile_outputs))
    link_outputs = []
    if compile_outputs and ctx.attr.expect_link and link_actions:
        link_inputs = [file.path for file in link_actions[0].inputs.to_list()]
        link_outputs = [file.path for file in link_actions[0].outputs.to_list()]
        asserts.true(
            env,
            compile_outputs[0] in link_inputs,
            "relocatable link does not consume compile output",
        )
        asserts.equals(env, 1, len(link_outputs))
    if objtool_actions:
        objtool_action = objtool_actions[0]
        asserts.equals(
            env,
            ctx.attr.expected_mode,
            _argument_after(objtool_action.argv, "-mode"),
        )
        asserts.equals(
            env,
            "CONFIG_FTRACE_MCOUNT_USE_OBJTOOL=y",
            _argument_after(objtool_action.argv, "-config_value"),
        )
        if ctx.attr.expect_force:
            asserts.true(env, "-force" in objtool_action.argv)
            asserts.true(env, "-objtool_arg=--noabs" in objtool_action.argv)
        objtool_inputs = [file.path for file in objtool_action.inputs.to_list()]
        if compile_outputs and not ctx.attr.expect_link:
            asserts.true(
                env,
                compile_outputs[0] in objtool_inputs,
                "objtool inputs %s do not contain compile output %s" % (
                    objtool_inputs,
                    compile_outputs[0],
                ),
            )
        if link_outputs:
            asserts.true(
                env,
                link_outputs[0] in objtool_inputs,
                "objtool does not consume relocatable link output",
            )
        if ctx.attr.expect_objcopy and objcopy_actions:
            objtool_outputs = [file.path for file in objtool_action.outputs.to_list()]
            objcopy_inputs = [file.path for file in objcopy_actions[0].inputs.to_list()]
            asserts.equals(env, 1, len(objtool_outputs))
            if objtool_outputs:
                asserts.true(
                    env,
                    objtool_outputs[0] in objcopy_inputs,
                    "objcopy inputs %s do not contain objtool output %s" % (
                        objcopy_inputs,
                        objtool_outputs[0],
                    ),
                )
            if compile_outputs:
                asserts.false(
                    env,
                    compile_outputs[0] in objcopy_inputs,
                    "objcopy consumes compile output directly instead of objtool output",
                )
    return analysistest.end(env)

_linux_object_objtool_action_test = analysistest.make(
    _linux_object_objtool_action_test_impl,
    attrs = {
        "expected_mode": attr.string(mandatory = True),
        "expect_force": attr.bool(),
        "expect_link": attr.bool(),
        "expect_objcopy": attr.bool(),
        "expect_objtool": attr.bool(default = True),
    },
)

def linux_object_objtool_test_suite(name):
    config = name + "_config"
    source_tree = name + "_source_tree"
    fixture_tags = ["manual"]
    linux_config(
        name = config,
        arch = "x86",
        config_flags = {
            "CONFIG_FTRACE_MCOUNT_USE_OBJTOOL": "n",
            "CONFIG_OBJTOOL": "y",
        },
        tags = fixture_tags,
    )
    linux_source_tree(
        name = source_tree,
        root = "linux_objects_test_fixture.c",
        tags = fixture_tags,
    )

    tests = []
    for suffix, object_mode, objtool_mode, force, delayed, module_root, transformed, use_objtool, assembly in [
        ("builtin", "y", "builtin", False, False, False, False, True, False),
        ("forced", "y", "builtin", True, False, False, True, True, False),
        ("module_member", "m", "module-member", False, False, False, False, True, False),
        ("module_delayed_member", "m", "module-member", False, True, False, False, True, False),
        ("module_forced_lto", "m", "module-member", True, True, False, False, True, False),
        ("module_single_lto", "m", "module-single", False, True, True, False, True, False),
        ("module_single_lto_assembly", "m", "module-single", False, True, True, False, True, True),
        ("module_single_lto_nonstandard", "m", "module-single", False, True, True, False, False, False),
    ]:
        object_target = name + "_" + suffix + "_object"
        config_fragment = {
            "CONFIG_FTRACE_MCOUNT_USE_OBJTOOL": "y",
        }
        if delayed:
            config_fragment["CONFIG_LTO_CLANG"] = "y"
        linux_object(
            name = object_target,
            config = ":" + config,
            config_fragment = config_fragment,
            mode = object_mode,
            module_root = module_root,
            object = "arch/x86/boot/startup/test.pi.o" if transformed else "test.o",
            objtool = "//internal/cmd/runandwrite" if use_objtool else None,
            objtool_args = ["--noabs"] if force else [],
            objtool_force = force,
            source_tree_info = ":" + source_tree,
            src = "linux_objects_test_fixture.S" if assembly else "linux_objects_test_fixture.c",
            tags = fixture_tags,
        )
        test = name + "_" + suffix
        _linux_object_objtool_action_test(
            name = test,
            expected_mode = objtool_mode,
            expect_force = force,
            expect_link = delayed and not assembly and object_mode == "m" and (module_root or force),
            expect_objcopy = transformed,
            expect_objtool = use_objtool,
            target_under_test = ":" + object_target,
        )
        tests.append(":" + test)

    native.test_suite(
        name = name,
        tests = tests,
    )
