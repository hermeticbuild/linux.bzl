"""Tests for deterministic compact-v7 lazy BUILD emission."""

load("@bazel_skylib//lib:unittest.bzl", "asserts", "unittest")
load("//internal:compact_v7_repository.bzl", "compact_v7_repository_build")

visibility("private")

_ABI = "linux.bzl/compact-v7/emitter-test"
_PAYLOAD_ID = "1111111111111111111111111111111111111111111111111111111111111111"
_ENVIRONMENT_ID = "2222222222222222222222222222222222222222222222222222222222222222"
_REACHABILITY_ID = "3333333333333333333333333333333333333333333333333333333333333333"
_DIRECT_RECIPE_ID = "4444444444444444444444444444444444444444444444444444444444444444"
_DIRECT_GROUP_ID = "5555555555555555555555555555555555555555555555555555555555555555"
_COMPOSITE_RECIPE_ID = "6666666666666666666666666666666666666666666666666666666666666666"
_COMPOSITE_GROUP_ID = "7777777777777777777777777777777777777777777777777777777777777777"
_NVHE_RECIPE_ID = "8888888888888888888888888888888888888888888888888888888888888888"
_NVHE_GROUP_ID = "9999999999999999999999999999999999999999999999999999999999999999"
_FAMILY_ID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
_DIRECT_SOURCE_GROUP_ID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
_SPECIAL_SOURCE_GROUP_ID = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
_NVHE_SOURCE_GROUP_ID = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
_DIRECT_CONTENT_ID = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
_SPECIAL_CONTENT_ID = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
_COMPOSITE_CONTENT_ID = "abababababababababababababababababababababababababababababababab"
_NVHE_CONTENT_ID = "acacacacacacacacacacacacacacacacacacacacacacacacacacacacacac"
_MODULE_RECIPE_ID = "adadadadadadadadadadadadadadadadadadadadadadadadadadadadadad"
_MODULE_GROUP_ID = "aeaeaeaeaeaeaeaeaeaeaeaeaeaeaeaeaeaeaeaeaeaeaeaeaeaeaeaeaeae"
_MODULE_SOURCE_GROUP_ID = "afafafafafafafafafafafafafafafafafafafafafafafafafafafafafaf"
_MODULE_CONTENT_ID = "b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0"
_DEBUG_PAYLOAD_ID = "b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1"
_DEBUG_ENVIRONMENT_ID = "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
_DEBUG_REACHABILITY_ID = "b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3"
_DEBUG_GROUP_ID = "b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4"
_DEBUG_SOURCE_GROUP_ID = "b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5"
_DEBUG_CONTENT_ID = "b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6"
_DEBUG_FAMILY_ID = "b7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b7"
_SUPPORT_SOURCE_SET_ID = "b8b8b8b8b8b8b8b8b8b8b8b8b8b8b8b8b8b8b8b8b8b8b8b8b8b8b8b8b8b8b8b8"
_FLAG_TERMINAL_ID = "b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9"
_REMOVE_TERMINAL_ID = "babebabebabebabebabebabebabebabebabebabebabebabebabebabebabebabe"
_FLAG_PROGRAM_ID = "bcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbc"
_REMOVE_PROGRAM_ID = "bdbdbdbdbdbdbdbdbdbdbdbdbdbdbdbdbdbdbdbdbdbdbdbdbdbdbdbdbdbdbdbd"
_DYNAMIC_PROBE_ID = "bebebebebebebebebebebebebebebebebebebebebebebebebebebebebebebebe"
_DYNAMIC_SELECT_ID = "bfbfbfbfbfbfbfbfbfbfbfbfbfbfbfbfbfbfbfbfbfbfbfbfbfbfbfbfbfbfbfbf"

def _source(index, path):
    return struct(index = index, path = path)

def _source_group(group_id, files, primary):
    return struct(
        file_indices = [source.index for source in files],
        id = group_id,
        primary_source = primary,
        primary_source_index = primary.index,
        source_files = files,
    )

def _program(
        argv = ["-O2"],
        program_id = _FLAG_PROGRAM_ID,
        root = _FLAG_TERMINAL_ID):
    return struct(
        argv = argv,
        effects = ["argv"],
        id = program_id,
        root = root,
    )

