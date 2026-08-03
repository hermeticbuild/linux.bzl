"""Focused analysis tests for Rust-for-Linux SDK selection."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts", "unittest")
load("//internal:linux_image_repository.bzl", "repositories_test_helpers")
load(
    "//internal:linux_objects.bzl",
    "LinuxConfigInfo",
    "LinuxGeneratedHeadersInfo",
)
load(
    "//internal:linux_rust.bzl",
    "linux_disabled_rust_kernel_sdk",
    "linux_rust_kernel_sdk",
    "linux_rust_test_helpers",
)
load("//internal:path_mapping.bzl", "directory_anchor")
load("//internal:providers.bzl", "LinuxRustSdkInfo")

visibility("private")

_LEGACY_PROFILE_JSON = """{
  "schema": "linux-rust-profile-v2",
  "architecture": "x86_64",
  "source_layout": "legacy",
  "target": {
    "kind": "generated",
    "generator_source": "scripts/generate_rust_target.rs",
    "stdin": "config_auto_conf",
    "output": "rust/target.json"
  },
  "common_flags": {
    "always": ["--edition=2021"],
    "version_predicates": []
  },
  "target_flags": {
    "always": ["--target={target_spec}", "@{rustc_cfg}"],
    "conditional": [
      {
        "config": "CONFIG_CC_OPTIMIZE_FOR_SIZE",
        "equals": "y",
        "flags": ["-Copt-level=s"],
        "else_flags": ["-Copt-level=2"],
        "version_predicates": []
      }
    ],
    "version_predicates": [
      {
        "at_least": "1.98.0",
        "add": ["-Cnext-solver=coherence"],
        "remove": [],
        "else_add": [],
        "else_remove": []
      }
    ]
  },
  "module": {
    "allowed_features": ["arbitrary_self_types"],
    "flags": ["--extern", "kernel", "-L{rust_dir}"],
    "version_predicates": []
  },
  "bindgen": {},
  "proc_macros": [],
  "crates": [],
  "exports": {},
  "runtime_objects": [],
  "unsupported_configs": []
}"""

_ENABLED_PROFILE_JSON = """{
  "schema": "linux-rust-profile-v2",
  "architecture": "x86_64",
  "source_layout": "legacy",
  "target": {
    "kind": "generated",
    "generator_source": "scripts/generate_rust_target.rs",
    "stdin": "config_auto_conf",
    "output": "rust/target.json"
  },
  "common_flags": {
    "always": ["--edition=2021"],
    "version_predicates": [
      {
        "at_least": "1.91.0",
        "add": ["--cfg", "rustc_at_least_1_91"],
        "remove": [],
        "else_add": [],
        "else_remove": []
      }
    ]
  },
  "target_flags": {
    "always": ["--target={target_spec}", "@{rustc_cfg}"],
    "conditional": [
      {
        "config": "CONFIG_DEBUG_INFO",
        "equals": "y",
        "flags": ["-Cdebuginfo=2"],
        "else_flags": [],
        "version_predicates": []
      },
      {
        "config": "CONFIG_DEBUG_INFO_DWARF5",
        "equals": "y",
        "flags": ["-Zdwarf-version=5"],
        "else_flags": [],
        "version_predicates": []
      }
    ],
    "version_predicates": [
      {
        "at_least": "1.98.0",
        "add": ["--cfg", "rustc_at_least_1_98"],
        "remove": [],
        "else_add": [],
        "else_remove": []
      }
    ]
  },
  "module": {
    "allowed_features": [],
    "flags": [],
    "version_predicates": [
      {
        "at_least": "1.99.0",
        "add": ["--cfg", "rustc_at_least_1_99"],
        "remove": [],
        "else_add": [],
        "else_remove": []
      }
    ]
  },
  "bindgen": {
    "parameters": "rust/bindgen_parameters",
    "common_flags": [],
    "bindings_header": "rust/bindings.h",
    "uapi_header": "rust/uapi.h",
    "helpers_source": "rust/helpers.c"
  },
  "proc_macros": [
    {
      "name": "test_macro",
      "source": "rust/test_macro.rs",
      "source_prefixes": [],
      "source_files": [],
      "uses_rustc_cfg": false,
      "flags": []
    }
  ],
  "crates": [
    {
      "name": "core",
      "source": "rustc://library/core/src/lib.rs",
      "source_prefixes": ["rustc://library/core/"],
      "source_files": [],
      "generated_inputs": [],
      "deps": [],
      "externs": [],
      "flags": [],
      "skip_flags": [],
      "objcopy_flags": [],
      "version_predicates": []
    }
  ],
  "generated_assembly": [],
  "exports": {
    "crates": [],
    "source": "rust/exports.c"
  },
  "runtime_objects": [],
  "unsupported_configs": []
}"""

_AARCH64_PROFILE_JSON = _ENABLED_PROFILE_JSON.replace(
    '"architecture": "x86_64"',
    '"architecture": "aarch64"',
).replace(
    """  "target": {
    "kind": "generated",
    "generator_source": "scripts/generate_rust_target.rs",
    "stdin": "config_auto_conf",
    "output": "rust/target.json"
  },""",
    """  "target": {
    "kind": "builtin",
    "builtin_triple": "aarch64-unknown-none"
  },""",
).replace(
    "--target={target_spec}",
    "--target=aarch64-unknown-none",
)

def _disabled_sdk_test_impl(ctx):
    env = analysistest.begin(ctx)
    sdk = analysistest.target_under_test(env)[LinuxRustSdkInfo]

    asserts.false(env, sdk.enabled)
    asserts.equals(env, [], sdk.compile_inputs.to_list())
    asserts.equals(env, [], sdk.module_flags)
    asserts.equals(env, [], sdk.module_version_predicates)
    asserts.equals(env, None, sdk.rustc)
    asserts.equals(env, {}, sdk.rustc_env)
    asserts.equals(env, [], sdk.rustc_files.to_list())
    asserts.equals(env, None, sdk.rustc_probe)
    asserts.equals(env, "", sdk.minimum_rustc_version)
    asserts.equals(env, None, sdk.objtree_anchor)
    asserts.equals(env, [], sdk.runtime_objects)
    asserts.equals(env, None, sdk.rust_dir_anchor)
    asserts.equals(env, None, sdk.target_spec)
    asserts.equals(env, 0, len(analysistest.target_actions(env)))
    return analysistest.end(env)

_disabled_sdk_test = analysistest.make(_disabled_sdk_test_impl)

def _rust_config_fixture_impl(ctx):
    config = ctx.actions.declare_file(ctx.label.name + ".config")
    cflags = ctx.actions.declare_file(ctx.label.name + ".cflags")
    auto_conf = ctx.actions.declare_file(ctx.label.name + ".include/config/auto.conf")
    autoconf_h = ctx.actions.declare_file(ctx.label.name + ".include/generated/autoconf.h")
    rustc_cfg = ctx.actions.declare_file(ctx.label.name + ".include/generated/rustc_cfg")
    kernel_release = ctx.actions.declare_file(ctx.label.name + ".include/config/kernel.release")
    rustc_probe = ctx.actions.declare_file(ctx.label.name + ".rust_toolchain.json")
    files = [
        config,
        cflags,
        auto_conf,
        autoconf_h,
        rustc_cfg,
        kernel_release,
        rustc_probe,
    ]
    for file in files[:-1]:
        ctx.actions.write(file, "")
    ctx.actions.write(
        rustc_probe,
        json.encode({
            "channel": "stable",
            "commit_hash": "0123456789abcdef",
            "commit_date": "2026-03-01",
            "llvm_version": "22.1.6",
            "llvm_version_code": 220106,
            "release": "1.97.0",
            "schema": "linux-rust-toolchain-probe-v2",
            "semver": "1.97.0",
            "version_code": 109700,
            "version_text": "rustc 1.97.0 (012345678 2026-03-01)",
        }),
    )
    include_dir = autoconf_h.dirname.rsplit("/", 1)[0]
    return [
        DefaultInfo(files = depset(files)),
        LinuxConfigInfo(
            aflags = cflags,
            auto_conf = auto_conf,
            auto_conf_cmd = auto_conf,
            autoconf_h = autoconf_h,
            cflags = cflags,
            config = config,
            config_flags = {
                "CONFIG_DEBUG_INFO": "y",
                "CONFIG_DEBUG_INFO_BTF": "y",
                "CONFIG_DEBUG_INFO_BTF_MODULES": "y",
                "CONFIG_DEBUG_INFO_DWARF5": "y",
                "CONFIG_RUST": "y",
            },
            files = depset(files),
            include_dir = include_dir,
            include_dir_anchor = directory_anchor(autoconf_h, include_dir),
            kernel_release = kernel_release,
            kernel_version = "6.18.2",
            rustc_cfg = rustc_cfg,
            rustc_probe = rustc_probe,
        ),
    ]

_rust_config_fixture = rule(implementation = _rust_config_fixture_impl)

def _rust_generated_headers_fixture_impl(ctx):
    return [
        DefaultInfo(files = depset()),
        LinuxGeneratedHeadersInfo(
            arch = ctx.attr.arch,
            cflags = None,
            families = {},
            files = depset(),
            include_dirs = [],
            include_dir_anchors = {},
            srcarch = ctx.attr.srcarch,
            vdsomunge = None,
        ),
    ]

_rust_generated_headers_fixture = rule(
    implementation = _rust_generated_headers_fixture_impl,
    attrs = {
        "arch": attr.string(default = "x86"),
        "srcarch": attr.string(default = "x86"),
    },
)

def _rust_source_fixture_impl(ctx):
    out = ctx.actions.declare_file(ctx.attr.out)
    ctx.actions.write(out, ctx.attr.content)
    return [DefaultInfo(files = depset([out]))]

_rust_source_fixture = rule(
    implementation = _rust_source_fixture_impl,
    attrs = {
        "content": attr.string(),
        "out": attr.string(mandatory = True),
    },
)

def _rust_host_link_outputs_fixture_impl(ctx):
    sdk = ctx.attr.sdk[LinuxRustSdkInfo]
    proc_macro_prefix = (
        "/" +
        ctx.attr.sdk.label.name +
        ".rust_sdk/rust/lib" +
        ctx.attr.proc_macro
    )
    proc_macros = [
        file
        for file in sdk.compile_inputs.to_list()
        if proc_macro_prefix in ("/" + file.short_path.replace("\\", "/"))
    ]
    if len(proc_macros) != 1:
        fail(
            "expected one %s proc macro output, got %s" %
            (ctx.attr.proc_macro, proc_macros),
        )
    outputs = [proc_macros[0]]
    if sdk.target_spec != None:
        outputs.append(sdk.target_spec)
    return [DefaultInfo(files = depset(outputs))]

_rust_host_link_outputs_fixture = rule(
    implementation = _rust_host_link_outputs_fixture_impl,
    attrs = {
        "proc_macro": attr.string(mandatory = True),
        "sdk": attr.label(
            mandatory = True,
            providers = [LinuxRustSdkInfo],
        ),
    },
)

def _path_mapping_key(path):
    """Returns an execroot path identity independent of Bazel's config mapping."""
    parts = path.replace("\\", "/").split("/")
    if (
        len(parts) >= 4 and
        parts[0] == "bazel-out" and
        parts[2] in ("bin", "genfiles")
    ):
        return "/".join([parts[0]] + parts[2:])
    return "/".join(parts)

