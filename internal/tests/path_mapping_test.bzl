"""Analysis test for File-backed directory arguments."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load(
    "//internal:compact_generator.bzl",
    "linux_compact_v7_metadata",
    "linux_kbuild_tree_validation",
    "linux_parser_validation",
)
load(
    "//internal:path_mapping.bzl",
    "add_directory_arg",
    "add_mapped_values",
    "directory_anchor",
    "path_mapped_run",
)

visibility("private")

def _directory_argument_probe_impl(ctx):
    anchor = ctx.actions.declare_file(ctx.label.name + ".tree/include/generated/anchor.h")
    runtime_tree = ctx.actions.declare_directory(ctx.label.name + ".runtime")
    out = ctx.actions.declare_file(ctx.label.name + ".out")
    ctx.actions.write(anchor, "")

    args = ctx.actions.args()
    add_directory_arg(
        args,
        directory_anchor(anchor, anchor.dirname.rsplit("/", 1)[0]),
        format = "-I%s",
    )
    add_mapped_values(
        args,
        ["-L" + runtime_tree.path + "/lib"],
        files = [runtime_tree],
    )
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._tool,
        inputs = [anchor],
        outputs = [out, runtime_tree],
        arguments = [args],
        mnemonic = "PathMappedDirectoryProbe",
    )
    return [DefaultInfo(files = depset([out]))]

_directory_argument_probe = rule(
    implementation = _directory_argument_probe_impl,
    attrs = {
        "_tool": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/runandwrite"),
            executable = True,
        ),
    },
)

def _directory_argument_test_impl(ctx):
    env = analysistest.begin(ctx)
    actions = [
        action
        for action in analysistest.target_actions(env)
        if action.mnemonic == "PathMappedDirectoryProbe"
    ]
    asserts.equals(env, 1, len(actions))
    if actions:
        directory_args = [arg for arg in actions[0].argv if arg.startswith("-I")]
        asserts.equals(env, 1, len(directory_args))
        if directory_args:
            asserts.true(
                env,
                directory_args[0].startswith("-Ibazel-out/cfg/bin/"),
                "expected stripped output path, got %s" % directory_args[0],
            )
        tree_args = [arg for arg in actions[0].argv if arg.startswith("-L")]
        asserts.equals(env, 1, len(tree_args))
        if tree_args:
            asserts.true(
                env,
                tree_args[0].startswith("-Lbazel-out/cfg/bin/"),
                "expected mapped TreeArtifact path, got %s" % tree_args[0],
            )
    return analysistest.end(env)

_directory_argument_test = analysistest.make(_directory_argument_test_impl)

def _fixture_file_impl(ctx):
    out = ctx.actions.declare_file(ctx.attr.out)
    ctx.actions.write(out, ctx.attr.content)
    return [DefaultInfo(files = depset([out]))]

_fixture_file = rule(
    implementation = _fixture_file_impl,
    attrs = {
        "content": attr.string(),
        "out": attr.string(mandatory = True),
    },
)

def _action_with_mnemonic(actions, mnemonic):
    matches = [action for action in actions if action.mnemonic == mnemonic]
    return matches[0] if len(matches) == 1 else None

def _argument_after(argv, flag):
    for index in range(len(argv) - 1):
        if argv[index] == flag:
            return argv[index + 1]
    return ""

def _assert_mapped_directory_argument(env, action, flag):
    value = _argument_after(action.argv, flag)
    asserts.true(env, value != "", "missing %s in %s" % (flag, action.argv))
    asserts.true(
        env,
        value.startswith("bazel-out/cfg/bin/"),
        "expected mapped path after %s, got %s" % (flag, value),
    )

def _assert_mapped_srctree_var(env, action):
    values = [arg for arg in action.argv if arg.startswith("srctree=")]
    asserts.equals(env, 1, len(values))
    if values:
        asserts.true(
            env,
            values[0].startswith("srctree=bazel-out/cfg/bin/"),
            "expected mapped srctree variable, got %s" % values[0],
        )

def _compact_path_mapping_test_impl(ctx):
    env = analysistest.begin(ctx)
    action = _action_with_mnemonic(
        analysistest.target_actions(env),
        "LinuxCompactV7Kconfig",
    )
    asserts.true(env, action != None)
    if action != None:
        _assert_mapped_directory_argument(env, action, "-srctree")
        _assert_mapped_srctree_var(env, action)
        asserts.equals(env, "linux.bzl/test/x86", _argument_after(action.argv, "-compile_environment_abi"))
        asserts.true(env, _argument_after(action.argv, "-graph_profile").endswith("graph_profile.json"))
        asserts.true(env, "-graph_profile_projection_out" in action.argv)
        generated_headers = [
            action.argv[index + 1]
            for index in range(len(action.argv) - 1)
            if action.argv[index] == "-generated_headers_for_config"
        ]
        asserts.equals(
            env,
            ["fixture=//internal/tests:fixture_generated_headers"],
            generated_headers,
        )
        for removed_flag in [
            "-compact_schema",
            "-generated_headers",
            "-source_config",
            "-source_tree_all_files_label",
            "-source_tree_arch_headers_label",
            "-source_tree_dtb_sources_label",
            "-source_tree_global_headers_label",
            "-source_tree_headers_label",
            "-source_tree_kbuild_files_label",
            "-source_tree_local_include_files_label",
            "-source_tree_lookup_files_label",
            "-source_tree_scripts_headers_label",
            "-source_tree_uapi_headers_label",
        ]:
            asserts.false(env, removed_flag in action.argv)
    return analysistest.end(env)

_compact_path_mapping_test = analysistest.make(_compact_path_mapping_test_impl)

def _parser_path_mapping_test_impl(ctx):
    env = analysistest.begin(ctx)
    actions = analysistest.target_actions(env)
    kconfig = _action_with_mnemonic(actions, "LinuxKconfigParseValidation")
    kbuild = _action_with_mnemonic(actions, "LinuxKbuildParseValidation")
    asserts.true(env, kconfig != None)
    asserts.true(env, kbuild != None)
    if kconfig != None:
        _assert_mapped_directory_argument(env, kconfig, "-srctree")
        _assert_mapped_srctree_var(env, kconfig)
    if kbuild != None:
        _assert_mapped_directory_argument(env, kbuild, "-kbuild_srctree")
        _assert_mapped_srctree_var(env, kbuild)
    return analysistest.end(env)

_parser_path_mapping_test = analysistest.make(_parser_path_mapping_test_impl)

def _kbuild_tree_path_mapping_test_impl(ctx):
    env = analysistest.begin(ctx)
    action = _action_with_mnemonic(
        analysistest.target_actions(env),
        "LinuxKbuildTreeParseValidation",
    )
    asserts.true(env, action != None)
    if action != None:
        _assert_mapped_directory_argument(env, action, "-kbuild_tree_root")
        _assert_mapped_srctree_var(env, action)
    return analysistest.end(env)

_kbuild_tree_path_mapping_test = analysistest.make(
    _kbuild_tree_path_mapping_test_impl,
)

def path_mapping_test(name):
    probe = name + "_probe"
    _directory_argument_probe(
        name = probe,
        tags = ["manual"],
    )
    _directory_argument_test(
        name = name + "_directory_test",
        tags = ["manual"],
        target_under_test = ":" + probe,
    )

    source_dir = name + ".source"
    kconfig = name + "_kconfig"
    _fixture_file(
        name = kconfig,
        content = 'mainmenu "path mapping test"\n',
        out = source_dir + "/Kconfig",
        tags = ["manual"],
    )
    kbuild = name + "_kbuild"
    _fixture_file(
        name = kbuild,
        content = "obj-y += fixture.o\n",
        out = source_dir + "/Makefile",
        tags = ["manual"],
    )
    config = name + "_config"
    _fixture_file(
        name = config,
        content = "CONFIG_64BIT=y\n",
        out = source_dir + "/.config",
        tags = ["manual"],
    )
    source = name + "_source"
    _fixture_file(
        name = source,
        content = "int fixture;\n",
        out = source_dir + "/fixture.c",
        tags = ["manual"],
    )

    compact = name + "_compact"
    linux_compact_v7_metadata(
        name = compact,
        compile_environment_abi = "linux.bzl/test/x86",
        configs = {
            ":" + config: "fixture",
        },
        graph_profile = ":graph_profile.json",
        generated_headers_by_config = {
            "fixture": "//internal/tests:fixture_generated_headers",
        },
        kbuild = ":" + kbuild,
        root = ":" + kconfig,
        srcs = [
            ":" + kbuild,
            ":" + kconfig,
            ":" + source,
        ],
        tags = ["manual"],
    )
    compact_test = compact + "_path_mapping_test"
    _compact_path_mapping_test(
        name = compact_test,
        tags = ["manual"],
        target_under_test = ":" + compact,
    )

    parser = name + "_parser"
    linux_parser_validation(
        name = parser,
        kbuild_recursive = True,
        kbuilds = {":" + kbuild: "kbuild"},
        kconfigs = {":" + kconfig: "kconfig"},
        source_root = ":" + kconfig,
        srcs = [
            ":" + kbuild,
            ":" + kconfig,
        ],
        tags = ["manual"],
    )
    parser_test = parser + "_path_mapping_test"
    _parser_path_mapping_test(
        name = parser_test,
        tags = ["manual"],
        target_under_test = ":" + parser,
    )

    kbuild_tree = name + "_kbuild_tree"
    linux_kbuild_tree_validation(
        name = kbuild_tree,
        source_root = ":" + kconfig,
        srcs = [
            ":" + kbuild,
            ":" + kconfig,
        ],
        tags = ["manual"],
    )
    kbuild_tree_test = kbuild_tree + "_path_mapping_test"
    _kbuild_tree_path_mapping_test(
        name = kbuild_tree_test,
        tags = ["manual"],
        target_under_test = ":" + kbuild_tree,
    )

    native.test_suite(
        name = name,
        tags = ["manual"],
        tests = [
            ":" + compact_test,
            ":" + kbuild_tree_test,
            ":" + name + "_directory_test",
            ":" + parser_test,
        ],
    )
