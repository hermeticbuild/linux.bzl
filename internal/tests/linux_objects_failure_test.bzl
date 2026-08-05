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
    "LinuxSourceTreeInfo",
    "linux_arm64_nvhe_object",
    "linux_cache_shape_check",
    "linux_compact_delta_image",
    "linux_compact_image",
    "linux_compile_environment_index",
    "linux_compressed_image",
    "linux_config",
    "linux_generic_generated_headers",
    "linux_object",
    "linux_source_input_index",
    "linux_source_tree",
    "linux_vmlinux",
    "linux_x86_generated_headers",
)
load("//internal:linux_rust.bzl", "linux_disabled_rust_kernel_sdk")

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

def _fake_vmlinux_source_inputs_impl(ctx):
    version = ctx.actions.declare_file(
        ctx.label.name + ".source/init/version-timestamp.c",
    )
    ctx.actions.write(version, "")
    return [DefaultInfo(files = depset([version]))]

_fake_vmlinux_source_inputs = rule(
    implementation = _fake_vmlinux_source_inputs_impl,
)

def _fake_linux_object_impl(ctx):
    out = ctx.actions.declare_file(ctx.label.name + ".o")
    ctx.actions.write(out, "")
    return [
        DefaultInfo(files = depset([out])),
        LinuxObjectInfo(
            content_id = ctx.attr.content_id,
            generated_headers = depset(),
            generated_include_dir_anchors = {},
            generated_include_dirs = [],
            mode = ctx.attr.mode,
            object = ctx.attr.object,
            output = out,
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
            environments = {},
        ),
    ]

_fake_compile_environment_index = rule(
    implementation = _fake_compile_environment_index_impl,
)

def _fake_generated_headers_impl(ctx):
    out = ctx.actions.declare_file(ctx.label.name + ".h")
    ctx.actions.write(out, "")
    cflags = None
    files = [out]
    if ctx.attr.emit_cflags:
        cflags = ctx.actions.declare_file(ctx.label.name + ".cflags.rsp")
        ctx.actions.write(cflags, "-mstack-protector-guard=tls\n")
        files.append(cflags)
    family = struct(
        arch = ctx.attr.arch,
        cflags = cflags,
        content_id = ctx.attr.family_content_id,
        files = depset(files),
        include_dir_anchors = {},
        include_dirs = [],
        name = ctx.attr.family_name,
        srcarch = ctx.attr.arch,
        vdsomunge = None,
    )
    return [
        DefaultInfo(files = depset([out])),
        LinuxGeneratedHeadersInfo(
            arch = ctx.attr.arch,
            cflags = cflags,
            families = {
                ctx.attr.family_name: family,
            },
            files = depset(files),
            include_dir_anchors = {},
            include_dirs = [],
            srcarch = ctx.attr.arch,
            vdsomunge = None,
        ),
    ]

_fake_generated_headers = rule(
    implementation = _fake_generated_headers_impl,
    attrs = {
        "arch": attr.string(default = "x86"),
        "emit_cflags": attr.bool(),
        "family_content_id": attr.string(mandatory = True),
        "family_name": attr.string(default = "all"),
    },
)

def _fake_arm64_generated_headers_impl(ctx):
    out = ctx.actions.declare_file(ctx.label.name + ".h")
    ctx.actions.write(out, "")
    family = struct(
        arch = "arm64",
        cflags = None,
        content_id = ctx.attr.family_content_id,
        files = depset([out]),
        include_dir_anchors = {},
        include_dirs = [],
        name = "all",
        srcarch = "arm64",
        vdsomunge = ctx.executable._vdsomunge,
    )
    return [
        DefaultInfo(files = depset([out])),
        LinuxGeneratedHeadersInfo(
            arch = "arm64",
            cflags = None,
            families = {
                "all": family,
            },
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
        "family_content_id": attr.string(mandatory = True),
        "_vdsomunge": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/runandwrite"),
            executable = True,
        ),
    },
)

def _fake_source_tree_info(root):
    return LinuxSourceTreeInfo(
        root = root,
    )

def _fake_nvhe_source_inputs_impl(ctx):
    root = ctx.actions.declare_file(ctx.label.name + ".source/Kconfig")
    hyp_lds = ctx.actions.declare_file(
        ctx.label.name + ".source/arch/arm64/kvm/hyp/nvhe/hyp.lds.S",
    )
    compiler_version = ctx.actions.declare_file(
        ctx.label.name + ".source/include/linux/compiler-version.h",
    )
    kconfig = ctx.actions.declare_file(
        ctx.label.name + ".source/include/linux/kconfig.h",
    )
    ctx.actions.write(root, "")
    ctx.actions.write(hyp_lds, "")
    ctx.actions.write(compiler_version, "")
    ctx.actions.write(kconfig, "")
    return [
        DefaultInfo(files = depset([hyp_lds, compiler_version, kconfig])),
        _fake_source_tree_info(root),
    ]

_fake_nvhe_source_inputs = rule(implementation = _fake_nvhe_source_inputs_impl)

def _fake_vdso32_source_inputs_impl(ctx):
    root = ctx.actions.declare_file(ctx.label.name + ".source/Kconfig")
    ctx.actions.write(root, "")
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
    return [
        DefaultInfo(files = depset(files)),
        _fake_source_tree_info(root),
    ]

_fake_vdso32_source_inputs = rule(implementation = _fake_vdso32_source_inputs_impl)

def _fake_arm_compressed_source_inputs_impl(ctx):
    files = []
    for path in [
        "arch/arm/boot/compressed/ashldi3.S",
        "arch/arm/boot/compressed/bswapsdi2.S",
        "arch/arm/boot/compressed/decompress.c",
        "arch/arm/boot/compressed/head.S",
        "arch/arm/boot/compressed/lib1funcs.S",
        "arch/arm/boot/compressed/misc.c",
        "arch/arm/boot/compressed/piggy.S",
        "arch/arm/boot/compressed/string.c",
        "arch/arm/boot/compressed/vmlinux.lds.S",
        "include/linux/compiler-version.h",
        "include/linux/compiler_types.h",
        "include/linux/kconfig.h",
    ]:
        out = ctx.actions.declare_file(ctx.label.name + ".source/" + path)
        ctx.actions.write(out, "")
        files.append(out)
    return [DefaultInfo(files = depset(files))]

_fake_arm_compressed_source_inputs = rule(
    implementation = _fake_arm_compressed_source_inputs_impl,
)

def _fake_arm_vdso_source_inputs_impl(ctx):
    files = []
    for path in [
        "arch/arm/vdso/note.c",
        "arch/arm/vdso/vdso.lds.S",
        "arch/arm/vdso/vgettimeofday.c",
        "include/linux/compiler-version.h",
        "include/linux/compiler_types.h",
        "include/linux/kconfig.h",
        "lib/vdso/gettimeofday.c",
    ]:
        out = ctx.actions.declare_file(ctx.label.name + ".source/" + path)
        ctx.actions.write(out, "")
        files.append(out)
    return [DefaultInfo(files = depset(files))]

_fake_arm_vdso_source_inputs = rule(
    implementation = _fake_arm_vdso_source_inputs_impl,
)

def _fake_powerpc_vdso_source_inputs_impl(ctx):
    files = []
    for path in [
        "arch/powerpc/kernel/vdso/cacheflush.S",
        "arch/powerpc/kernel/vdso/datapage.S",
        "arch/powerpc/kernel/vdso/getcpu.S",
        "arch/powerpc/kernel/vdso/getrandom.S",
        "arch/powerpc/kernel/vdso/gettimeofday.S",
        "arch/powerpc/kernel/vdso/note.S",
        "arch/powerpc/kernel/vdso/sigtramp32.S",
        "arch/powerpc/kernel/vdso/sigtramp64.S",
        "arch/powerpc/kernel/vdso/vdso32.lds.S",
        "arch/powerpc/kernel/vdso/vdso64.lds.S",
        "arch/powerpc/kernel/vdso/vgetrandom-chacha.S",
        "arch/powerpc/kernel/vdso/vgetrandom.c",
        "arch/powerpc/kernel/vdso/vgettimeofday.c",
        "arch/powerpc/lib/crtsavres.S",
        "lib/vdso/getrandom.c",
        "lib/vdso/gettimeofday.c",
    ]:
        out = ctx.actions.declare_file(ctx.label.name + ".source/" + path)
        ctx.actions.write(out, "")
        files.append(out)
    return [DefaultInfo(files = depset(files))]

_fake_powerpc_vdso_source_inputs = rule(
    implementation = _fake_powerpc_vdso_source_inputs_impl,
)

def _fake_powerpc_purgatory_source_inputs_impl(ctx):
    root = ctx.actions.declare_file(ctx.label.name + ".source/Kconfig")
    ctx.actions.write(root, "")
    files = []
    for path in [
        "arch/powerpc/purgatory/kexec-purgatory.S",
        "arch/powerpc/purgatory/trampoline_64.S",
        "include/linux/compiler-version.h",
        "include/linux/compiler_types.h",
        "include/linux/kconfig.h",
    ]:
        out = ctx.actions.declare_file(ctx.label.name + ".source/" + path)
        ctx.actions.write(out, "")
        files.append(out)
    return [
        DefaultInfo(files = depset(files)),
        _fake_source_tree_info(root),
    ]

