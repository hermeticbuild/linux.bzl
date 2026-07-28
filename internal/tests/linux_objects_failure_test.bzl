"""Analysis tests for Linux rules that intentionally fail closed."""

load(
    "@bazel_skylib//lib:unittest.bzl",
    "analysistest",
    "asserts",
)
load("@bazel_skylib//rules:diff_test.bzl", "diff_test")
load(
    "//internal:linux_objects.bzl",
    "LinuxCompileEnvironmentIndexInfo",
    "LinuxGeneratedHeadersInfo",
    "LinuxImageInfo",
    "LinuxObjectInfo",
    "linux_arm64_nvhe_object",
    "linux_cache_shape_check",
    "linux_compact_delta_image",
    "linux_compact_image",
    "linux_compile_environment_index",
    "linux_compressed_image",
    "linux_config",
    "linux_object",
    "linux_source_input_index",
    "linux_source_tree",
)

visibility("private")

def _fake_linux_image_impl(ctx):
    out = ctx.actions.declare_file(ctx.label.name + ".vmlinux")
    ctx.actions.write(out, "")
    return [
        DefaultInfo(files = depset([out])),
        LinuxImageInfo(
            archives = [],
            module_objects = [],
            objects = [],
            output = out,
        ),
    ]

_fake_linux_image = rule(implementation = _fake_linux_image_impl)

def _fake_linux_object_impl(ctx):
    out = ctx.actions.declare_file(ctx.label.name + ".o")
    ctx.actions.write(out, "")
    return [
        DefaultInfo(files = depset([out])),
        LinuxObjectInfo(
            config_fragment = {},
            content_id = ctx.attr.content_id,
            flags = [],
            generated_headers = depset(),
            generated_include_dir_anchors = {},
            generated_include_dirs = [],
            mode = ctx.attr.mode,
            object = ctx.attr.object,
            output = out,
            source = "",
        ),
    ]

_fake_linux_object = rule(
    implementation = _fake_linux_object_impl,
    attrs = {
        "content_id": attr.string(mandatory = True),
        "mode": attr.string(mandatory = True, values = ["m", "y"]),
        "object": attr.string(mandatory = True),
    },
)

def _fake_compile_environment_index_impl(_ctx):
    return [
        LinuxCompileEnvironmentIndexInfo(
            config_payloads = {},
            environments = {},
            expected_abi = "test-abi",
            header_groups = {},
        ),
    ]

_fake_compile_environment_index = rule(
    implementation = _fake_compile_environment_index_impl,
)

def _fake_generated_headers_impl(ctx):
    out = ctx.actions.declare_file(ctx.label.name + ".h")
    ctx.actions.write(out, "")
    return [
        DefaultInfo(files = depset([out])),
        LinuxGeneratedHeadersInfo(
            arch = "x86",
            cflags = None,
            content_id = ctx.attr.content_id,
            files = depset([out]),
            include_dir_anchors = {},
            include_dirs = [],
            srcarch = "x86",
            vdsomunge = None,
        ),
    ]

_fake_generated_headers = rule(
    implementation = _fake_generated_headers_impl,
    attrs = {
        "content_id": attr.string(mandatory = True),
    },
)

def _fake_arm64_generated_headers_impl(ctx):
    out = ctx.actions.declare_file(ctx.label.name + ".h")
    ctx.actions.write(out, "")
    return [
        DefaultInfo(files = depset([out])),
        LinuxGeneratedHeadersInfo(
            arch = "arm64",
            cflags = None,
            content_id = ctx.attr.content_id,
            files = depset([out]),
            include_dir_anchors = {},
            include_dirs = [],
            srcarch = "arm64",
            vdsomunge = ctx.executable._vdsomunge,
        ),
    ]

_fake_arm64_generated_headers = rule(
    implementation = _fake_arm64_generated_headers_impl,
    attrs = {
        "content_id": attr.string(mandatory = True),
        "_vdsomunge": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/runandwrite"),
            executable = True,
        ),
    },
)

def _fake_nvhe_source_inputs_impl(ctx):
    hyp_lds = ctx.actions.declare_file(
        ctx.label.name + ".source/arch/arm64/kvm/hyp/nvhe/hyp.lds.S",
    )
    compiler_version = ctx.actions.declare_file(
        ctx.label.name + ".source/include/linux/compiler-version.h",
    )
    kconfig = ctx.actions.declare_file(
        ctx.label.name + ".source/include/linux/kconfig.h",
    )
    ctx.actions.write(hyp_lds, "")
    ctx.actions.write(compiler_version, "")
    ctx.actions.write(kconfig, "")
    return [DefaultInfo(files = depset([hyp_lds, compiler_version, kconfig]))]

