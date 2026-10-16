"""Local QEMU boot test for the prototype's output ELF."""

load("@rules_cc//cc:action_names.bzl", "ACTION_NAMES")
load("@rules_cc//cc:find_cc_toolchain.bzl", "find_cc_toolchain", "use_cc_toolchain")
load("@rules_cc//cc/common:cc_common.bzl", "cc_common")
load(":compiler_metadata.bzl", "KernelCompilerMetadataInfo")
load(":qemu_tools.bzl", "KernelQemuInfo")

def _impl(ctx):
    qemu = ctx.attr._qemu[KernelQemuInfo]
    launcher = ctx.actions.declare_file(ctx.label.name + ".py")
    ctx.actions.write(launcher, "#!/usr/bin/env python3\nimport runpy,sys\nsys.argv=" + repr([ctx.file._runner.short_path, ctx.file.kernel.short_path, ctx.file.init.short_path, qemu.executable.short_path, qemu.data.short_path, qemu.spec["machine"], qemu.spec["accel"]]) + "\nrunpy.run_path(sys.argv[0],run_name='__main__')\n", is_executable = True)
    return [DefaultInfo(executable = launcher, runfiles = ctx.runfiles(files = [ctx.file._runner, ctx.file.kernel, ctx.file.init], transitive_files = qemu.files))]

kernel_boot_test = rule(implementation = _impl, test = True, attrs = {
    "_qemu": attr.label(default = "//qemu_tools:x86_64", providers = [KernelQemuInfo]),
    "kernel": attr.label(mandatory = True, allow_single_file = True),
    "init": attr.label(mandatory = True, allow_single_file = True),
    "_runner": attr.label(default = "//rules:boot_test.py", allow_single_file = True),
})

def _init_impl(ctx):
    cc = find_cc_toolchain(ctx)
    features = cc_common.configure_features(ctx = ctx, cc_toolchain = cc)
    compile_vars = cc_common.create_compile_variables(cc_toolchain = cc, feature_configuration = features)
    compiler = cc_common.get_tool_for_action(feature_configuration = features, action_name = ACTION_NAMES.c_compile)
    flags = cc_common.get_memory_inefficient_command_line(feature_configuration = features, action_name = ACTION_NAMES.c_compile, variables = compile_vars)
    obj = ctx.actions.declare_file(ctx.label.name + ".o")
    ctx.actions.run(
        executable = compiler,
        arguments = flags + ["-c", ctx.file.src.path, "-o", obj.path],
        inputs = depset([ctx.file.src], transitive = [cc.all_files]),
        outputs = [obj],
        mnemonic = "BootInitCC",
    )
    metadata = ctx.attr._compiler_metadata[KernelCompilerMetadataInfo]
    if metadata.toolchain_id != cc.toolchain_id:
        fail("Boot fixture compiler metadata does not match its selected C++ toolchain")
    linker = metadata.tool_files["LD"]
    elf = ctx.actions.declare_file(ctx.label.name)
    ctx.actions.run(
        executable = linker,
        arguments = ["-static", "-e", "_start", obj.path, "-o", elf.path],
        inputs = depset([obj], transitive = [cc.all_files, metadata.files]),
        outputs = [elf],
        mnemonic = "BootInitLD",
    )
    return [DefaultInfo(files = depset([elf]))]

boot_init_binary = rule(implementation = _init_impl, fragments = ["cpp"], toolchains = use_cc_toolchain(), attrs = {
    "src": attr.label(allow_single_file = [".S"], mandatory = True),
    "_compiler_metadata": attr.label(default = "//:compiler_metadata", providers = [KernelCompilerMetadataInfo]),
})