def _assert_host_rust_runtime_link(env, action):
    unstable_options_indices = [
        index
        for index in range(len(action.argv))
        if action.argv[index] == "-Zunstable-options"
    ]
    link_self_contained_indices = [
        index
        for index in range(len(action.argv))
        if action.argv[index] == "-Clink-self-contained=-linker"
    ]
    asserts.equals(env, 1, len(unstable_options_indices))
    asserts.equals(env, 1, len(link_self_contained_indices))
    if unstable_options_indices and link_self_contained_indices:
        asserts.true(
            env,
            unstable_options_indices[0] < link_self_contained_indices[0],
            "-Zunstable-options must precede the unstable link-self-contained value",
        )

    runtime_prefix = "-Clink-arg="
    runtime_indices = [
        index
        for index in range(len(action.argv))
        if (
            action.argv[index].startswith(runtime_prefix) and
            action.argv[index].endswith(".a")
        )
    ]
    asserts.true(
        env,
        len(runtime_indices) > 0,
        "expected static C++ runtime archives in %s" % action.argv,
    )
    input_paths = {
        _path_mapping_key(file.path): True
        for file in action.inputs.to_list()
    }
    for argument in action.argv:
        if not argument.startswith("-Clink-arg="):
            continue
        path = argument[len("-Clink-arg="):]
        if path.startswith("-L") or path.startswith("-B"):
            path = path[2:]
        if "bazel-out/" in path:
            asserts.true(
                env,
                _path_mapping_key(path) in input_paths,
                "host linker path %s is not an action input" % path,
            )
    for index in runtime_indices:
        path = action.argv[index][len(runtime_prefix):]
        asserts.false(
            env,
            path.startswith("/"),
            "expected execroot-relative runtime path, got %s" % path,
        )
        asserts.true(
            env,
            _path_mapping_key(path) in input_paths,
            "runtime archive %s is not an action input" % path,
        )

    unwind_indices = [
        index
        for index in range(len(action.argv))
        if action.argv[index] == "-Clink-arg=--unwindlib=none"
    ]
    if unwind_indices:
        asserts.true(
            env,
            max(runtime_indices) < unwind_indices[0],
            "runtime archives must precede host default-library flags",
        )