_fake_powerpc_purgatory_source_inputs = rule(
    implementation = _fake_powerpc_purgatory_source_inputs_impl,
)

def _fake_x86_purgatory_source_inputs_impl(ctx):
    root = ctx.actions.declare_file(ctx.label.name + ".source/Kconfig")
    ctx.actions.write(root, "")
    files = []
    for path in [
        "arch/x86/boot/compressed/string.c",
        "arch/x86/purgatory/entry64.S",
        "arch/x86/purgatory/kexec-purgatory.S",
        "arch/x86/purgatory/purgatory.c",
        "arch/x86/purgatory/setup-x86_64.S",
        "arch/x86/purgatory/stack.S",
        "include/linux/compiler-version.h",
        "include/linux/compiler_types.h",
        "include/linux/kconfig.h",
        "lib/crypto/sha256.c",
    ]:
        out = ctx.actions.declare_file(ctx.label.name + ".source/" + path)
        ctx.actions.write(out, "")
        files.append(out)
    return [
        DefaultInfo(files = depset(files)),
        _fake_source_tree_info(root),
    ]

_fake_x86_purgatory_source_inputs = rule(
    implementation = _fake_x86_purgatory_source_inputs_impl,
)

def _fake_riscv_purgatory_source_inputs_impl(ctx):
    root = ctx.actions.declare_file(ctx.label.name + ".source/Kconfig")
    ctx.actions.write(root, "")
    files = []
    for path in [
        "arch/riscv/purgatory/kexec-purgatory.S",
        "arch/riscv/purgatory/purgatory.c",
        "arch/riscv/purgatory/entry.S",
        "lib/crypto/sha256.c",
        "lib/string.c",
        "lib/ctype.c",
        "arch/riscv/lib/memcpy.S",
        "arch/riscv/lib/memset.S",
        "arch/riscv/lib/strcmp.S",
        "arch/riscv/lib/strlen.S",
        "arch/riscv/lib/strncmp.S",
        "include/linux/compiler-version.h",
        "include/linux/compiler_types.h",
        "include/linux/kconfig.h",
    ]:
        out = ctx.actions.declare_file(ctx.label.name + ".source/" + path)
        ctx.actions.write(out, "")
        files.append(out)
    return [
        DefaultInfo(files = depset(files)),
        _fake_source_tree_info(root),
    ]

_fake_riscv_purgatory_source_inputs = rule(
    implementation = _fake_riscv_purgatory_source_inputs_impl,
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

def _image_output_groups_test_impl(ctx):
    env = analysistest.begin(ctx)
    output_groups = analysistest.target_under_test(env)[OutputGroupInfo]
    asserts.true(env, hasattr(output_groups, "image"))
    asserts.false(env, hasattr(output_groups, "modinfo"))
    asserts.false(env, hasattr(output_groups, "modules"))
    return analysistest.end(env)

_image_output_groups_test = analysistest.make(_image_output_groups_test_impl)

def _arm_vmlinux_text_offset_test_impl(ctx):
    env = analysistest.begin(ctx)
    actions = [
        action
        for action in analysistest.target_actions(env)
        if action.mnemonic == "LinuxVmlinuxLinkerScript"
    ]
    asserts.equals(env, 1, len(actions))
    if actions:
        offsets = [
            arg
            for arg in actions[0].argv
            if arg.startswith("-DTEXT_OFFSET=")
        ]
        asserts.equals(env, ["-DTEXT_OFFSET=" + ctx.attr.expected], offsets)
    return analysistest.end(env)

_arm_vmlinux_text_offset_test = analysistest.make(
    _arm_vmlinux_text_offset_test_impl,
    attrs = {
        "expected": attr.string(mandatory = True),
    },
)

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
        for basename in ctx.attr.expected_generated_headers:
            generated_header_inputs = [
                file
                for file in compile_actions[0].inputs.to_list()
                if file.basename == basename
            ]
            asserts.equals(env, 1, len(generated_header_inputs))
        for basename in ctx.attr.unexpected_generated_headers:
            duplicate_header_inputs = [
                file
                for file in compile_actions[0].inputs.to_list()
                if file.basename == basename
            ]
            asserts.equals(env, 0, len(duplicate_header_inputs))
        if ctx.attr.expected_generated_cflags:
            generated_cflags = [
                arg
                for arg in compile_actions[0].argv
                if arg.endswith("/" + ctx.attr.expected_generated_cflags)
            ]
            asserts.equals(env, 1, len(generated_cflags))
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
        "expected_generated_cflags": attr.string(),
        "expected_generated_headers": attr.string_list(),
        "expected_payload_id": attr.string(mandatory = True),
        "unexpected_generated_headers": attr.string_list(),
        "unexpected_input": attr.string(mandatory = True),
    },
)

def _indexed_assembly_source_test_impl(ctx):
    env = analysistest.begin(ctx)
    actions = [
        action
        for action in analysistest.target_actions(env)
        if action.mnemonic == "LinuxObjectCompile"
    ]
    asserts.equals(env, 1, len(actions))
    if actions:
        action = actions[0]
        argv = action.argv
        asserts.true(env, "-D__ASSEMBLY__" in argv)
        compile_index = argv.index("-c")
        asserts.true(env, argv[compile_index + 1].endswith("/lib/crypto/x86/blake2s-core.S"))
        input_basenames = [file.basename for file in action.inputs.to_list()]
        asserts.true(env, "blake2s-core.S" in input_basenames)
        asserts.false(env, "blake2s.h" in input_basenames)
        generated_cflags = [
            arg
            for arg in argv
            if arg.endswith("/" + ctx.attr.unexpected_generated_cflags)
        ]
        asserts.equals(env, 0, len(generated_cflags))
    return analysistest.end(env)

_indexed_assembly_source_test = analysistest.make(
    _indexed_assembly_source_test_impl,
    attrs = {
        "unexpected_generated_cflags": attr.string(mandatory = True),
    },
)

def _object_remove_flags_test_impl(ctx):
    env = analysistest.begin(ctx)
    actions = analysistest.target_actions(env)
    compile_actions = [action for action in actions if action.mnemonic == "LinuxObjectCompile"]
    symversion_actions = [action for action in actions if action.mnemonic == "LinuxGenksyms"]
    filter_actions = [action for action in actions if action.mnemonic == "LinuxFlagFilter"]
    asserts.equals(env, 1, len(compile_actions))
    asserts.equals(env, 1, len(symversion_actions))
    asserts.equals(env, 2, len(filter_actions))
    if compile_actions:
        argv = compile_actions[0].argv
        asserts.false(env, "-mgeneral-regs-only" in argv)
        asserts.false(env, "-DREMOVE" in argv)
        asserts.true(env, "-DKEEP_COMPILE" in argv)
        asserts.true(env, "-DREMOVE_SUFFIX" in argv)
    if symversion_actions:
        argv = symversion_actions[0].argv
        asserts.false(env, "-mgeneral-regs-only" in argv)
        asserts.false(env, "-DREMOVE" in argv)
        asserts.true(env, "-DKEEP_SYMVERSION" in argv)
        asserts.true(env, "-DREMOVE_SUFFIX" in argv)
    for action in filter_actions:
        asserts.true(env, "-remove" in action.argv)
        asserts.true(env, "-mgeneral-regs-only" in action.argv)
        asserts.true(env, "-DREMOVE" in action.argv)
    return analysistest.end(env)

_object_remove_flags_test = analysistest.make(_object_remove_flags_test_impl)

def _empty_system_certificates_test_impl(ctx):
    env = analysistest.begin(ctx)
    actions = [
        action
        for action in analysistest.target_actions(env)
        if action.mnemonic == "LinuxObjectCompile"
    ]
    asserts.equals(env, 1, len(actions))
    if actions:
        action = actions[0]
        input_paths = [file.short_path for file in action.inputs.to_list()]
        for suffix in [
            "/certs/signing_key.x509",
            "/certs/x509_certificate_list",
        ]:
            asserts.true(
                env,
                any([path.endswith(suffix) for path in input_paths]),
                "system certificates compile action is missing %s" % suffix,
            )
        asserts.true(
            env,
            any([arg.startswith("-Wa,-I,") for arg in action.argv]),
            "system certificates compile action is missing its generated assembler include root",
        )
    return analysistest.end(env)

_empty_system_certificates_test = analysistest.make(_empty_system_certificates_test_impl)

_X86_PRECISE_HEADER_FAMILIES = [
    "static",
    "timeconst",
    "compile",
    "version",
    "utsrelease",
    "utsversion",
    "cpufeatures",
    "bounds",
    "asm_offsets",
    "rq_offsets",
    "kvm_offsets",
]

def _family_paths(info):
    return sorted([file.path for file in info.files.to_list()])