def _recipe(
        recipe_id,
        kind,
        language = "",
        mode = "y",
        module_root = False,
        objtool_disabled = True,
        flag_argv = ["-O2"],
        flag_program_id = _FLAG_PROGRAM_ID,
        flag_root = _FLAG_TERMINAL_ID,
        remove_argv = []):
    flag_program = _program(
        argv = flag_argv,
        program_id = flag_program_id,
        root = flag_root,
    )
    remove_flag_program = _program(
        argv = remove_argv,
        program_id = _REMOVE_PROGRAM_ID,
        root = _REMOVE_TERMINAL_ID,
    )
    return struct(
        flag_program = flag_program,
        flag_program_id = flag_program.id,
        id = recipe_id,
        kind = kind,
        language = language,
        mode = mode,
        modname = "",
        module_root = module_root,
        objtool_args = [],
        objtool_disabled = objtool_disabled,
        objtool_force = False,
        remove_flag_program = remove_flag_program,
        remove_flag_program_id = remove_flag_program.id,
    )

def _environment(payload, families = [], environment_id = _ENVIRONMENT_ID):
    return struct(
        abi = _ABI,
        config_payload = payload,
        config_payload_id = payload.id,
        generated_header_families = families,
        generated_header_family_ids = [family.id for family in families],
        id = environment_id,
    )

def _object(
        target,
        content_id,
        object_path,
        recipe,
        group_id,
        reachability,
        environment = None,
        source_group = None,
        members = [],
        closure = None):
    if closure == None:
        closure = [target]
    return struct(
        action_source_group = source_group,
        action_source_group_id = source_group.id if source_group != None else "",
        closure = closure,
        compile_environment = environment,
        compile_environment_id = environment.id if environment != None else "",
        content_id = content_id,
        dependency_targets = [],
        member_targets = members,
        object = object_path,
        reachability = reachability,
        reachability_id = reachability.id,
        recipe = recipe,
        recipe_group_id = group_id,
        recipe_id = recipe.id,
        target = target,
    )

def _recipe_group(group_id, recipe, reachability, objects):
    return struct(
        id = group_id,
        object_targets = sorted([obj.target for obj in objects]),
        objects = objects,
        reachability = reachability,
        reachability_id = reachability.id,
        recipe = recipe,
        recipe_id = recipe.id,
    )