def _action_has_argument_pair(action, flag, value):
    for index in range(len(action.argv) - 1):
        if action.argv[index] == flag and action.argv[index + 1] == value:
            return True
    return False

def _action_has_input_suffix(action, suffix):
    for file in action.inputs.to_list():
        if ("/" + file.short_path.replace("\\", "/")).endswith(suffix):
            return True
    return False

def _action_has_input_basename_containing(action, value):
    for file in action.inputs.to_list():
        if value in file.basename:
            return True
    return False

def _action_has_rust_std_rlib(action):
    for file in action.inputs.to_list():
        if file.basename.startswith("libstd-") and file.basename.endswith(".rlib"):
            return True
    return False

def _action_has_tool_input(action, name):
    for file in action.inputs.to_list():
        if file.basename in (name, name + ".exe"):
            return True
    return False

def _assert_no_unrelated_rust_tools(env, action):
    for name in [
        "cargo",
        "cargo-clippy",
        "clippy-driver",
        "rustdoc",
        "rustfmt",
    ]:
        asserts.false(
            env,
            _action_has_tool_input(action, name),
            "%s should not be an input to %s" % (name, action.mnemonic),
        )

def _assert_no_c_toolchain_inputs(env, action):
    for name in [
        "clang",
        "ld.lld",
        "llvm-ar",
    ]:
        asserts.false(
            env,
            _action_has_tool_input(action, name),
            "%s should not be an input to %s" % (name, action.mnemonic),
        )

