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

def _generator_variable_args_test_impl(ctx):
    env = unittest.begin(ctx)
    asserts.equals(
        env,
        [
            "-var",
            "ARCH=x86",
            "-var",
            "SRCARCH=x86",
            "-var",
            "srctree=D:/_bazel/external/+linux_source_repository+linux_6_18_39",
        ],
        repositories_test_helpers.generator_variable_args(
            {
                "ARCH": "x86",
                "SRCARCH": "x86",
            },
            "D:\\_bazel\\external\\+linux_source_repository+linux_6_18_39",
        ),
    )
    return unittest.end(env)

generator_variable_args_test = unittest.make(_generator_variable_args_test_impl)

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
    return unittest.end(env)

graph_configs_args_test = unittest.make(_graph_configs_args_test_impl)

def _metadata_with_key(metadata, collection, key, value):
    result = dict(metadata)
    if collection:
        items = list(result[collection])
        item = dict(items[0])
        item[key] = value
        items[0] = item
        result[collection] = items
    else:
        result[key] = value
    return result

def _metadata_without_key(metadata, collection, key):
    result = dict(metadata)
    if collection:
        items = list(result[collection])
        item = dict(items[0])
        item.pop(key)
        items[0] = item
        result[collection] = items
    else:
        result.pop(key)
    return result

def _metadata_key_validation_test_impl(ctx):
    env = unittest.begin(ctx)
    metadata = {
        "configs": [{
            "config_payload": "",
            "module_object_targets": [],
            "name": "",
            "object_targets": [],
        }],
        "config_payloads": [{
            "content": "",
            "id": "",
        }],
        "compile_environments": [{
            "abi": "",
            "config_payload": "",
            "generated_header_families": [],
            "id": "",
        }],
        "generated_header_families": [{
            "config_payload": "",
            "dependencies": [],
            "id": "",
            "labels": [],
            "name": "",
            "source_input_group": 0,
            "srcarch": "",
        }],
        "object_variants": [{
            "compile_environment": "",
            "content_id": "",
            "deps": [],
            "flags": [],
            "members": [],
            "mode": "",
            "modname": "",
            "object": "",
            "remove_flags": [],
            "source": "",
            "source_input_group": 0,
            "target": "",
        }],
        "source_files": [{
            "digest": "",
            "path": "",
        }],
        "source_input_groups": [],
    }
    asserts.equals(
        env,
        "",
        repositories_test_helpers.content_graph_metadata_structure_error(metadata),
    )
    cases = [
        ("", "schema", "v0.0.13"),
        ("", "object_packages", []),
        ("configs", "package", ""),
        ("configs", "image_target", "base_image"),
        ("config_payloads", "fragment", {}),
        ("compile_environments", "schema", ""),
        ("generated_header_families", "source_inputs", []),
        ("source_files", "package", ""),
        ("object_variants", "source_includes", []),
        ("object_variants", "source_includes_complete", False),
        ("object_variants", "source_inputs", []),
    ]
    for collection, key, value in cases:
        error = repositories_test_helpers.content_graph_metadata_structure_error(
            _metadata_with_key(metadata, collection, key, value),
        )
        asserts.true(
            env,
            key in error,
            "metadata with retired field %r should be rejected, got %r" % (key, error),
        )
    sparse = {
        "configs": [{
            "name": "base",
            "object_targets": [],
        }],
        "config_payloads": [{
            "content": "",
            "id": "",
        }],
        "compile_environments": [{
            "abi": "",
            "config_payload": "",
            "id": "",
        }],
        "generated_header_families": [{
            "config_payload": "",
            "id": "",
            "name": "",
            "srcarch": "",
        }],
        "object_variants": [{
            "mode": "y",
            "object": "init/main.o",
            "target": "init",
        }],
        "source_files": [{
            "digest": "",
            "path": "",
        }],
        "source_input_groups": [],
    }
    asserts.equals(
        env,
        "",
        repositories_test_helpers.content_graph_metadata_structure_error(sparse),
    )
    invalid = [
        (
            _metadata_without_key(metadata, "", "configs"),
            "configs",
        ),
        (
            _metadata_with_key(metadata, "", "configs", None),
            "configs",
        ),
        (
            _metadata_with_key(metadata, "", "configs", {}),
            "configs",
        ),
        (
            _metadata_without_key(metadata, "configs", "object_targets"),
            "object_targets",
        ),
        (
            _metadata_with_key(metadata, "configs", "object_targets", None),
            "object_targets",
        ),
        (
            _metadata_with_key(metadata, "configs", "object_targets", ""),
            "object_targets",
        ),
        (
            _metadata_with_key(metadata, "configs", "object_targets", [1]),
            "object_targets",
        ),
        (
            _metadata_with_key(metadata, "configs", "config_payload", None),
            "config_payload",
        ),
        (
            _metadata_with_key(metadata, "configs", "module_object_targets", None),
            "module_object_targets",
        ),
        (
            _metadata_with_key(metadata, "compile_environments", "generated_header_families", None),
            "generated_header_families",
        ),
        (
            _metadata_with_key(metadata, "generated_header_families", "labels", None),
            "labels",
        ),
        (
            _metadata_with_key(metadata, "generated_header_families", "source_input_group", None),
            "source_input_group",
        ),
        (
            _metadata_with_key(metadata, "object_variants", "source", None),
            "source",
        ),
        (
            _metadata_with_key(metadata, "object_variants", "flags", None),
            "flags",
        ),
        (
            _metadata_with_key(metadata, "object_variants", "source_input_group", "1"),
            "source_input_group",
        ),
    ]
    for candidate, want in invalid:
        error = repositories_test_helpers.content_graph_metadata_structure_error(candidate)
        asserts.true(
            env,
            want in error,
            "invalid metadata should mention %r, got %r" % (want, error),
        )
    return unittest.end(env)