def _generated_header_family_layout_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    info = target[LinuxGeneratedHeadersInfo]
    precise_paths = []
    precise_include_dirs = []
    include_dir_owners = {}
    for name in _X86_PRECISE_HEADER_FAMILIES:
        paths = _family_paths(info.families[name])
        asserts.true(env, len(paths) > 0, "%s family must publish files" % name)
        for path in paths:
            asserts.true(
                env,
                (".headers/%s/" % name) in path,
                "%s family file is not isolated under its own root: %s" % (name, path),
            )
        precise_paths.extend(paths)
        for include_dir in info.families[name].include_dirs:
            asserts.true(
                env,
                (".headers/%s/" % name) in include_dir,
                "%s family include directory is not isolated: %s" % (name, include_dir),
            )
            asserts.false(
                env,
                include_dir in include_dir_owners,
                "%s and %s share include directory %s" %
                (include_dir_owners.get(include_dir), name, include_dir),
            )
            include_dir_owners[include_dir] = name
            precise_include_dirs.append(include_dir)
    asserts.equals(env, sorted(precise_paths), _family_paths(info.families["all"]))
    asserts.equals(env, _family_paths(info.families["all"]), _family_paths(info))
    asserts.equals(env, sorted(precise_include_dirs), sorted(info.families["all"].include_dirs))
    asserts.equals(env, sorted(info.families["all"].include_dirs), sorted(info.include_dirs))
    asserts.equals(env, 52, len(info.families["static"].files.to_list()))
    version_include_dirs = info.families["version"].include_dirs
    asserts.equals(env, 2, len(version_include_dirs))
    asserts.true(env, any([
        include_dir.endswith(".headers/version/include")
        for include_dir in version_include_dirs
    ]))
    asserts.true(env, any([
        include_dir.endswith(".headers/version/include/generated/uapi")
        for include_dir in version_include_dirs
    ]))

    actions = analysistest.target_actions(env)
    version_mnemonics = {
        "LinuxCompileHeader": "compile",
        "LinuxUTSReleaseHeader": "utsrelease",
        "LinuxUTSVersionHeader": "utsversion",
        "LinuxVersionHeader": "version",
    }
    for mnemonic, family_name in version_mnemonics.items():
        matching = [action for action in actions if action.mnemonic == mnemonic]
        asserts.equals(env, 1, len(matching))
        if matching:
            action = matching[0]
            outputs = action.outputs.to_list()
            asserts.equals(env, 1, len(outputs))
            if outputs:
                asserts.true(env, (".headers/%s/" % family_name) in outputs[0].path)
            input_basenames = [file.basename for file in action.inputs.to_list()]
            asserts.equals(env, family_name == "utsversion", ".config" in input_basenames)
            asserts.equals(env, family_name == "utsrelease", "kernel.release" in input_basenames)
            asserts.equals(env, family_name == "utsversion", "-config" in action.argv)
            asserts.equals(env, family_name == "utsrelease", "-kernel_release" in action.argv)
            asserts.equals(env, family_name == "version", "-kernel_version" in action.argv)
            if family_name == "version":
                version_flag_index = action.argv.index("-kernel_version")
                asserts.equals(env, "6.18.2", action.argv[version_flag_index + 1])
    return analysistest.end(env)

_generated_header_family_layout_test = analysistest.make(
    _generated_header_family_layout_test_impl,
)

def _generated_header_family_reuse_test_impl(ctx):
    env = analysistest.begin(ctx)
    variant = analysistest.target_under_test(env)[LinuxGeneratedHeadersInfo]
    base = ctx.attr.base_generated_headers[LinuxGeneratedHeadersInfo]
    for name in _X86_PRECISE_HEADER_FAMILIES[:-1]:
        asserts.equals(
            env,
            _family_paths(base.families[name]),
            _family_paths(variant.families[name]),
            "%s family must reuse the canonical producer" % name,
        )
        asserts.equals(env, base.families[name].include_dirs, variant.families[name].include_dirs)
    asserts.true(
        env,
        _family_paths(base.families["kvm_offsets"]) != _family_paths(variant.families["kvm_offsets"]),
        "changed kvm_offsets family must use a local producer",
    )
    for path in _family_paths(variant.families["kvm_offsets"]):
        asserts.true(env, ".headers/kvm_offsets/" in path)

    precise_paths = []
    for name in _X86_PRECISE_HEADER_FAMILIES:
        precise_paths.extend(_family_paths(variant.families[name]))
    asserts.equals(env, sorted(precise_paths), _family_paths(variant.families["all"]))
    asserts.equals(env, _family_paths(variant.families["all"]), _family_paths(variant))

    actions = analysistest.target_actions(env)
    offsets_asm = [action for action in actions if action.mnemonic == "LinuxOffsetsAsm"]
    offsets_header = [action for action in actions if action.mnemonic == "LinuxOffsetsHeader"]
    asserts.equals(env, 1, len(offsets_asm))
    asserts.equals(env, 1, len(offsets_header))
    asserts.equals(env, 2, len(actions))
    if offsets_asm:
        input_paths = {
            file.path: True
            for file in offsets_asm[0].inputs.to_list()
        }
        dependency_names = [
            "asm_offsets",
            "utsrelease",
        ]
        for name in _X86_PRECISE_HEADER_FAMILIES[:-1]:
            for path in _family_paths(variant.families[name]):
                asserts.equals(
                    env,
                    name in dependency_names,
                    path in input_paths,
                    "local kvm_offsets action dependency mismatch for %s input %s" % (name, path),
                )
    return analysistest.end(env)

_generated_header_family_reuse_test = analysistest.make(
    _generated_header_family_reuse_test_impl,
    attrs = {
        "base_generated_headers": attr.label(providers = [LinuxGeneratedHeadersInfo]),
    },
)

def _generated_header_earliest_reuse_test_impl(ctx):
    env = analysistest.begin(ctx)
    selected = analysistest.target_under_test(env)[LinuxGeneratedHeadersInfo]
    earliest = ctx.attr.earliest_generated_headers[LinuxGeneratedHeadersInfo]
    later = ctx.attr.later_generated_headers[LinuxGeneratedHeadersInfo]
    for name in ["all"] + _X86_PRECISE_HEADER_FAMILIES:
        asserts.equals(
            env,
            _family_paths(earliest.families[name]),
            _family_paths(selected.families[name]),
            "%s family must select the earliest reusable provider" % name,
        )
        asserts.true(
            env,
            _family_paths(later.families[name]) != _family_paths(selected.families[name]),
            "%s family unexpectedly selected the later reusable provider" % name,
        )
    asserts.equals(env, 0, len(analysistest.target_actions(env)))
    return analysistest.end(env)

_generated_header_earliest_reuse_test = analysistest.make(
    _generated_header_earliest_reuse_test_impl,
    attrs = {
        "earliest_generated_headers": attr.label(providers = [LinuxGeneratedHeadersInfo]),
        "later_generated_headers": attr.label(providers = [LinuxGeneratedHeadersInfo]),
    },
)

def _generic_generated_header_anchors_test_impl(ctx):
    env = analysistest.begin(ctx)
    info = analysistest.target_under_test(env)[LinuxGeneratedHeadersInfo]
    arch_uapi_suffix = "/arch/%s/include/generated/uapi" % ctx.attr.arch
    arch_uapi_dirs = [
        include_dir
        for include_dir in info.include_dirs
        if include_dir.endswith(arch_uapi_suffix)
    ]
    asserts.equals(env, 1, len(arch_uapi_dirs))
    for include_dir in info.include_dirs:
        asserts.true(
            env,
            include_dir in info.include_dir_anchors,
            "generated include directory lacks a file-backed anchor: %s" % include_dir,
        )
    return analysistest.end(env)

_generic_generated_header_anchors_test = analysistest.make(
    _generic_generated_header_anchors_test_impl,
    attrs = {
        "arch": attr.string(mandatory = True),
    },
)

