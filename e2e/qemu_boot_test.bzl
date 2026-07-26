"""Private QEMU userspace boot test support for the compatibility workspace."""

load("@linux.bzl", "LinuxKernelInfo")

visibility("private")

_QEMU_TOOLCHAIN = "@rules_qemu//qemu:exec_toolchain_type"
_QEMU_TARGET_SETTING = "//:qemu_system_target"

def _qemu_system_target_impl(_ctx):
    return []

qemu_system_target = rule(
    implementation = _qemu_system_target_impl,
    build_setting = config.string(flag = True),
)

def _runfile_path(ctx, file):
    if file.short_path.startswith("../"):
        return file.short_path[3:]
    return ctx.workspace_name + "/" + file.short_path

def _qemu_boot_binary_impl(ctx):
    kernel = ctx.attr.kernel[LinuxKernelInfo]
    qemu = ctx.toolchains[_QEMU_TOOLCHAIN]
    if kernel.arch != ctx.attr.arch:
        fail("kernel architecture is {}, expected {}".format(kernel.arch, ctx.attr.arch))
    if qemu.target_arch != ctx.attr.arch:
        fail("QEMU toolchain architecture is {}, expected {}".format(qemu.target_arch, ctx.attr.arch))

    config = ctx.actions.declare_file(ctx.label.name + ".json")
    ctx.actions.write(
        output = config,
        content = json.encode({
            "accel": "tcg",
            "arch": ctx.attr.arch,
            "expect": ctx.attr.expect,
            "initramfs": _runfile_path(ctx, ctx.file.initramfs),
            "kernel": _runfile_path(ctx, kernel.image),
            "kernel_args": ctx.attr.kernel_args,
            "machine": qemu.machine,
            "qemu_args": ctx.attr.qemu_args,
            "qemu_system": _runfile_path(ctx, qemu.qemu_system),
            "system_data_anchor": _runfile_path(ctx, qemu.system_data_anchor),
            "timeout_seconds": ctx.attr.timeout_seconds,
        }) + "\n",
    )

    executable = ctx.actions.declare_file(ctx.label.name)
    ctx.actions.symlink(
        output = executable,
        target_file = ctx.executable._runner,
        is_executable = True,
    )

    runfiles = ctx.runfiles(
        files = [
            config,
            ctx.file.initramfs,
            kernel.image,
            qemu.qemu_system,
            qemu.system_data_anchor,
        ],
        transitive_files = qemu.system_data_files,
    )
    runfiles = runfiles.merge(ctx.attr._runner[DefaultInfo].default_runfiles)
    return [
        DefaultInfo(
            executable = executable,
            runfiles = runfiles,
        ),
        RunEnvironmentInfo(
            environment = {
                "LINUX_BZL_QEMU_CONFIG": _runfile_path(ctx, config),
            },
        ),
    ]

_qemu_boot_binary = rule(
    implementation = _qemu_boot_binary_impl,
    attrs = {
        "arch": attr.string(
            mandatory = True,
            values = ["aarch64", "x86_64"],
        ),
        "expect": attr.string(
            mandatory = True,
        ),
        "initramfs": attr.label(
            allow_single_file = True,
            mandatory = True,
        ),
        "kernel": attr.label(
            mandatory = True,
            providers = [LinuxKernelInfo],
        ),
        "kernel_args": attr.string_list(),
        "qemu_args": attr.string_list(),
        "timeout_seconds": attr.int(
            default = 60,
        ),
        "_runner": attr.label(
            cfg = "exec",
            default = "//cmd/qemuboot",
            executable = True,
        ),
    },
    executable = True,
    toolchains = [_QEMU_TOOLCHAIN],
)

def _qemu_target_transition_impl(_settings, attr):
    return {
        _QEMU_TARGET_SETTING: attr.qemu_target,
    }

_qemu_target_transition = transition(
    implementation = _qemu_target_transition_impl,
    inputs = [],
    outputs = [_QEMU_TARGET_SETTING],
)

def _qemu_boot_test_impl(ctx):
    binary = ctx.attr.binary
    if type(binary) == "list":
        if len(binary) != 1:
            fail("QEMU target transition produced {} configurations, expected 1".format(len(binary)))
        binary = binary[0]

    executable = ctx.actions.declare_file(ctx.label.name)
    ctx.actions.symlink(
        output = executable,
        target_file = binary[DefaultInfo].files_to_run.executable,
        is_executable = True,
    )
    return [
        DefaultInfo(
            executable = executable,
            runfiles = binary[DefaultInfo].default_runfiles,
        ),
        binary[RunEnvironmentInfo],
    ]

_qemu_boot_test = rule(
    implementation = _qemu_boot_test_impl,
    attrs = {
        "binary": attr.label(
            cfg = _qemu_target_transition,
            executable = True,
            mandatory = True,
        ),
        "qemu_target": attr.string(
            mandatory = True,
            values = ["aarch64", "x86_64"],
        ),
        "_allowlist_function_transition": attr.label(
            default = "@bazel_tools//tools/allowlists/function_transition_allowlist",
        ),
    },
    test = True,
)

def qemu_boot_test(
        name,
        arch,
        kernel,
        initramfs,
        kernel_args,
        qemu_args = [],
        expect = "LINUX_BZL_BOOT_OK",
        timeout_seconds = 60,
        **kwargs):
    """Declares a private system-QEMU userspace boot test."""
    binary = name + "_binary"
    _qemu_boot_binary(
        name = binary,
        arch = arch,
        expect = expect,
        initramfs = initramfs,
        kernel = kernel,
        kernel_args = kernel_args,
        qemu_args = qemu_args,
        tags = ["manual"],
        timeout_seconds = timeout_seconds,
        visibility = ["//visibility:private"],
    )
    _qemu_boot_test(
        name = name,
        binary = binary,
        exec_compatible_with = ["@platforms//os:linux"],
        qemu_target = arch,
        **kwargs
    )
