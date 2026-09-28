"""Hermetic script runtime toolchain used by mapped kernel actions."""

visibility("//internal/...")

SCRIPT_RUNTIME_TOOLCHAIN_TYPE = str(Label("//internal:script_runtime_toolchain_type"))

_LINUX_X86_64_EXEC_CONSTRAINTS = [
    "@platforms//os:linux",
    "@platforms//cpu:x86_64",
]

ScriptRuntimeToolchainInfo = provider(
    doc = "A script interpreter and multicall utility selected for an execution platform.",
    fields = {
        "applets": "Command-name to executable File overrides installed over multicall applets.",
        "execution_constraints": "Constraint labels implemented by this runtime.",
        "files": "Complete depset of files needed to execute the runtime.",
        "interpreter": "Executable File used as the script interpreter.",
        "interpreter_args": "Arguments placed between the interpreter and script path.",
        "multicall": "Executable File used to invoke the runtime's utility applets.",
    },
)

def _script_runtime_impl(ctx):
    runtime = ctx.file.runtime
    applets = {}
    applet_files = []
    applet_closures = []
    for name, target in ctx.attr.applets.items():
        if not _valid_applet_name(name):
            fail("script runtime has invalid applet name %r" % name)
        target_files = target[DefaultInfo].files.to_list()
        if len(target_files) != 1:
            fail("script runtime applet %r must provide exactly one executable file, found %d" % (name, len(target_files)))
        executable = target_files[0]
        applets[name] = executable
        applet_files.append(executable)
        applet_closures.append(target[DefaultInfo].files)
    files = depset(
        direct = [runtime] + applet_files,
        transitive = [ctx.attr.runtime[DefaultInfo].files] + applet_closures,
    )
    info = ScriptRuntimeToolchainInfo(
        applets = applets,
        execution_constraints = [str(target.label) for target in ctx.attr._execution_constraints],
        files = files,
        interpreter = runtime,
        interpreter_args = ctx.attr.interpreter_args,
        multicall = runtime,
    )
    return [
        DefaultInfo(files = files),
        info,
        platform_common.ToolchainInfo(script_runtime = info),
    ]

linux_script_runtime = rule(
    implementation = _script_runtime_impl,
    attrs = {
        "applets": attr.string_keyed_label_dict(
            allow_files = True,
            cfg = "exec",
        ),
        "interpreter_args": attr.string_list(mandatory = True),
        "runtime": attr.label(
            allow_single_file = True,
            # Toolchain implementation targets retain the consumer's target
            # configuration. Only the executable payload crosses to the
            # selected execution platform.
            cfg = "exec",
            mandatory = True,
        ),
        "_execution_constraints": attr.label_list(
            default = [Label(label) for label in _LINUX_X86_64_EXEC_CONSTRAINTS],
        ),
    },
)

def _valid_applet_name(name):
    if not name or name[0] == "-":
        return False
    for character in name.elems():
        if character not in "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.":
            return False
    return True

def linux_x86_64_script_runtime_toolchain(
        name,
        runtime,
        applets = {},
        implementation_name = None,
        interpreter_args = ["sh"],
        visibility = None):
    """Declares a Linux/x86-64 script runtime and its Bazel toolchain."""
    implementation_name = implementation_name or name + "_implementation"
    linux_script_runtime(
        name = implementation_name,
        applets = applets,
        interpreter_args = interpreter_args,
        runtime = runtime,
    )

    # The implementation is target-configured, so its compatibility must not
    # describe the runtime CPU. Toolchain resolution owns that constraint.
    native.toolchain(
        name = name,
        exec_compatible_with = _LINUX_X86_64_EXEC_CONSTRAINTS,
        toolchain = ":" + implementation_name,
        toolchain_type = SCRIPT_RUNTIME_TOOLCHAIN_TYPE,
        visibility = visibility,
    )

def script_runtime_toolchain(ctx, exec_group = None):
    """Returns the script runtime selected in the requested execution scope."""
    if exec_group == None:
        toolchain = ctx.toolchains[SCRIPT_RUNTIME_TOOLCHAIN_TYPE]
    else:
        toolchain = ctx.exec_groups[exec_group].toolchains[SCRIPT_RUNTIME_TOOLCHAIN_TYPE]
    return toolchain.script_runtime