def _arm_compressed_flagfilter_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    actions = analysistest.target_actions(env)
    filter_actions = [
        action
        for action in actions
        if action.mnemonic == "LinuxFlagFilter"
    ]
    asserts.true(env, len(filter_actions) > 0, "ARM compressed image did not filter Kbuild flags")
    zstd_actions = [action for action in actions if action.mnemonic == "LinuxARMZSTD"]
    decompressor_actions = [action for action in actions if action.mnemonic == "LinuxARMDecompressor"]
    append_actions = [
        action
        for action in actions
        if action.mnemonic == "LinuxX86BootTool" and "append-size" in action.argv
    ]
    piggy_actions = [
        action
        for action in actions
        if action.mnemonic == "LinuxARMCompressedCompile" and any([
            output.path.endswith("/arch/arm/boot/compressed/piggy.o")
            for output in action.outputs.to_list()
        ])
    ]
    asserts.equals(env, 1, len(zstd_actions))
    asserts.equals(env, 1, len(decompressor_actions))
    asserts.equals(env, 1, len(append_actions))
    asserts.equals(env, 1, len(piggy_actions))
    if zstd_actions:
        asserts.true(env, "-22" in zstd_actions[0].argv)
        asserts.true(env, "--ultra" in zstd_actions[0].argv)
        asserts.true(env, "-stdin" in zstd_actions[0].argv)
        asserts.true(env, any([
            output.path.endswith("/arch/arm/boot/compressed/piggy_data.zst")
            for output in zstd_actions[0].outputs.to_list()
        ]))
    if append_actions:
        asserts.true(env, any([
            output.path.endswith("/arch/arm/boot/compressed/piggy_data")
            for output in append_actions[0].outputs.to_list()
        ]))
    if decompressor_actions:
        asserts.true(env, any([
            input.path.endswith("/arch/arm/boot/compressed/decompress.c")
            for input in decompressor_actions[0].inputs.to_list()
        ]))
        asserts.true(env, any([
            output.path.endswith("/arch/arm/boot/compressed/decompress.adaptive.c")
            for output in decompressor_actions[0].outputs.to_list()
        ]))
    if piggy_actions:
        asserts.true(env, any([
            input.path.endswith("/arch/arm/boot/compressed/piggy_data")
            for input in piggy_actions[0].inputs.to_list()
        ]))
    decompressor_compiles = [
        action
        for action in actions
        if action.mnemonic == "LinuxARMCompressedCompile" and any([
            input.path.endswith("/arch/arm/boot/compressed/decompress.adaptive.c")
            for input in action.inputs.to_list()
        ])
    ]
    asserts.equals(env, 1, len(decompressor_compiles))
    asserts.true(env, any([
        output.path.endswith(".zImage")
        for output in target[DefaultInfo].files.to_list()
    ]))
    return analysistest.end(env)

_arm_compressed_flagfilter_test = analysistest.make(
    _arm_compressed_flagfilter_test_impl,
)

def _arm_vdso_actions_test_impl(ctx):
    env = analysistest.begin(ctx)
    actions = analysistest.target_actions(env)
    compile_actions = [
        action
        for action in actions
        if action.mnemonic == "LinuxARMVDSOCompile" and any([
            output.path.endswith("/arch/arm/vdso/vgettimeofday.o")
            for output in action.outputs.to_list()
        ])
    ]
    asserts.equals(env, 1, len(compile_actions))
    if compile_actions:
        action = compile_actions[0]
        generic_source = "/lib/vdso/gettimeofday.c"
        force_include_indices = [
            index
            for index in range(len(action.argv) - 1)
            if action.argv[index] == "-include" and action.argv[index + 1].endswith(generic_source)
        ]
        asserts.equals(env, 1, len(force_include_indices))
        asserts.true(env, any([
            input.path.endswith(generic_source)
            for input in action.inputs.to_list()
        ]))
    return analysistest.end(env)

_arm_vdso_actions_test = analysistest.make(
    _arm_vdso_actions_test_impl,
)

def _powerpc_vdso_actions_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    output_paths = [file.path for file in target[DefaultInfo].files.to_list()]
    for suffix in [
        "/arch/powerpc/kernel/vdso/vdso32.so.dbg",
        "/arch/powerpc/kernel/vdso/vdso64.so.dbg",
        "/include/generated/vdso32-offsets.h",
        "/include/generated/vdso64-offsets.h",
    ]:
        asserts.true(
            env,
            any([path.endswith(suffix) for path in output_paths]),
            "PowerPC generated headers omit %s" % suffix,
        )
    actions = analysistest.target_actions(env)
    compile32 = [action for action in actions if action.mnemonic == "LinuxPowerPCVDSO32Compile"]
    compile64 = [action for action in actions if action.mnemonic == "LinuxPowerPCVDSO64Compile"]
    link32 = [action for action in actions if action.mnemonic == "LinuxPowerPCVDSO32Link"]
    link64 = [action for action in actions if action.mnemonic == "LinuxPowerPCVDSO64Link"]
    asserts.equals(env, 11, len(compile32))
    asserts.equals(env, 10, len(compile64))
    asserts.equals(env, 1, len(link32))
    asserts.equals(env, 1, len(link64))
    for action in compile64:
        asserts.false(env, "-ffixed-r30" in action.argv)
    if link32:
        asserts.true(env, "elf32lppc" in link32[0].argv)
        asserts.true(env, "--eh-frame-hdr" in link32[0].argv)
    if link64:
        asserts.true(env, "elf64lppc" in link64[0].argv)
        asserts.true(env, "--eh-frame-hdr" in link64[0].argv)
    return analysistest.end(env)

_powerpc_vdso_actions_test = analysistest.make(
    _powerpc_vdso_actions_test_impl,
)

def _powerpc_purgatory_actions_test_impl(ctx):
    env = analysistest.begin(ctx)
    actions = analysistest.target_actions(env)
    compile_actions = [action for action in actions if action.mnemonic == "LinuxPowerPCPurgatoryCompile"]
    link_actions = [action for action in actions if action.mnemonic == "LinuxPowerPCPurgatoryLink"]
    object_actions = [action for action in actions if action.mnemonic == "LinuxObjectCompile"]
    asserts.equals(env, 1, len(compile_actions))
    asserts.equals(env, 1, len(link_actions))
    asserts.equals(env, 1, len(object_actions))
    if link_actions:
        asserts.true(env, "-EL" in link_actions[0].argv)
        asserts.true(env, "elf64lppc" in link_actions[0].argv)
        asserts.true(env, "--no-undefined" in link_actions[0].argv)
    if object_actions:
        input_paths = [file.path for file in object_actions[0].inputs.to_list()]
        asserts.true(env, any([path.endswith("/arch/powerpc/purgatory/purgatory.ro") for path in input_paths]))
    return analysistest.end(env)

_powerpc_purgatory_actions_test = analysistest.make(
    _powerpc_purgatory_actions_test_impl,
)

def _x86_purgatory_actions_test_impl(ctx):
    env = analysistest.begin(ctx)
    actions = analysistest.target_actions(env)
    compile_actions = [action for action in actions if action.mnemonic == "LinuxPurgatoryCompile"]
    filter_actions = [action for action in actions if action.mnemonic == "LinuxFlagFilter"]
    asserts.equals(env, 6, len(compile_actions))
    asserts.equals(env, 6, len(filter_actions))
    filter_outputs = []
    for action in filter_actions:
        outputs = action.outputs.to_list()
        asserts.equals(env, 1, len(outputs))
        if outputs:
            filter_outputs.append(outputs[0].path)
        if any([output.path.endswith("-c.rsp") for output in outputs]):
            for flag in [
                "-pg",
                "-mcmodel=kernel",
                "-fstack-protector-strong",
                "-mretpoline",
                "-mfunction-return=thunk-extern",
                "-fsanitize=kcfi",
                "-flto=thin",
                "-fsplit-lto-unit",
            ]:
                asserts.true(env, flag in action.argv)
            asserts.true(env, "-fprofile-use=" in action.argv)
        else:
            asserts.true(env, "-gdwarf-5" in action.argv)
            asserts.true(env, "-Wa,-gdwarf-5" in action.argv)
    asserts.equals(env, 6, len({path: True for path in filter_outputs}))
    for action in compile_actions:
        response_indices = [
            index
            for index in range(len(action.argv))
            if "x86-purgatory-" in action.argv[index] and action.argv[index].endswith(".rsp")
        ]
        asserts.equals(env, 1, len(response_indices))
        if response_indices:
            response_index = response_indices[0]
            for positive in ["-mcmodel=small", "-fno-stack-protector", "-fpic", "-fvisibility=hidden"]:
                positive_indices = [index for index in range(len(action.argv)) if action.argv[index] == positive]
                asserts.true(env, any([index > response_index for index in positive_indices]))
            input_paths = [file.path for file in action.inputs.to_list()]
            asserts.true(env, action.argv[response_index][1:] in input_paths)
    return analysistest.end(env)

_x86_purgatory_actions_test = analysistest.make(
    _x86_purgatory_actions_test_impl,
)

def _riscv_purgatory_actions_test_impl(ctx):
    env = analysistest.begin(ctx)
    actions = analysistest.target_actions(env)
    compile_actions = [action for action in actions if action.mnemonic == "LinuxRISCVPurgatoryCompile"]
    link_actions = [action for action in actions if action.mnemonic == "LinuxRISCVPurgatoryLink"]
    check_actions = [action for action in actions if action.mnemonic == "LinuxRISCVPurgatoryCheck"]
    object_actions = [action for action in actions if action.mnemonic == "LinuxObjectCompile"]
    asserts.equals(env, 10, len(compile_actions))
    asserts.equals(env, 1, len(link_actions))
    asserts.equals(env, 1, len(check_actions))
    asserts.equals(env, 1, len(object_actions))
    if object_actions:
        input_paths = [file.path for file in object_actions[0].inputs.to_list()]
        asserts.true(env, any([path.endswith("/arch/riscv/purgatory/purgatory.ro") for path in input_paths]))
        asserts.true(env, any([path.endswith("/arch/riscv/purgatory/purgatory.chk") for path in input_paths]))
    return analysistest.end(env)

_riscv_purgatory_actions_test = analysistest.make(
    _riscv_purgatory_actions_test_impl,
)