def _action_has_argument_containing(action, value):
    for argument in action.argv:
        if value in argument:
            return True
    return False

def _action_has_argument_ending_with(action, suffix):
    for argument in action.argv:
        if argument.replace("\\", "/").endswith(suffix):
            return True
    return False

def _enabled_sdk_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    sdk = target[LinuxRustSdkInfo]
    asserts.true(env, sdk.enabled)
    asserts.equals(env, "1.78.0", sdk.minimum_rustc_version)
    asserts.true(env, sdk.rustc_probe != None)
    asserts.true(env, sdk.target_spec != None)
    asserts.equals(env, "1", sdk.rustc_env.get("RUSTC_BOOTSTRAP"))
    asserts.true(env, sdk.rustc != None)
    rustc_file_paths = {
        file.path: True
        for file in sdk.rustc_files.to_list()
    }
    asserts.true(env, sdk.rustc.path in rustc_file_paths)
    asserts.true(
        env,
        any([
            "rustc_driver" in file.basename
            for file in sdk.rustc_files.to_list()
        ]),
        "external modules require the selected rustc runtime libraries",
    )
    asserts.false(
        env,
        any([
            file.basename.endswith(".rlib")
            for file in sdk.rustc_files.to_list()
        ]),
        "external modules use the kernel SDK crates instead of the toolchain standard library",
    )
    asserts.equals(
        env,
        [],
        sorted([
            file.path
            for file in sdk.compile_inputs.to_list()
            if file.path in rustc_file_paths
        ]),
        "Rust metadata and compiler runtime inputs must be disjoint",
    )
    asserts.true(env, "-Zdwarf-version=5" in sdk.module_flags)
    asserts.true(env, "-Cdebuginfo=2" in sdk.module_flags)
    asserts.equals(
        env,
        ["1.91.0", "1.98.0", "1.99.0"],
        [
            predicate["at_least"]
            for predicate in sdk.module_version_predicates
        ],
    )
    target_generator_actions = [
        action
        for action in analysistest.target_actions(env)
        if action.mnemonic == "LinuxRustTargetGeneratorCompile"
    ]
    asserts.equals(env, 1, len(target_generator_actions))
    if target_generator_actions:
        _assert_host_rust_runtime_link(env, target_generator_actions[0])
        _assert_no_unrelated_rust_tools(env, target_generator_actions[0])
        asserts.true(
            env,
            _action_has_rust_std_rlib(target_generator_actions[0]),
            "target generator requires the execution-platform Rust standard library",
        )
        asserts.true(
            env,
            _action_has_argument_ending_with(target_generator_actions[0], "/rustc"),
            "target generator must invoke the execution-platform rustc",
        )
        asserts.equals(
            env,
            1,
            len([
                arg
                for arg in target_generator_actions[0].argv
                if arg.startswith("--sysroot=")
            ]),
            "target generator must use the resolved execution-platform sysroot",
        )
    core_actions = [
        action
        for action in analysistest.target_actions(env)
        if (
            action.mnemonic == "LinuxRustc" and
            _action_has_argument_pair(action, "--crate-name", "core")
        )
    ]
    asserts.equals(env, 1, len(core_actions))
    if core_actions:
        _assert_no_unrelated_rust_tools(env, core_actions[0])
        _assert_no_c_toolchain_inputs(env, core_actions[0])
        asserts.true(
            env,
            _action_has_input_basename_containing(core_actions[0], "rustc_driver"),
            "kernel crates require the selected rustc runtime libraries",
        )
        asserts.false(
            env,
            _action_has_rust_std_rlib(core_actions[0]),
            "kernel crates use their configured core crate instead of the toolchain standard library",
        )
        asserts.equals(
            env,
            [sdk.rustc],
            [
                file
                for file in core_actions[0].inputs.to_list()
                if file.basename == sdk.rustc.basename
            ],
            "kernel crates must use only the target-platform rustc",
        )
        asserts.true(
            env,
            _action_has_argument_ending_with(core_actions[0], "/" + sdk.rustc.basename),
            "kernel crates must invoke rustc",
        )
        asserts.true(env, "-Zdwarf-version=5" in core_actions[0].argv)
        asserts.true(env, "-Cdebuginfo=2" in core_actions[0].argv)
        asserts.true(
            env,
            _action_has_argument_containing(core_actions[0], '"at_least":"1.91.0"'),
            "core rustc action is missing the common rustc-version predicate",
        )
        asserts.true(
            env,
            _action_has_argument_containing(core_actions[0], '"at_least":"1.98.0"'),
            "core rustc action is missing the target rustc-version predicate",
        )
        asserts.true(
            env,
            _action_has_input_suffix(core_actions[0], ".rust_toolchain.json"),
            "core rustc action is missing the selected rustc probe",
        )
        for suffix in [
            "/library/core/src/lib.rs",
            "/library/portable-simd/crates/core_simd/src/core_simd_docs.md",
            "/library/portable-simd/crates/core_simd/src/mod.rs",
            "/library/stdarch/crates/core_arch/src/mod.rs",
            "/library/stdarch/crates/core_arch/src/x86/mod.rs",
        ]:
            asserts.true(
                env,
                _action_has_input_suffix(core_actions[0], suffix),
                "core rustc action is missing %s" % suffix,
            )
        asserts.false(
            env,
            _action_has_input_suffix(core_actions[0], "/library/std/src/lib.rs"),
            "core rustc action should not depend on the full Rust source archive",
        )
    proc_macro_actions = [
        action
        for action in analysistest.target_actions(env)
        if action.mnemonic == "LinuxRustProcMacro"
    ]
    asserts.equals(env, 1, len(proc_macro_actions))
    if proc_macro_actions:
        _assert_host_rust_runtime_link(env, proc_macro_actions[0])
        _assert_no_unrelated_rust_tools(env, proc_macro_actions[0])
        asserts.true(
            env,
            _action_has_rust_std_rlib(proc_macro_actions[0]),
            "procedural macros require the execution-platform Rust standard library",
        )
        asserts.true(
            env,
            _action_has_argument_ending_with(proc_macro_actions[0], "/rustc"),
            "procedural macros must invoke the execution-platform rustc",
        )
        sysroots = [
            arg
            for arg in proc_macro_actions[0].argv
            if arg.startswith("--sysroot=")
        ]
        asserts.equals(env, 1, len(sysroots))
        if sysroots:
            asserts.true(env, len(sysroots[0]) > len("--sysroot="))
    return analysistest.end(env)

