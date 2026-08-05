"""Analysis tests for Kbuild module source-version metadata."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load(
    "//internal:linux_object_groups.bzl",
    "LinuxObjectActionGroupInfo",
    "linux_object_action_group",
)
load(
    "//internal:linux_objects.bzl",
    "LinuxGeneratedHeadersInfo",
    "LinuxObjectInfo",
    "linux_compact_image",
    "linux_compile_environment_index",
    "linux_composite_object",
    "linux_config",
    "linux_object",
    "linux_source_input_index",
    "linux_source_tree",
    "linux_vmlinux",
)
load("//internal:linux_rust.bzl", "linux_disabled_rust_kernel_sdk")

visibility("private")

_PLAIN_PAYLOAD = "1111111111111111111111111111111111111111111111111111111111111111"
_MODVERSION_PAYLOAD = "2222222222222222222222222222222222222222222222222222222222222222"
_SRCVERSION_ALL_PAYLOAD = "3333333333333333333333333333333333333333333333333333333333333333"
_PLAIN_ENVIRONMENT = "4444444444444444444444444444444444444444444444444444444444444444"
_MODVERSION_ENVIRONMENT = "5555555555555555555555555555555555555555555555555555555555555555"
_SRCVERSION_ALL_ENVIRONMENT = "6666666666666666666666666666666666666666666666666666666666666666"
_GENERATED_HEADER_ENVIRONMENT = "1818181818181818181818181818181818181818181818181818181818181818"
_CONSUMER_HEADER_FAMILY = "1919191919191919191919191919191919191919191919191919191919191919"

def _actions_with_mnemonic(actions, mnemonic):
    return [action for action in actions if action.mnemonic == mnemonic]

def _argument_after(argv, flag):
    for index in range(len(argv) - 1):
        if argv[index] == flag:
            return argv[index + 1]
    return ""

def _source_version_mappings(argv):
    mappings = {}
    canonical_order = []
    for index in range(len(argv) - 3):
        if argv[index] != "-physical":
            continue
        if argv[index + 2] != "-canonical":
            continue
        canonical = argv[index + 3]
        mappings[canonical] = argv[index + 1]
        canonical_order.append(canonical)
    return struct(
        by_path = mappings,
        order = canonical_order,
    )

def _generated_source_version_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    info = target[LinuxObjectInfo]
    actions = analysistest.target_actions(env)
    compile_actions = _actions_with_mnemonic(actions, "LinuxObjectCompile")
    source_version_actions = _actions_with_mnemonic(actions, "LinuxSourceVersionCmd")

    asserts.equals(env, [info.object], info.module_leaf_objects)
    asserts.equals(env, 1, len(info.source_version_records))
    asserts.equals(env, 1, len(compile_actions))
    asserts.equals(env, 1, len(source_version_actions))
    if compile_actions:
        depfiles = [
            file.path
            for file in compile_actions[0].outputs.to_list()
            if file.path.endswith(".d")
        ]
        asserts.equals(env, 1, len(depfiles))
    if source_version_actions and info.source_version_records:
        action = source_version_actions[0]
        mappings = _source_version_mappings(action.argv)
        asserts.equals(env, ctx.attr.expected_primary, _argument_after(action.argv, "-primary"))
        asserts.equals(env, sorted(mappings.order), mappings.order)
        action_inputs = {file.path: True for file in action.inputs.to_list()}
        staged_paths = [path_file.path for path_file in info.source_version_records[0].path_files]
        asserts.equals(env, sorted(staged_paths), staged_paths)
        for path in ctx.attr.expected_generated_paths:
            asserts.true(env, path in mappings.by_path, "missing generated source-version mapping for %s" % path)
            if path in mappings.by_path:
                asserts.true(
                    env,
                    mappings.by_path[path] in action_inputs,
                    "generated source-version input %s is not an action input" % path,
                )
            asserts.true(env, path in staged_paths, "generated source-version input %s is not staged" % path)
    return analysistest.end(env)

_generated_source_version_test = analysistest.make(
    _generated_source_version_test_impl,
    attrs = {
        "expected_generated_paths": attr.string_list(mandatory = True),
        "expected_primary": attr.string(mandatory = True),
    },
)

def _consumer_generated_headers_test_impl(ctx):
    env = analysistest.begin(ctx)
    info = analysistest.target_under_test(env)[LinuxObjectInfo]
    producer = ctx.attr.producer[LinuxObjectInfo]
    generated_headers = ctx.attr.generated_headers[LinuxGeneratedHeadersInfo]
    actions = _actions_with_mnemonic(analysistest.target_actions(env), "LinuxSourceVersionCmd")
    expected_paths = [
        "drivers/test/hyp_constants.h",
        "drivers/test/parser.asn1.h",
    ]

    asserts.equals(env, ["drivers/test/parser.asn1.h"], [path_file.path for path_file in producer.generated_header_path_files])
    asserts.equals(env, ["drivers/test/hyp_constants.h"], [path_file.path for path_file in generated_headers.path_files])
    asserts.equals(env, 1, len(info.source_version_records))
    asserts.equals(env, 1, len(actions))
    if info.source_version_records and actions:
        mappings = _source_version_mappings(actions[0].argv)
        action_inputs = {file.path: True for file in actions[0].inputs.to_list()}
        staged_paths = [path_file.path for path_file in info.source_version_records[0].path_files]
        asserts.equals(env, sorted(staged_paths), staged_paths)
        for path in expected_paths:
            asserts.true(env, path in mappings.by_path, "missing generated-header mapping for %s" % path)
            if path in mappings.by_path:
                asserts.true(
                    env,
                    mappings.by_path[path] in action_inputs,
                    "generated header %s is not an action input" % path,
                )
            asserts.true(env, path in staged_paths, "generated header %s is not staged" % path)
    return analysistest.end(env)

_consumer_generated_headers_test = analysistest.make(
    _consumer_generated_headers_test_impl,
    attrs = {
        "generated_headers": attr.label(mandatory = True, providers = [LinuxGeneratedHeadersInfo]),
        "producer": attr.label(mandatory = True, providers = [LinuxObjectInfo]),
    },
)

def _assembler_source_version_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    info = target[LinuxObjectInfo]
    actions = analysistest.target_actions(env)
    compile_actions = _actions_with_mnemonic(actions, "LinuxObjectCompile")
    source_version_actions = _actions_with_mnemonic(actions, "LinuxSourceVersionCmd")

    asserts.equals(env, [ctx.attr.expected_object], info.module_leaf_objects)
    asserts.equals(env, ctx.attr.expected_source_versions, len(info.source_version_records))
    asserts.equals(env, 1, len(info.symversion_records))
    asserts.equals(env, ctx.attr.expected_source_versions, len(source_version_actions))
    asserts.equals(env, 1, len(compile_actions))
    if compile_actions:
        depfiles = [
            file
            for file in compile_actions[0].outputs.to_list()
            if file.path.endswith(".d")
        ]
        asserts.equals(env, ctx.attr.expected_source_versions, len(depfiles))
    return analysistest.end(env)

_assembler_source_version_test = analysistest.make(
    _assembler_source_version_test_impl,
    attrs = {
        "expected_object": attr.string(mandatory = True),
        "expected_source_versions": attr.int(mandatory = True),
    },
)

def _grouped_raw_assembler_test_impl(ctx):
    env = analysistest.begin(ctx)
    group = analysistest.target_under_test(env)[LinuxObjectActionGroupInfo]
    info = group.objects["raw"]
    actions = analysistest.target_actions(env)
    compile_actions = _actions_with_mnemonic(actions, "LinuxObjectCompile")

    asserts.equals(env, ["drivers/test/grouped_raw.o"], info.module_leaf_objects)
    asserts.equals(env, [], info.source_version_records)
    asserts.equals(env, 1, len(info.symversion_records))
    asserts.equals(env, 0, len(_actions_with_mnemonic(actions, "LinuxSourceVersionCmd")))
    asserts.equals(env, 1, len(compile_actions))
    if compile_actions:
        asserts.equals(
            env,
            [],
            [file.path for file in compile_actions[0].outputs.to_list() if file.path.endswith(".d")],
        )
    return analysistest.end(env)

_grouped_raw_assembler_test = analysistest.make(_grouped_raw_assembler_test_impl)

def _mixed_module_metadata_test_impl(ctx):
    env = analysistest.begin(ctx)
    info = analysistest.target_under_test(env)[LinuxObjectInfo]
    asserts.equals(
        env,
        ["drivers/test/mixed_c.o", "drivers/test/mixed_raw.o"],
        info.module_leaf_objects,
    )
    asserts.equals(env, ["drivers/test/mixed_c.o"], [record.object for record in info.source_version_records])
    asserts.equals(
        env,
        ["drivers/test/mixed_c.o", "drivers/test/mixed_raw.o"],
        [record.object for record in info.symversion_records],
    )
    return analysistest.end(env)

_mixed_module_metadata_test = analysistest.make(_mixed_module_metadata_test_impl)

def _action_with_output_suffix(actions, suffix):
    matches = []
    for action in actions:
        if any([output.path.endswith(suffix) for output in action.outputs.to_list()]):
            matches.append(action)
    return matches

def _mixed_module_staging_test_impl(ctx):
    env = analysistest.begin(ctx)
    actions = analysistest.target_actions(env)
    c_info = ctx.attr.c_leaf[LinuxObjectInfo]
    raw_info = ctx.attr.raw_leaf[LinuxObjectInfo]

    manifests = _action_with_output_suffix(actions, "/drivers/test/mixed.mod")
    asserts.equals(env, 1, len(manifests))
    if manifests:
        asserts.equals(
            env,
            "drivers/test/mixed_c.o\ndrivers/test/mixed_raw.o\n",
            manifests[0].content,
        )

    staged_c = _action_with_output_suffix(actions, "/drivers/test/.mixed_c.o.cmd")
    staged_raw = _action_with_output_suffix(actions, "/drivers/test/.mixed_raw.o.cmd")
    staged_raw_without_symversions = _action_with_output_suffix(actions, "/drivers/test/.raw_no_sym.o.cmd")
    asserts.equals(env, 1, len(staged_c))
    asserts.equals(env, 1, len(staged_raw))
    asserts.equals(env, 1, len(staged_raw_without_symversions))
    if staged_c:
        asserts.equals(
            env,
            [c_info.source_version_records[0].cmd.path],
            [file.path for file in staged_c[0].inputs.to_list()],
        )
        asserts.false(
            env,
            c_info.symversion_records[0].cmd.path in [file.path for file in staged_c[0].inputs.to_list()],
            "C leaf must stage its combined source/symbol-version command",
        )
    if staged_raw:
        asserts.equals(
            env,
            [raw_info.symversion_records[0].cmd.path],
            [file.path for file in staged_raw[0].inputs.to_list()],
        )
    if staged_raw_without_symversions:
        asserts.equals(env, "", staged_raw_without_symversions[0].content)
        asserts.equals(env, [], staged_raw_without_symversions[0].inputs.to_list())

    raw_without_symversions_manifests = _action_with_output_suffix(actions, "/drivers/test/raw_no_sym.mod")
    asserts.equals(env, 1, len(raw_without_symversions_manifests))
    if raw_without_symversions_manifests:
        asserts.equals(env, "drivers/test/raw_no_sym.o\n", raw_without_symversions_manifests[0].content)

    modpost_actions = _actions_with_mnemonic(actions, "LinuxModpost")
    asserts.equals(env, 1, len(modpost_actions))
    if modpost_actions:
        modpost_inputs = [file.path for file in modpost_actions[0].inputs.to_list()]
        asserts.true(env, any([path.endswith("/drivers/test/mixed.mod") for path in modpost_inputs]))
        asserts.true(env, any([path.endswith("/drivers/test/.mixed_c.o.cmd") for path in modpost_inputs]))
        asserts.true(env, any([path.endswith("/drivers/test/.mixed_raw.o.cmd") for path in modpost_inputs]))
        asserts.true(env, any([path.endswith("/drivers/test/.raw_no_sym.o.cmd") for path in modpost_inputs]))

    modinfo_actions = _actions_with_mnemonic(actions, "LinuxModuleModinfoCheck")
    asserts.equals(env, 2, len(modinfo_actions))
    for action in modinfo_actions:
        asserts.false(
            env,
            "-allow-version" in action.argv,
            "a module with a raw assembler leaf cannot carry MODULE_VERSION",
        )
    return analysistest.end(env)

_mixed_module_staging_test = analysistest.make(
    _mixed_module_staging_test_impl,
    attrs = {
        "c_leaf": attr.label(mandatory = True, providers = [LinuxObjectInfo]),
        "raw_leaf": attr.label(mandatory = True, providers = [LinuxObjectInfo]),
    },
)

def _failure_test_impl(ctx):
    env = analysistest.begin(ctx)
    asserts.expect_failure(env, ctx.attr.expected_error)
    return analysistest.end(env)

_failure_test = analysistest.make(
    _failure_test_impl,
    attrs = {"expected_error": attr.string(mandatory = True)},
    expect_failure = True,
)

def _fake_generated_headers_impl(ctx):
    header = ctx.actions.declare_file(ctx.label.name + ".h")
    ctx.actions.write(header, "")
    path_files = []
    if ctx.attr.canonical_path:
        path_files.append(struct(
            file = header,
            path = ctx.attr.canonical_path,
        ))
    family = struct(
        arch = "x86",
        cflags = None,
        content_id = ctx.attr.family_content_id,
        files = depset([header]),
        include_dir_anchors = {},
        include_dirs = [],
        name = "all",
        path_files = path_files,
        srcarch = "x86",
        vdsomunge = None,
    )
    return [
        DefaultInfo(files = depset([header])),
        LinuxGeneratedHeadersInfo(
            arch = "x86",
            cflags = None,
            families = {"all": family},
            files = depset([header]),
            include_dir_anchors = {},
            include_dirs = [],
            path_files = path_files,
            srcarch = "x86",
            vdsomunge = None,
        ),
    ]

_fake_generated_headers = rule(
    implementation = _fake_generated_headers_impl,
    attrs = {
        "canonical_path": attr.string(),
        "family_content_id": attr.string(
            default = "7777777777777777777777777777777777777777777777777777777777777777",
        ),
    },
)

def linux_source_versions_test_suite(name):
    """Defines focused source-version and module-staging analysis tests."""
    fixture_tags = ["manual"]
    source_tree = name + "_source_tree"
    source_inputs = name + "_source_inputs"
    grouped_source_inputs = name + "_grouped_source_inputs"
    compile_environments = name + "_compile_environments"
    consumer_generated_headers = name + "_consumer_generated_headers"

    linux_source_tree(
        name = source_tree,
        root = "source_versions/Kconfig",
        tags = fixture_tags,
    )
    linux_source_input_index(
        name = source_inputs,
        groups = ["1", "2", "3", "4", "5", "6"],
        source_tree_info = ":" + source_tree,
        srcs = [
            "source_versions/shipped.c_shipped",
            "source_versions/parser.asn1",
            "source_versions/poly1305.pl",
            "source_versions/raw.s",
            "source_versions/preprocessed.S",
            "source_versions/regular.c",
        ],
        tags = fixture_tags,
    )
    linux_source_input_index(
        name = grouped_source_inputs,
        groups = ["1"],
        source_tree_info = ":" + source_tree,
        srcs = ["source_versions/asm/raw.s"],
        tags = fixture_tags,
    )
    _fake_generated_headers(
        name = consumer_generated_headers,
        canonical_path = "drivers/test/hyp_constants.h",
        family_content_id = _CONSUMER_HEADER_FAMILY,
        tags = fixture_tags,
    )
    linux_compile_environment_index(
        name = compile_environments,
        compile_environments = {
            _PLAIN_ENVIRONMENT: json.encode({
                "abi": "tests/source-versions/x86",
                "config_payload": _PLAIN_PAYLOAD,
                "generated_header_families": [],
            }),
            _MODVERSION_ENVIRONMENT: json.encode({
                "abi": "tests/source-versions/x86",
                "config_payload": _MODVERSION_PAYLOAD,
                "generated_header_families": [],
            }),
            _SRCVERSION_ALL_ENVIRONMENT: json.encode({
                "abi": "tests/source-versions/x86",
                "config_payload": _SRCVERSION_ALL_PAYLOAD,
                "generated_header_families": [],
            }),
            _GENERATED_HEADER_ENVIRONMENT: json.encode({
                "abi": "tests/source-versions/x86",
                "config_payload": _PLAIN_PAYLOAD,
                "generated_header_families": [_CONSUMER_HEADER_FAMILY],
            }),
        },
        config_payloads = {
            _PLAIN_PAYLOAD: "CONFIG_MODULES=y\n",
            _MODVERSION_PAYLOAD: "CONFIG_GENKSYMS=y\nCONFIG_MODULES=y\nCONFIG_MODVERSIONS=y\n",
            _SRCVERSION_ALL_PAYLOAD: "CONFIG_GENKSYMS=y\nCONFIG_MODULES=y\nCONFIG_MODULE_SRCVERSION_ALL=y\nCONFIG_MODVERSIONS=y\n",
        },
        expected_abi = "tests/source-versions/x86",
        generated_headers = [":" + consumer_generated_headers],
        tags = fixture_tags,
    )

    generated_cases = [
        struct(
            asn1_compiler = None,
            expected_paths = ["drivers/test/shipped.c"],
            name = "shipped",
            object = "drivers/test/shipped.o",
            source_input_file = 6,
        ),
        struct(
            asn1_compiler = "//internal/cmd/runandwrite",
            expected_paths = [
                "drivers/test/parser.asn1.c",
                "drivers/test/parser.asn1.h",
            ],
            name = "asn1",
            object = "drivers/test/parser.asn1.o",
            source_input_file = 1,
        ),
        struct(
            asn1_compiler = None,
            expected_paths = ["lib/crypto/x86/poly1305-x86_64-cryptogams.S"],
            name = "perlasm",
            object = "lib/crypto/x86/poly1305-x86_64-cryptogams.o",
            source_input_file = 2,
        ),
    ]
    tests = []
    for case in generated_cases:
        target = name + "_" + case.name + "_object"
        linux_object(
            name = target,
            asn1_compiler = case.asn1_compiler,
            compile_environment_id = _PLAIN_ENVIRONMENT,
            compile_environment_index = ":" + compile_environments,
            content_id = ("8" if case.name == "shipped" else "9" if case.name == "asn1" else "a") * 64,
            mode = "m",
            object = case.object,
            source_input_file = case.source_input_file,
            source_input_group = case.source_input_file,
            source_input_index = ":" + source_inputs,
            tags = fixture_tags,
        )
        test = target + "_test"
        _generated_source_version_test(
            name = test,
            expected_generated_paths = case.expected_paths,
            expected_primary = case.expected_paths[0],
            target_under_test = ":" + target,
        )
        tests.append(":" + test)

    consumer = name + "_generated_header_consumer"
    linux_object(
        name = consumer,
        compile_environment_id = _GENERATED_HEADER_ENVIRONMENT,
        compile_environment_index = ":" + compile_environments,
        content_id = "2020202020202020202020202020202020202020202020202020202020202020",
        deps = [":" + name + "_asn1_object"],
        mode = "m",
        object = "drivers/test/consumer.o",
        source_input_file = 5,
        source_input_group = 5,
        source_input_index = ":" + source_inputs,
        tags = fixture_tags,
    )
    consumer_test = consumer + "_test"
    _consumer_generated_headers_test(
        name = consumer_test,
        generated_headers = ":" + consumer_generated_headers,
        producer = ":" + name + "_asn1_object",
        target_under_test = ":" + consumer,
    )
    tests.append(":" + consumer_test)

    for suffix, source_input_file, expected_source_versions in [
        ("raw", 4, 0),
        ("preprocessed", 3, 1),
    ]:
        object_path = "drivers/test/%s.o" % suffix
        target = name + "_" + suffix + "_object"
        linux_object(
            name = target,
            compile_environment_id = _MODVERSION_ENVIRONMENT,
            compile_environment_index = ":" + compile_environments,
            content_id = ("b" if suffix == "raw" else "c") * 64,
            genksyms = "//internal/cmd/runandwrite",
            mode = "m",
            object = object_path,
            source_input_file = source_input_file,
            source_input_group = source_input_file,
            source_input_index = ":" + source_inputs,
            symversions = True,
            tags = fixture_tags,
            version = "6.18.39",
        )
        test = target + "_test"
        _assembler_source_version_test(
            name = test,
            expected_object = object_path,
            expected_source_versions = expected_source_versions,
            target_under_test = ":" + target,
        )
        tests.append(":" + test)

    raw_srcversion_all = name + "_raw_srcversion_all_object"
    linux_object(
        name = raw_srcversion_all,
        compile_environment_id = _SRCVERSION_ALL_ENVIRONMENT,
        compile_environment_index = ":" + compile_environments,
        content_id = "d" * 64,
        genksyms = "//internal/cmd/runandwrite",
        mode = "m",
        object = "drivers/test/raw_srcversion_all.o",
        source_input_file = 4,
        source_input_group = 4,
        source_input_index = ":" + source_inputs,
        symversions = True,
        tags = fixture_tags,
        version = "6.18.39",
    )
    raw_srcversion_all_test = raw_srcversion_all + "_test"
    _failure_test(
        name = raw_srcversion_all_test,
        expected_error = "CONFIG_MODULE_SRCVERSION_ALL=y requires depfile-capable C or preprocessed assembly (.S)",
        target_under_test = ":" + raw_srcversion_all,
    )
    tests.append(":" + raw_srcversion_all_test)

    grouped_raw = name + "_grouped_raw"
    linux_object_action_group(
        name = grouped_raw,
        arch = "x86",
        compile_environment_index = ":" + compile_environments,
        genksyms = "//internal/cmd/runandwrite",
        language = "asm",
        mode = "m",
        objects = {
            "raw": json.encode({
                "compile_environment": _MODVERSION_ENVIRONMENT,
                "content_id": "1212121212121212121212121212121212121212121212121212121212121212",
                "object": "drivers/test/grouped_raw.o",
                "source_input_file": 1,
                "source_input_group": 1,
            }),
        },
        reachable_configs = ["base"],
        reachability_id = "1313131313131313131313131313131313131313131313131313131313131313",
        recipe_id = "1414141414141414141414141414141414141414141414141414141414141414",
        source_input_index = ":" + grouped_source_inputs,
        srcarch = "x86",
        symversions = True,
        tags = fixture_tags,
        version = "6.18.39",
    )
    grouped_raw_test = grouped_raw + "_test"
    _grouped_raw_assembler_test(
        name = grouped_raw_test,
        target_under_test = ":" + grouped_raw,
    )
    tests.append(":" + grouped_raw_test)

    grouped_raw_srcversion_all = name + "_grouped_raw_srcversion_all"
    linux_object_action_group(
        name = grouped_raw_srcversion_all,
        arch = "x86",
        compile_environment_index = ":" + compile_environments,
        genksyms = "//internal/cmd/runandwrite",
        language = "asm",
        mode = "m",
        objects = {
            "raw": json.encode({
                "compile_environment": _SRCVERSION_ALL_ENVIRONMENT,
                "content_id": "1515151515151515151515151515151515151515151515151515151515151515",
                "object": "drivers/test/grouped_raw_srcversion_all.o",
                "source_input_file": 1,
                "source_input_group": 1,
            }),
        },
        reachable_configs = ["base"],
        reachability_id = "1616161616161616161616161616161616161616161616161616161616161616",
        recipe_id = "1717171717171717171717171717171717171717171717171717171717171717",
        source_input_index = ":" + grouped_source_inputs,
        srcarch = "x86",
        symversions = True,
        tags = fixture_tags,
        version = "6.18.39",
    )
    grouped_raw_srcversion_all_test = grouped_raw_srcversion_all + "_test"
    _failure_test(
        name = grouped_raw_srcversion_all_test,
        expected_error = "CONFIG_MODULE_SRCVERSION_ALL=y requires depfile-capable C or preprocessed assembly (.S)",
        target_under_test = ":" + grouped_raw_srcversion_all,
    )
    tests.append(":" + grouped_raw_srcversion_all_test)

    mixed_c = name + "_mixed_c"
    linux_object(
        name = mixed_c,
        compile_environment_id = _MODVERSION_ENVIRONMENT,
        compile_environment_index = ":" + compile_environments,
        content_id = "e" * 64,
        genksyms = "//internal/cmd/runandwrite",
        mode = "m",
        object = "drivers/test/mixed_c.o",
        source_input_file = 5,
        source_input_group = 5,
        source_input_index = ":" + source_inputs,
        symversions = True,
        tags = fixture_tags,
        version = "6.18.39",
    )
    mixed_raw = name + "_mixed_raw"
    linux_object(
        name = mixed_raw,
        compile_environment_id = _MODVERSION_ENVIRONMENT,
        compile_environment_index = ":" + compile_environments,
        content_id = "f" * 64,
        genksyms = "//internal/cmd/runandwrite",
        mode = "m",
        object = "drivers/test/mixed_raw.o",
        source_input_file = 4,
        source_input_group = 4,
        source_input_index = ":" + source_inputs,
        symversions = True,
        tags = fixture_tags,
        version = "6.18.39",
    )
    mixed_module = name + "_mixed_module"
    linux_composite_object(
        name = mixed_module,
        content_id = "0" * 64,
        mode = "m",
        module_root = True,
        object = "drivers/test/mixed.o",
        objects = [
            ":" + mixed_c,
            ":" + mixed_raw,
        ],
        tags = fixture_tags,
    )
    mixed_metadata_test = mixed_module + "_metadata_test"
    _mixed_module_metadata_test(
        name = mixed_metadata_test,
        target_under_test = ":" + mixed_module,
    )
    tests.append(":" + mixed_metadata_test)

    generated_headers = name + "_generated_headers"
    config = name + "_config"
    builtin = name + "_builtin"
    raw_without_symversions = name + "_raw_without_symversions"
    image = name + "_image"
    rust_sdk = name + "_rust_sdk"
    vmlinux = name + "_vmlinux"
    _fake_generated_headers(
        name = generated_headers,
        tags = fixture_tags,
    )
    linux_config(
        name = config,
        arch = "x86",
        config_flags = {
            "CONFIG_GENKSYMS": "y",
            "CONFIG_MODULES": "y",
            "CONFIG_MODVERSIONS": "y",
        },
        tags = fixture_tags,
        version = "6.18.39",
    )
    linux_object(
        name = builtin,
        compile_environment_id = _PLAIN_ENVIRONMENT,
        compile_environment_index = ":" + compile_environments,
        content_id = "7" * 64,
        mode = "y",
        object = "drivers/test/builtin.o",
        source_input_file = 5,
        source_input_group = 5,
        source_input_index = ":" + source_inputs,
        tags = fixture_tags,
    )
    linux_object(
        name = raw_without_symversions,
        compile_environment_id = _MODVERSION_ENVIRONMENT,
        compile_environment_index = ":" + compile_environments,
        content_id = "2121212121212121212121212121212121212121212121212121212121212121",
        mode = "m",
        module_root = True,
        object = "drivers/test/raw_no_sym.o",
        source_input_file = 4,
        source_input_group = 4,
        source_input_index = ":" + source_inputs,
        tags = fixture_tags,
    )
    linux_compact_image(
        name = image,
        module_objects = [
            ":" + mixed_module,
            ":" + raw_without_symversions,
        ],
        objects = [":" + builtin],
        tags = fixture_tags,
    )
    linux_disabled_rust_kernel_sdk(
        name = rust_sdk,
        tags = fixture_tags,
    )
    linux_vmlinux(
        name = vmlinux,
        arch = "x86",
        config = ":" + config,
        format = "x86_64",
        generated_headers = ":" + generated_headers,
        image = ":" + image,
        kallsyms = "false",
        linker_script = "source_versions/scripts/module.lds.S",
        rust_sdk = ":" + rust_sdk,
        source_root = "source_versions/Kconfig",
        source_tree = [
            "source_versions/init/version-timestamp.c",
            "source_versions/scripts/mod/devicetable-offsets.c",
            "source_versions/scripts/mod/file2alias.c",
            "source_versions/scripts/mod/modpost.c",
            "source_versions/scripts/mod/sumversion.c",
            "source_versions/scripts/mod/symsearch.c",
            "source_versions/scripts/module-common.c",
            "source_versions/scripts/module.lds.S",
        ],
        srcarch = "x86",
        tags = fixture_tags,
        version = "6.18.39",
    )
    mixed_staging_test = vmlinux + "_mixed_staging_test"
    _mixed_module_staging_test(
        name = mixed_staging_test,
        c_leaf = ":" + mixed_c,
        raw_leaf = ":" + mixed_raw,
        target_under_test = ":" + vmlinux,
    )
    tests.append(":" + mixed_staging_test)

    native.test_suite(
        name = name,
        tests = tests,
    )