def _generic_generated_headers_fixture(name, arch, config, tags):
    kwargs = {
        "name": name,
        "arch": arch,
        "asm_offsets_c": "linux_objects_test_fixture.c",
        "bounds_c": "linux_objects_test_fixture.c",
        "config": config,
        "family_content_ids": {
            "all": "abababababababababababababababababababababababababababababababab",
        },
        "rq_offsets_c": "linux_objects_test_fixture.c",
        "source_root": "linux_objects_test_fixture.c",
        "syscall_tbl": "linux_objects_test_fixture.c",
        "tags": tags,
        "uts_machine": arch,
    }
    if arch == "arm":
        kwargs.update({
            "mach_types": "linux_objects_test_fixture.c",
            "vdsomunge": "//internal/cmd/runandwrite",
        })
    linux_generic_generated_headers(**kwargs)

def _x86_generated_headers_fixture(
        name,
        config,
        family_content_ids,
        family_dependency_ids,
        reusable_generated_headers,
        tags):
    linux_x86_generated_headers(
        name = name,
        asm_offsets_c = "linux_objects_test_fixture.c",
        bounds_c = "linux_objects_test_fixture.c",
        config = config,
        cpufeatures_h = "linux_objects_test_fixture.c",
        family_content_ids = family_content_ids,
        family_dependency_ids = family_dependency_ids,
        kvm_asm_offsets_c = "linux_objects_test_fixture.c",
        orc_types_h = "linux_objects_test_fixture.c",
        required_features_h = "linux_objects_test_fixture.c",
        reusable_generated_headers = reusable_generated_headers,
        rq_offsets_c = "linux_objects_test_fixture.c",
        source_root = "linux_objects_test_fixture.c",
        syscall_32_tbl = "linux_objects_test_fixture.c",
        syscall_64_tbl = "linux_objects_test_fixture.c",
        tags = tags,
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
    header_family_id = "3333333333333333333333333333333333333333333333333333333333333333"
    arm64_environment_id = "4444444444444444444444444444444444444444444444444444444444444444"
    arm64_header_family_id = "5555555555555555555555555555555555555555555555555555555555555555"
    vdso32_object_id = "6666666666666666666666666666666666666666666666666666666666666666"
    precise_header_family_id = "7777777777777777777777777777777777777777777777777777777777777777"
    second_precise_header_family_id = "8888888888888888888888888888888888888888888888888888888888888888"
    precise_environment_id = "9999999999999999999999999999999999999999999999999999999999999999"

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
        compile_environment_id = environment_id,
        compile_environment_index = ":" + name + "_compile_environment_index",
        content_id = object_a_id,
        mode = "y",
        object = "certs/system_certificates.o",
        source_input_file = 1,
        source_input_group = 1,
        source_input_index = ":" + name + "_certificate_source_input_index",
        tags = fixture_tags,
    )

    generated_headers = name + "_generated_headers_a"
    duplicate_generated_headers = name + "_generated_headers_z"
    _fake_generated_headers(
        name = generated_headers,
        emit_cflags = True,
        family_content_id = header_family_id,
        tags = fixture_tags,
    )
    _fake_generated_headers(
        name = duplicate_generated_headers,
        family_content_id = header_family_id,
        tags = fixture_tags,
    )
    compile_environment_index = name + "_compile_environment_index"
    linux_compile_environment_index(
        name = compile_environment_index,
        compile_environments = {
            environment_id: json.encode({
                "abi": "x86_64-linux-gnu",
                "config_payload": payload_id,
                "generated_header_families": [header_family_id],
            }),
        },
        config_payloads = {
            payload_id: "CONFIG_TEST=y\n",
        },
        expected_abi = "x86_64-linux-gnu",
        generated_headers = [
            ":" + generated_headers,
            ":" + duplicate_generated_headers,
        ],
        tags = fixture_tags,
    )
    arm64_generated_headers = name + "_arm64_generated_headers"
    _fake_arm64_generated_headers(
        name = arm64_generated_headers,
        family_content_id = arm64_header_family_id,
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
                "generated_header_families": [arm64_header_family_id],
            }),
        },
        config_payloads = {
            payload_id: "CONFIG_TEST=y\n",
        },
        expected_abi = "arm64-linux-gnu",
        generated_headers = [":" + arm64_generated_headers],
        tags = fixture_tags,
    )
    unbound_generated_headers = name + "_unbound_generated_headers"
    _fake_generated_headers(
        name = unbound_generated_headers,
        family_content_id = "",
        tags = fixture_tags,
    )
    unbound_header_index = name + "_unbound_header_index"
    linux_compile_environment_index(
        name = unbound_header_index,
        compile_environments = {
            environment_id: json.encode({
                "abi": "x86_64-linux-gnu",
                "config_payload": payload_id,
                "generated_header_families": [header_family_id],
            }),
        },
        config_payloads = {
            payload_id: "CONFIG_TEST=y\n",
        },
        expected_abi = "x86_64-linux-gnu",
        generated_headers = [":" + unbound_generated_headers],
        tags = fixture_tags,
    )
    precise_generated_headers = name + "_precise_generated_headers"
    _fake_generated_headers(
        name = precise_generated_headers,
        family_content_id = precise_header_family_id,
        family_name = "static",
        tags = fixture_tags,
    )
    mixed_header_family_index = name + "_mixed_header_family_index"
    linux_compile_environment_index(
        name = mixed_header_family_index,
        compile_environments = {
            environment_id: json.encode({
                "abi": "x86_64-linux-gnu",
                "config_payload": payload_id,
                "generated_header_families": [
                    header_family_id,
                    precise_header_family_id,
                ],
            }),
        },
        config_payloads = {
            payload_id: "CONFIG_TEST=y\n",
        },
        expected_abi = "x86_64-linux-gnu",
        generated_headers = [
            ":" + generated_headers,
            ":" + precise_generated_headers,
        ],
        tags = fixture_tags,
    )
    second_precise_generated_headers = name + "_second_precise_generated_headers"
    _fake_generated_headers(
        name = second_precise_generated_headers,
        family_content_id = second_precise_header_family_id,
        family_name = "asm_offsets",
        tags = fixture_tags,
    )
    precise_header_family_index = name + "_precise_header_family_index"
    linux_compile_environment_index(
        name = precise_header_family_index,
        compile_environments = {
            precise_environment_id: json.encode({
                "abi": "x86_64-linux-gnu",
                "config_payload": payload_id,
                "generated_header_families": [
                    precise_header_family_id,
                    second_precise_header_family_id,
                ],
            }),
        },
        config_payloads = {
            payload_id: "CONFIG_TEST=y\n",
        },
        expected_abi = "x86_64-linux-gnu",
        generated_headers = [
            ":" + precise_generated_headers,
            ":" + second_precise_generated_headers,
        ],
        tags = fixture_tags,
    )
    source_tree = name + "_source_tree"
    linux_source_tree(
        name = source_tree,
        root = "linux_modules_test_fixture.rs",
        tags = fixture_tags,
    )
    source_input_index = name + "_source_input_index"
    linux_source_input_index(
        name = source_input_index,
        groups = ["1,2,3,4"],
        srcs = [
            "include/linux/compiler-version.h",
            "include/linux/compiler_types.h",
            "include/linux/kconfig.h",
            "linux_objects_test_fixture.c",
        ],
        source_tree_info = ":" + source_tree,
        tags = fixture_tags,
    )
    certificate_source_input_index = name + "_certificate_source_input_index"
    linux_source_input_index(
        name = certificate_source_input_index,
        groups = ["1"],
        srcs = ["linux_objects_test_fixture.S"],
        source_tree_info = ":" + source_tree,
        tags = fixture_tags,
    )
    assembly_source_input_index = name + "_assembly_source_input_index"
    linux_source_input_index(
        name = assembly_source_input_index,
        groups = ["1", "2"],
        srcs = [
            # keep
            "lib/crypto/x86/blake2s.h",
            "lib/crypto/x86/blake2s-core.S",
        ],
        source_tree_info = ":" + source_tree,
        tags = fixture_tags,
    )
    indexed_assembly_object = name + "_indexed_assembly_object"
    linux_object(
        name = indexed_assembly_object,
        compile_environment_id = environment_id,
        compile_environment_index = ":" + compile_environment_index,
        content_id = object_c_id,
        mode = "y",
        object = "lib/crypto/x86/blake2s-core.o",
        source_input_file = 1,
        source_input_group = 1,
        source_input_index = ":" + assembly_source_input_index,
        tags = fixture_tags,
    )
    indexed_assembly_object_test = indexed_assembly_object + "_test"
    _indexed_assembly_source_test(
        name = indexed_assembly_object_test,
        target_under_test = ":" + indexed_assembly_object,
        unexpected_generated_cflags = generated_headers + ".cflags.rsp",
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
        srcs = [":" + nvhe_source_inputs],
        source_tree_info = ":" + nvhe_source_inputs,
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
        srcs = [":" + vdso32_source_inputs],
        source_tree_info = ":" + vdso32_source_inputs,
        tags = fixture_tags,
    )
    duplicate_source_group_index = name + "_duplicate_source_group_index"
    linux_source_input_index(
        name = duplicate_source_group_index,
        groups = ["1", "1"],
        srcs = ["linux_objects_test_fixture.c"],
        source_tree_info = ":" + source_tree,
        tags = fixture_tags,
    )
    out_of_range_source_file_index = name + "_out_of_range_source_file_index"
    linux_source_input_index(
        name = out_of_range_source_file_index,
        groups = ["2"],
        srcs = ["linux_objects_test_fixture.c"],
        source_tree_info = ":" + source_tree,
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
        tags = fixture_tags,
    )
    indexed_object_test = indexed_object + "_test"
    _content_addressed_object_test(
        name = indexed_object_test,
        expected_content_id = object_a_id,
        expected_generated_cflags = generated_headers + ".cflags.rsp",
        expected_generated_headers = [generated_headers + ".h"],
        expected_payload_id = payload_id,
        target_under_test = ":" + indexed_object,
        unexpected_generated_headers = [duplicate_generated_headers + ".h"],
        unexpected_input = "linux_modules_test_fixture.rs",
    )
    remove_flags_payload_id = "1818181818181818181818181818181818181818181818181818181818181818"
    remove_flags_environment_id = "1919191919191919191919191919191919191919191919191919191919191919"
    remove_flags_environment_index = name + "_remove_flags_environment_index"
    linux_compile_environment_index(
        name = remove_flags_environment_index,
        compile_environments = {
            remove_flags_environment_id: json.encode({
                "abi": "x86_64-linux-gnu",
                "config_payload": remove_flags_payload_id,
                "generated_header_families": [header_family_id],
            }),
        },
        config_payloads = {
            remove_flags_payload_id: "CONFIG_GENKSYMS=y\nCONFIG_MODVERSIONS=y\n",
        },
        expected_abi = "x86_64-linux-gnu",
        generated_headers = [":" + generated_headers],
        tags = fixture_tags,
    )
    remove_flags_object = name + "_remove_flags_object"
    linux_object(
        name = remove_flags_object,
        compile_environment_id = remove_flags_environment_id,
        compile_environment_index = ":" + remove_flags_environment_index,
        content_id = "2020202020202020202020202020202020202020202020202020202020202020",
        flags = [
            "-mgeneral-regs-only",
            "-DREMOVE",
            "-DKEEP_COMPILE",
            "-DREMOVE_SUFFIX",
        ],
        genksyms = "//internal/cmd/runandwrite",
        mode = "m",
        object = "crypto/aegis128-neon-inner.o",
        remove_flags = [
            "-mgeneral-regs-only",
            "-DREMOVE",
        ],
        source_input_file = 4,
        source_input_group = 1,
        source_input_index = ":" + source_input_index,
        symversion_flags = [
            "-mgeneral-regs-only",
            "-DREMOVE",
            "-DKEEP_SYMVERSION",
            "-DREMOVE_SUFFIX",
        ],
        symversion_remove_flags = [
            "-mgeneral-regs-only",
            "-DREMOVE",
        ],
        symversions = True,
        tags = fixture_tags,
        version = "6.18.39",
    )
    remove_flags_object_test = remove_flags_object + "_test"
    _object_remove_flags_test(
        name = remove_flags_object_test,
        target_under_test = ":" + remove_flags_object,
    )
    precise_family_object = name + "_precise_family_object"
    linux_object(
        name = precise_family_object,
        compile_environment_id = precise_environment_id,
        compile_environment_index = ":" + precise_header_family_index,
        content_id = object_b_id,
        mode = "y",
        object = "precise-family.o",
        source_input_file = 4,
        source_input_group = 1,
        source_input_index = ":" + source_input_index,
        tags = fixture_tags,
    )
    precise_family_object_test = precise_family_object + "_test"
    _content_addressed_object_test(
        name = precise_family_object_test,
        expected_content_id = object_b_id,
        expected_generated_headers = [
            precise_generated_headers + ".h",
            second_precise_generated_headers + ".h",
        ],
        expected_payload_id = payload_id,
        target_under_test = ":" + precise_family_object,
        unexpected_generated_headers = [generated_headers + ".h"],
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
    empty_source_inputs_nvhe = name + "_empty_source_inputs_nvhe"
    linux_arm64_nvhe_object(
        name = empty_source_inputs_nvhe,
        compile_environment_id = environment_id,
        compile_environment_index = ":" + compile_environment_index,
        content_id = object_b_id,
        mode = "y",
        object = "arch/arm64/kvm/hyp/nvhe/kvm_nvhe.o",
        objects = [":" + name + "_object_a"],
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
                "generated_header_families": [],
            }),
        },
        config_payloads = {
            payload_id: "CONFIG_TEST=y\n",
        },
        expected_abi = "expected-abi",
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
    generic_header_tests = []
    for arch in ["arm", "powerpc"]:
        generic_headers = name + "_%s_generated_headers" % arch
        _generic_generated_headers_fixture(
            name = generic_headers,
            arch = arch,
            config = ":" + equivalent_config,
            tags = fixture_tags,
        )
        generic_headers_test = generic_headers + "_anchors_test"
        _generic_generated_header_anchors_test(
            name = generic_headers_test,
            arch = arch,
            target_under_test = ":" + generic_headers,
        )
        generic_header_tests.append(":" + generic_headers_test)

    arm_compressed_config = name + "_arm_compressed_config"
    linux_config(
        name = arm_compressed_config,
        arch = "arm",
        config_flags = {
            "CONFIG_ARM": "y",
            "CONFIG_AUTO_ZRELADDR": "y",
            "CONFIG_KERNEL_ZSTD": "y",
        },
        tags = fixture_tags,
    )
    arm_compressed_headers = name + "_arm_compressed_headers"
    _fake_generated_headers(
        name = arm_compressed_headers,
        arch = "arm",
        family_content_id = header_family_id,
        tags = fixture_tags,
    )
    arm_compressed_sources = name + "_arm_compressed_sources"
    _fake_arm_compressed_source_inputs(
        name = arm_compressed_sources,
        tags = fixture_tags,
    )
    arm_compressed_image = name + "_arm_compressed_image"
    linux_compressed_image(
        name = arm_compressed_image,
        arch = "arm",
        config = ":" + arm_compressed_config,
        extension = "zImage",
        format = "arm_zimage",
        generated_headers = ":" + arm_compressed_headers,
        image = ":" + image,
        source_root = "linux_objects_test_fixture.c",
        source_tree = [":" + arm_compressed_sources],
        srcarch = "arm",
        tags = fixture_tags,
    )
    arm_compressed_image_test = arm_compressed_image + "_flagfilter_test"
    _arm_compressed_flagfilter_test(
        name = arm_compressed_image_test,
        target_under_test = ":" + arm_compressed_image,
    )
    generic_header_tests.append(":" + arm_compressed_image_test)

    arm_vmlinux_sources = name + "_arm_vmlinux_sources"
    _fake_vmlinux_source_inputs(
        name = arm_vmlinux_sources,
        tags = fixture_tags,
    )
    arm_vmlinux_rust_sdk = name + "_arm_vmlinux_rust_sdk"
    linux_disabled_rust_kernel_sdk(
        name = arm_vmlinux_rust_sdk,
        tags = fixture_tags,
    )
    for suffix, expected_offset, extra_config_flags in [
        ("default", "0x00008000", {}),
        ("realtek", "0x00108000", {"CONFIG_ARCH_REALTEK": "y"}),
    ]:
        config_flags = {"CONFIG_ARM": "y"}
        config_flags.update(extra_config_flags)
        arm_vmlinux_config = name + "_arm_vmlinux_" + suffix + "_config"
        linux_config(
            name = arm_vmlinux_config,
            arch = "arm",
            config_flags = config_flags,
            tags = fixture_tags,
            version = "6.12.0",
        )
        arm_vmlinux = name + "_arm_vmlinux_" + suffix
        linux_vmlinux(
            name = arm_vmlinux,
            arch = "arm",
            config = ":" + arm_vmlinux_config,
            format = "armv7",
            generated_headers = ":" + arm_compressed_headers,
            image = ":" + image,
            kallsyms = "false",
            linker_script = "linux_objects_test_fixture.S",
            rust_sdk = ":" + arm_vmlinux_rust_sdk,
            source_root = "linux_objects_test_fixture.c",
            source_tree = [
                ":" + arm_vmlinux_sources,
                "include/linux/compiler-version.h",
                "include/linux/compiler_types.h",
                "include/linux/kconfig.h",
            ],
            srcarch = "arm",
            tags = fixture_tags,
            version = "6.12.0",
        )
        arm_vmlinux_test = arm_vmlinux + "_text_offset_test"
        _arm_vmlinux_text_offset_test(
            name = arm_vmlinux_test,
            expected = expected_offset,
            target_under_test = ":" + arm_vmlinux,
        )
        generic_header_tests.append(":" + arm_vmlinux_test)

    arm_vdso_config = name + "_arm_vdso_config"
    linux_config(
        name = arm_vdso_config,
        arch = "arm",
        config_flags = {
            "CONFIG_ARM": "y",
            "CONFIG_GENERIC_GETTIMEOFDAY": "y",
            "CONFIG_VDSO": "y",
        },
        tags = fixture_tags,
    )
    arm_vdso_sources = name + "_arm_vdso_sources"
    _fake_arm_vdso_source_inputs(
        name = arm_vdso_sources,
        tags = fixture_tags,
    )
    arm_vdso_headers = name + "_arm_vdso_headers"
    linux_generic_generated_headers(
        name = arm_vdso_headers,
        arch = "arm",
        asm_offsets_c = "linux_objects_test_fixture.c",
        bounds_c = "linux_objects_test_fixture.c",
        config = ":" + arm_vdso_config,
        family_content_ids = {
            "all": "bcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbc",
        },
        mach_types = "linux_objects_test_fixture.c",
        rq_offsets_c = "linux_objects_test_fixture.c",
        source_root = "linux_objects_test_fixture.c",
        source_tree = [":" + arm_vdso_sources],
        syscall_tbl = "linux_objects_test_fixture.c",
        tags = fixture_tags,
        uts_machine = "armv7l",
        vdsomunge = "//internal/cmd/runandwrite",
    )
    arm_vdso_headers_test = arm_vdso_headers + "_actions_test"
    _arm_vdso_actions_test(
        name = arm_vdso_headers_test,
        target_under_test = ":" + arm_vdso_headers,
    )
    generic_header_tests.append(":" + arm_vdso_headers_test)

    powerpc_vdso_config = name + "_powerpc_vdso_config"
    linux_config(
        name = powerpc_vdso_config,
        arch = "powerpc",
        config_flags = {
            "CONFIG_CPU_LITTLE_ENDIAN": "y",
            "CONFIG_GENERIC_GETTIMEOFDAY": "y",
            "CONFIG_PPC64": "y",
            "CONFIG_PPC64_ELF_ABI_V2": "y",
            "CONFIG_VDSO32": "y",
            "CONFIG_VDSO_GETRANDOM": "y",
        },
        tags = fixture_tags,
    )
    powerpc_vdso_sources = name + "_powerpc_vdso_sources"
    _fake_powerpc_vdso_source_inputs(
        name = powerpc_vdso_sources,
        tags = fixture_tags,
    )
    powerpc_vdso_headers = name + "_powerpc_vdso_headers"
    linux_generic_generated_headers(
        name = powerpc_vdso_headers,
        arch = "powerpc",
        asm_offsets_c = "linux_objects_test_fixture.c",
        bounds_c = "linux_objects_test_fixture.c",
        config = ":" + powerpc_vdso_config,
        family_content_ids = {
            "all": "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd",
        },
        rq_offsets_c = "linux_objects_test_fixture.c",
        source_root = "linux_objects_test_fixture.c",
        source_tree = [":" + powerpc_vdso_sources],
        syscall_tbl = "linux_objects_test_fixture.c",
        tags = fixture_tags,
        uts_machine = "ppc64le",
    )
    powerpc_vdso_headers_test = powerpc_vdso_headers + "_actions_test"
    _powerpc_vdso_actions_test(
        name = powerpc_vdso_headers_test,
        target_under_test = ":" + powerpc_vdso_headers,
    )
    generic_header_tests.append(":" + powerpc_vdso_headers_test)

    x86_purgatory_headers = name + "_x86_purgatory_headers"
    _fake_generated_headers(
        name = x86_purgatory_headers,
        arch = "x86",
        family_content_id = "1414141414141414141414141414141414141414141414141414141414141414",
        tags = fixture_tags,
    )
    x86_purgatory_sources = name + "_x86_purgatory_sources"
    _fake_x86_purgatory_source_inputs(
        name = x86_purgatory_sources,
        tags = fixture_tags,
    )
    x86_purgatory_source_index = name + "_x86_purgatory_source_index"
    linux_source_input_index(
        name = x86_purgatory_source_index,
        groups = [",".join([str(index) for index in range(1, 11)])],
        srcs = [":" + x86_purgatory_sources],
        source_tree_info = ":" + x86_purgatory_sources,
        tags = fixture_tags,
    )
    x86_purgatory_environment_id = "1515151515151515151515151515151515151515151515151515151515151515"
    x86_purgatory_payload_id = "1616161616161616161616161616161616161616161616161616161616161616"
    x86_purgatory_environment_index = name + "_x86_purgatory_environment_index"
    linux_compile_environment_index(
        name = x86_purgatory_environment_index,
        arch = "x86",
        compile_environments = {
            x86_purgatory_environment_id: json.encode({
                "abi": "x86_64-linux-gnu",
                "config_payload": x86_purgatory_payload_id,
                "generated_header_families": ["1414141414141414141414141414141414141414141414141414141414141414"],
            }),
        },
        config_payloads = {
            x86_purgatory_payload_id: "CONFIG_64BIT=y\nCONFIG_CC_IS_CLANG=y\nCONFIG_X86_64=y\n",
        },
        expected_abi = "x86_64-linux-gnu",
        generated_headers = [":" + x86_purgatory_headers],
        tags = fixture_tags,
    )
    x86_purgatory_object = name + "_x86_purgatory_object"
    linux_object(
        name = x86_purgatory_object,
        arch = "x86",
        compile_environment_id = x86_purgatory_environment_id,
        compile_environment_index = ":" + x86_purgatory_environment_index,
        content_id = "1717171717171717171717171717171717171717171717171717171717171717",
        mode = "y",
        object = "arch/x86/purgatory/kexec-purgatory.o",
        source_input_file = 3,
        source_input_group = 1,
        source_input_index = ":" + x86_purgatory_source_index,
        srcarch = "x86",
        tags = fixture_tags,
    )
    x86_purgatory_object_test = x86_purgatory_object + "_actions_test"
    _x86_purgatory_actions_test(
        name = x86_purgatory_object_test,
        target_under_test = ":" + x86_purgatory_object,
    )
    generic_header_tests.append(":" + x86_purgatory_object_test)

    powerpc_purgatory_headers = name + "_powerpc_purgatory_headers"
    _fake_generated_headers(
        name = powerpc_purgatory_headers,
        arch = "powerpc",
        family_content_id = "3434343434343434343434343434343434343434343434343434343434343434",
        tags = fixture_tags,
    )
    powerpc_purgatory_sources = name + "_powerpc_purgatory_sources"
    _fake_powerpc_purgatory_source_inputs(
        name = powerpc_purgatory_sources,
        tags = fixture_tags,
    )
    powerpc_purgatory_source_index = name + "_powerpc_purgatory_source_index"
    linux_source_input_index(
        name = powerpc_purgatory_source_index,
        groups = [",".join([str(index) for index in range(1, 6)])],
        srcs = [":" + powerpc_purgatory_sources],
        source_tree_info = ":" + powerpc_purgatory_sources,
        tags = fixture_tags,
    )
    powerpc_purgatory_environment_id = "4545454545454545454545454545454545454545454545454545454545454545"
    powerpc_purgatory_payload_id = "5656565656565656565656565656565656565656565656565656565656565656"
    powerpc_purgatory_environment_index = name + "_powerpc_purgatory_environment_index"
    linux_compile_environment_index(
        name = powerpc_purgatory_environment_index,
        arch = "powerpc",
        compile_environments = {
            powerpc_purgatory_environment_id: json.encode({
                "abi": "powerpc64le-linux-gnu",
                "config_payload": powerpc_purgatory_payload_id,
                "generated_header_families": ["3434343434343434343434343434343434343434343434343434343434343434"],
            }),
        },
        config_payloads = {
            powerpc_purgatory_payload_id: "CONFIG_64BIT=y\nCONFIG_CC_IS_CLANG=y\nCONFIG_CPU_LITTLE_ENDIAN=y\nCONFIG_PPC64=y\nCONFIG_PPC64_ELF_ABI_V2=y\n",
        },
        expected_abi = "powerpc64le-linux-gnu",
        generated_headers = [":" + powerpc_purgatory_headers],
        tags = fixture_tags,
    )
    powerpc_purgatory_object = name + "_powerpc_purgatory_object"
    linux_object(
        name = powerpc_purgatory_object,
        arch = "powerpc",
        compile_environment_id = powerpc_purgatory_environment_id,
        compile_environment_index = ":" + powerpc_purgatory_environment_index,
        content_id = "6767676767676767676767676767676767676767676767676767676767676767",
        mode = "y",
        object = "arch/powerpc/purgatory/kexec-purgatory.o",
        source_input_file = 1,
        source_input_group = 1,
        source_input_index = ":" + powerpc_purgatory_source_index,
        srcarch = "powerpc",
        tags = fixture_tags,
    )
    powerpc_purgatory_object_test = powerpc_purgatory_object + "_actions_test"
    _powerpc_purgatory_actions_test(
        name = powerpc_purgatory_object_test,
        target_under_test = ":" + powerpc_purgatory_object,
    )
    generic_header_tests.append(":" + powerpc_purgatory_object_test)

    riscv_purgatory_headers = name + "_riscv_purgatory_headers"
    _fake_generated_headers(
        name = riscv_purgatory_headers,
        arch = "riscv",
        family_content_id = "dededededededededededededededededededededededededededededededede",
        tags = fixture_tags,
    )
    riscv_purgatory_sources = name + "_riscv_purgatory_sources"
    _fake_riscv_purgatory_source_inputs(
        name = riscv_purgatory_sources,
        tags = fixture_tags,
    )
    riscv_purgatory_source_index = name + "_riscv_purgatory_source_index"
    linux_source_input_index(
        name = riscv_purgatory_source_index,
        groups = [",".join([str(index) for index in range(1, 15)])],
        srcs = [":" + riscv_purgatory_sources],
        source_tree_info = ":" + riscv_purgatory_sources,
        tags = fixture_tags,
    )
    riscv_purgatory_environment_id = "efefefefefefefefefefefefefefefefefefefefefefefefefefefefefefefef"
    riscv_purgatory_payload_id = "1212121212121212121212121212121212121212121212121212121212121212"
    riscv_purgatory_environment_index = name + "_riscv_purgatory_environment_index"
    linux_compile_environment_index(
        name = riscv_purgatory_environment_index,
        arch = "riscv",
        compile_environments = {
            riscv_purgatory_environment_id: json.encode({
                "abi": "riscv64-linux-gnu",
                "config_payload": riscv_purgatory_payload_id,
                "generated_header_families": ["dededededededededededededededededededededededededededededededede"],
            }),
        },
        config_payloads = {
            riscv_purgatory_payload_id: "CONFIG_64BIT=y\nCONFIG_ARCH_RV64I=y\nCONFIG_RISCV=y\n",
        },
        expected_abi = "riscv64-linux-gnu",
        generated_headers = [":" + riscv_purgatory_headers],
        tags = fixture_tags,
    )
    riscv_purgatory_object = name + "_riscv_purgatory_object"
    linux_object(
        name = riscv_purgatory_object,
        arch = "riscv",
        compile_environment_id = riscv_purgatory_environment_id,
        compile_environment_index = ":" + riscv_purgatory_environment_index,
        content_id = "1313131313131313131313131313131313131313131313131313131313131313",
        mode = "y",
        object = "arch/riscv/purgatory/kexec-purgatory.o",
        source_input_file = 1,
        source_input_group = 1,
        source_input_index = ":" + riscv_purgatory_source_index,
        srcarch = "riscv",
        tags = fixture_tags,
    )
    riscv_purgatory_object_test = riscv_purgatory_object + "_actions_test"
    _riscv_purgatory_actions_test(
        name = riscv_purgatory_object_test,
        target_under_test = ":" + riscv_purgatory_object,
    )
    generic_header_tests.append(":" + riscv_purgatory_object_test)
    failure_cases = [
        (empty_image, "requires at least one compiled object"),
        (duplicate_source_group_index, "duplicate or non-canonical group"),
        (empty_source_inputs_nvhe, "source_input_group 2 is out of range"),
        (invalid_environment_object, "must be a full lowercase SHA-256 content ID"),
        (mismatched_abi_index, "does not match expected_abi"),
        (mixed_header_family_index, "mixes all with precise generated-header families"),
        (out_of_range_primary_source_object, "source_input_file 5 is out of range"),
        (out_of_range_source_file_index, "file index 2 is out of range"),
        (unbound_header_index, "family all content ID must be a full lowercase SHA-256 content ID"),
    ]
    tests = [
        ":" + certificate_object + "_test",
        ":" + indexed_assembly_object_test,
        ":" + indexed_object_test,
        ":" + precise_family_object_test,
        ":" + remove_flags_object_test,
    ] + generic_header_tests
    _empty_system_certificates_test(
        name = certificate_object + "_test",
        target_under_test = ":" + certificate_object,
    )

    base_header_family_ids = {
        "all": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
        "asm_offsets": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
        "bounds": "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
        "compile": "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
        "cpufeatures": "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
        "kvm_offsets": "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
        "rq_offsets": "0000000000000000000000000000000000000000000000000000000000000000",
        "static": "1111111111111111111111111111111111111111111111111111111111111111",
        "timeconst": "2222222222222222222222222222222222222222222222222222222222222222",
        "utsrelease": "3333333333333333333333333333333333333333333333333333333333333333",
        "utsversion": "4444444444444444444444444444444444444444444444444444444444444444",
        "version": "5555555555555555555555555555555555555555555555555555555555555555",
    }
    variant_header_family_ids = dict(base_header_family_ids)
    variant_header_family_ids.update({
        "all": "6666666666666666666666666666666666666666666666666666666666666666",
        "kvm_offsets": "7777777777777777777777777777777777777777777777777777777777777777",
    })
    header_family_dependency_ids = {
        "asm_offsets:bounds": base_header_family_ids["bounds"],
        "bounds:cpufeatures": base_header_family_ids["cpufeatures"],
        "kvm_offsets:asm_offsets": base_header_family_ids["asm_offsets"],
        "kvm_offsets:utsrelease": base_header_family_ids["utsrelease"],
        "rq_offsets:asm_offsets": base_header_family_ids["asm_offsets"],
        "rq_offsets:timeconst": base_header_family_ids["timeconst"],
    }
    non_earlier_header_dependency = name + "_non_earlier_header_dependency"
    _x86_generated_headers_fixture(
        name = non_earlier_header_dependency,
        config = ":" + equivalent_config,
        family_content_ids = base_header_family_ids,
        family_dependency_ids = {
            "bounds:kvm_offsets": base_header_family_ids["kvm_offsets"],
        },
        reusable_generated_headers = [],
        tags = fixture_tags,
    )
    mismatched_header_dependency_id = name + "_mismatched_header_dependency_id"
    _x86_generated_headers_fixture(
        name = mismatched_header_dependency_id,
        config = ":" + equivalent_config,
        family_content_ids = base_header_family_ids,
        family_dependency_ids = {
            "kvm_offsets:asm_offsets": base_header_family_ids["bounds"],
        },
        reusable_generated_headers = [],
        tags = fixture_tags,
    )
    failure_cases.extend([
        (non_earlier_header_dependency, "depends on non-earlier family kvm_offsets"),
        (mismatched_header_dependency_id, "content ID does not match selected family"),
    ])
    missing_header_family_ids = name + "_missing_header_family_ids"
    _x86_generated_headers_fixture(
        name = missing_header_family_ids,
        config = ":" + equivalent_config,
        family_content_ids = {},
        family_dependency_ids = {},
        reusable_generated_headers = [],
        tags = fixture_tags,
    )
    failure_cases.append(
        (missing_header_family_ids, "family_content_ids has families [], expected"),
    )
    base_generated_header_producer = name + "_base_generated_header_producer"
    _x86_generated_headers_fixture(
        name = base_generated_header_producer,
        config = ":" + equivalent_config,
        family_content_ids = base_header_family_ids,
        family_dependency_ids = header_family_dependency_ids,
        reusable_generated_headers = [],
        tags = fixture_tags,
    )
    variant_generated_header_producer = name + "_variant_generated_header_producer"
    _x86_generated_headers_fixture(
        name = variant_generated_header_producer,
        config = ":" + equivalent_config,
        family_content_ids = variant_header_family_ids,
        family_dependency_ids = header_family_dependency_ids,
        reusable_generated_headers = [":" + base_generated_header_producer],
        tags = fixture_tags,
    )
    later_generated_header_producer = name + "_later_generated_header_producer"
    _x86_generated_headers_fixture(
        name = later_generated_header_producer,
        config = ":" + equivalent_config,
        family_content_ids = base_header_family_ids,
        family_dependency_ids = header_family_dependency_ids,
        reusable_generated_headers = [],
        tags = fixture_tags,
    )
    earliest_reuse_generated_header_producer = name + "_earliest_reuse_generated_header_producer"
    _x86_generated_headers_fixture(
        name = earliest_reuse_generated_header_producer,
        config = ":" + equivalent_config,
        family_content_ids = base_header_family_ids,
        family_dependency_ids = header_family_dependency_ids,
        reusable_generated_headers = [
            ":" + base_generated_header_producer,
            ":" + later_generated_header_producer,
        ],
        tags = fixture_tags,
    )
    generated_header_layout_test = base_generated_header_producer + "_test"
    _generated_header_family_layout_test(
        name = generated_header_layout_test,
        target_under_test = ":" + base_generated_header_producer,
    )
    generated_header_reuse_test = variant_generated_header_producer + "_test"
    _generated_header_family_reuse_test(
        name = generated_header_reuse_test,
        base_generated_headers = ":" + base_generated_header_producer,
        target_under_test = ":" + variant_generated_header_producer,
    )
    generated_header_earliest_reuse_test = earliest_reuse_generated_header_producer + "_test"
    _generated_header_earliest_reuse_test(
        name = generated_header_earliest_reuse_test,
        earliest_generated_headers = ":" + base_generated_header_producer,
        later_generated_headers = ":" + later_generated_header_producer,
        target_under_test = ":" + earliest_reuse_generated_header_producer,
    )
    tests.extend([
        ":" + generated_header_earliest_reuse_test,
        ":" + generated_header_layout_test,
        ":" + generated_header_reuse_test,
    ])
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