_enabled_sdk_test = analysistest.make(_enabled_sdk_test_impl)

def _builtin_target_sdk_test_impl(ctx):
    env = analysistest.begin(ctx)
    sdk = analysistest.target_under_test(env)[LinuxRustSdkInfo]
    asserts.true(env, sdk.enabled)
    asserts.equals(env, None, sdk.target_spec)
    asserts.true(env, sdk.objtree_anchor != None)
    asserts.true(env, "--target=aarch64-unknown-none" in sdk.module_flags)
    asserts.true(env, "-Zdwarf-version=5" in sdk.module_flags)
    asserts.true(env, "-Cdebuginfo=2" in sdk.module_flags)
    target_generator_actions = [
        action
        for action in analysistest.target_actions(env)
        if action.mnemonic in [
            "LinuxRustTargetGeneratorCompile",
            "LinuxRustTargetGenerate",
        ]
    ]
    asserts.equals(env, 0, len(target_generator_actions))
    return analysistest.end(env)

_builtin_target_sdk_test = analysistest.make(_builtin_target_sdk_test_impl)

def _repository_protocol_test_impl(ctx):
    env = unittest.begin(ctx)
    generated = repositories_test_helpers.kernel_root_build(
        arch = "x86_64",
        version = "6.18.39",
        source_repo = "@@linux_sources",
        minimum_rustc_version = "1.78.0",
        rust_profile_json = _LEGACY_PROFILE_JSON,
        platform = "@@llvm//platforms:linux_x86_64",
        base_config = "//configs:x86_64",
        base_header_family_dependencies = {"all": {}},
        base_header_family_ids = {
            "all": "1111111111111111111111111111111111111111111111111111111111111111",
        },
        base_rust_enabled = False,
        config_mode = "default",
        graph_image = "//graph:x86_64_image",
        module_make_vars = {},
        variant_configs = {"rust": "//configs:rust"},
        variant_core_configs = {"rust": "rust"},
        variant_graph_images = {"rust": "//graph:rust_image"},
        variant_header_family_dependencies = {"rust": {"all": {}}},
        variant_header_family_ids = {
            "rust": {
                "all": "2222222222222222222222222222222222222222222222222222222222222222",
            },
        },
        variant_header_configs = {"rust": "rust"},
        variant_rust_enabled = {"rust": True},
        rules_repo = "@@linux_bzl",
    )

    asserts.equals(
        env,
        "compact-v8-adaptive-content-graph",
        repositories_test_helpers.generator_protocol,
    )
    asserts.true(env, "rust_profile_json = " in generated)
    asserts.true(env, 'minimum_rustc_version = "1.78.0",' in generated)
    asserts.true(env, "base_rust_enabled = False," in generated)
    asserts.true(
        env,
        'base_header_family_dependencies = {\n        "all": {},\n    },' in generated,
    )
    asserts.true(
        env,
        'base_header_family_ids = {\n        "all": "1111111111111111111111111111111111111111111111111111111111111111",\n    },' in generated,
    )
    asserts.true(
        env,
        'variant_header_family_dependencies = {\n        "rust": {\n            "all": {},\n        },\n    },' in generated,
    )
    asserts.true(
        env,
        'variant_header_family_ids = {\n        "rust": {\n            "all": "2222222222222222222222222222222222222222222222222222222222222222",\n        },\n    },' in generated,
    )
    asserts.true(
        env,
        'variant_rust_enabled = {\n        "rust": True,\n    },' in generated,
    )
    return unittest.end(env)