metadata_key_validation_test = unittest.make(_metadata_key_validation_test_impl)

def _generated_object_inputs_test_impl(ctx):
    env = unittest.begin(ctx)
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
            indexed,
        ),
    )
    asserts.false(
        env,
        repositories_test_helpers.generated_object_block_has_buildable_inputs(
            incomplete_indexed,
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
        expected = "linux.bzl/compact-v6/llvm-22.1.8/x86/x86",
        tags = ["manual"],
    )
    _compile_environment_abi_failure_test(
        name = name,
        target_under_test = ":" + subject,
    )

def _generated_header_config_index_test_impl(ctx):
    env = unittest.begin(ctx)
    metadata = {
        "generated_header_families": [
            {
                "id": "1111111111111111111111111111111111111111111111111111111111111111",
                "name": "static",
                "labels": [
                    "//:_base_x86_generated_headers",
                    "//:_variant_debug_x86_generated_headers",
                    "//:_variant_lz4_x86_generated_headers",
                ],
            },
            {
                "dependencies": ["1111111111111111111111111111111111111111111111111111111111111111"],
                "id": "2222222222222222222222222222222222222222222222222222222222222222",
                "name": "asm_offsets",
                "labels": [
                    "//:_base_x86_generated_headers",
                    "//:_variant_lz4_x86_generated_headers",
                ],
            },
            {
                "dependencies": ["1111111111111111111111111111111111111111111111111111111111111111"],
                "id": "3333333333333333333333333333333333333333333333333333333333333333",
                "name": "asm_offsets",
                "labels": ["//:_variant_debug_x86_generated_headers"],
            },
            {
                "id": "4444444444444444444444444444444444444444444444444444444444444444",
                "name": "all",
                "labels": [
                    "//:_base_x86_generated_headers",
                    "//:_variant_debug_x86_generated_headers",
                    "//:_variant_lz4_x86_generated_headers",
                ],
            },
        ],
    }
    generated_headers = {
        "x86_64": "//:_base_x86_generated_headers",
        "debug": "//:_variant_debug_x86_generated_headers",
        "lz4": "//:_variant_lz4_x86_generated_headers",
    }
    index = repositories_test_helpers.generated_header_config_index(
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
        index.aliases,
    )
    asserts.equals(
        env,
        {
            "x86_64": {
                "all": "4444444444444444444444444444444444444444444444444444444444444444",
                "asm_offsets": "2222222222222222222222222222222222222222222222222222222222222222",
                "static": "1111111111111111111111111111111111111111111111111111111111111111",
            },
            "debug": {
                "all": "4444444444444444444444444444444444444444444444444444444444444444",
                "asm_offsets": "3333333333333333333333333333333333333333333333333333333333333333",
                "static": "1111111111111111111111111111111111111111111111111111111111111111",
            },
            "lz4": {
                "all": "4444444444444444444444444444444444444444444444444444444444444444",
                "asm_offsets": "2222222222222222222222222222222222222222222222222222222222222222",
                "static": "1111111111111111111111111111111111111111111111111111111111111111",
            },
        },
        index.family_ids,
    )
    asserts.equals(
        env,
        {
            "x86_64": {
                "all": {},
                "asm_offsets": {
                    "static": "1111111111111111111111111111111111111111111111111111111111111111",
                },
                "static": {},
            },
            "debug": {
                "all": {},
                "asm_offsets": {
                    "static": "1111111111111111111111111111111111111111111111111111111111111111",
                },
                "static": {},
            },
            "lz4": {
                "all": {},
                "asm_offsets": {
                    "static": "1111111111111111111111111111111111111111111111111111111111111111",
                },
                "static": {},
            },
        },
        index.family_dependencies,
    )
    return unittest.end(env)

generated_header_config_index_test = unittest.make(_generated_header_config_index_test_impl)

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
        minimum_rustc_version = "1.78.0",
        rust_profile_json = "",
        platform = "@@llvm//platforms:linux_x86_64",
        base_config = "//configs:x86_64",
        base_header_family_dependencies = {
            "asm_offsets": {
                "static": "1111111111111111111111111111111111111111111111111111111111111111",
            },
            "static": {},
        },
        base_header_family_ids = {
            "asm_offsets": "2222222222222222222222222222222222222222222222222222222222222222",
            "static": "1111111111111111111111111111111111111111111111111111111111111111",
        },
        base_rust_enabled = False,
        config_mode = "default",
        graph_image = "//graph:x86_64_image",
        variant_configs = {"lz4": "//configs:lz4"},
        variant_core_configs = {"lz4": "x86_64"},
        variant_graph_images = {"lz4": "//graph:lz4_image"},
        variant_header_family_dependencies = {
            "lz4": {
                "asm_offsets": {
                    "static": "1111111111111111111111111111111111111111111111111111111111111111",
                },
                "static": {},
            },
        },
        variant_header_family_ids = {
            "lz4": {
                "asm_offsets": "2222222222222222222222222222222222222222222222222222222222222222",
                "static": "1111111111111111111111111111111111111111111111111111111111111111",
            },
        },
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
        'base_header_family_dependencies = {\n        "asm_offsets": {\n            "static": "1111111111111111111111111111111111111111111111111111111111111111",\n        },\n        "static": {},\n    },' in generated,
    )
    asserts.true(
        env,
        'base_header_family_ids = {\n        "asm_offsets": "2222222222222222222222222222222222222222222222222222222222222222",\n        "static": "1111111111111111111111111111111111111111111111111111111111111111",\n    },' in generated,
    )
    asserts.true(
        env,
        'variant_header_family_dependencies = {\n        "lz4": {\n            "asm_offsets": {\n                "static": "1111111111111111111111111111111111111111111111111111111111111111",\n            },\n            "static": {},\n        },\n    },' in generated,
    )
    asserts.true(
        env,
        'variant_header_family_ids = {\n        "lz4": {\n            "asm_offsets": "2222222222222222222222222222222222222222222222222222222222222222",\n            "static": "1111111111111111111111111111111111111111111111111111111111111111",\n        },\n    },' in generated,
    )
    return unittest.end(env)