_fake_nvhe_source_inputs = rule(implementation = _fake_nvhe_source_inputs_impl)

def _fake_vdso32_source_inputs_impl(ctx):
    files = []
    for path in [
        "arch/arm64/kernel/vdso32-wrap.S",
        "arch/arm64/kernel/vdso32/note.c",
        "arch/arm64/kernel/vdso32/vdso.lds.S",
        "arch/arm64/kernel/vdso32/vgettimeofday.c",
        "include/linux/compiler-version.h",
        "include/linux/compiler_types.h",
        "include/linux/kconfig.h",
        "lib/vdso/gettimeofday.c",
    ]:
        out = ctx.actions.declare_file(ctx.label.name + ".source/" + path)
        ctx.actions.write(out, "")
        files.append(out)
    return [DefaultInfo(files = depset(files))]

_fake_vdso32_source_inputs = rule(implementation = _fake_vdso32_source_inputs_impl)

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

def _image_output_groups_test_impl(ctx):
    env = analysistest.begin(ctx)
    output_groups = analysistest.target_under_test(env)[OutputGroupInfo]
    asserts.true(env, hasattr(output_groups, "image"))
    asserts.false(env, hasattr(output_groups, "modinfo"))
    asserts.false(env, hasattr(output_groups, "modules"))
    return analysistest.end(env)

_image_output_groups_test = analysistest.make(_image_output_groups_test_impl)

def _content_addressed_object_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    info = target[LinuxObjectInfo]
    asserts.equals(env, ctx.attr.expected_content_id, info.content_id)
    compile_actions = [
        action
        for action in analysistest.target_actions(env)
        if action.mnemonic == "LinuxObjectCompile"
    ]
    asserts.equals(env, 1, len(compile_actions))
    if compile_actions:
        config_inputs = [
            file
            for file in compile_actions[0].inputs.to_list()
            if ctx.attr.expected_payload_id in file.short_path
        ]
        asserts.true(env, len(config_inputs) > 0, "compile action did not select indexed config payload")
        generated_header_inputs = [
            file
            for file in compile_actions[0].inputs.to_list()
            if file.basename == ctx.attr.expected_generated_header
        ]
        asserts.equals(env, 1, len(generated_header_inputs))
        duplicate_header_inputs = [
            file
            for file in compile_actions[0].inputs.to_list()
            if file.basename == ctx.attr.unexpected_generated_header
        ]
        asserts.equals(env, 0, len(duplicate_header_inputs))
        unexpected_inputs = [
            file
            for file in compile_actions[0].inputs.to_list()
            if file.basename == ctx.attr.unexpected_input
        ]
        asserts.equals(env, 0, len(unexpected_inputs))
    return analysistest.end(env)

_content_addressed_object_test = analysistest.make(
    _content_addressed_object_test_impl,
    attrs = {
        "expected_content_id": attr.string(mandatory = True),
        "expected_generated_header": attr.string(mandatory = True),
        "expected_payload_id": attr.string(mandatory = True),
        "unexpected_generated_header": attr.string(mandatory = True),
        "unexpected_input": attr.string(mandatory = True),
    },
)

def _exact_nvhe_source_lookup_test_impl(ctx):
    env = analysistest.begin(ctx)
    linker_actions = [
        action
        for action in analysistest.target_actions(env)
        if action.mnemonic == "LinuxArm64NvheLinkerScript"
    ]
    compile_actions = [
        action
        for action in analysistest.target_actions(env)
        if action.mnemonic == "LinuxVmlinuxCompile"
    ]
    asserts.equals(env, 1, len(linker_actions))
    asserts.equals(env, 1, len(compile_actions))
    for action in linker_actions + compile_actions:
        inputs = [file.short_path for file in action.inputs.to_list()]
        asserts.true(
            env,
            any([path.endswith("/arch/arm64/kvm/hyp/nvhe/hyp.lds.S") for path in inputs]),
            "%s action did not consume hyp.lds.S from the exact source group" % action.mnemonic,
        )
        asserts.true(
            env,
            any([path.endswith("/include/linux/kconfig.h") for path in inputs]),
            "%s action did not consume kconfig.h from the exact source group" % action.mnemonic,
        )
        asserts.true(
            env,
            any([path.endswith("/include/linux/compiler-version.h") for path in inputs]),
            "%s action did not consume compiler-version.h from the exact source group" % action.mnemonic,
        )
        asserts.false(
            env,
            any([path.endswith("/include/linux/compiler_types.h") for path in inputs]),
            "%s action retained a C-only legacy preinclude" % action.mnemonic,
        )
        asserts.false(
            env,
            any([path.endswith("/linux_modules_test_fixture.rs") for path in inputs]),
            "%s action retained the legacy source-tree root marker" % action.mnemonic,
        )
    return analysistest.end(env)

