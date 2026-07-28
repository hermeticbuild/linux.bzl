"""Tests for kconfig repository platform selection."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts", "unittest")
load("//internal:kconfig_tool_filename.bzl", "kconfig_tool_filename")
load("//internal:linux_image_repository.bzl", "repositories_test_helpers")

visibility("private")

def _kconfig_tool_filename_test_impl(ctx):
    env = unittest.begin(ctx)
    asserts.equals(env, "kconfig.exe", kconfig_tool_filename("windows_amd64", "kconfig"))
    asserts.equals(env, "kconfig_parse.exe", kconfig_tool_filename("windows_amd64", "kconfig_parse"))
    asserts.equals(env, "kconfig", kconfig_tool_filename("linux_amd64", "kconfig"))
    return unittest.end(env)

kconfig_tool_filename_test = unittest.make(_kconfig_tool_filename_test_impl)

def _graph_config_args_test_impl(ctx):
    env = unittest.begin(ctx)
    asserts.equals(
        env,
        [
            "-config",
            "aarch64=configs/base.config",
            "-config_mode",
            "allnoconfig",
        ],
        repositories_test_helpers.graph_config_args(
            "aarch64",
            "configs/base.config",
            "allnoconfig",
        ),
    )
    return unittest.end(env)

graph_config_args_test = unittest.make(_graph_config_args_test_impl)

def _graph_host_tool_args_test_impl(ctx):
    env = unittest.begin(ctx)
    asserts.equals(
        env,
        [
            "-source_asn1_compiler",
            "//:_base_asn1_compiler_tool",
        ],
        repositories_test_helpers.graph_host_tool_args("x86_64"),
    )
    asserts.equals(
        env,
        [
            "-source_asn1_compiler",
            "//:_base_asn1_compiler_tool",
            "-source_relacheck",
            "//:_base_relacheck_tool",
        ],
        repositories_test_helpers.graph_host_tool_args("aarch64"),
    )
    return unittest.end(env)

graph_host_tool_args_test = unittest.make(_graph_host_tool_args_test_impl)

def _graph_configs_args_test_impl(ctx):
    env = unittest.begin(ctx)
    asserts.equals(
        env,
        [
            "-config",
            "debug=configs/debug.config",
            "-config",
            "x86_64=configs/base.config",
            "-config_mode",
            "default",
        ],
        repositories_test_helpers.graph_configs_args(
            {
                "x86_64": "configs/base.config",
                "debug": "configs/debug.config",
            },
            "default",
        ),
    )
    asserts.equals(env, "v0.0.13", repositories_test_helpers.content_schema)
    return unittest.end(env)

graph_configs_args_test = unittest.make(_graph_configs_args_test_impl)

def _generated_object_inputs_test_impl(ctx):
    env = unittest.begin(ctx)
    legacy = """
    name = "legacy",
    src = "//source:legacy.c",
"""
    indexed = """
    name = "indexed",
    source_input_file = 7,
    source_input_group = 3,
    source_input_index = ":_source_input_index",
"""
    incomplete_indexed = """
    name = "incomplete",
    source_input_file = 7,
    source_input_index = ":_source_input_index",