def _x86_model(
        include_fallback = True,
        include_module = False,
        fallback_object_path = "lib/oid_registry.o",
        config_names = ["base"],
        dynamic_flags = False,
        flag_argv = ["-O2"],
        probe_candidate_argv = ["-Wunknown-test-option"],
        remove_argv = []):
    source_files = [
        _source(1, "include/linux/compiler-version.h"),
        _source(2, "include/linux/compiler_types.h"),
        _source(3, "include/linux/kconfig.h"),
        _source(4, "kernel/a.c"),
        _source(5, "lib/oid_registry.c"),
        _source(6, "module/only.c"),
        _source(7, "arch/x86/kernel/vmlinux.lds.S"),
    ]
    direct_sources = _source_group(
        _DIRECT_SOURCE_GROUP_ID,
        source_files[:4],
        source_files[3],
    )
    special_sources = _source_group(
        _SPECIAL_SOURCE_GROUP_ID,
        source_files[:3] + [source_files[4]],
        source_files[4],
    )
    payload = struct(
        content = "CONFIG_CC_IS_CLANG=y\nCONFIG_X86_64=y\n",
        id = _PAYLOAD_ID,
    )
    environment = _environment(payload)
    reachability = struct(
        configs = config_names,
        id = _REACHABILITY_ID,
    )
    direct_recipe = _recipe(
        _DIRECT_RECIPE_ID,
        "compile",
        language = "c",
        objtool_disabled = False,
        flag_argv = flag_argv,
        flag_root = _DYNAMIC_SELECT_ID if dynamic_flags else _FLAG_TERMINAL_ID,
        remove_argv = remove_argv,
    )
    direct = _object(
        "direct",
        _DIRECT_CONTENT_ID,
        "kernel/a.o",
        direct_recipe,
        _DIRECT_GROUP_ID,
        reachability,
        environment = environment,
        source_group = direct_sources,
    )
    objects = [direct]
    recipe_groups = {}
    roots = ["direct"]
    module_roots = []
    if include_fallback:
        special = _object(
            "special",
            _SPECIAL_CONTENT_ID,
            fallback_object_path,
            direct_recipe,
            _DIRECT_GROUP_ID,
            reachability,
            environment = environment,
            source_group = special_sources,
        )
        composite_recipe = _recipe(
            _COMPOSITE_RECIPE_ID,
            "composite",
            flag_argv = [],
            flag_program_id = _REMOVE_PROGRAM_ID,
            flag_root = _REMOVE_TERMINAL_ID,
        )
        composite = _object(
            "composite",
            _COMPOSITE_CONTENT_ID,
            "kernel/built-in.o",
            composite_recipe,
            _COMPOSITE_GROUP_ID,
            reachability,
            members = ["direct", "special"],
            closure = ["composite", "direct", "special"],
        )
        objects.extend([special, composite])
        recipe_groups[_COMPOSITE_GROUP_ID] = _recipe_group(
            _COMPOSITE_GROUP_ID,
            composite_recipe,
            reachability,
            [composite],
        )
        roots = ["composite"]
    if include_module:
        module_sources = _source_group(
            _MODULE_SOURCE_GROUP_ID,
            source_files[:3] + [source_files[5]],
            source_files[5],
        )
        module_recipe = _recipe(
            _MODULE_RECIPE_ID,
            "compile",
            language = "c",
            mode = "m",
            module_root = True,
        )
        module = _object(
            "module",
            _MODULE_CONTENT_ID,
            "module/only.o",
            module_recipe,
            _MODULE_GROUP_ID,
            reachability,
            environment = environment,
            source_group = module_sources,
        )
        objects.append(module)
        recipe_groups[_MODULE_GROUP_ID] = _recipe_group(
            _MODULE_GROUP_ID,
            module_recipe,
            reachability,
            [module],
        )
        module_roots = ["module"]
    recipe_groups[_DIRECT_GROUP_ID] = _recipe_group(
        _DIRECT_GROUP_ID,
        direct_recipe,
        reachability,
        [obj for obj in objects if obj.recipe_id == _DIRECT_RECIPE_ID],
    )
    configs = {
        config_name: struct(
            module_object_targets = module_roots,
            name = config_name,
            object_targets = roots,
            object_variants = objects,
            support_source_set = struct(file_indices = [7]),
            support_source_set_id = _SUPPORT_SOURCE_SET_ID,
        )
        for config_name in config_names
    }
    return struct(
        compile_environment_abi = _ABI,
        compile_environments = {_ENVIRONMENT_ID: environment},
        config_payloads = {_PAYLOAD_ID: payload},
        configs = configs,
        flag_nodes = {
            _DYNAMIC_SELECT_ID: struct(
                children = [_FLAG_TERMINAL_ID, _REMOVE_TERMINAL_ID],
                id = _DYNAMIC_SELECT_ID,
                kind = "select",
                probe = _DYNAMIC_PROBE_ID,
                when_false = _REMOVE_TERMINAL_ID,
                when_true = _FLAG_TERMINAL_ID,
            ),
        } if dynamic_flags else {},
        flag_programs = {
            _FLAG_PROGRAM_ID: direct_recipe.flag_program,
            _REMOVE_PROGRAM_ID: direct_recipe.remove_flag_program,
        },
        flag_terminals = {
            _FLAG_TERMINAL_ID: struct(argv = flag_argv),
            _REMOVE_TERMINAL_ID: struct(argv = remove_argv),
        },
        generated_header_families = {},
        kbuild_probes = {
            _DYNAMIC_PROBE_ID: struct(
                candidate_argv = probe_candidate_argv,
                context_program = _REMOVE_PROGRAM_ID,
                id = _DYNAMIC_PROBE_ID,
                kind = "cc_option",
                language = "c",
                srcarch = "x86",
            ),
        } if dynamic_flags else {},
        object_variants = {obj.target: obj for obj in objects},
        recipe_groups = recipe_groups,
        source_files = source_files,
    )