_exact_nvhe_source_lookup_test = analysistest.make(_exact_nvhe_source_lookup_test_impl)

def _exact_vdso32_source_inputs_test_impl(ctx):
    env = analysistest.begin(ctx)
    compile_actions = [
        action
        for action in analysistest.target_actions(env)
        if action.mnemonic == "LinuxARM64VDSO32Compile"
    ]
    linker_script_actions = [
        action
        for action in analysistest.target_actions(env)
        if action.mnemonic == "LinuxARM64VDSO32LinkerScript"
    ]
    asserts.equals(env, 2, len(compile_actions))
    asserts.equals(env, 1, len(linker_script_actions))
    for action in compile_actions + linker_script_actions:
        inputs = [file.short_path for file in action.inputs.to_list()]
        for suffix in [
            "/arch/arm64/kernel/vdso32/note.c",
            "/arch/arm64/kernel/vdso32/vdso.lds.S",
            "/arch/arm64/kernel/vdso32/vgettimeofday.c",
            "/lib/vdso/gettimeofday.c",
        ]:
            asserts.true(
                env,
                any([path.endswith(suffix) for path in inputs]),
                "%s action did not consume %s from the exact source group" % (action.mnemonic, suffix),
            )
        asserts.false(
            env,
            any([path.endswith("/linux_modules_test_fixture.rs") for path in inputs]),
            "%s action retained the legacy source-tree root marker" % action.mnemonic,
        )
    return analysistest.end(env)

_exact_vdso32_source_inputs_test = analysistest.make(_exact_vdso32_source_inputs_test_impl)

def _delta_order_test_impl(ctx):
    env = analysistest.begin(ctx)
    image = analysistest.target_under_test(env)[LinuxImageInfo]
    asserts.equals(
        env,
        ctx.attr.expected_objects,
        [obj.content_id for obj in image.objects],
    )
    asserts.equals(
        env,
        ctx.attr.expected_modules,
        [obj.content_id for obj in image.module_objects],
    )
    return analysistest.end(env)

_delta_order_test = analysistest.make(
    _delta_order_test_impl,
    attrs = {
        "expected_modules": attr.string_list(),
        "expected_objects": attr.string_list(),
    },
)

def _cache_shape_test_impl(ctx):
    env = analysistest.begin(ctx)
    actions = analysistest.target_actions(env)
    asserts.equals(env, 1, len(actions))
    return analysistest.end(env)

_cache_shape_test = analysistest.make(_cache_shape_test_impl)