_repository_protocol_test = unittest.make(_repository_protocol_test_impl)

def _legacy_profile_flags_test_impl(ctx):
    env = unittest.begin(ctx)
    profile = linux_rust_test_helpers.decode_profile(
        _LEGACY_PROFILE_JSON,
        "x86",
    )
    config = struct(config_flags = {
        "CONFIG_CC_OPTIMIZE_FOR_SIZE": "n",
    })
    resolved = linux_rust_test_helpers.profile_target_flags(
        profile,
        config,
        {
            "rustc_cfg": "cfg",
            "target_spec": "target.json",
        },
    )
    asserts.equals(
        env,
        [
            "--edition=2021",
            "--target=target.json",
            "@cfg",
            "-Copt-level=2",
        ],
        resolved.flags,
    )
    asserts.equals(env, ["1.98.0"], [
        predicate["at_least"]
        for predicate in resolved.predicates
    ])
    return unittest.end(env)

_legacy_profile_flags_test = unittest.make(_legacy_profile_flags_test_impl)

def _conditional_unless_config_test_impl(ctx):
    env = unittest.begin(ctx)
    profile = linux_rust_test_helpers.decode_profile(
        _LEGACY_PROFILE_JSON.replace(
            '"config": "CONFIG_CC_OPTIMIZE_FOR_SIZE",',
            '"config": "CONFIG_CC_OPTIMIZE_FOR_SIZE",\n        "unless_config": "CONFIG_FORCE_SPEED",',
        ),
        "x86",
    )
    replacements = {
        "rustc_cfg": "cfg",
        "target_spec": "target.json",
    }
    optimized = linux_rust_test_helpers.profile_target_flags(
        profile,
        struct(config_flags = {
            "CONFIG_CC_OPTIMIZE_FOR_SIZE": "y",
        }),
        replacements,
    )
    forced_speed = linux_rust_test_helpers.profile_target_flags(
        profile,
        struct(config_flags = {
            "CONFIG_CC_OPTIMIZE_FOR_SIZE": "y",
            "CONFIG_FORCE_SPEED": "y",
        }),
        replacements,
    )
    asserts.true(env, "-Copt-level=s" in optimized.flags)
    asserts.false(env, "-Copt-level=2" in optimized.flags)
    asserts.false(env, "-Copt-level=s" in forced_speed.flags)
    asserts.true(env, "-Copt-level=2" in forced_speed.flags)
    return unittest.end(env)