def _arm64_model():
    source_files = [
        _source(1, "arch/arm64/kvm/hyp/nvhe/hyp.lds.S"),
        _source(2, "arch/arm64/kvm/hyp/nvhe/member.c"),
        _source(3, "include/linux/compiler-version.h"),
        _source(4, "include/linux/compiler_types.h"),
        _source(5, "include/linux/kconfig.h"),
        _source(6, "arch/arm64/kernel/vmlinux.lds.S"),
    ]
    member_sources = _source_group(
        _DIRECT_SOURCE_GROUP_ID,
        source_files[1:],
        source_files[1],
    )
    nvhe_sources = _source_group(
        _NVHE_SOURCE_GROUP_ID,
        [source_files[0], source_files[2], source_files[4]],
        source_files[0],
    )
    payload = struct(
        content = "CONFIG_ARM64=y\nCONFIG_CC_IS_CLANG=y\n",
        id = _PAYLOAD_ID,
    )
    family = struct(
        dependencies = [],
        id = _FAMILY_ID,
        labels = ["//:_base_arm64_generated_headers"],
        source_set = None,
    )
    environment = _environment(payload, families = [family])
    reachability = struct(
        configs = ["base"],
        id = _REACHABILITY_ID,
    )
    member_recipe = _recipe(
        _DIRECT_RECIPE_ID,
        "compile",
        language = "c",
    )
    member = _object(
        "member",
        _DIRECT_CONTENT_ID,
        "arch/arm64/kvm/hyp/nvhe/member.o",
        member_recipe,
        _DIRECT_GROUP_ID,
        reachability,
        environment = environment,
        source_group = member_sources,
    )
    nvhe_recipe = _recipe(
        _NVHE_RECIPE_ID,
        "arm64_nvhe",
        flag_argv = [],
        flag_program_id = _REMOVE_PROGRAM_ID,
        flag_root = _REMOVE_TERMINAL_ID,
    )
    nvhe = _object(
        "nvhe",
        _NVHE_CONTENT_ID,
        "arch/arm64/kvm/hyp/nvhe/kvm_nvhe.o",
        nvhe_recipe,
        _NVHE_GROUP_ID,
        reachability,
        environment = environment,
        source_group = nvhe_sources,
        members = ["member"],
        closure = ["member", "nvhe"],
    )
    objects = [member, nvhe]
    config = struct(
        module_object_targets = [],
        name = "base",
        object_targets = ["nvhe"],
        object_variants = objects,
        support_source_set = struct(file_indices = [6]),
        support_source_set_id = _SUPPORT_SOURCE_SET_ID,
    )
    return struct(
        compile_environment_abi = _ABI,
        compile_environments = {_ENVIRONMENT_ID: environment},
        config_payloads = {_PAYLOAD_ID: payload},
        configs = {"base": config},
        flag_nodes = {},
        flag_programs = {
            _FLAG_PROGRAM_ID: member_recipe.flag_program,
            _REMOVE_PROGRAM_ID: member_recipe.remove_flag_program,
        },
        flag_terminals = {
            _FLAG_TERMINAL_ID: struct(argv = ["-O2"]),
            _REMOVE_TERMINAL_ID: struct(argv = []),
        },
        generated_header_families = {_FAMILY_ID: family},
        kbuild_probes = {},
        object_variants = {obj.target: obj for obj in objects},
        recipe_groups = {
            _DIRECT_GROUP_ID: _recipe_group(
                _DIRECT_GROUP_ID,
                member_recipe,
                reachability,
                [member],
            ),
            _NVHE_GROUP_ID: _recipe_group(
                _NVHE_GROUP_ID,
                nvhe_recipe,
                reachability,
                [nvhe],
            ),
        },
        source_files = source_files,
    )

