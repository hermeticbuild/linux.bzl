"""Analysis tests for the public Rust-for-Linux module rule."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts", "unittest")
load("//internal:linux_module_actions.bzl", "linux_module_actions")
load("//internal:linux_modules.bzl", "linux_module", "linux_module_sdk")
load("//internal:providers.bzl", "LinuxModuleInfo", "LinuxModuleSdkInfo", "LinuxVmlinuxInfo")

visibility("private")

def _fake_module_kernel_impl(ctx):
    module_symvers = ctx.actions.declare_file(ctx.label.name + ".Module.symvers")
    rustc = ctx.actions.declare_file(ctx.label.name + ".rustc")
    rustc_probe = ctx.actions.declare_file(ctx.label.name + ".rustc_probe.json")
    rustc_cfg = ctx.actions.declare_file(ctx.label.name + ".rust_sdk/include/generated/rustc_cfg")
    objtree_anchor_file = ctx.actions.declare_file(ctx.label.name + ".rust_sdk/.anchor")
    rust_dir_anchor_file = ctx.actions.declare_file(ctx.label.name + ".rust_sdk/rust/.anchor")
    for output in [
        module_symvers,
        objtree_anchor_file,
        rustc,
        rustc_cfg,
        rustc_probe,
        rust_dir_anchor_file,
    ]:
        ctx.actions.write(output, "")

    return [
        DefaultInfo(files = depset([module_symvers])),
        LinuxModuleSdkInfo(
            arch = "aarch64",
            btf_tools = struct(
                btfmutate = None,
                llvm_objcopy = None,
                pahole = None,
                resolve_btfids = None,
            ),
            config = struct(
                config_flags = {
                    "CONFIG_DEBUG_INFO_BTF": "y",
                    "CONFIG_DEBUG_INFO_BTF_MODULES": "y",
                    "CONFIG_MODULES": "y",
                    "CONFIG_RUST": "y",
                },
                rustc_cfg = rustc_cfg,
            ),
            kernel_key = ctx.attr.kernel_key,
            module_symvers = module_symvers,
            rust = struct(
                compile_inputs = depset(),
                enabled = True,
                module_flags = [],
                module_version_predicates = [{
                    "add": ["--cfg", "new_rustc"],
                    "at_least": "1.98.0",
                    "else_add": [],
                    "else_remove": [],
                    "remove": [],
                }],
                objtool = None,
                objtree = objtree_anchor_file.dirname,
                objtree_anchor = struct(file = objtree_anchor_file, parents = 0),
                rust_dir = rust_dir_anchor_file.dirname,
                rust_dir_anchor = struct(file = rust_dir_anchor_file, parents = 0),
                rustc = rustc,
                rustc_env = {},
                rustc_files = depset([rustc]),
                rustc_probe = rustc_probe,
                minimum_rustc_version = "1.78.0",
                target_spec = None,
            ),
            version = "6.18.39",
        ),
    ]

_fake_module_kernel = rule(
    implementation = _fake_module_kernel_impl,
    attrs = {
        "kernel_key": attr.string(mandatory = True),
    },
)

def _fake_vmlinux_impl(ctx):
    return [
        LinuxVmlinuxInfo(
            arch = ctx.attr.arch,
            srcarch = ctx.attr.srcarch,
        ),
    ]

_fake_vmlinux = rule(
    implementation = _fake_vmlinux_impl,
    attrs = {
        "arch": attr.string(mandatory = True),
        "srcarch": attr.string(mandatory = True),
    },
)

def _fake_linux_module_impl(ctx):
    ko = ctx.actions.declare_file(ctx.label.name + ".ko")
    module_symvers = ctx.actions.declare_file(ctx.label.name + ".Module.symvers")
    ctx.actions.write(ko, "")
    ctx.actions.write(module_symvers, "")
    return [
        DefaultInfo(files = depset([ko])),
        LinuxModuleInfo(
            kernel_key = ctx.attr.kernel_key,
            ko = ko,
            module_symvers = module_symvers,
        ),
    ]

_fake_linux_module = rule(
    implementation = _fake_linux_module_impl,
    attrs = {
        "kernel_key": attr.string(mandatory = True),
    },
)

def _module_dependency_failure_test_impl(ctx):
    env = analysistest.begin(ctx)
    asserts.expect_failure(env, "built against a different configured kernel")
    return analysistest.end(env)

_module_dependency_failure_test = analysistest.make(
    _module_dependency_failure_test_impl,
    expect_failure = True,
)

def _module_command_flags_test_impl(ctx):
    env = unittest.begin(ctx)
    asserts.equals(
        env,
        [
            "-config",
            "kernel.config",
            "-objtool",
            "objtool",
            "-in",
            "module.raw.o",
            "-mode",
            "module",
            "-out",
            "module.o",
        ],
        linux_module_actions.objtool_args(
            "kernel.config",
            "objtool",
            "module.raw.o",
            "module.o",
            "module",
        ),
    )
    asserts.equals(
        env,
        [
            "-config",
            "kernel.config",
            "-objtool",
            "objtool",
            "-in",
            "module.raw.o",
            "-mode",
            "module",
            "-out",
            "module.o",
            "-force",
            "-objtool_arg=--custom",
        ],
        linux_module_actions.objtool_args(
            "kernel.config",
            "objtool",
            "module.raw.o",
            "module.o",
            "module",
            force = True,
            extra_args = ["--custom"],
        ),
    )
    asserts.equals(env, "ELFCLASS32", linux_module_actions.kernel_elf_class("armv7"))
    for arch in ["aarch64", "ppc64le", "riscv64", "x86_64"]:
        asserts.equals(env, "ELFCLASS64", linux_module_actions.kernel_elf_class(arch))
    return unittest.end(env)

_module_command_flags_test = unittest.make(_module_command_flags_test_impl)

def _module_root_objtool_policy_test_impl(ctx):
    env = unittest.begin(ctx)
    normal = struct(config_flags = {})
    lto = struct(config_flags = {"CONFIG_LTO_CLANG": "y"})
    ibt = struct(config_flags = {"CONFIG_X86_KERNEL_IBT": "y"})
    single = struct(module_root_kind = "single")
    composite = struct(module_root_kind = "composite")

    asserts.false(env, linux_module_actions.module_root_needs_objtool(normal, single))
    asserts.false(env, linux_module_actions.module_root_needs_objtool(lto, single))
    asserts.false(env, linux_module_actions.module_root_needs_objtool(normal, composite))
    asserts.true(env, linux_module_actions.module_root_needs_objtool(lto, composite))
    asserts.true(env, linux_module_actions.module_root_needs_objtool(ibt, composite))
    return unittest.end(env)

_module_root_objtool_policy_test = unittest.make(_module_root_objtool_policy_test_impl)

def _module_btf_flags_test_impl(ctx):
    env = unittest.begin(ctx)
    config = struct(config_flags = {
        "CONFIG_PAHOLE_HAS_LANG_EXCLUDE": "y",
        "CONFIG_PAHOLE_VERSION": "131",
    })
    internal_flags = linux_module_actions.pahole_flags(
        config,
        "6.18.39",
    )
    external_flags = linux_module_actions.pahole_flags(
        config,
        "6.18.39",
        external_module = True,
    )
    asserts.true(env, "--lang_exclude=rust" in internal_flags)
    asserts.false(env, "--btf_features=distilled_base" in internal_flags)
    asserts.true(env, "--lang_exclude=rust" in external_flags)
    asserts.true(env, "--btf_features=distilled_base" in external_flags)
    for version, pahole_version, want_distilled_base in [
        ("6.12.96", "126", True),
        ("6.12.96", "127", True),
        ("6.18.39", "126", False),
        ("6.18.39", "127", False),
        ("6.18.39", "128", True),
    ]:
        flags = linux_module_actions.pahole_flags(
            struct(config_flags = {
                "CONFIG_PAHOLE_VERSION": pahole_version,
            }),
            version,
            external_module = True,
        )
        asserts.equals(
            env,
            want_distilled_base,
            "--btf_features=distilled_base" in flags,
            "Linux %s with pahole 1.%s" % (version, pahole_version),
        )
    return unittest.end(env)

_module_btf_flags_test = unittest.make(_module_btf_flags_test_impl)

def _module_sanitizer_flags_test_impl(ctx):
    env = unittest.begin(ctx)
    flags = linux_module_actions.module_metadata_sanitizer_flags(
        struct(config_flags = {
            "CONFIG_CC_HAS_KASAN_MEMINTRINSIC_PREFIX": "y",
            "CONFIG_KASAN": "y",
            "CONFIG_KASAN_GENERIC": "y",
            "CONFIG_KASAN_INLINE": "y",
            "CONFIG_KASAN_SHADOW_OFFSET": "0xdffffc0000000000",
            "CONFIG_KASAN_STACK": "y",
            "CONFIG_KCSAN": "y",
            "CONFIG_UBSAN": "y",
            "CONFIG_UBSAN_BOOL": "y",
            "CONFIG_UBSAN_INTEGER_WRAP": "y",
        }),
        "external/linux",
        "6.18.39",
    )
    asserts.equals(
        env,
        [
            "-fsanitize=kernel-address",
            "-mllvm",
            "-asan-mapping-offset=0xdffffc0000000000",
            "-mllvm",
            "-asan-instrumentation-with-call-threshold=10000",
            "-mllvm",
            "-asan-stack=1",
            "-mllvm",
            "-asan-instrument-allocas=1",
            "-mllvm",
            "-asan-globals=1",
            "-mllvm",
            "-asan-kernel-mem-intrinsic-prefix=1",
            "-fsanitize=bool",
            "-DINTEGER_WRAP",
            "-fsanitize-undefined-ignore-overflow-pattern=all",
            "-fsanitize=signed-integer-overflow",
            "-fsanitize=unsigned-integer-overflow",
            "-fsanitize=implicit-signed-integer-truncation",
            "-fsanitize=implicit-unsigned-integer-truncation",
            "-fsanitize-ignorelist=external/linux/scripts/integer-wrap-ignore.scl",
            "-fsanitize=thread",
            "-fno-optimize-sibling-calls",
            "-mllvm",
            "-tsan-instrument-read-before-write=1",
            "-mllvm",
            "-tsan-distinguish-volatile=1",
            "-mllvm",
            "-tsan-instrument-func-entry-exit=0",
        ],
        flags,
    )
    asserts.true(
        env,
        "-fsanitize=thread" in flags,
        "Linux 6.18 module metadata must use KCSAN flags",
    )
    flags_612 = linux_module_actions.module_metadata_sanitizer_flags(
        struct(config_flags = {
            "CONFIG_KCSAN": "y",
            "CONFIG_UBSAN": "y",
            "CONFIG_UBSAN_SIGNED_WRAP": "y",
        }),
        "external/linux",
        "6.12.96",
    )
    asserts.equals(env, ["-fsanitize=signed-integer-overflow"], flags_612)
    return unittest.end(env)

_module_sanitizer_flags_test = unittest.make(_module_sanitizer_flags_test_impl)

def _module_sdk_arch_failure_test_impl(ctx):
    env = analysistest.begin(ctx)
    asserts.expect_failure(env, "declares Linux ARCH arm (profile armv7, SRCARCH arm), but")
    return analysistest.end(env)

_module_sdk_arch_failure_test = analysistest.make(
    _module_sdk_arch_failure_test_impl,
    expect_failure = True,
)

def linux_module_test_suite(name):
    kernel = name + "_kernel"
    dependency = name + "_foreign_dependency"
    module = name + "_module"
    mismatched_vmlinux = name + "_mismatched_vmlinux"
    mismatched_sdk = name + "_mismatched_sdk"

    _fake_module_kernel(
        name = kernel,
        kernel_key = "kernel-a",
        tags = ["manual"],
    )
    _fake_linux_module(
        name = dependency,
        kernel_key = "kernel-b",
        tags = ["manual"],
    )
    _fake_vmlinux(
        name = mismatched_vmlinux,
        arch = "x86_64",
        srcarch = "x86",
        tags = ["manual"],
    )
    linux_module_sdk(
        name = mismatched_sdk,
        arch = "arm",
        version = "6.18.39",
        vmlinux = ":" + mismatched_vmlinux,
        tags = ["manual"],
    )
    linux_module(
        name = module,
        deps = [":" + dependency],
        kernel = ":" + kernel,
        srcs = ["linux_modules_test_fixture.rs"],
        tags = ["manual"],
    )
    _module_dependency_failure_test(
        name = name + "_dependency_mismatch_test",
        target_under_test = ":" + module,
    )
    command_flags_test = name + "_command_flags_test"
    _module_command_flags_test(name = command_flags_test)
    root_objtool_policy_test = name + "_root_objtool_policy_test"
    _module_root_objtool_policy_test(name = root_objtool_policy_test)
    btf_flags_test = name + "_btf_flags_test"
    _module_btf_flags_test(name = btf_flags_test)
    sanitizer_flags_test = name + "_sanitizer_flags_test"
    _module_sanitizer_flags_test(name = sanitizer_flags_test)
    sdk_arch_failure_test = name + "_sdk_arch_failure_test"
    _module_sdk_arch_failure_test(
        name = sdk_arch_failure_test,
        target_under_test = ":" + mismatched_sdk,
    )

    native.test_suite(
        name = name,
        tests = [
            ":" + name + "_dependency_mismatch_test",
            ":" + btf_flags_test,
            ":" + command_flags_test,
            ":" + root_objtool_policy_test,
            ":" + sanitizer_flags_test,
            ":" + sdk_arch_failure_test,
        ],
    )
