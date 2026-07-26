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
  "schema": "linux-rust-profile-v1",
  "architecture": "x86_64",
  "source_layout": "legacy",
  "target": {},
  "common_flags": ["--edition=2021"],
  "target_flags": {
    "always": ["--target={target_spec}", "@{rustc_cfg}"],
    "conditional": [
      {
        "config": "CONFIG_CC_OPTIMIZE_FOR_SIZE",
        "equals": "y",
        "flags": ["-Copt-level=s"],
        "else_flags": ["-Copt-level=2"]
      }
    ]
  },
  "module": {
    "allowed_features": ["arbitrary_self_types"],
    "flags": ["--extern", "kernel", "-L{rust_dir}"]
  },
  "bindgen": {},
  "proc_macros": [],
  "crates": [],
  "exports": {},
  "runtime_objects": []
}"""

_ENABLED_PROFILE_JSON = """{
  "schema": "linux-rust-profile-v1",
  "architecture": "x86_64",
  "source_layout": "legacy",
  "target": {
    "generator_source": "scripts/generate_rust_target.rs",
    "stdin": "config_auto_conf",
    "output": "rust/target.json"
  },
  "common_flags": ["--edition=2021"],
  "target_flags": {
    "always": ["--target={target_spec}", "@{rustc_cfg}"],
    "conditional": []
  },
  "module": {
    "allowed_features": [],
    "flags": []
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
  "crates": [],
  "generated_assembly": [],
  "exports": {
    "crates": [],
    "source": "rust/exports.c"
  },
  "runtime_objects": []
}"""

def _disabled_sdk_test_impl(ctx):
    env = analysistest.begin(ctx)
    sdk = analysistest.target_under_test(env)[LinuxRustSdkInfo]

    asserts.false(env, sdk.enabled)
    asserts.equals(env, [], sdk.compile_inputs.to_list())
    asserts.equals(env, [], sdk.module_flags)
    asserts.equals(env, None, sdk.rustc)
    asserts.equals(env, {}, sdk.rustc_env)
    asserts.equals(env, [], sdk.rustc_files.to_list())
    asserts.equals(env, "", sdk.rustc_version)
    asserts.equals(env, None, sdk.rustc_version_runner)
    asserts.equals(env, None, sdk.objtree_anchor)
    asserts.equals(env, [], sdk.runtime_objects)
    asserts.equals(env, None, sdk.rust_dir_anchor)
    asserts.equals(env, None, sdk.target_spec)
    return analysistest.end(env)

_disabled_sdk_test = analysistest.make(_disabled_sdk_test_impl)

def _rust_config_fixture_impl(ctx):
    config = ctx.actions.declare_file(ctx.label.name + ".config")
    cflags = ctx.actions.declare_file(ctx.label.name + ".cflags")
    auto_conf = ctx.actions.declare_file(ctx.label.name + ".include/config/auto.conf")
    autoconf_h = ctx.actions.declare_file(ctx.label.name + ".include/generated/autoconf.h")
    rustc_cfg = ctx.actions.declare_file(ctx.label.name + ".include/generated/rustc_cfg")
    kernel_release = ctx.actions.declare_file(ctx.label.name + ".include/config/kernel.release")
    files = [
        config,
        cflags,
        auto_conf,
        autoconf_h,
        rustc_cfg,
        kernel_release,
    ]
    for file in files:
        ctx.actions.write(file, "")
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
            config_flags = {"CONFIG_RUST": "y"},
            files = depset(files),
            include_dir = include_dir,
            include_dir_anchor = directory_anchor(autoconf_h, include_dir),
            kernel_release = kernel_release,
            rustc_cfg = rustc_cfg,
        ),
    ]

_rust_config_fixture = rule(implementation = _rust_config_fixture_impl)

def _rust_generated_headers_fixture_impl(_ctx):
    return [
        DefaultInfo(files = depset()),
        LinuxGeneratedHeadersInfo(
            arch = "x86",
            cflags = None,
            files = depset(),
            include_dirs = [],
            include_dir_anchors = {},
            srcarch = "x86",
            vdsomunge = None,
        ),
    ]

_rust_generated_headers_fixture = rule(
    implementation = _rust_generated_headers_fixture_impl,
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

def _enabled_sdk_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    sdk = target[LinuxRustSdkInfo]
    asserts.true(env, sdk.enabled)
    proc_macro_actions = [
        action
        for action in analysistest.target_actions(env)
        if action.mnemonic == "LinuxRustProcMacro"
    ]
    asserts.equals(env, 1, len(proc_macro_actions))
    if proc_macro_actions:
        sysroots = [
            arg
            for arg in proc_macro_actions[0].argv
            if arg.startswith("--sysroot=")
        ]
        asserts.equals(env, 1, len(sysroots))
        if sysroots:
            asserts.true(
                env,
                sysroots[0].startswith("--sysroot=bazel-out/"),
                "expected an execroot-relative Rust sysroot, got %s" % sysroots[0],
            )
    return analysistest.end(env)

_enabled_sdk_test = analysistest.make(_enabled_sdk_test_impl)

def _repository_protocol_test_impl(ctx):
    env = unittest.begin(ctx)
    generated = repositories_test_helpers.kernel_root_build(
        arch = "x86_64",
        version = "6.18.39",
        source_repo = "@@linux_sources",
        rust_profile_json = _LEGACY_PROFILE_JSON,
        platform = "@@llvm//platforms:linux_x86_64",
        base_config = "//configs:x86_64",
        base_rust_enabled = False,
        config_mode = "default",
        graph_image = "//graph/base:x86_64_image",
        variant_configs = {"rust": "//configs:rust"},
        variant_graph_images = {"rust": "//graph/rust:rust_image"},
        variant_rust_enabled = {"rust": True},
        rules_repo = "@@linux_bzl",
    )

    asserts.equals(
        env,
        "compact-v3-rust-profile",
        repositories_test_helpers.generator_protocol,
    )
    asserts.true(env, "rust_profile_json = " in generated)
    asserts.true(env, "base_rust_enabled = False," in generated)
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
    flags = linux_rust_test_helpers.profile_target_flags(
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
        flags,
    )
    return unittest.end(env)

_legacy_profile_flags_test = unittest.make(_legacy_profile_flags_test_impl)

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

    protocol_test = name + "_repository_protocol_test"
    _repository_protocol_test(name = protocol_test)
    dead_code_test = name + "_dead_code_elimination_test"
    _unsupported_dead_code_elimination_test(name = dead_code_test)
    legacy_profile_test = name + "_legacy_profile_flags_test"
    _legacy_profile_flags_test(name = legacy_profile_test)
    extend_flags_test = name + "_extend_kernel_c_flags_test"
    _extend_kernel_c_flags_test(name = extend_flags_test)

    native.test_suite(
        name = name,
        tests = [
            ":" + disabled_test,
            ":" + enabled_test,
            ":" + protocol_test,
            ":" + dead_code_test,
            ":" + extend_flags_test,
            ":" + legacy_profile_test,
        ],
    )
