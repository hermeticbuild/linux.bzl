load("@bazel_skylib//lib:shell.bzl", "shell")
load(":expressions.bzl", "evaluate", "program", "source_index")

def make_setup(ctx, config, prepared, execution, tools, builtin_include, target_flags, host_compile_flags, host_link_flags, capabilities, requests, files, flag_program):
    tools = dict(tools)

    # Keep raw executable roles for action ownership, but give native probes
    # and recipes the selected compiler target even before Kbuild adds flags.
    cc_arguments = [tools["CC"]] + target_flags
    make_tools = dict(tools, CC = " ".join([shell.quote(arg) for arg in cc_arguments]))
    source = prepared.root.path
    sources = source_index([source + "/" + p for p in prepared.logical])
    sources["__execution_tools__"] = {n: f.path for n, f in execution.commands.items()}
    sources["__compiler_include__"] = builtin_include
    sources["__host_machine__"] = "x86_64"
    sources["__probe_environment__"] = dict(execution.environment)
    sources["__probe_environment__"].update(make_tools)
    sources["__probe_environment__"].update(ARCH = config.arch, SRCARCH = config.architecture.srcarch, SUBARCH = config.architecture.subarch, HEADER_ARCH = config.architecture.header_arch)
    sources["__requests__"] = requests
    sources["__capabilities__"] = dict(flag_program["capabilities"])
    sources["__capabilities__"].update(capabilities)
    sources["__includes__"] = flag_program["image_flag_programs"]
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
    initial.update(make_tools)
    initial["__command_line__"] = dict(make_tools, ARCH = config.arch)
    setup_sources = dict(sources)
    setup_sources["__includes__"] = dict(flag_program["image_setup_programs"])
    architecture_makefile = "arch/" + config.architecture.srcarch + "/Makefile"

    # Architecture rules and simple variables belong to this native include
    # position. Replaying the file after setup changes early compiler probes.
    setup_sources["__includes__"][architecture_makefile] = flag_program["image_programs"][architecture_makefile]
    context = program(flag_program["global"], initial, setup_sources)
    variables = context.variables

    # Selected tools are command-line inputs to native Kbuild, whose ordinary
    # Makefile assignments cannot replace them. Bind before derived exports.
    for name, tool in make_tools.items():
        variables[name] = {"value": tool}
    variables = program(flag_program["image_utilities"], variables, sources, cells = True).variables
    lz4 = evaluate([{"op": "reference", "name": [{"op": "literal", "value": "LZ4"}]}], variables, sources).strip()
    if lz4 not in ("lz4", "lz4c"):
        fail("Unsupported source-selected LZ4 command: " + lz4)
    tools["LZ4"] = execution.commands[lz4].path
    make_tools["LZ4"] = tools["LZ4"]
    variables["LZ4"] = {"value": tools["LZ4"]}
    variables = program(flag_program["image_version"], variables, sources, cells = True).variables
    variables = program(flag_program["image_cpp"], variables, sources, cells = True).variables

    link_sources = dict(sources)
    link_sources["__includes__"] = flag_program["image_link_programs"]
    variables = program(flag_program["image_link_global"], variables, link_sources, cells = True).variables
    release = ".".join([evaluate([{"op": "reference", "name": [{"op": "literal", "value": name}]}], variables, sources) for name in ["VERSION", "PATCHLEVEL", "SUBLEVEL"]]) + evaluate([{"op": "reference", "name": [{"op": "literal", "value": "EXTRAVERSION"}]}], variables, sources) + config.values.get("LOCALVERSION", "")
    for name, val in {"KERNELRELEASE": release, "KBUILD_BUILD_VERSION": "1", "KBUILD_BUILD_TIMESTAMP": "Thu Jan 1 00:00:00 UTC 1970"}.items():
        variables[name] = {"value": val}
    variables = program(flag_program["image_exports"], variables, sources, cells = True).variables

    # Early export directives precede module dependencies such as the native
    # sub_make_done assignment, which can change their invocation guards.
    variables = program(flag_program["image_module_variables"], variables, sources, cells = True).variables
    for name in list(tools) + ["LLVM", "LLVM_IAS", "KBUILD_BUILD_VERSION", "KBUILD_BUILD_TIMESTAMP"]:
        variables["__exports__"][name] = True
    setup_file = ctx.actions.declare_file("setup.json")
    observed = {name: evaluate([dict(op = "reference", name = [dict(op = "literal", value = name)])], variables, sources) for name in ["KBUILD_CFLAGS", "KBUILD_AFLAGS", "KBUILD_CPPFLAGS", "KBUILD_LDFLAGS", "KBUILD_HOSTCFLAGS", "KBUILD_HOSTLDFLAGS"]}
    ctx.actions.write(setup_file, json.encode(dict(tools = tools, architecture = config.arch, flags = observed)))
    return struct(
        program = flag_program,
        file = setup_file,
        identity = config.identity,
        tools = tools,
        make_tools = make_tools,
        cc_arguments = cc_arguments,
        variables = variables,
        sources = sources,
        files = files,
        host_compile_flags = host_compile_flags,
        host_link_flags = host_link_flags,
        architecture = config.architecture,
        architecture_rules = context.rules,
        architecture_assignments = context.target_assignments,
        host_architecture = config.architecture,
        driver_tools = "",
    )
