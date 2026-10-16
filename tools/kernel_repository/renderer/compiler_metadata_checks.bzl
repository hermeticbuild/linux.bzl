"""Independent native compiler-fact checks; no fallback substitutions."""

load("@rules_cc//cc:action_names.bzl", "ACTION_NAMES")
load("@rules_cc//cc:find_cc_toolchain.bzl", "find_cc_toolchain", "use_cc_toolchain")
load("@rules_cc//cc/common:cc_common.bzl", "cc_common")
load(":architecture.bzl", "KernelArchitectureInfo")
load(":compiler_metadata.bzl", "KernelCompilerMetadataInfo")
load(":execution_tools.bzl", "EXECUTION_TOOLS_TYPE", "execution_tools", "run_recipe")
load(":source_tree.bzl", "KernelSourceTreeInfo")

def _impl(ctx):
    metadata = ctx.attr._metadata[KernelCompilerMetadataInfo]
    source = ctx.attr._source[KernelSourceTreeInfo]
    cc = find_cc_toolchain(ctx)
    if metadata.toolchain_id != cc.toolchain_id:
        fail("Metadata check and compiler must select the same C++ toolchain")
    features = cc_common.configure_features(ctx = ctx, cc_toolchain = cc)
    flags = cc_common.get_memory_inefficient_command_line(
        feature_configuration = features,
        action_name = ACTION_NAMES.c_compile,
        variables = cc_common.create_compile_variables(cc_toolchain = cc, feature_configuration = features),
    )
    execution = execution_tools(ctx)
    spec = ctx.actions.declare_file(ctx.label.name + ".inputs.json")
    report = ctx.actions.declare_file(ctx.label.name + ".json")
    ctx.actions.write(spec, json.encode({
        "tools": metadata.tools,
        "flags": flags,
        "shell": execution.shell.path,
        "source": source.root.path,
        "builtin_include": metadata.builtin_include,
        "assembler_flags": ["-fintegrated-as"] if metadata.assembler == "LLVM" else [],
        "expected": {
            "compiler": ("Clang" if metadata.compiler == "clang" else "GCC") + " " + str(metadata.compiler_version),
            "assembler": metadata.assembler + " " + str(metadata.assembler_version),
            "linker": metadata.linker + " " + str(metadata.linker_version),
        },
    }))
    run_recipe(
        ctx,
        execution,
        command = 'python3 -I "%s" "%s" "%s"' % (ctx.file._runner.path, spec.path, report.path),
        inputs = depset([ctx.file._runner, spec], transitive = [cc.all_files, metadata.files, source.files]),
        outputs = [report],
        mnemonic = "KernelCompilerMetadataCheck",
        env = {"SRCARCH": ctx.attr._architecture[KernelArchitectureInfo].srcarch},
    )
    return [DefaultInfo(files = depset([report]))]

kernel_compiler_metadata_check = rule(implementation = _impl, fragments = ["cpp"], toolchains = use_cc_toolchain() + [EXECUTION_TOOLS_TYPE], attrs = {
    "_metadata": attr.label(default = "//:compiler_metadata", providers = [KernelCompilerMetadataInfo]),
    "_source": attr.label(default = "//:prepared_source", providers = [KernelSourceTreeInfo], cfg = "exec"),
    "_architecture": attr.label(default = "//:architecture", providers = [KernelArchitectureInfo]),
    "_runner": attr.label(default = "//rules:check_compiler_metadata.py", allow_single_file = True),
})

def _test_impl(ctx):
    reports = [f for f in ctx.attr.checks[DefaultInfo].files.to_list() if f.basename.endswith("compiler_metadata_check.json")]
    if not reports:
        fail("Expected independently validated compiler metadata reports")
    aliases = {"compiler-metadata/%d.json" % i: f for i, f in enumerate(reports)}
    script = ctx.actions.declare_file(ctx.label.name + ".sh")
    ctx.actions.write(script, "#!/bin/sh\nset -eu\n" + "\n".join(['test -s "$TEST_SRCDIR/$TEST_WORKSPACE/%s"' % p for p in aliases]) + "\n", is_executable = True)
    return [DefaultInfo(executable = script, runfiles = ctx.runfiles(symlinks = aliases))]

kernel_compiler_metadata_test = rule(implementation = _test_impl, test = True, attrs = {
    "checks": attr.label(mandatory = True),
})