def _partitioned_x86_model():
    source_files = [
        _source(1, "debug/btf.c"),
        _source(2, "include/linux/compiler-version.h"),
        _source(3, "include/linux/compiler_types.h"),
        _source(4, "include/linux/kconfig.h"),
        _source(5, "kernel/base.c"),
        _source(6, "arch/x86/kernel/vmlinux.lds.S"),
    ]
    base_sources = _source_group(
        _SPECIAL_SOURCE_GROUP_ID,
        source_files[1:],
        source_files[4],
    )
    debug_sources = _source_group(
        _DEBUG_SOURCE_GROUP_ID,
        source_files[:4],
        source_files[0],
    )
    base_payload = struct(
        content = "CONFIG_X86_64=y\n",
        id = _PAYLOAD_ID,
    )
    debug_payload = struct(
        content = "CONFIG_DEBUG_INFO_BTF=y\nCONFIG_X86_64=y\n",
        id = _DEBUG_PAYLOAD_ID,
    )
    shared_family = struct(
        dependencies = [],
        id = _FAMILY_ID,
        labels = [
            "//:_base_x86_generated_headers",
            "//:_debug_x86_generated_headers",
        ],
        source_set = None,
    )
    debug_family = struct(
        dependencies = [],
        id = _DEBUG_FAMILY_ID,
        labels = ["//:_debug_x86_generated_headers"],
        source_set = None,
    )
    base_environment = _environment(base_payload, families = [shared_family])
    debug_environment = _environment(
        debug_payload,
        families = [shared_family, debug_family],
        environment_id = _DEBUG_ENVIRONMENT_ID,
    )
    base_reachability = struct(
        configs = ["base", "lz4"],
        id = _REACHABILITY_ID,
    )
    debug_reachability = struct(
        configs = ["debug"],
        id = _DEBUG_REACHABILITY_ID,
    )
    recipe = _recipe(
        _DIRECT_RECIPE_ID,
        "compile",
        language = "c",
    )
    base = _object(
        "base_special",
        _SPECIAL_CONTENT_ID,
        "lib/oid_registry.o",
        recipe,
        _DIRECT_GROUP_ID,
        base_reachability,
        environment = base_environment,
        source_group = base_sources,
    )
    debug = _object(
        "debug_special",
        _DEBUG_CONTENT_ID,
        "lib/crc32.o",
        recipe,
        _DEBUG_GROUP_ID,
        debug_reachability,
        environment = debug_environment,
        source_group = debug_sources,
    )
    configs = {
        "base": struct(
            module_object_targets = [],
            name = "base",
            object_targets = [base.target],
            object_variants = [base],
            support_source_set = struct(file_indices = [6]),
            support_source_set_id = _SUPPORT_SOURCE_SET_ID,
        ),
        "debug": struct(
            module_object_targets = [],
            name = "debug",
            object_targets = [debug.target],
            object_variants = [debug],
            support_source_set = struct(file_indices = [6]),
            support_source_set_id = _SUPPORT_SOURCE_SET_ID,
        ),
        "lz4": struct(
            module_object_targets = [],
            name = "lz4",
            object_targets = [base.target],
            object_variants = [base],
            support_source_set = struct(file_indices = [6]),
            support_source_set_id = _SUPPORT_SOURCE_SET_ID,
        ),
    }
    return struct(
        compile_environment_abi = _ABI,
        compile_environments = {
            _ENVIRONMENT_ID: base_environment,
            _DEBUG_ENVIRONMENT_ID: debug_environment,
        },
        config_payloads = {
            _PAYLOAD_ID: base_payload,
            _DEBUG_PAYLOAD_ID: debug_payload,
        },
        configs = configs,
        flag_nodes = {},
        flag_programs = {
            _FLAG_PROGRAM_ID: recipe.flag_program,
            _REMOVE_PROGRAM_ID: recipe.remove_flag_program,
        },
        flag_terminals = {
            _FLAG_TERMINAL_ID: struct(argv = ["-O2"]),
            _REMOVE_TERMINAL_ID: struct(argv = []),
        },
        generated_header_families = {
            _FAMILY_ID: shared_family,
            _DEBUG_FAMILY_ID: debug_family,
        },
        kbuild_probes = {},
        object_variants = {
            base.target: base,
            debug.target: debug,
        },
        recipe_groups = {
            _DIRECT_GROUP_ID: _recipe_group(
                _DIRECT_GROUP_ID,
                recipe,
                base_reachability,
                [base],
            ),
            _DEBUG_GROUP_ID: _recipe_group(
                _DEBUG_GROUP_ID,
                recipe,
                debug_reachability,
                [debug],
            ),
        },
        source_files = source_files,
    )

def _emit(model, arch, source_objtool = "", source_objcopy = ""):
    return compact_v7_repository_build(
        model,
        arch = arch,
        srcarch = arch,
        rules_repo = "@linux_bzl",
        source_label_package = "@linux//",
        source_root_label = "@linux//:Kconfig",
        graph_profile = "//:_graph_profile",
        version = "6.18.2",
        source_objcopy = source_objcopy,
        source_objtool = source_objtool,
    )

def _rule_block(build_file, name):
    marker = "    name = %r,\n" % name
    start = build_file.find(marker)
    if start < 0:
        fail("emitted BUILD omits rule %r" % name)
    end = build_file.find("\n)\n", start)
    if end < 0:
        fail("emitted BUILD has unterminated rule %r" % name)
    return build_file[start:end]