_conditional_unless_config_test = unittest.make(_conditional_unless_config_test_impl)

def _rustc_source_prefixes_test_impl(ctx):
    env = unittest.begin(ctx)
    asserts.equals(
        env,
        [
            "rustc://library/core/",
            "rustc://library/portable-simd/crates/core_simd/",
            "rustc://library/stdarch/crates/core_arch/",
        ],
        linux_rust_test_helpers.rustc_source_prefixes([
            "rustc://library/core/",
            "rustc://library/stdarch/crates/core_arch/",
        ]),
    )
    return unittest.end(env)

_rustc_source_prefixes_test = unittest.make(_rustc_source_prefixes_test_impl)

def _extend_kernel_c_flags_test_impl(ctx):
    env = unittest.begin(ctx)
    flags = struct(
        directory_anchors = {"include": "anchor"},
        mapped_files = ["cflags"],
        values = ["-Iinclude"],
    )
    extended = linux_rust_test_helpers.extend_kernel_c_flags(
        flags,
        ["-D__BINDGEN__"],
    )
    asserts.equals(env, ["-Iinclude"], flags.values)
    asserts.equals(env, ["-Iinclude", "-D__BINDGEN__"], extended.values)
    asserts.equals(env, flags.mapped_files, extended.mapped_files)
    asserts.equals(env, flags.directory_anchors, extended.directory_anchors)
    return unittest.end(env)

_extend_kernel_c_flags_test = unittest.make(_extend_kernel_c_flags_test_impl)

def _unsupported_dead_code_elimination_test_impl(ctx):
    env = unittest.begin(ctx)
    asserts.equals(
        env,
        ["CONFIG_LD_DEAD_CODE_DATA_ELIMINATION"],
        linux_rust_test_helpers.unsupported_config_symbols(struct(
            config_flags = {
                "CONFIG_LD_DEAD_CODE_DATA_ELIMINATION": "y",
            },
        )),
    )
    return unittest.end(env)

_unsupported_dead_code_elimination_test = unittest.make(
    _unsupported_dead_code_elimination_test_impl,
)

def _unsupported_rust_hardening_test_impl(ctx):
    env = unittest.begin(ctx)
    asserts.equals(
        env,
        [
            "CONFIG_CFI",
            "CONFIG_SHADOW_CALL_STACK",
            "CONFIG_UBSAN",
        ],
        linux_rust_test_helpers.unsupported_config_symbols(struct(
            config_flags = {
                "CONFIG_CFI": "y",
                "CONFIG_SHADOW_CALL_STACK": "y",
                "CONFIG_UBSAN": "y",
            },
        )),
    )
    return unittest.end(env)

_unsupported_rust_hardening_test = unittest.make(
    _unsupported_rust_hardening_test_impl,
)

