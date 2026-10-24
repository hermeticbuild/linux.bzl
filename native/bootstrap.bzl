"""Declared native builds and packaging for the standalone repository generator."""

load("@rules_cc//cc:find_cc_toolchain.bzl", "find_cc_toolchain", "use_cc_toolchain")
load("@rules_cc//cc/common:cc_common.bzl", "cc_common")
load("@rules_cc//cc/common:cc_info.bzl", "CcInfo")
load(":make_ast.bzl", "NativeMakeInfo")

NativeParserInfo = provider(
    "Namespaced GNU Make objects for the isolated exporter.",
    fields = ["compilation_outputs"],
)

def _features(ctx, toolchain):
    return cc_common.configure_features(
        ctx = ctx,
        cc_toolchain = toolchain,
        requested_features = ctx.features,
        unsupported_features = ctx.disabled_features,
    )

def _namespace(ctx, compilation_outputs):
    inputs = depset(compilation_outputs.objects + compilation_outputs.pic_objects).to_list()
    outputs = {source: ctx.actions.declare_file(ctx.label.name + "/" + str(i) + ".o") for i, source in enumerate(inputs)}
    specification = ctx.actions.declare_file(ctx.label.name + ".namespace.json")
    ctx.actions.write(specification, json.encode({
        "nm": ctx.file._nm.path,
        "objcopy": ctx.file._objcopy.path,
        "namespace": ctx.attr.namespace,
        "symbol_prefix": "_" if ctx.target_platform_has_constraint(ctx.attr._macos[platform_common.ConstraintValueInfo]) else "",
        "objects": [[source.path, output.path] for source, output in outputs.items()],
    }))
    ctx.actions.run(
        executable = ctx.executable._builder,
        arguments = ["namespace", specification.path],
        env = ctx.attr._builder[RunEnvironmentInfo].environment,
        inputs = [specification, ctx.file._nm, ctx.file._objcopy] + inputs,
        tools = [ctx.attr._builder[DefaultInfo].files_to_run],
        outputs = outputs.values(),
        mnemonic = "NamespaceNativeParser",
    )

    # The linker selects PIC or non-PIC objects from its feature configuration.
    # Namespacing must preserve both collections, including an empty collection.
    renamed = cc_common.create_compilation_outputs(
        objects = depset([outputs[source] for source in compilation_outputs.objects]),
        pic_objects = depset([outputs[source] for source in compilation_outputs.pic_objects]),
    )
    return [DefaultInfo(files = depset(outputs.values())), NativeParserInfo(compilation_outputs = renamed)]

def _make_parser_impl(ctx):
    return _namespace(ctx, ctx.attr.make[NativeMakeInfo].compilation_outputs)

make_parser = rule(
    implementation = _make_parser_impl,
    attrs = {
        "make": attr.label(default = "//native:make_ast", providers = [NativeMakeInfo]),
        "namespace": attr.string(default = "kernel_make_"),
        "_builder": attr.label(default = "//:bootstrap_builder", executable = True, cfg = "exec"),
        "_nm": attr.label(default = "@llvm//tools:llvm-nm", allow_single_file = True, cfg = "exec"),
        "_objcopy": attr.label(default = "@llvm//tools:llvm-objcopy", allow_single_file = True, cfg = "exec"),
        "_macos": attr.label(default = "@platforms//os:macos"),
    },
)

