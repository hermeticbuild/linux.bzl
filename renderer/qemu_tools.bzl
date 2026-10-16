"""Optional emulator tools selected through rules_qemu's system toolchain."""

load("@bazel_skylib//rules:common_settings.bzl", "string_flag")
load(":qemu_guests.bzl", "QEMU_GUESTS")

KernelQemuInfo = provider(doc = "Kernel Qemu Info.", fields = ["spec", "files", "executable", "data"])
_FLAG = str(Label("//qemu_tools:guest"))
_TYPE = Label("@rules_qemu//qemu:exec_toolchain_type")

def _guest_transition_impl(_settings, attr):
    return {_FLAG: attr.guest}

_guest_transition = transition(implementation = _guest_transition_impl, inputs = [], outputs = [_FLAG])

def _tool_impl(ctx):
    tool = ctx.toolchains[_TYPE]
    spec = {
        "executable": tool.qemu_system.path,
        "data": tool.system_data_anchor.path,
        "system_target": tool.system_target,
        "target_arch": tool.target_arch,
        "machine": tool.machine,
        "accel": tool.accel,
    }
    manifest = ctx.actions.declare_file(ctx.label.name + ".json")
    ctx.actions.write(manifest, json.encode(spec))
    files = depset([manifest, tool.qemu_system, tool.system_data_anchor], transitive = [tool.system_data_files])
    return [
        KernelQemuInfo(spec = spec, files = files, executable = tool.qemu_system, data = tool.system_data_anchor),
        DefaultInfo(files = files, runfiles = ctx.runfiles(transitive_files = files)),
    ]

_kernel_qemu_tool = rule(implementation = _tool_impl, cfg = _guest_transition, toolchains = [_TYPE], attrs = {
    "guest": attr.string(mandatory = True),
    "_allowlist_function_transition": attr.label(default = "@bazel_tools//tools/allowlists/function_transition_allowlist"),
})

def _tools_impl(ctx):
    manifest = ctx.actions.declare_file(ctx.label.name + ".json")
    ctx.actions.write(manifest, json.encode({t.label.name: t[KernelQemuInfo].spec for t in ctx.attr.tools}))
    return [DefaultInfo(files = depset([manifest], transitive = [t[KernelQemuInfo].files for t in ctx.attr.tools]))]

_qemu_tools = rule(implementation = _tools_impl, attrs = {"tools": attr.label_list(providers = [KernelQemuInfo])})

# Fixed package fixtures or action helpers, not target-naming macros.
# buildifier: disable=unnamed-macro
def qemu_tools():
    string_flag(name = "guest", build_setting_default = "x86_64")
    for guest in QEMU_GUESTS:
        native.config_setting(name = "guest_" + guest, flag_values = {":guest": guest})
        _kernel_qemu_tool(name = guest, guest = guest, tags = ["manual"])
    _qemu_tools(name = "all_tools", tools = [":" + guest for guest in QEMU_GUESTS], tags = ["manual"])
