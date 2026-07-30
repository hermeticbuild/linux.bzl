"""Tests for repository-side compact-v7 validation and indexing."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts", "unittest")
load("//internal:compact_v7_repository.bzl", "compact_v7_repository_model")

visibility("private")

_PROFILE = "0000000000000000000000000000000000000000000000000000000000000000"
_PAYLOAD = "1111111111111111111111111111111111111111111111111111111111111111"
_ENVIRONMENT = "2222222222222222222222222222222222222222222222222222222222222222"
_FAMILY = "3333333333333333333333333333333333333333333333333333333333333333"
_SOURCE_SET_LEAF = "4444444444444444444444444444444444444444444444444444444444444444"
_SOURCE_SET_ROOT = "5555555555555555555555555555555555555555555555555555555555555555"
_SOURCE_GROUP = "6666666666666666666666666666666666666666666666666666666666666666"
_FLAGS_TERMINAL = "7777777777777777777777777777777777777777777777777777777777777777"
_REMOVE_TERMINAL = "8888888888888888888888888888888888888888888888888888888888888888"
_FLAGS_PROGRAM = "9999999999999999999999999999999999999999999999999999999999999999"
_REMOVE_PROGRAM = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
_REACHABILITY = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
_RECIPE = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
_RECIPE_GROUP = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
_CONTENT = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
_UNKNOWN = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
_PROBE = "0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f"
_NODE = "8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f"
_DYNAMIC_PROGRAM = "afafafafafafafafafafafafafafafafafafafafafafafafafafafafafafafaf"
_TARGET = "kernel_obj__" + _CONTENT[:24]
_ABI = "linux.bzl/compact-v7/test-abi"

def _valid_metadata():
    return {
        "action_recipe_groups": [{
            "id": _RECIPE_GROUP,
            "objects": [_TARGET],
            "reachability": _REACHABILITY,
            "recipe": _RECIPE,
        }],
        "action_recipes": [{
            "flag_program": _FLAGS_PROGRAM,
            "id": _RECIPE,
            "kind": "compile",
            "language": "c",
            "mode": "y",
            "remove_flag_program": _REMOVE_PROGRAM,
        }],
        "action_source_groups": [{
            "id": _SOURCE_GROUP,
            "primary_source": 3,
            "source_set": _SOURCE_SET_ROOT,
        }],
        "compile_environment_abi": _ABI,
        "compile_environments": [{
            "abi": _ABI,
            "config_payload": _PAYLOAD,
            "generated_header_families": [_FAMILY],
            "id": _ENVIRONMENT,
        }],
        "config_payloads": [{
            "content": "CONFIG_TEST=y\n",
            "id": _PAYLOAD,
        }],
        "configs": [{
            "config_payload": _PAYLOAD,
            "module_object_targets": [],
            "name": "base",
            "object_targets": [_TARGET],
            "support_source_set": _SOURCE_SET_LEAF,
        }],
        "flag_nodes": [],
        "flag_programs": [
            {
                "effects": ["argv"],
                "id": _FLAGS_PROGRAM,
                "root": _FLAGS_TERMINAL,
            },
            {
                "effects": ["argv"],
                "id": _REMOVE_PROGRAM,
                "root": _REMOVE_TERMINAL,
            },
        ],
        "flag_terminals": [
            {
                "argv": ["-O2"],
                "id": _FLAGS_TERMINAL,
            },
            {
                "argv": ["-Werror"],
                "id": _REMOVE_TERMINAL,
            },
        ],
        "generated_header_families": [{
            "config_payload": _PAYLOAD,
            "dependencies": [],
            "id": _FAMILY,
            "labels": ["//:_base_x86_generated_headers"],
            "name": "static",
            "source_set": _SOURCE_SET_LEAF,
            "srcarch": "x86",
        }],
        "kbuild_probes": [],
        "object_variants": [{
            "action_source_group": _SOURCE_GROUP,
            "compile_environment": _ENVIRONMENT,
            "content_id": _CONTENT,
            "deps": [],
            "members": [],
            "object": "kernel/obj.o",
            "reachability": _REACHABILITY,
            "recipe": _RECIPE,
            "recipe_group": _RECIPE_GROUP,
            "target": _TARGET,
        }],
        "protocol": "compact-v7-lazy-action-graph",
        "reachability_signatures": [{
            "configs": ["base"],
            "id": _REACHABILITY,
        }],
        "source_files": [
            {
                "digest": "abababababababababababababababababababababababababababababababab",
                "path": "arch/x86/kernel/vmlinux.lds.S",
            },
            {
                "digest": "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd",
                "path": "include/a.h",
            },
            {
                "digest": "efefefefefefefefefefefefefefefefefefefefefefefefefefefefefefefef",
                "path": "kernel/a.c",
            },
        ],
        "source_sets": [
            {
                "children": [],
                "files": [1, 2],
                "id": _SOURCE_SET_LEAF,
            },
            {
                "children": [_SOURCE_SET_LEAF],
                "files": [3],
                "id": _SOURCE_SET_ROOT,
            },
        ],
        "toolchain_profile": _PROFILE,
    }

def _replace_record(metadata, collection, updates):
    result = dict(metadata)
    records = list(result[collection])
    record = dict(records[0])
    record.update(updates)
    records[0] = record
    result[collection] = records
    return result

def _model_test_impl(ctx):
    env = unittest.begin(ctx)
    model = compact_v7_repository_model(_valid_metadata(), _PROFILE, _ABI)

    source_group = model.action_source_groups[_SOURCE_GROUP]
    asserts.equals(env, [1, 2, 3], source_group.file_indices)
    asserts.equals(env, "kernel/a.c", source_group.primary_source.path)
    asserts.equals(
        env,
        ["arch/x86/kernel/vmlinux.lds.S", "include/a.h", "kernel/a.c"],
        [source.path for source in source_group.source_files],
    )
    asserts.equals(env, 2, model.source_sets[_SOURCE_SET_ROOT].depth)
    asserts.equals(env, ["-O2"], model.flag_programs[_FLAGS_PROGRAM].argv)
    asserts.equals(env, _ENVIRONMENT, model.object_variants[_TARGET].compile_environment_id)
    asserts.false(env, hasattr(model.recipes[_RECIPE], "compile_environment"))
    asserts.equals(env, [_TARGET], model.configs["base"].reachable_object_targets)
    asserts.equals(env, _SOURCE_SET_LEAF, model.configs["base"].support_source_set_id)
    asserts.equals(
        env,
        ["arch/x86/kernel/vmlinux.lds.S", "include/a.h"],
        [source.path for source in model.configs["base"].support_source_set.files],
    )
    asserts.equals(
        env,
        _RECIPE + "__" + _REACHABILITY,
        model.recipe_groups[_RECIPE_GROUP].stable_key,
    )
    asserts.equals(env, 1, model.graph_stats.object_count)
    asserts.equals(env, 3, model.graph_stats.action_source_file_memberships)
    asserts.equals(env, 5, model.graph_stats.source_set_expanded_file_memberships)
    asserts.equals(env, 2, model.graph_stats.max_source_set_depth)

    dynamic_metadata = _valid_metadata()
    dynamic_metadata["kbuild_probes"] = [{
        "candidate_argv": ["-mrecord-mcount"],
        "context_program": _FLAGS_PROGRAM,
        "id": _PROBE,
        "kind": "cc_option",
        "language": "c",
        "srcarch": "x86",
    }]
    dynamic_metadata["flag_nodes"] = [{
        "id": _NODE,
        "kind": "select",
        "probe": _PROBE,
        "when_false": _REMOVE_TERMINAL,
        "when_true": _FLAGS_TERMINAL,
    }]
    dynamic_metadata["flag_programs"] = dynamic_metadata["flag_programs"] + [{
        "effects": ["argv"],
        "id": _DYNAMIC_PROGRAM,
        "root": _NODE,
    }]
    dynamic_recipe = dict(dynamic_metadata["action_recipes"][0])
    dynamic_recipe["flag_program"] = _DYNAMIC_PROGRAM
    dynamic_metadata["action_recipes"] = [dynamic_recipe]
    dynamic_model = compact_v7_repository_model(dynamic_metadata, _PROFILE, _ABI)
    asserts.equals(env, _NODE, dynamic_model.flag_programs[_DYNAMIC_PROGRAM].root)
    asserts.equals(env, _PROBE, dynamic_model.flag_nodes[_NODE].probe)

    arm64_metadata = _valid_metadata()
    arm64_sources = list(arm64_metadata["source_files"])
    arm64_linker_script = dict(arm64_sources[0])
    arm64_linker_script["path"] = "arch/arm64/kernel/vmlinux.lds.S"
    arm64_sources[0] = arm64_linker_script
    arm64_metadata["source_files"] = arm64_sources
    arm64_family = dict(arm64_metadata["generated_header_families"][0])
    arm64_family["srcarch"] = "arm64"
    arm64_metadata["generated_header_families"] = [arm64_family]
    arm64_model = compact_v7_repository_model(arm64_metadata, _PROFILE, _ABI)
    asserts.equals(
        env,
        "arch/arm64/kernel/vmlinux.lds.S",
        arm64_model.configs["base"].support_source_set.files[0].path,
    )
    return unittest.end(env)

_model_test = unittest.make(_model_test_impl)

def _validation_subject_impl(ctx):
    compact_v7_repository_model(
        json.decode(ctx.attr.metadata),
        _PROFILE,
        _ABI,
    )
    return []

_validation_subject = rule(
    implementation = _validation_subject_impl,
    attrs = {
        "metadata": attr.string(mandatory = True),
    },
)

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

def _failure_case(name, metadata, expected_error):
    subject = name + "_subject"
    _validation_subject(
        name = subject,
        metadata = json.encode(metadata),
        tags = ["manual"],
    )
    _failure_test(
        name = name,
        expected_error = expected_error,
        target_under_test = ":" + subject,
    )

def compact_v7_repository_test_suite(name):
    tests = []

    model_test = name + "_model"
    _model_test(name = model_test)
    tests.append(model_test)

    profile = _valid_metadata()
    profile["toolchain_profile"] = _UNKNOWN
    test_name = name + "_profile_mismatch"
    _failure_case(test_name, profile, "does not match expected profile")
    tests.append(test_name)

    abi = _valid_metadata()
    abi["compile_environment_abi"] = "linux.bzl/compact-v7/other-abi"
    test_name = name + "_abi_mismatch"
    _failure_case(test_name, abi, "does not match expected ABI")
    tests.append(test_name)

    cycle = _valid_metadata()
    source_sets = list(cycle["source_sets"])
    leaf = dict(source_sets[0])
    leaf["children"] = [_SOURCE_SET_ROOT]
    source_sets[0] = leaf
    cycle["source_sets"] = source_sets
    test_name = name + "_source_set_cycle"
    _failure_case(test_name, cycle, "source set graph contains a cycle")
    tests.append(test_name)

    probes = _valid_metadata()
    probes["kbuild_probes"] = [{"id": _UNKNOWN}]
    test_name = name + "_dynamic_probe"
    _failure_case(test_name, probes, "missing required field")
    tests.append(test_name)

    environment = _replace_record(
        _valid_metadata(),
        "object_variants",
        {"compile_environment": _UNKNOWN},
    )
    test_name = name + "_object_environment"
    _failure_case(test_name, environment, "references unknown compile environment")
    tests.append(test_name)

    recipe_environment = _replace_record(
        _valid_metadata(),
        "action_recipes",
        {"compile_environment": _ENVIRONMENT},
    )
    test_name = name + "_recipe_environment"
    _failure_case(test_name, recipe_environment, "unknown field \"compile_environment\"")
    tests.append(test_name)

    effects = _valid_metadata()
    programs = list(effects["flag_programs"])
    program = dict(programs[0])
    program["effects"] = ["argv", "input"]
    programs[0] = program
    effects["flag_programs"] = programs
    test_name = name + "_terminal_effects"
    _failure_case(test_name, effects, "do not match root effects")
    tests.append(test_name)

    support_sources = _replace_record(
        _valid_metadata(),
        "configs",
        {"support_source_set": _UNKNOWN},
    )
    test_name = name + "_config_support_sources"
    _failure_case(test_name, support_sources, "references unknown support source set")
    tests.append(test_name)

    missing_support_sources = _valid_metadata()
    missing_support_sources["configs"] = [{
        key: value
        for key, value in missing_support_sources["configs"][0].items()
        if key != "support_source_set"
    }]
    test_name = name + "_config_missing_support_sources"
    _failure_case(test_name, missing_support_sources, "missing required field \"support_source_set\"")
    tests.append(test_name)

    empty_support_sources = _valid_metadata()
    empty_source_sets = list(empty_support_sources["source_sets"])
    empty_source_set = dict(empty_source_sets[0])
    empty_source_set["files"] = []
    empty_source_sets[0] = empty_source_set
    empty_support_sources["source_sets"] = empty_source_sets
    test_name = name + "_config_empty_support_sources"
    _failure_case(test_name, empty_support_sources, "source_sets[0] must not be empty")
    tests.append(test_name)

    wrong_support_sources = _valid_metadata()
    wrong_source_sets = list(wrong_support_sources["source_sets"])
    wrong_leaf = dict(wrong_source_sets[0])
    wrong_leaf["files"] = [2]
    wrong_root = dict(wrong_source_sets[1])
    wrong_root["files"] = [1, 3]
    wrong_source_sets[0] = wrong_leaf
    wrong_source_sets[1] = wrong_root
    wrong_support_sources["source_sets"] = wrong_source_sets
    test_name = name + "_config_wrong_support_sources"
    _failure_case(
        test_name,
        wrong_support_sources,
        "omits expected linker script \"arch/x86/kernel/vmlinux.lds.S\"",
    )
    tests.append(test_name)

    no_srcarch = _valid_metadata()
    no_srcarch["generated_header_families"] = []
    no_srcarch["compile_environments"] = [
        dict(no_srcarch["compile_environments"][0], generated_header_families = []),
    ]
    test_name = name + "_config_support_sources_without_srcarch"
    _failure_case(test_name, no_srcarch, "cannot infer srcarch")
    tests.append(test_name)

    ambiguous_srcarch = _valid_metadata()
    ambiguous_family = dict(ambiguous_srcarch["generated_header_families"][0])
    ambiguous_family["id"] = _UNKNOWN
    ambiguous_family["srcarch"] = "arm64"
    ambiguous_srcarch["generated_header_families"] = [
        ambiguous_srcarch["generated_header_families"][0],
        ambiguous_family,
    ]
    test_name = name + "_config_support_sources_ambiguous_srcarch"
    _failure_case(test_name, ambiguous_srcarch, "cannot infer a unique srcarch")
    tests.append(test_name)

    roots = _valid_metadata()
    roots["configs"] = [dict(roots["configs"][0], object_targets = [_TARGET, _TARGET])]
    test_name = name + "_duplicate_config_root"
    _failure_case(test_name, roots, "repeats %r" % _TARGET)
    tests.append(test_name)

    native.test_suite(
        name = name,
        tests = tests,
    )
