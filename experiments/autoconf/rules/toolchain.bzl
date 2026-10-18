load("@kernel_inputs//rules:flag_program.bzl", "FLAG_PROGRAM")
load(":expressions.bzl", "evaluate", "program", "source_index")

KernelToolchainInfo = provider("Kernel compiler and Make setup.", fields = [])

def make_setup(ctx, config, prepared, execution, tools, builtin_include, _compile_flags, host_compile_flags, host_link_flags, capabilities, requests, files):
    source = prepared.root.path
    sources = source_index([source + "/" + p for p in prepared.logical])
    sources["__execution_tools__"] = {n: f.path for n, f in execution.commands.items()}
    sources["__compiler_include__"] = builtin_include
    sources["__host_machine__"] = "x86_64"
    sources["__probe_environment__"] = dict(execution.environment)
    sources["__probe_environment__"].update(tools)
    sources["__probe_environment__"].update(ARCH = config.arch, SRCARCH = config.architecture.srcarch, SUBARCH = config.architecture.subarch, HEADER_ARCH = config.architecture.header_arch)
    sources["__requests__"] = requests
    sources["__capabilities__"] = dict(FLAG_PROGRAM["capabilities"])
    sources["__capabilities__"].update(capabilities)
    sources["__includes__"] = FLAG_PROGRAM["image_flag_programs"]
    sources["__source_root__"] = source
    initial = {
        "__config__": config.make_values,
        "ARCH": config.arch,
        "SRCARCH": config.architecture.srcarch,
        "SUBARCH": config.architecture.subarch,
        "HEADER_ARCH": config.architecture.header_arch,
        "srctree": source,
        "srcroot": source,
        "objtree": "__object__",
        "LLVM": "1" if config.values.get("CC_IS_CLANG") == "y" else "",
        "LLVM_IAS": "1" if config.values.get("AS_IS_LLVM") == "y" else "0",
        "building_out_of_srctree": "1",
        "need-config": "1",
        "need-compiler": "1",
        "KCONFIG_CONFIG": "__config__",
    }
    initial.update(tools)
    initial["__command_line__"] = dict(tools, ARCH = config.arch)
    setup_sources = dict(sources)
    setup_sources["__includes__"] = FLAG_PROGRAM["image_setup_programs"]
    variables = program(FLAG_PROGRAM["global"], initial, setup_sources).variables

    # Selected tools are command-line inputs to native Kbuild, whose ordinary
    # Makefile assignments cannot replace them. Bind before derived exports.
    for name, tool in tools.items():
        variables[name] = {"value": tool}
    variables = program(FLAG_PROGRAM["image_utilities"], variables, sources, cells = True).variables
    variables = program(FLAG_PROGRAM["image_version"], variables, sources, cells = True).variables
    variables = program(FLAG_PROGRAM["image_cpp"], variables, sources, cells = True).variables

    link_sources = dict(sources)
    link_sources["__includes__"] = FLAG_PROGRAM["image_link_programs"]
    variables = program(FLAG_PROGRAM["image_link_global"], variables, link_sources, cells = True).variables
    release = ".".join([evaluate([{"op": "reference", "name": [{"op": "literal", "value": name}]}], variables, sources) for name in ["VERSION", "PATCHLEVEL", "SUBLEVEL"]]) + evaluate([{"op": "reference", "name": [{"op": "literal", "value": "EXTRAVERSION"}]}], variables, sources) + config.make_values.get("CONFIG_LOCALVERSION", "")
    for name, val in {"KERNELRELEASE": release, "KBUILD_BUILD_VERSION": "1", "KBUILD_BUILD_TIMESTAMP": "Thu Jan 1 00:00:00 UTC 1970"}.items():
        variables[name] = {"value": val}
    variables = program(FLAG_PROGRAM["image_exports"], variables, sources, cells = True).variables
    for name in list(tools) + ["LLVM", "LLVM_IAS", "KBUILD_BUILD_VERSION", "KBUILD_BUILD_TIMESTAMP"]:
        variables["__exports__"][name] = True
    setup_file = ctx.actions.declare_file("setup.json")
    observed = {name: evaluate([dict(op = "reference", name = [dict(op = "literal", value = name)])], variables, sources) for name in ["KBUILD_CFLAGS", "KBUILD_AFLAGS", "KBUILD_CPPFLAGS", "KBUILD_LDFLAGS", "KBUILD_HOSTCFLAGS", "KBUILD_HOSTLDFLAGS"]}
    ctx.actions.write(setup_file, json.encode(dict(tools = tools, architecture = config.arch, flags = observed)))
    return struct(
        file = setup_file,
        identity = config.identity,
        tools = tools,
        variables = variables,
        sources = sources,
        files = files,
        host_compile_flags = host_compile_flags,
        host_link_flags = host_link_flags,
        architecture = config.architecture,
        host_architecture = config.architecture,
        driver_tools = "",
    )
