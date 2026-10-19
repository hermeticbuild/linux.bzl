"""Build the generic GNU Make syntax exporter from its declared source owner."""

load("@rules_cc//cc:find_cc_toolchain.bzl", "find_cc_toolchain", "use_cc_toolchain")
load("@rules_cc//cc/common:cc_common.bzl", "cc_common")

NativeMakeInfo = provider("GNU Make objects shared by the executable and bundled exporter.", fields = ["compilation_outputs"])

def _sources_impl(ctx):
    root = ctx.path(ctx.attr.source).dirname
    changed = {}
    for line in ctx.read(ctx.attr.patch).splitlines():
        if line.startswith("+++ b/"):
            path = line[len("+++ b/"):]
            if not path.startswith("src/") or "/../" in path:
                fail("Unexpected GNU Make AST patch path: " + path)
            changed[path] = True
    for directory in ["src", "lib"]:
        for source in root.get_child(directory).readdir():
            path = directory + "/" + source.basename
            if path in changed:
                # Patching must never write through a symlink into @make.
                ctx.file(path, ctx.read(source), executable = False)
            else:
                ctx.symlink(source, path)
    ctx.patch(ctx.attr.patch, strip = 1)
    ctx.symlink(root.get_child("COPYING"), "COPYING")
    ctx.file("BUILD.bazel", """package(default_visibility = ["//visibility:public"])
exports_files(["COPYING"])
filegroup(name = "sources", srcs = glob(["src/**", "lib/**"]))
""")

make_ast_sources = repository_rule(
    implementation = _sources_impl,
    attrs = {
        "source": attr.label(default = "@make//:MODULE.bazel"),
        "patch": attr.label(default = "//native:gnu-make-4.4.1.patch"),
    },
)

def _binary_impl(ctx):
    original = ctx.file.original_anchor.dirname + "/"
    prepared = ctx.file.prepared_anchor.dirname + "/"
    replacements = {file.path[len(prepared):]: file for file in ctx.files.prepared}
    sources, headers, generated_includes = [], list(ctx.files.exports), []
    for file in ctx.files.inputs + ctx.files.glob_inputs:
        if file.is_source:
            file = replacements[file.path[len(original):]]
        else:
            generated_includes.append(file.dirname)
        if file.extension == "c":
            sources.append(file)
        else:
            headers.append(file)
    toolchain = find_cc_toolchain(ctx)
    features = cc_common.configure_features(ctx = ctx, cc_toolchain = toolchain, requested_features = ctx.features, unsupported_features = ctx.disabled_features)
    _, objects = cc_common.compile(
        actions = ctx.actions,
        name = ctx.label.name,
        cc_toolchain = toolchain,
        feature_configuration = features,
        srcs = sources,
        public_hdrs = headers,
        includes = [prepared + "src", prepared + "lib", ctx.files.exports[0].dirname] + generated_includes,
        defines = ["HAVE_CONFIG_H", 'LIBDIR="/usr/local/lib"', 'INCLUDEDIR="/usr/local/include"', 'LOCALEDIR="/usr/local/share/locale"'],
        user_compile_flags = ["-std=gnu11", "-w"],
    )
    linked = cc_common.link(
        actions = ctx.actions,
        name = ctx.label.name,
        cc_toolchain = toolchain,
        feature_configuration = features,
        compilation_outputs = objects,
        user_link_flags = ["-ldl"] if ctx.target_platform_has_constraint(ctx.attr._linux[platform_common.ConstraintValueInfo]) else [],
        output_type = "executable",
    )
    return [DefaultInfo(executable = linked.executable, files = depset([linked.executable])), NativeMakeInfo(compilation_outputs = objects)]

make_ast = rule(
    implementation = _binary_impl,
    executable = True,
    fragments = ["cpp"],
    toolchains = use_cc_toolchain(),
    attrs = {
        "inputs": attr.label(default = "@make//:posix_parser_inputs"),
        "glob_inputs": attr.label_list(),
        "prepared": attr.label(default = "@make_ast_sources//:sources"),
        "original_anchor": attr.label(default = "@make//:MODULE.bazel", allow_single_file = True),
        "prepared_anchor": attr.label(default = "@make_ast_sources//:COPYING", allow_single_file = True),
        "exports": attr.label_list(allow_files = [".inc"], mandatory = True),
        "_linux": attr.label(default = "@platforms//os:linux"),
    },
)