def _split_fallback_test_impl(ctx):
    env = unittest.begin(ctx)
    result = _emit(
        _x86_model(),
        "x86",
        source_objtool = "//:_base_x86_objtool",
    )
    payload_path = "_config_payloads/" + _PAYLOAD_ID + ".config"

    asserts.equals(env, ["special"], result.fallback_targets)
    asserts.equals(env, [_PAYLOAD_ID], result.analysis_config_payload_ids)
    asserts.equals(
        env,
        "CONFIG_CC_IS_CLANG=y\nCONFIG_X86_64=y\n",
        result.config_payload_files[payload_path],
    )
    asserts.true(env, "linux_object_action_group(" in result.build_file)
    asserts.true(
        env,
        'exports_files(["graph_profile_projection.json", "metadata.json"], visibility = ["//visibility:public"])' in result.build_file,
    )
    asserts.true(env, "linux_object(" in result.build_file)
    asserts.true(env, "linux_object_action_group_import(" in result.build_file)
    asserts.true(env, "linux_composite_object_action_group(" in result.build_file)
    asserts.true(env, "linux_grouped_image(" in result.build_file)
    asserts.true(env, "linux_grouped_modules(" in result.build_file)
    asserts.true(env, "config_payload_files" in result.build_file)
    asserts.true(env, "config_payload_values" in result.build_file)
    asserts.true(env, ":_group_" + _DIRECT_GROUP_ID + "_legacy" in result.build_file)
    asserts.equals(env, "_config_0_image", result.config_targets["base"].image)
    asserts.equals(env, "_config_0_modules", result.config_targets["base"].modules)
    asserts.equals(env, "_config_0_sources", result.config_targets["base"].sources)
    return unittest.end(env)

_split_fallback_test = unittest.make(_split_fallback_test_impl)

def _dynamic_fallback_program_test_impl(ctx):
    env = unittest.begin(ctx)
    result = _emit(
        _x86_model(
            dynamic_flags = True,
            fallback_object_path = "init/version.o",
            flag_argv = ["-include", "$(obj)/utsversion-tmp.h"],
            probe_candidate_argv = [
                "-include",
                "__LINUX_BZL_SRCTREE__/include/linux/compiler-version.h",
            ],
        ),
        "x86",
        source_objtool = "//:_base_x86_objtool",
    )
    fallback = _rule_block(result.build_file, "_legacy_special")
    flag_programs = _rule_block(result.build_file, "_flag_programs")

    asserts.true(env, "flag_program = %r" % _FLAG_PROGRAM_ID in fallback)
    asserts.true(env, 'flag_programs = ":_flag_programs"' in fallback)
    asserts.true(env, "remove_flag_program = %r" % _REMOVE_PROGRAM_ID in fallback)
    asserts.true(env, "needs_object_dir = True" in fallback)
    asserts.true(env, "needs_utsversion_tmp = True" in fallback)
    asserts.false(env, "\n    flags =" in fallback)
    asserts.false(env, "\n    remove_flags =" in fallback)
    asserts.true(env, '"include/linux/compiler-version.h"' in flag_programs)
    asserts.false(env, '"kernel/a.c"' in flag_programs)
    asserts.false(env, '"lib/oid_registry.c"' in flag_programs)
    return unittest.end(env)

_dynamic_fallback_program_test = unittest.make(_dynamic_fallback_program_test_impl)

def _file_only_payload_test_impl(ctx):
    env = unittest.begin(ctx)
    result = _emit(
        _x86_model(
            include_fallback = False,
            include_module = True,
        ),
        "x86",
        source_objtool = "//:_base_x86_objtool",
    )

    asserts.equals(env, [], result.fallback_targets)
    asserts.equals(env, [], result.analysis_config_payload_ids)
    asserts.equals(env, {}, result.fallback_source_index_by_reachability)
    asserts.false(env, "CONFIG_CC_IS_CLANG=y" in result.build_file)
    asserts.false(env, "linux_object_action_group_import(" in result.build_file)
    asserts.equals(
        env,
        0,
        len(result.build_file.split("linux_source_input_index(")) - 1,
    )
    asserts.equals(
        env,
        0,
        len(result.build_file.split("linux_source_tree(")) - 1,
    )
    asserts.true(env, "_config_payloads/" + _PAYLOAD_ID + ".config" in result.build_file)
    asserts.equals(
        env,
        [
            "include/linux/compiler-version.h",
            "include/linux/compiler_types.h",
            "include/linux/kconfig.h",
            "kernel/a.c",
            "arch/x86/kernel/vmlinux.lds.S",
        ],
        result.config_targets["base"].source_paths,
    )
    return unittest.end(env)