def linux_objects_fail_closed_test_suite(name):
    """Instantiates analysis tests for supported Linux object/image rules."""
    image = name + "_input_image"
    fixture_tags = ["manual"]
    object_a_id = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    object_b_id = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
    object_c_id = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
    module_m_id = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
    module_n_id = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
    unknown_module_id = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
    payload_id = "1111111111111111111111111111111111111111111111111111111111111111"
    environment_id = "2222222222222222222222222222222222222222222222222222222222222222"
    header_group_id = "3333333333333333333333333333333333333333333333333333333333333333"
    arm64_environment_id = "4444444444444444444444444444444444444444444444444444444444444444"
    arm64_header_group_id = "5555555555555555555555555555555555555555555555555555555555555555"
    vdso32_object_id = "6666666666666666666666666666666666666666666666666666666666666666"

    _fake_linux_image(
        name = image,
        tags = fixture_tags,
    )
    empty_image = name + "_empty_image"
    linux_compact_image(
        name = empty_image,
        objects = [],
        tags = fixture_tags,
    )

    certificate_object = name + "_certificate_object"
    linux_object(
        name = certificate_object,
        src = "linux_objects_test_fixture.c",
        mode = "y",
        object = "certs/system_certificates.o",
        tags = fixture_tags,
    )

    generated_headers = name + "_generated_headers_a"
    duplicate_generated_headers = name + "_generated_headers_z"
    _fake_generated_headers(
        name = generated_headers,
        content_id = header_group_id,
        tags = fixture_tags,
    )
    _fake_generated_headers(
        name = duplicate_generated_headers,
        content_id = header_group_id,
        tags = fixture_tags,
    )
    compile_environment_index = name + "_compile_environment_index"
    linux_compile_environment_index(
        name = compile_environment_index,
        compile_environments = {
            environment_id: json.encode({
                "abi": "x86_64-linux-gnu",
                "config_payload": payload_id,
                "header_groups": [header_group_id],
            }),
        },
        config_payloads = {
            payload_id: "CONFIG_TEST=y\n",
        },
        expected_abi = "x86_64-linux-gnu",
        generated_headers = {
            ":" + generated_headers: header_group_id,
            ":" + duplicate_generated_headers: header_group_id,
        },
        tags = fixture_tags,
    )
    arm64_generated_headers = name + "_arm64_generated_headers"
    _fake_arm64_generated_headers(
        name = arm64_generated_headers,
        content_id = arm64_header_group_id,
        tags = fixture_tags,
    )
    arm64_compile_environment_index = name + "_arm64_compile_environment_index"
    linux_compile_environment_index(
        name = arm64_compile_environment_index,
        arch = "arm64",
        compile_environments = {
            arm64_environment_id: json.encode({
                "abi": "arm64-linux-gnu",
                "config_payload": payload_id,
                "header_groups": [arm64_header_group_id],
            }),
        },
        config_payloads = {
            payload_id: "CONFIG_TEST=y\n",
        },
        expected_abi = "arm64-linux-gnu",
        generated_headers = {
            ":" + arm64_generated_headers: arm64_header_group_id,
        },
        tags = fixture_tags,
    )
    unbound_generated_headers = name + "_unbound_generated_headers"
    _fake_generated_headers(
        name = unbound_generated_headers,
        content_id = "",
        tags = fixture_tags,
    )
    unbound_header_index = name + "_unbound_header_index"
    linux_compile_environment_index(
        name = unbound_header_index,
        compile_environments = {
            environment_id: json.encode({
                "abi": "x86_64-linux-gnu",
                "config_payload": payload_id,
                "header_groups": [header_group_id],
            }),
        },
        config_payloads = {
            payload_id: "CONFIG_TEST=y\n",
        },
        expected_abi = "x86_64-linux-gnu",
        generated_headers = {
            ":" + unbound_generated_headers: header_group_id,
        },
        tags = fixture_tags,
    )
    source_tree = name + "_source_tree"
    linux_source_tree(
        name = source_tree,
        headers = [
            "include/linux/compiler-version.h",
            "include/linux/compiler_types.h",
            "include/linux/kconfig.h",
        ],
        root = "linux_modules_test_fixture.rs",
        tags = fixture_tags,
    )
    source_input_index = name + "_source_input_index"
    linux_source_input_index(
        name = source_input_index,
        groups = ["1,2,3,4"],
        source_paths = [
            "include/linux/compiler-version.h",
            "include/linux/compiler_types.h",
            "include/linux/kconfig.h",
            "linux_objects_test_fixture.c",
        ],
        srcs = [
            "include/linux/compiler-version.h",
            "include/linux/compiler_types.h",
            "include/linux/kconfig.h",
            "linux_objects_test_fixture.c",
        ],
        tags = fixture_tags,
    )
    nvhe_source_inputs = name + "_nvhe_source_inputs"
    _fake_nvhe_source_inputs(
        name = nvhe_source_inputs,
        tags = fixture_tags,
    )
    nvhe_source_input_index = name + "_nvhe_source_input_index"
    linux_source_input_index(
        name = nvhe_source_input_index,
        groups = ["1,2,3"],
        source_paths = [
            "arch/arm64/kvm/hyp/nvhe/hyp.lds.S",
            "include/linux/compiler-version.h",
            "include/linux/kconfig.h",
        ],
        srcs = [":" + nvhe_source_inputs],
        tags = fixture_tags,
    )
    vdso32_source_inputs = name + "_vdso32_source_inputs"
    _fake_vdso32_source_inputs(
        name = vdso32_source_inputs,
        tags = fixture_tags,
    )
    vdso32_source_input_index = name + "_vdso32_source_input_index"
    linux_source_input_index(
        name = vdso32_source_input_index,
        groups = ["1,2,3,4,5,6,7,8"],
        source_paths = [
            "arch/arm64/kernel/vdso32-wrap.S",
            "arch/arm64/kernel/vdso32/note.c",
            "arch/arm64/kernel/vdso32/vdso.lds.S",
            "arch/arm64/kernel/vdso32/vgettimeofday.c",
            "include/linux/compiler-version.h",
            "include/linux/compiler_types.h",
            "include/linux/kconfig.h",
            "lib/vdso/gettimeofday.c",
        ],
        srcs = [":" + vdso32_source_inputs],
        tags = fixture_tags,
    )
    duplicate_source_group_index = name + "_duplicate_source_group_index"
    linux_source_input_index(
        name = duplicate_source_group_index,
        groups = ["1", "1"],
        source_paths = ["linux_objects_test_fixture.c"],
        srcs = ["linux_objects_test_fixture.c"],
        tags = fixture_tags,
    )
    out_of_range_source_file_index = name + "_out_of_range_source_file_index"
    linux_source_input_index(
        name = out_of_range_source_file_index,
        groups = ["2"],
        source_paths = ["linux_objects_test_fixture.c"],
        srcs = ["linux_objects_test_fixture.c"],
        tags = fixture_tags,
    )
    duplicate_source_file_index = name + "_duplicate_source_file_index"
    linux_source_input_index(
        name = duplicate_source_file_index,
        groups = ["1,2"],
        source_paths = ["duplicate.h", "duplicate.h"],
        srcs = [
            "include/linux/compiler-version.h",
            "include/linux/compiler_types.h",
        ],
        tags = fixture_tags,
    )
    indexed_object = name + "_indexed_object"
    linux_object(
        name = indexed_object,
        compile_environment_id = environment_id,
        compile_environment_index = ":" + compile_environment_index,
        content_id = object_a_id,
        mode = "y",
        object = "indexed.o",
        source_input_file = 4,
        source_input_group = 1,
        source_input_index = ":" + source_input_index,
        source_tree_info = ":" + source_tree,
        tags = fixture_tags,
    )
    indexed_object_test = indexed_object + "_test"
    _content_addressed_object_test(
        name = indexed_object_test,
        expected_content_id = object_a_id,
        expected_generated_header = generated_headers + ".h",
        expected_payload_id = payload_id,
        target_under_test = ":" + indexed_object,
        unexpected_generated_header = duplicate_generated_headers + ".h",
        unexpected_input = "linux_modules_test_fixture.rs",
    )

    empty_compile_environment_index = name + "_empty_compile_environment_index"
    _fake_compile_environment_index(
        name = empty_compile_environment_index,
        tags = fixture_tags,
    )
    invalid_environment_object = name + "_invalid_environment_object"
    linux_object(
        name = invalid_environment_object,
        compile_environment_id = "not-a-sha256",
        compile_environment_index = ":" + empty_compile_environment_index,
        content_id = object_a_id,
        mode = "y",
        object = "invalid-environment.o",
        source_input_file = 4,
        source_input_group = 1,
        source_input_index = ":" + source_input_index,
        tags = fixture_tags,
    )
    missing_content_id_object = name + "_missing_content_id_object"
    linux_object(
        name = missing_content_id_object,
        compile_environment_id = environment_id,
        compile_environment_index = ":" + compile_environment_index,
        mode = "y",
        object = "missing-content-id.o",
        source_input_file = 4,
        source_input_group = 1,
        source_input_index = ":" + source_input_index,
        tags = fixture_tags,
    )
    incomplete_source_inputs_object = name + "_incomplete_source_inputs_object"
    linux_object(
        name = incomplete_source_inputs_object,
        compile_environment_id = environment_id,
        compile_environment_index = ":" + compile_environment_index,
        content_id = object_c_id,
        mode = "y",
        object = "incomplete-source-inputs.o",
        src = "linux_objects_test_fixture.c",
        tags = fixture_tags,
    )
    out_of_range_primary_source_object = name + "_out_of_range_primary_source_object"
    linux_object(
        name = out_of_range_primary_source_object,
        compile_environment_id = environment_id,
        compile_environment_index = ":" + compile_environment_index,
        content_id = object_c_id,
        mode = "y",
        object = "out-of-range-primary-source.o",
        source_input_file = 5,
        source_input_group = 1,
        source_input_index = ":" + source_input_index,
        tags = fixture_tags,
    )
    missing_content_id_nvhe = name + "_missing_content_id_nvhe"
    linux_arm64_nvhe_object(
        name = missing_content_id_nvhe,
        compile_environment_id = environment_id,
        compile_environment_index = ":" + compile_environment_index,
        mode = "y",
        object = "arch/arm64/kvm/hyp/nvhe/kvm_nvhe.o",
        source_input_group = 1,
        source_input_index = ":" + nvhe_source_input_index,
        tags = fixture_tags,
    )
    incomplete_source_inputs_nvhe = name + "_incomplete_source_inputs_nvhe"
    linux_arm64_nvhe_object(
        name = incomplete_source_inputs_nvhe,
        compile_environment_id = environment_id,
        compile_environment_index = ":" + compile_environment_index,
        content_id = object_c_id,
        mode = "y",
        object = "arch/arm64/kvm/hyp/nvhe/kvm_nvhe.o",
        tags = fixture_tags,
    )
    empty_source_inputs_nvhe = name + "_empty_source_inputs_nvhe"
    linux_arm64_nvhe_object(
        name = empty_source_inputs_nvhe,
        compile_environment_id = environment_id,
        compile_environment_index = ":" + compile_environment_index,
        content_id = object_b_id,
        mode = "y",
        object = "arch/arm64/kvm/hyp/nvhe/kvm_nvhe.o",
        source_input_group = 2,
        source_input_index = ":" + nvhe_source_input_index,
        tags = fixture_tags,
    )
    mismatched_abi_index = name + "_mismatched_abi_index"
    linux_compile_environment_index(
        name = mismatched_abi_index,
        compile_environments = {
            environment_id: json.encode({
                "abi": "actual-abi",
                "config_payload": payload_id,
                "header_groups": [],
            }),
        },
        config_payloads = {
            payload_id: "CONFIG_TEST=y\n",
        },
        expected_abi = "expected-abi",
        tags = fixture_tags,
    )
    legacy_config = name + "_legacy_config"
    linux_config(
        name = legacy_config,
        tags = fixture_tags,
    )
    equivalent_config = name + "_equivalent_config"
    linux_config(
        name = equivalent_config,
        config_flags = {
            "CONFIG_TEST": "y",
        },
        tags = fixture_tags,
    )
    indexed_equivalent_object = name + "_indexed_equivalent_object"
    linux_object(
        name = indexed_equivalent_object,
        compile_environment_id = environment_id,
        compile_environment_index = ":" + compile_environment_index,
        content_id = object_b_id,
        mode = "y",
        object = "equivalent.o",
        source_input_file = 4,
        source_input_group = 1,
        source_input_index = ":" + source_input_index,
        source_tree_info = ":" + source_tree,
        tags = fixture_tags,
    )
    legacy_equivalent_object = name + "_legacy_equivalent_object"
    linux_object(
        name = legacy_equivalent_object,
        config = ":" + equivalent_config,
        mode = "y",
        object = "equivalent.o",
        source_tree_info = ":" + source_tree,
        src = "linux_objects_test_fixture.c",
        tags = fixture_tags,
    )
    native.filegroup(
        name = indexed_equivalent_object + "_output",
        output_group = "object",
        srcs = [":" + indexed_equivalent_object],
    )
    native.filegroup(
        name = legacy_equivalent_object + "_output",
        output_group = "object",
        srcs = [":" + legacy_equivalent_object],
    )
    equivalent_object_test = name + "_indexed_object_output_test"
    diff_test(
        name = equivalent_object_test,
        file1 = ":" + indexed_equivalent_object + "_output",
        file2 = ":" + legacy_equivalent_object + "_output",
    )
    mixed_environment_object = name + "_mixed_environment_object"
    linux_object(
        name = mixed_environment_object,
        compile_environment_id = environment_id,
        compile_environment_index = ":" + empty_compile_environment_index,
        config = ":" + legacy_config,
        content_id = object_a_id,
        mode = "y",
        object = "mixed-environment.o",
        source_input_file = 4,
        source_input_group = 1,
        source_input_index = ":" + source_input_index,
        tags = fixture_tags,
    )

    failure_cases = [
        (empty_image, "requires at least one compiled object"),
        (certificate_object, "hermetic certificate embedding and signing are not implemented"),
        (duplicate_source_file_index, "duplicate or non-canonical source path"),
        (duplicate_source_group_index, "duplicate or non-canonical group"),
        (empty_source_inputs_nvhe, "source_input_group 2 is out of range"),
        (incomplete_source_inputs_nvhe, "requires compile_environment_index and source_input_index together"),
        (incomplete_source_inputs_object, "requires compile_environment_index and source_input_index together"),
        (invalid_environment_object, "must be a full lowercase SHA-256 content ID"),
        (mismatched_abi_index, "does not match expected_abi"),
        (missing_content_id_nvhe, "linux_arm64_nvhe_object content_id must be a full lowercase SHA-256 content ID"),
        (missing_content_id_object, "linux_object content_id must be a full lowercase SHA-256 content ID"),
        (mixed_environment_object, "mutually exclusive with legacy config/generated_headers"),
        (out_of_range_primary_source_object, "source_input_file 5 is out of range"),
        (out_of_range_source_file_index, "file index 2 is out of range"),
        (unbound_header_index, "must publish content ID"),
    ]
    tests = [
        ":" + equivalent_object_test,
        ":" + indexed_object_test,
    ]
    for target, expected_error in failure_cases:
        test_name = target + "_test"
        _failure_test(
            name = test_name,
            expected_error = expected_error,
            target_under_test = ":" + target,
        )
        tests.append(":" + test_name)

    object_targets = {}
    for suffix, content_id, mode in [
        ("a", object_a_id, "y"),
        ("b", object_b_id, "y"),
        ("c", object_c_id, "y"),
        ("m", module_m_id, "m"),
        ("n", module_n_id, "m"),
    ]:
        target = name + "_object_" + suffix
        _fake_linux_object(
            name = target,
            content_id = content_id,
            mode = mode,
            object = suffix + ".o",
            tags = fixture_tags,
        )
        object_targets[suffix] = ":" + target

    exact_nvhe = name + "_exact_nvhe"
    linux_arm64_nvhe_object(
        name = exact_nvhe,
        compile_environment_id = environment_id,
        compile_environment_index = ":" + compile_environment_index,
        content_id = object_c_id,
        mode = "y",
        object = "arch/arm64/kvm/hyp/nvhe/kvm_nvhe.o",
        objects = [object_targets["a"]],
        source_input_group = 1,
        source_input_index = ":" + nvhe_source_input_index,
        source_tree_info = ":" + source_tree,
        tags = fixture_tags,
    )
    exact_nvhe_test = exact_nvhe + "_test"
    _exact_nvhe_source_lookup_test(
        name = exact_nvhe_test,
        target_under_test = ":" + exact_nvhe,
    )
    tests.append(":" + exact_nvhe_test)

    exact_vdso32 = name + "_exact_vdso32"
    linux_object(
        name = exact_vdso32,
        compile_environment_id = arm64_environment_id,
        compile_environment_index = ":" + arm64_compile_environment_index,
        content_id = vdso32_object_id,
        mode = "y",
        object = "arch/arm64/kernel/vdso32-wrap.o",
        source_input_file = 1,
        source_input_group = 1,
        source_input_index = ":" + vdso32_source_input_index,
        source_tree_info = ":" + source_tree,
        tags = fixture_tags,
    )
    exact_vdso32_test = exact_vdso32 + "_test"
    _exact_vdso32_source_inputs_test(
        name = exact_vdso32_test,
        target_under_test = ":" + exact_vdso32,
    )
    tests.append(":" + exact_vdso32_test)

    compact_base = name + "_compact_base"
    linux_compact_image(
        name = compact_base,
        module_objects = [
            object_targets["m"],
            object_targets["n"],
        ],
        objects = [
            object_targets["a"],
            object_targets["b"],
        ],
        tags = fixture_tags,
    )
    invalid_compact_base = name + "_invalid_compact_base_mode"
    linux_compact_image(
        name = invalid_compact_base,
        objects = [object_targets["m"]],
        tags = fixture_tags,
    )
    invalid_compact_base_test = invalid_compact_base + "_test"
    _failure_test(
        name = invalid_compact_base_test,
        expected_error = "has mode \"m\", want \"y\"",
        target_under_test = ":" + invalid_compact_base,
    )
    tests.append(":" + invalid_compact_base_test)

    compact_delta = name + "_compact_delta"
    linux_compact_delta_image(
        name = compact_delta,
        add_objects = [
            object_targets["c"],
        ],
        base_image = ":" + compact_base,
        ordered_content_ids = [
            object_c_id,
            object_a_id,
        ],
        ordered_module_content_ids = [
            module_n_id,
            module_m_id,
        ],
        remove_content_ids = [
            object_b_id,
        ],
        tags = fixture_tags,
    )
    compact_delta_test = compact_delta + "_test"
    _delta_order_test(
        name = compact_delta_test,
        expected_modules = [
            module_n_id,
            module_m_id,
        ],
        expected_objects = [
            object_c_id,
            object_a_id,
        ],
        target_under_test = ":" + compact_delta,
    )
    tests.append(":" + compact_delta_test)

    compact_full = name + "_compact_full"
    linux_compact_image(
        name = compact_full,
        module_objects = [
            object_targets["n"],
            object_targets["m"],
        ],
        objects = [
            object_targets["c"],
            object_targets["a"],
        ],
        tags = fixture_tags,
    )
    compact_delta_archive_test = compact_delta + "_archive_test"
    diff_test(
        name = compact_delta_archive_test,
        file1 = ":" + compact_delta,
        file2 = ":" + compact_full,
    )
    tests.append(":" + compact_delta_archive_test)

    duplicate_base_object = name + "_duplicate_base_object"
    _fake_linux_object(
        name = duplicate_base_object,
        content_id = object_a_id,
        mode = "y",
        object = "duplicate-a.o",
        tags = fixture_tags,
    )
    readded_base_delta = name + "_readded_base_delta"
    linux_compact_delta_image(
        name = readded_base_delta,
        add_objects = [":" + duplicate_base_object],
        base_image = ":" + compact_base,
        ordered_content_ids = [
            object_a_id,
            object_b_id,
        ],
        ordered_module_content_ids = [
            module_m_id,
            module_n_id,
        ],
        remove_content_ids = [object_a_id],
        tags = fixture_tags,
    )
    readded_base_delta_test = readded_base_delta + "_test"
    _failure_test(
        name = readded_base_delta_test,
        expected_error = "re-adds base content ID",
        target_under_test = ":" + readded_base_delta,
    )
    tests.append(":" + readded_base_delta_test)

    invalid_delta = name + "_invalid_delta_order"
    linux_compact_delta_image(
        name = invalid_delta,
        base_image = ":" + compact_base,
        ordered_content_ids = [object_a_id],
        ordered_module_content_ids = [
            module_m_id,
            module_n_id,
        ],
        tags = fixture_tags,
    )
    invalid_delta_test = invalid_delta + "_test"
    _failure_test(
        name = invalid_delta_test,
        expected_error = "omits built-in content ID(s)",
        target_under_test = ":" + invalid_delta,
    )
    tests.append(":" + invalid_delta_test)

    duplicate_module_order = name + "_duplicate_module_order"
    linux_compact_delta_image(
        name = duplicate_module_order,
        base_image = ":" + compact_base,
        ordered_content_ids = [
            object_a_id,
            object_b_id,
        ],
        ordered_module_content_ids = [
            module_m_id,
            module_m_id,
            module_n_id,
        ],
        tags = fixture_tags,
    )
    duplicate_module_order_test = duplicate_module_order + "_test"
    _failure_test(
        name = duplicate_module_order_test,
        expected_error = "repeats ordered module content ID",
        target_under_test = ":" + duplicate_module_order,
    )
    tests.append(":" + duplicate_module_order_test)

    unknown_module_order = name + "_unknown_module_order"
    linux_compact_delta_image(
        name = unknown_module_order,
        base_image = ":" + compact_base,
        ordered_content_ids = [
            object_a_id,
            object_b_id,
        ],
        ordered_module_content_ids = [
            module_m_id,
            module_n_id,
            unknown_module_id,
        ],
        tags = fixture_tags,
    )
    unknown_module_order_test = unknown_module_order + "_test"
    _failure_test(
        name = unknown_module_order_test,
        expected_error = "orders unknown module content ID",
        target_under_test = ":" + unknown_module_order,
    )
    tests.append(":" + unknown_module_order_test)

    omitted_module_order = name + "_omitted_module_order"
    linux_compact_delta_image(
        name = omitted_module_order,
        base_image = ":" + compact_base,
        ordered_content_ids = [
            object_a_id,
            object_b_id,
        ],
        ordered_module_content_ids = [module_m_id],
        tags = fixture_tags,
    )
    omitted_module_order_test = omitted_module_order + "_test"
    _failure_test(
        name = omitted_module_order_test,
        expected_error = "omits module content ID(s)",
        target_under_test = ":" + omitted_module_order,
    )
    tests.append(":" + omitted_module_order_test)

    cache_shape = name + "_cache_shape"
    linux_cache_shape_check(
        name = cache_shape,
        images = [
            ":" + compact_base,
            ":" + compact_delta,
        ],
        shared_objects = ["a.o"],
        tags = fixture_tags,
    )
    cache_shape_test = cache_shape + "_test"
    _cache_shape_test(
        name = cache_shape_test,
        target_under_test = ":" + cache_shape,
    )
    tests.append(":" + cache_shape_test)

    real_arm64_image = name + "_real_arm64_image"
    linux_compressed_image(
        name = real_arm64_image,
        arch = "arm64",
        format = "arm64_image",
        image = ":" + image,
        tags = fixture_tags,
    )
    output_groups_test = real_arm64_image + "_output_groups_test"
    _image_output_groups_test(
        name = output_groups_test,
        target_under_test = ":" + real_arm64_image,
    )
    tests.append(":" + output_groups_test)

    native.test_suite(
        name = name,
        tests = tests,
    )
