"""Explicit compiler facts paired with the selected C++ toolchain."""

load("@bazel_skylib//rules/directory:providers.bzl", "DirectoryInfo")
load("@rules_cc//cc:action_names.bzl", "ACTION_NAMES")
load("@rules_cc//cc:find_cc_toolchain.bzl", "find_cc_toolchain", "use_cc_toolchain")
load("@rules_cc//cc/common:cc_common.bzl", "cc_common")

KernelCompilerMetadataInfo = provider(doc = "Kernel Compiler Metadata Info.", fields = ["toolchain_id", "compiler", "compiler_version", "assembler", "assembler_version", "linker", "linker_version", "tools", "tool_files", "files", "builtin_include", "capabilities", "driver_linker_aliases"])

def _impl(ctx):
    cc = find_cc_toolchain(ctx)
    if cc.toolchain_id not in ctx.attr.toolchain_ids:
        fail("Compiler metadata is not registered for selected C++ toolchain %s; select its matching metadata provider" % cc.toolchain_id)
    features = cc_common.configure_features(ctx = ctx, cc_toolchain = cc)
    selected = cc_common.get_tool_for_action(feature_configuration = features, action_name = ACTION_NAMES.c_compile)
    tools, tool_files, files = {}, {}, []
    for target, role in ctx.attr.tools.items():
        executable = target[DefaultInfo].files_to_run.executable
        if executable == None:
            candidates = target[DefaultInfo].files.to_list()
            if len(candidates) != 1:
                fail("Compiler metadata tool must expose one executable: %s" % target.label)
            executable = candidates[0]
        tools[role] = executable.path
        tool_files[role] = executable
        files.extend([target[DefaultInfo].files, target[DefaultInfo].default_runfiles.files])
    if tools.get("CC") != selected:
        fail("Compiler metadata describes %s, selected toolchain uses %s; select matching metadata" % (tools.get("CC"), selected))
    family = ctx.attr._compiler[config_common.FeatureFlagInfo].value
    if family != ctx.attr.compiler:
        fail("Compiler metadata family %s does not match selected compiler %s" % (ctx.attr.compiler, family))
    headers = ctx.attr.builtin_headers[DirectoryInfo]

    # A toolchain may expose its installed resource directory as one source
    # directory input. Adding individual children as well produces overlapping
    # sandbox inputs; retain only headers not already covered by cc.all_files.
    declared = {f.path: True for f in cc.all_files.to_list()}
    extra_headers = []
    for file in headers.transitive_files.to_list():
        parts = file.path.split("/")
        if not any(["/".join(parts[:i]) in declared for i in range(1, len(parts) + 1)]):
            extra_headers.append(file)
    files.append(depset(extra_headers))
    return [KernelCompilerMetadataInfo(
        toolchain_id = cc.toolchain_id,
        compiler = ctx.attr.compiler,
        compiler_version = ctx.attr.compiler_version,
        assembler = ctx.attr.assembler,
        assembler_version = ctx.attr.assembler_version,
        linker = ctx.attr.linker,
        linker_version = ctx.attr.linker_version,
        tools = tools,
        tool_files = tool_files,
        files = depset(transitive = files),
        builtin_include = headers.path,
        capabilities = ctx.attr.capabilities,
        driver_linker_aliases = ctx.attr.driver_linker_aliases,
    )]

kernel_compiler_metadata = rule(implementation = _impl, fragments = ["cpp"], toolchains = use_cc_toolchain(), attrs = {
    "compiler": attr.string(mandatory = True),
    "toolchain_ids": attr.string_list(mandatory = True),
    "compiler_version": attr.int(mandatory = True),
    "assembler": attr.string(mandatory = True),
    "assembler_version": attr.int(mandatory = True),
    "linker": attr.string(mandatory = True),
    "linker_version": attr.int(mandatory = True),
    "tools": attr.label_keyed_string_dict(cfg = "exec", allow_files = True, mandatory = True),
    "builtin_headers": attr.label(providers = [DirectoryInfo], mandatory = True),
    "capabilities": attr.string_list(),
    "driver_linker_aliases": attr.string_list(mandatory = True),
    "_compiler": attr.label(default = "@rules_cc//cc/compiler:compiler", providers = [config_common.FeatureFlagInfo]),
})

# Bazel callback signatures include inputs this implementation does not use.
# buildifier: disable=unused-variable
def _legacy_impl(ctx):
    # Retain the explicitly warned policy for older config-only prototypes.
    # Executable image generators replace this with a compiler-bound adapter.
    return [KernelCompilerMetadataInfo(
        compiler_version = None,
        assembler_version = None,
        linker_version = None,
        assembler = None,
        linker = None,
        capabilities = None,
    )]

legacy_compiler_metadata = rule(implementation = _legacy_impl)

def _exec_headers_impl(ctx):
    return [ctx.attr.directory[DirectoryInfo]]

exec_compiler_headers = rule(implementation = _exec_headers_impl, attrs = {
    "directory": attr.label(cfg = "exec", providers = [DirectoryInfo], mandatory = True),
})
