"""Build the resolver's host library and public headers from its kernel source."""

def build_libbpf(ctx, prepared, host, libraries, package_command, builder, support_files):
    """Record the native static-library build in a measured kernel callback.

    The caller selects the execution-platform compiler and library contexts,
    supplies their artifact closure, and invokes this only for a BTF consumer.
    `headers` uses the callback's existing virtual-directory transport.
    """
    execution = ctx.execution
    archive = ctx.actions.declare_file("libbpf/libbpf.a")
    headers = ctx.actions.declare_directory("libbpf/headers")
    spec = ctx.actions.declare_file("libbpf/build.json")
    ctx.actions.write(spec, json.encode(dict(
        source = prepared.root.path,
        roles = host.roles,
        compiler = host.spec,
        library_cflags = libraries.cflags,
        library_flags = libraries.libs,
        library_files = [file.path for file in libraries.files.to_list()],
        package_command = package_command.path,
        tools = {name: file.path for name, file in execution.commands.items()},
        bin = execution.bin_dir,
        environment = execution.host_compile_env,
        path_environment = execution.path_environment,
    )))
    ctx.actions.run(
        executable = execution.python,
        arguments = ["-B", builder.path, spec.path, archive.path, headers.path],
        inputs = depset([builder, package_command, spec], transitive = [prepared.files, execution.files, libraries.files, support_files]),
        outputs = [archive, headers],
        env = dict(execution.host_compile_env, PYTHONPATH = builder.dirname.rpartition("/")[0] or ".", PYTHONDONTWRITEBYTECODE = "1"),
        mnemonic = "KernelLibbpf",
        progress_message = "Building source-native host libbpf and public headers",
    )
    return struct(archive = archive, headers = headers, include = headers.path + "/include", files = depset([archive, headers]))