"""
    asserts.true(
        env,
        repositories_test_helpers.generated_object_block_has_buildable_inputs(
            legacy,
            "v0.0.12",
        ),
    )
    asserts.false(
        env,
        repositories_test_helpers.generated_object_block_has_buildable_inputs(
            indexed,
            "v0.0.12",
        ),
    )
    asserts.true(
        env,
        repositories_test_helpers.generated_object_block_has_buildable_inputs(
            indexed,
            repositories_test_helpers.content_schema,
        ),
    )
    asserts.false(
        env,
        repositories_test_helpers.generated_object_block_has_buildable_inputs(
            incomplete_indexed,
            repositories_test_helpers.content_schema,
        ),
    )
    return unittest.end(env)

generated_object_inputs_test = unittest.make(_generated_object_inputs_test_impl)

def _compile_environment_abi_subject_impl(ctx):
    repositories_test_helpers.validate_compile_environment_abi(
        ctx.attr.actual,
        ctx.attr.expected,
        ctx.label,
    )
    return []

_compile_environment_abi_subject = rule(
    implementation = _compile_environment_abi_subject_impl,
    attrs = {
        "actual": attr.string(mandatory = True),
        "expected": attr.string(mandatory = True),
    },
)

def _compile_environment_abi_failure_test_impl(ctx):
    env = analysistest.begin(ctx)
    asserts.expect_failure(env, "does not match expected ABI")
    return analysistest.end(env)

_compile_environment_abi_failure_test = analysistest.make(
    _compile_environment_abi_failure_test_impl,
    expect_failure = True,
)

def compile_environment_abi_test(name):
    subject = name + "_subject"
    _compile_environment_abi_subject(
        name = subject,
        actual = "unexpected-abi",
        expected = "linux.bzl/compact-v4/llvm-22.1.8/x86/x86",
        tags = ["manual"],
    )
    _compile_environment_abi_failure_test(
        name = name,
        target_under_test = ":" + subject,
    )

def _header_config_aliases_test_impl(ctx):
    env = unittest.begin(ctx)
    metadata = {
        "header_groups": [
            {
                "id": "1111111111111111111111111111111111111111111111111111111111111111",
                "labels": [
                    "//:_base_x86_generated_headers",
                    "//:_variant_lz4_x86_generated_headers",
                ],
            },
            {
                "id": "2222222222222222222222222222222222222222222222222222222222222222",
                "labels": ["//:_variant_debug_x86_generated_headers"],
            },
        ],
    }
    generated_headers = {
        "x86_64": "//:_base_x86_generated_headers",
        "debug": "//:_variant_debug_x86_generated_headers",
        "lz4": "//:_variant_lz4_x86_generated_headers",
    }
    aliases = repositories_test_helpers.header_config_aliases(
        metadata,
        generated_headers,
        "x86_64",
    )
    asserts.equals(
        env,
        {
            "x86_64": "x86_64",
            "debug": "debug",
            "lz4": "x86_64",
        },
        aliases,
    )
    index = repositories_test_helpers.header_config_index(
        metadata,
        generated_headers,
        "x86_64",
    )
    asserts.equals(
        env,
        {
            "x86_64": "1111111111111111111111111111111111111111111111111111111111111111",
            "debug": "2222222222222222222222222222222222222222222222222222222222222222",
            "lz4": "1111111111111111111111111111111111111111111111111111111111111111",
        },
        index.content_ids,
    )
    return unittest.end(env)

header_config_aliases_test = unittest.make(_header_config_aliases_test_impl)

def _core_config_aliases_test_impl(ctx):
    env = unittest.begin(ctx)
    names = [
        "x86_64",
        "debug",
        "header_split",
        "lz4",
        "module_order",
        "noncompression",
        "object_order",
        "rust_split",
    ]
    metadata = {
        "configs": [
            {
                "name": name,
                "object_targets": (
                    ["b", "a"] if name == "object_order" else ["a", "c"] if name == "debug" else ["a", "b"]
                ),
                "module_object_targets": ["n", "m"] if name == "module_order" else ["m", "n"],
            }
            for name in names
        ],
    }
    configs = {
        name: {
            "CONFIG_COMMON": "y",
            "CONFIG_KERNEL_GZIP": "y" if name == "x86_64" else "n",
            "CONFIG_KERNEL_LZ4": "n" if name == "x86_64" else "y",
        }
        for name in names
    }
    configs["debug"]["CONFIG_DEBUG_INFO"] = "y"
    configs["noncompression"]["CONFIG_WERROR"] = "y"
    rust_enabled = {
        name: name == "rust_split"
        for name in names
    }
    header_configs = {
        name: name if name == "header_split" else "x86_64"
        for name in names
    }

    aliases = repositories_test_helpers.core_config_aliases(
        metadata,
        configs,
        rust_enabled,
        header_configs,
        "x86_64",
    )
    asserts.equals(
        env,
        {
            "x86_64": "x86_64",
            "debug": "debug",
            "header_split": "header_split",
            "lz4": "x86_64",
            "module_order": "module_order",
            "noncompression": "noncompression",
            "object_order": "object_order",
            "rust_split": "rust_split",
        },
        aliases,
    )

    generated = repositories_test_helpers.kernel_root_build(
        arch = "x86_64",
        version = "6.18.39",
        source_repo = "@@linux_sources",
        rust_profile_json = "",
        platform = "@@llvm//platforms:linux_x86_64",
        base_config = "//configs:x86_64",
        base_header_content_id = "1111111111111111111111111111111111111111111111111111111111111111",
        base_rust_enabled = False,
        config_mode = "default",
        graph_image = "//graph:x86_64_image",
        variant_configs = {"lz4": "//configs:lz4"},
        variant_core_configs = {"lz4": "x86_64"},
        variant_graph_images = {"lz4": "//graph:lz4_image"},
        variant_header_content_ids = {"lz4": "1111111111111111111111111111111111111111111111111111111111111111"},
        variant_header_configs = {"lz4": "x86_64"},
        variant_rust_enabled = {"lz4": False},
        rules_repo = "@@linux_bzl",
    )
    asserts.true(
        env,
        'variant_core_configs = {\n        "lz4": "x86_64",\n    },' in generated,
    )
    asserts.true(
        env,
        'base_header_content_id = "1111111111111111111111111111111111111111111111111111111111111111",' in generated,
    )
    asserts.true(
        env,
        'variant_header_content_ids = {\n        "lz4": "1111111111111111111111111111111111111111111111111111111111111111",\n    },' in generated,
    )
    return unittest.end(env)

core_config_aliases_test = unittest.make(_core_config_aliases_test_impl)
