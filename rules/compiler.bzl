"""Execution tools bound to the selected C/C++ toolchain, without probe answers."""

load("@autoconf.bzl//:autoconf.bzl", "toolchain_context")
load("@bazel_skylib//rules/directory:providers.bzl", "DirectoryInfo")
load("@rules_cc//cc:action_names.bzl", "ACTION_NAMES")
load("@rules_cc//cc:find_cc_toolchain.bzl", "find_cc_toolchain", "use_cc_toolchain")
load("@rules_cc//cc/common:cc_common.bzl", "cc_common")

KernelCompilerInfo = provider("Selected compiler commands and their artifact closure.", fields = ["context", "roles", "files", "tools", "builtin_include", "target_flags", "assembler_flags"])

def _impl(ctx):
    tc = toolchain_context(ctx)
    cc = find_cc_toolchain(ctx)
    features = cc_common.configure_features(ctx = ctx, cc_toolchain = cc, requested_features = ctx.features, unsupported_features = ctx.disabled_features)
    roles = {
        "CC": tc.spec["cc"],
        "AS": cc_common.get_tool_for_action(feature_configuration = features, action_name = ACTION_NAMES.assemble),
        "CXX": cc_common.get_tool_for_action(feature_configuration = features, action_name = ACTION_NAMES.cpp_compile),
    }
    for role, field in {"LD": "ld_executable", "AR": "ar_executable", "NM": "nm_executable", "OBJCOPY": "objcopy_executable", "OBJDUMP": "objdump_executable", "STRIP": "strip_executable"}.items():
        value = getattr(cc, field, None)
        if value:
            roles[role] = value
    files, tools = [tc.files], []
    for target, role in ctx.attr.tools.items():
        info = target[DefaultInfo]
        executable = info.files_to_run.executable
        if executable == None:
            candidates = info.files.to_list()
            if len(candidates) != 1:
                fail("Compiler role %s must provide one executable" % role)
            executable = candidates[0]
        if role == "CC" and executable.path != tc.spec["cc"]:
            fail("Compiler roles describe %s, selected toolchain uses %s" % (executable.path, tc.spec["cc"]))
        roles[role] = executable.path
        files.extend([info.files, info.default_runfiles.files])
        if info.files_to_run.executable:
            tools.append(info.files_to_run)
    for role in ["CC", "CXX", "LD", "AR", "NM", "OBJCOPY", "OBJDUMP", "READELF", "STRIP"]:
        if role not in roles:
            fail("Selected compiler needs an explicit %s executable label" % role)
    builtin = cc.built_in_include_directories[0] if cc.built_in_include_directories else ""
    if ctx.attr.builtin_headers:
        headers = ctx.attr.builtin_headers[DirectoryInfo]
        builtin = headers.path
        files.append(headers.transitive_files)
    closure = depset(transitive = files).to_list()
    directories = [f.path + "/" for f in closure if f.is_directory]
    closure = depset([f for f in closure if not any([f.path.startswith(prefix) for prefix in directories])])
    return [KernelCompilerInfo(context = tc, roles = roles, files = closure, tools = tools, builtin_include = builtin, target_flags = ctx.attr.target_flags, assembler_flags = ctx.attr.assembler_flags)]

kernel_compiler = rule(implementation = _impl, fragments = ["cpp"], toolchains = use_cc_toolchain(), attrs = {
    "tools": attr.label_keyed_string_dict(cfg = "exec", allow_files = True),
    "target_flags": attr.string_list(doc = "Compiler target selection, preceding source-owned Kbuild flags."),
    "assembler_flags": attr.string_list(),
    "builtin_headers": attr.label(cfg = "exec", providers = [DirectoryInfo]),
})