core_config_aliases_test = unittest.make(_core_config_aliases_test_impl)

def _graph_arch_tool_args_test_impl(ctx):
    env = unittest.begin(ctx)
    asserts.equals(
        env,
        [
            "-source_objtool",
            "//:_base_x86_objtool",
        ],
        repositories_test_helpers.graph_arch_tool_args("x86_64"),
    )
    asserts.equals(
        env,
        [
            "-source_relacheck",
            "//:_base_relacheck_tool",
        ],
        repositories_test_helpers.graph_arch_tool_args("aarch64"),
    )
    return unittest.end(env)

graph_arch_tool_args_test = unittest.make(_graph_arch_tool_args_test_impl)

def _without_rust_toolchain_config_test_impl(ctx):
    env = unittest.begin(ctx)
    asserts.equals(
        env,
        {
            "CONFIG_CFI_CLANG": "y",
            "CONFIG_RUST": "y",
        },
        repositories_test_helpers.without_rust_toolchain_config({
            "CONFIG_CFI_CLANG": "y",
            "CONFIG_HAVE_CFI_ICALL_NORMALIZE_INTEGERS_RUSTC": "y",
            "CONFIG_RUST": "y",
            "CONFIG_RUSTC_HAS_COERCE_POINTEE": "y",
            "CONFIG_RUSTC_LLVM_VERSION": "220106",
            "CONFIG_RUSTC_VERSION": "109700",
            "CONFIG_RUSTC_VERSION_TEXT": "rustc 1.97.0-nightly",
            "CONFIG_RUST_IS_AVAILABLE": "y",
        }),
    )
    return unittest.end(env)

without_rust_toolchain_config_test = unittest.make(_without_rust_toolchain_config_test_impl)