_file_only_payload_test = unittest.make(_file_only_payload_test_impl)

def _shared_group_dedup_test_impl(ctx):
    env = unittest.begin(ctx)
    result = _emit(
        _x86_model(
            include_fallback = False,
            config_names = ["base", "lz4"],
            flag_argv = [
                "-DCC=$(CONFIG_CC_IS_CLANG)",
                "-DARCH=${CONFIG_X86_64}",
            ],
            remove_argv = ["-DREMOVE=$(CONFIG_CC_IS_CLANG)"],
        ),
        "x86",
        source_objtool = "//:_base_x86_objtool",
    )
    group_label = ":_group_" + _DIRECT_GROUP_ID + "_actions"
    projection_groups = "groups = [%r]" % group_label

    asserts.equals(env, group_label[1:], result.group_target_by_object["direct"])
    asserts.equals(
        env,
        1,
        len(result.build_file.split("linux_object_action_group(")) - 1,
    )
    asserts.equals(env, [], result.fallback_targets)
    asserts.true(env, "-DCC=$(CONFIG_CC_IS_CLANG)" in result.build_file)
    asserts.true(env, "-DARCH=${CONFIG_X86_64}" in result.build_file)
    asserts.true(env, "-DREMOVE=$(CONFIG_CC_IS_CLANG)" in result.build_file)
    asserts.equals(
        env,
        2,
        len(result.build_file.split(projection_groups)) - 1,
    )
    asserts.equals(
        env,
        {_REACHABILITY_ID: ":_compile_environment_index_" + _REACHABILITY_ID},
        result.compile_environment_index_by_reachability,
    )
    asserts.equals(
        env,
        1,
        len(result.build_file.split("linux_compile_environment_index(")) - 1,
    )
    asserts.equals(env, "_config_0_image", result.config_targets["base"].image)
    asserts.equals(env, "_config_1_image", result.config_targets["lz4"].image)
    return unittest.end(env)

_shared_group_dedup_test = unittest.make(_shared_group_dedup_test_impl)

def _reachability_partition_test_impl(ctx):
    env = unittest.begin(ctx)
    result = _emit(_partitioned_x86_model(), "x86")
    base_environment_label = result.compile_environment_index_by_reachability[_REACHABILITY_ID]
    debug_environment_label = result.compile_environment_index_by_reachability[_DEBUG_REACHABILITY_ID]
    base_source_label = result.fallback_source_index_by_reachability[_REACHABILITY_ID]
    debug_source_label = result.fallback_source_index_by_reachability[_DEBUG_REACHABILITY_ID]
    base_environment = _rule_block(result.build_file, base_environment_label[1:])
    debug_environment = _rule_block(result.build_file, debug_environment_label[1:])
    base_sources = _rule_block(result.build_file, base_source_label[1:])
    debug_sources = _rule_block(result.build_file, debug_source_label[1:])
    base_object = _rule_block(result.build_file, "_legacy_base_special")

    asserts.equals(
        env,
        2,
        len(result.build_file.split("linux_compile_environment_index(")) - 1,
    )
    asserts.equals(
        env,
        2,
        len(result.build_file.split("linux_source_input_index(")) - 1,
    )
    asserts.equals(
        env,
        {
            _REACHABILITY_ID: ":_fallback_source_input_index_" + _REACHABILITY_ID,
            _DEBUG_REACHABILITY_ID: ":_fallback_source_input_index_" + _DEBUG_REACHABILITY_ID,
        },
        result.fallback_source_index_by_reachability,
    )
    asserts.true(env, _ENVIRONMENT_ID in base_environment)
    asserts.true(env, _PAYLOAD_ID in base_environment)
    asserts.true(env, "//:_base_x86_generated_headers" in base_environment)
    asserts.false(env, _DEBUG_ENVIRONMENT_ID in base_environment)
    asserts.false(env, _DEBUG_PAYLOAD_ID in base_environment)
    asserts.false(env, "//:_debug_x86_generated_headers" in base_environment)
    asserts.true(env, _DEBUG_ENVIRONMENT_ID in debug_environment)
    asserts.true(env, _DEBUG_PAYLOAD_ID in debug_environment)
    asserts.true(env, "//:_debug_x86_generated_headers" in debug_environment)
    asserts.false(env, "//:_base_x86_generated_headers" in debug_environment)
    asserts.false(env, _ENVIRONMENT_ID in debug_environment)

    asserts.true(env, "@linux//:kernel/base.c" in base_sources)
    asserts.false(env, "@linux//:debug/btf.c" in base_sources)
    asserts.true(env, "@linux//:debug/btf.c" in debug_sources)
    asserts.false(env, "@linux//:kernel/base.c" in debug_sources)
    asserts.true(env, base_environment_label in base_object)
    asserts.true(env, base_source_label in base_object)
    asserts.false(env, debug_environment_label in base_object)
    asserts.false(env, debug_source_label in base_object)

    base_group = ":_group_" + _DIRECT_GROUP_ID + "_legacy"
    projection_groups = "groups = [%r]" % base_group
    asserts.equals(
        env,
        2,
        len(result.build_file.split(projection_groups)) - 1,
    )
    return unittest.end(env)