def _exporter_impl(ctx):
    toolchain = find_cc_toolchain(ctx)
    features = _features(ctx, toolchain)
    macos = ctx.target_platform_has_constraint(ctx.attr._macos[platform_common.ConstraintValueInfo])
    payload = ctx.actions.declare_file(ctx.label.name + ".zip")
    specification = ctx.actions.declare_file(ctx.label.name + ".payload.json")
    ctx.actions.write(specification, json.encode({
        "output": payload.path,
        "python_metadata": ctx.file.python_metadata.path,
        "make_module": ctx.file.make_module.path,
        "stdlib": [file.path for file in ctx.files.stdlib],
        "renderer": [file.path for file in ctx.files.renderer],
        "license_files": [file.path for file in ctx.files.license_files],
    }))
    ctx.actions.run(
        executable = ctx.executable._builder,
        arguments = ["payload", specification.path],
        env = ctx.attr._builder[RunEnvironmentInfo].environment,
        inputs = [specification, ctx.file.python_metadata, ctx.file.make_module] + ctx.files.stdlib + ctx.files.renderer + ctx.files.license_files,
        tools = [ctx.attr._builder[DefaultInfo].files_to_run],
        outputs = [payload],
        mnemonic = "PackageGeneratorResources",
    )
    _, launcher = cc_common.compile(
        actions = ctx.actions,
        name = ctx.label.name,
        cc_toolchain = toolchain,
        feature_configuration = features,
        srcs = [ctx.file.launcher],
        compilation_contexts = [ctx.attr.python[CcInfo].compilation_context],
    )
    linked = cc_common.link(
        actions = ctx.actions,
        name = ctx.label.name + "_unbundled",
        cc_toolchain = toolchain,
        feature_configuration = features,
        compilation_outputs = cc_common.merge_compilation_outputs(compilation_outputs = [
            ctx.attr.make[NativeParserInfo].compilation_outputs,
            launcher,
        ]),
        linking_contexts = [ctx.attr.python[CcInfo].linking_context],
        output_type = "executable",
        link_deps_statically = True,
        # Mach-O signatures cover the linked payload. Appending bytes after the
        # final link would invalidate the executable's code-signing envelope.
        user_link_flags = [
            "-Wl,-sectcreate,__TEXT,__pythonzip," + payload.path,
            # ARM64 defaults to signing; require the same envelope on x86-64.
            "-Wl,-adhoc_codesign",
            # The C++ toolchain adds Foundation and libc++; this C executable
            # must retain only libraries referenced by the embedded runtime.
            "-Wl,-dead_strip_dylibs",
        ] if macos else ["-static"],
        additional_inputs = [payload] if macos else [],
    )
    output = ctx.actions.declare_file("kbuild-exporter")
    specification = ctx.actions.declare_file(ctx.label.name + ".bundle.json")
    ctx.actions.write(specification, json.encode({
        "binary": linked.executable.path,
        "output": output.path,
        "payload": payload.path,
        "python_metadata": ctx.file.python_metadata.path,
    }))
    ctx.actions.run(
        executable = ctx.executable._builder,
        arguments = ["bundle", specification.path],
        env = ctx.attr._builder[RunEnvironmentInfo].environment,
        inputs = [specification, linked.executable, payload, ctx.file.python_metadata],
        tools = [ctx.attr._builder[DefaultInfo].files_to_run],
        outputs = [output],
        mnemonic = "BundleKernelRepository",
    )
    return [DefaultInfo(executable = output, files = depset([output]))]

syntax_exporter = rule(
    implementation = _exporter_impl,
    executable = True,
    fragments = ["cpp"],
    toolchains = use_cc_toolchain(),
    attrs = {
        "launcher": attr.label(allow_single_file = True, default = "//:launcher.c"),
        "make": attr.label(providers = [NativeParserInfo], mandatory = True),
        "python": attr.label(default = "@bootstrap_python//:runtime", providers = [CcInfo]),
        "python_metadata": attr.label(default = "@bootstrap_python//:PYTHON.json", allow_single_file = True),
        "make_module": attr.label(default = "@make//:MODULE.bazel", allow_single_file = True),
        "stdlib": attr.label(default = "@bootstrap_python//:stdlib"),
        "renderer": attr.label_list(allow_files = True),
        "license_files": attr.label_list(allow_files = True),
        "_builder": attr.label(default = "//:bootstrap_builder", executable = True, cfg = "exec"),
        "_macos": attr.label(default = "@platforms//os:macos"),
    },
)

def _archive_impl(ctx):
    output = ctx.actions.declare_file(ctx.label.name + ".tar.gz")
    repository = ctx.actions.declare_directory(ctx.label.name)
    specification = ctx.actions.declare_file(ctx.label.name + ".json")
    ctx.actions.write(specification, json.encode(dict(
        tool = ctx.executable.exporter.path,
        repository = repository.path,
        output = output.path,
    )))
    ctx.actions.run(
        executable = ctx.executable._builder,
        arguments = ["archive", specification.path],
        env = ctx.attr._builder[RunEnvironmentInfo].environment,
        inputs = [specification, ctx.executable.exporter],
        tools = [ctx.attr._builder[DefaultInfo].files_to_run],
        outputs = [output, repository],
        mnemonic = "PackageKbuildExporter",
    )
    return [DefaultInfo(files = depset([output, repository]))]

syntax_exporter_archive = rule(
    implementation = _archive_impl,
    attrs = {
        "exporter": attr.label(executable = True, cfg = "target", mandatory = True),
        "_builder": attr.label(default = "//:bootstrap_builder", executable = True, cfg = "exec"),
    },
)

def python_input(name):
    return select({
        Label("//native:linux_aarch64_musl"): "@bootstrap_python_linux_aarch64//:" + name,
        Label("//native:linux_x86_64_musl"): "@bootstrap_python//:" + name,
        Label("//native:macos_aarch64"): "@bootstrap_python_macos//:" + name,
        Label("//native:macos_x86_64"): "@bootstrap_python_macos_x86_64//:" + name,
    }, no_match_error = "Exporter builds require Linux musl or macOS on aarch64 or x86_64")