def linux_rust_test_suite(name):
    disabled_sdk = name + "_disabled_sdk"
    linux_disabled_rust_kernel_sdk(
        name = disabled_sdk,
        tags = ["manual"],
    )

    disabled_test = disabled_sdk + "_test"
    _disabled_sdk_test(
        name = disabled_test,
        target_under_test = ":" + disabled_sdk,
    )

    source_dir = name + ".source"
    source_files = {}
    for suffix, content in {
        "Kconfig": 'mainmenu "Rust SDK test"\n',
        "rust/bindgen_parameters": "",
        "rust/bindings.h": "",
        "rust/exports.c": "",
        "rust/helpers.c": "",
        "rust/test_macro.rs": "",
        "rust/uapi.h": "",
        "scripts/generate_rust_target.rs": "fn main() {}\n",
    }.items():
        target = name + "_source_" + suffix.replace("/", "_").replace(".", "_")
        _rust_source_fixture(
            name = target,
            content = content,
            out = source_dir + "/" + suffix,
            tags = ["manual"],
        )
        source_files[suffix] = ":" + target

    config = name + "_enabled_config"
    _rust_config_fixture(
        name = config,
        tags = ["manual"],
    )
    generated_headers = name + "_enabled_generated_headers"
    _rust_generated_headers_fixture(
        name = generated_headers,
        tags = ["manual"],
    )
    enabled_sdk = name + "_enabled_sdk"
    linux_rust_kernel_sdk(
        name = enabled_sdk,
        arch = "x86",
        config = ":" + config,
        generated_headers = ":" + generated_headers,
        minimum_rustc_version = "1.78.0",
        profile_json = _ENABLED_PROFILE_JSON,
        source_root = source_files["Kconfig"],
        source_tree = [
            source_files[path]
            for path in sorted(source_files.keys())
        ],
        srcarch = "x86",
        tags = ["manual"],
    )
    enabled_test = enabled_sdk + "_test"
    _enabled_sdk_test(
        name = enabled_test,
        tags = ["manual"],
        target_under_test = ":" + enabled_sdk,
    )
    _rust_host_link_outputs_fixture(
        name = name + "_host_link_outputs",
        proc_macro = "test_macro",
        sdk = ":" + enabled_sdk,
        tags = ["manual"],
    )

    builtin_generated_headers = name + "_builtin_generated_headers"
    _rust_generated_headers_fixture(
        name = builtin_generated_headers,
        arch = "arm64",
        srcarch = "arm64",
        tags = ["manual"],
    )
    builtin_sdk = name + "_builtin_sdk"
    linux_rust_kernel_sdk(
        name = builtin_sdk,
        arch = "arm64",
        config = ":" + config,
        generated_headers = ":" + builtin_generated_headers,
        minimum_rustc_version = "1.78.0",
        profile_json = _AARCH64_PROFILE_JSON,
        source_root = source_files["Kconfig"],
        source_tree = [
            source_files[path]
            for path in sorted(source_files.keys())
        ],
        srcarch = "arm64",
        tags = ["manual"],
    )
    builtin_test = builtin_sdk + "_test"
    _builtin_target_sdk_test(
        name = builtin_test,
        tags = ["manual"],
        target_under_test = ":" + builtin_sdk,
    )

    protocol_test = name + "_repository_protocol_test"
    _repository_protocol_test(name = protocol_test)
    dead_code_test = name + "_dead_code_elimination_test"
    _unsupported_dead_code_elimination_test(name = dead_code_test)
    hardening_test = name + "_unsupported_rust_hardening_test"
    _unsupported_rust_hardening_test(name = hardening_test)
    legacy_profile_test = name + "_legacy_profile_flags_test"
    _legacy_profile_flags_test(name = legacy_profile_test)
    conditional_unless_config_test = name + "_conditional_unless_config_test"
    _conditional_unless_config_test(name = conditional_unless_config_test)
    source_prefixes_test = name + "_rustc_source_prefixes_test"
    _rustc_source_prefixes_test(name = source_prefixes_test)
    extend_flags_test = name + "_extend_kernel_c_flags_test"
    _extend_kernel_c_flags_test(name = extend_flags_test)

    native.test_suite(
        name = name,
        tests = [
            ":" + builtin_test,
            ":" + disabled_test,
            ":" + enabled_test,
            ":" + protocol_test,
            ":" + dead_code_test,
            ":" + conditional_unless_config_test,
            ":" + extend_flags_test,
            ":" + hardening_test,
            ":" + legacy_profile_test,
            ":" + source_prefixes_test,
        ],
    )