_reachability_partition_test = unittest.make(_reachability_partition_test_impl)

def _x86_pi_fallback_test_impl(ctx):
    env = unittest.begin(ctx)
    result = _emit(
        _x86_model(
            fallback_object_path = "arch/x86/boot/startup/gdt_idt.pi.o",
        ),
        "x86",
        source_objtool = "//:_base_x86_objtool",
    )

    asserts.equals(env, ["special"], result.fallback_targets)
    asserts.false(env, "relacheck" in result.build_file)
    return unittest.end(env)

_x86_pi_fallback_test = unittest.make(_x86_pi_fallback_test_impl)

def _x86_inat_fallback_test_impl(ctx):
    env = unittest.begin(ctx)
    result = _emit(
        _x86_model(
            fallback_object_path = "arch/x86/lib/inat.o",
        ),
        "x86",
        source_objtool = "//:_base_x86_objtool",
    )

    asserts.equals(env, ["special"], result.fallback_targets)
    asserts.true(env, '"arch/x86/lib/inat.o"' in result.build_file)
    return unittest.end(env)

_x86_inat_fallback_test = unittest.make(_x86_inat_fallback_test_impl)

def _arm64_nvhe_test_impl(ctx):
    env = unittest.begin(ctx)
    result = _emit(
        _arm64_model(),
        "arm64",
        source_objcopy = "@llvm//tools:llvm-objcopy",
    )

    asserts.equals(env, [], result.fallback_targets)
    asserts.equals(env, [], result.analysis_config_payload_ids)
    asserts.true(env, "linux_arm64_nvhe_object_action_group(" in result.build_file)
    asserts.true(env, "@llvm//tools:llvm-objcopy" in result.build_file)
    asserts.true(env, "//:_base_arm64_generated_headers" in result.build_file)
    asserts.false(env, "linux_arm64_nvhe_object(" in result.build_file)
    return unittest.end(env)

_arm64_nvhe_test = unittest.make(_arm64_nvhe_test_impl)

def compact_v7_repository_build_test_suite(name):
    split = name + "_split_fallback"
    _split_fallback_test(name = split)
    dynamic_fallback = name + "_dynamic_fallback_program"
    _dynamic_fallback_program_test(name = dynamic_fallback)
    file_only = name + "_file_only_payload"
    _file_only_payload_test(name = file_only)
    shared_group_dedup = name + "_shared_group_dedup"
    _shared_group_dedup_test(name = shared_group_dedup)
    reachability_partition = name + "_reachability_partition"
    _reachability_partition_test(name = reachability_partition)
    x86_pi = name + "_x86_pi_fallback"
    _x86_pi_fallback_test(name = x86_pi)
    x86_inat = name + "_x86_inat_fallback"
    _x86_inat_fallback_test(name = x86_inat)
    nvhe = name + "_arm64_nvhe"
    _arm64_nvhe_test(name = nvhe)
    native.test_suite(
        name = name,
        tests = [
            split,
            dynamic_fallback,
            file_only,
            shared_group_dedup,
            reachability_partition,
            x86_pi,
            x86_inat,
            nvhe,
        ],
    )
