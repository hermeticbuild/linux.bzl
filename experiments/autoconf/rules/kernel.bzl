"""Autoconf-driven complete kernel artifact graph, planned in Starlark."""

load("@autoconf.bzl//:autoconf.bzl", "AutoconfInfo", "read_results")
load("@kernel_inputs//rules:catalog.bzl", "ACTIONS")
load("@kernel_inputs//rules:data.bzl", "PROFILE")
load("@kernel_inputs//rules:flag_program.bzl", "FLAG_PROGRAM")
load("@kernel_inputs//rules:sources.bzl", "SOURCES")
load("@rules_cc//cc:find_cc_toolchain.bzl", "use_cc_toolchain")
load(":compiler.bzl", "KernelCompilerInfo")
load(":compiler_inputs.bzl", "bind_compiler_inputs")
load(":host_tools.bzl", "build_objtool")
load(":image.bzl", "build_image")
load(":kconfig_link.bzl", "link_sources")
load(":runtime.bzl", "context", "read_payload", "writer")
load(":schedule.bzl", "schedule", "write_payload")
load(":solver.bzl", "resolve_options")
load(":toolchain.bzl", "make_setup")

CC_TOOLCHAIN = "@rules_cc//cc:toolchain_type"

# Fixed map_directory callback signature.
# buildifier: disable=unused-variable
def _expand(tctx, input_directories, output_directories, tools, additional_inputs, additional_params):
    measured = read_payload(input_directories["answers"])
    if measured.get("converged", False) and not additional_params["final"]:
        write = writer(tctx, tools["python"], tools["emit"], output_directories["scratch"])
        manifest = tctx.declare_file("requests.json", directory = output_directories["kernel"])
        tc = json.decode(additional_params["toolchain"])
        write(manifest, json.encode(dict(previous = additional_params["previous"], requests = {}, cc = tc["cc"], ld = additional_params["ld"], shell = "/bin/sh")))
        return
    sources = {name: json.decode(body) for name, body in SOURCES.items()}
    for patch in measured["patches"]:
        if "nodes" in patch:
            sources[patch["file"]] = patch["nodes"]
        else:
            sources[patch["file"]][patch["index"]] = patch["node"]
    arch = struct(**json.decode(additional_params["architecture"]))
    records = link_sources(sources, arch)
    custom = read_results(input_directories["custom"]) if "custom" in input_directories else {}
    for encoded in json.decode(additional_params["extra_records"]):
        record = json.decode(encoded)
        records.append(dict(record, order = len(records) + 1, evaluation_rank = len(records) + 1, unit = record["symbol"]))
    custom_inputs = {name: ["y" if value else "n", "tristate"] if type(value) == "bool" else [str(value), "int" if type(value) == "int" else "string"] for name, value in custom.items()}
    records = bind_compiler_inputs(records, custom_inputs)
    choice_members = {r["symbol"]: True for r in records if r.get("parent_choice", 0)}
    requested = {name: value for name, value in PROFILE["base"].items() if additional_params["mode"] != "qualification" or name not in choice_members}
    if additional_params["mode"] not in ["allno", "qualification"]:
        requested.update(PROFILE["requested"])
    elif additional_params["mode"] == "allno":
        requested["64BIT"] = "y"
    requested.update(json.decode(additional_params["requested"]))
    config = resolve_options(records, requested, list(requested))

    # buildifier: disable=print
    print("Kernel Kconfig:", len(config.values), "symbols;", len(measured["capabilities"]), "measured Kbuild answers; final =", additional_params["final"])
    commands = {name.removeprefix("cmd_"): file for name, file in tools.items() if name.startswith("cmd_")}
    tc = json.decode(additional_params["toolchain"])
    originals = {f.path: f for f in additional_inputs["originals"].to_list()}
    prepared = struct(root = tools["source_root"], files = depset([tools["source_root"]]), logical = {p: originals[path] for p, path in json.decode(additional_params["logical"]).items()})
    host_roles = json.decode(additional_params["host_roles"])
    runtime_tools = dict(tools, commands = commands, host_cc = host_roles["HOSTCC"], host_cxx = host_roles["HOSTCXX"], target_cc = tc["cc"], environment = tc["compile_env"], host_compile_env = json.decode(additional_params["host_compile_env"]), host_link_env = json.decode(additional_params["host_link_env"]))
    params = struct(python = tools["python"], executor = tools["executor"], materialize = tools["materialize"], link_script = tools["link_script"])
    planning = not additional_params["final"]
    recorded = [] if additional_params["final"] and not additional_params["config_only"] else None
    ctx = context(tctx, output_directories, runtime_tools, params, planning = planning, recorded = recorded)

    def file(name):
        return ctx.actions.declare_file(name)

    dotconfig, header, report = file(".config"), file("include/generated/autoconf.h"), file("config-report.json")
    ctx.actions.write(dotconfig, config.config)
    ctx.actions.write(header, config.header)
    ctx.actions.write(report, json.encode(dict(values = config.values, compiler = measured["compiler"], patches = len(measured["patches"]), probes = len(measured["capabilities"]), toolchain = measured["toolchain"], requested = requested, request_order = list(requested), raw_requested = json.decode(additional_params["requested"]), mode = additional_params["mode"])))
    if additional_params["config_only"]:
        return
    types = {r["symbol"]: r["type"] for r in records if r["symbol"]}
    written = dict([line.split("=", 1) for line in config.config.splitlines() if line.startswith("CONFIG_")])
    make_values = {name: json.decode(value) if types.get(name.removeprefix("CONFIG_")) == "string" else value for name, value in written.items()}
    conf = struct(
        values = config.values,
        make_values = make_values,
        architecture = arch,
        arch = arch.arch,
        identity = "kernel",
        config = dotconfig,
        header = header,
        report = report,
    )
    role_tools = json.decode(additional_params["roles"])
    role_tools.update(json.decode(additional_params["host_roles"]))
    for name, tool in {"CONFIG_SHELL": "sh", "BASH": "bash", "PYTHON3": "python3", "PERL": "perl", "AWK": "awk", "LEX": "flex", "YACC": "bison", "KGZIP": "gzip", "KBZIP2": "bzip2", "KLZMA": "lzma", "LZOP": "lzop", "XZ": "xz", "ZSTD": "zstd"}.items():
        if tool in commands:
            role_tools[name] = commands[tool].path
    requests = {}
    all_files = depset([tools["source_root"]], transitive = [tools["execution_files"], tools["compiler_files"]])
    setup = make_setup(ctx, conf, prepared, ctx.execution, role_tools, additional_params["builtin_include"], tc["compile_flags"], json.decode(additional_params["host_compile_flags"]), json.decode(additional_params["host_link_flags"]), measured["capabilities"], requests, all_files)
    programs = {p.rpartition("/")[0] or ".": body for p, body in FLAG_PROGRAM["image_programs"].items() if p.rsplit("/", 1)[-1] == "Makefile"}
    programs.update({p.rpartition("/")[0] or ".": body for p, body in FLAG_PROGRAM["image_programs"].items() if p.rsplit("/", 1)[-1] == "Kbuild"})
    catalog = struct(actions = ACTIONS, programs = programs, aliases = {out: p for p, d in ACTIONS.items() for out in d["outs"]})
    if not (planning and requests):
        objtool = build_objtool(ctx, prepared, arch, json.decode(additional_params["host_compile_flags"]), json.decode(additional_params["host_link_flags"]), measured["host_capabilities"]) if config.values.get("OBJTOOL") == "y" else struct(executable = None)
        build_image(ctx, conf, setup, prepared, catalog, objtool)

    # buildifier: disable=print
    print("Kernel probe requests:", len(requests))
    if not planning and requests:
        fail("Kbuild probing did not converge: " + str(requests.keys()))
    if recorded != None:
        write_payload(tctx, tools["python"], tools["emit"], output_directories["schedule"], recorded)
    if planning:
        write = writer(tctx, tools["python"], tools["emit"], output_directories["scratch"])
        manifest = tctx.declare_file("requests.json", directory = output_directories["kernel"])
        write(manifest, json.encode(dict(
            previous = additional_params["previous"],
            requests = requests,
            config = config.config,
            header = config.header,
            cc = tc["cc"],
            ld = additional_params["ld"],
            shell = "/bin/sh",
        )))

def _impl(ctx):
    compiler = ctx.attr.compiler[KernelCompilerInfo]
    host = ctx.attr.host_compiler[KernelCompilerInfo]
    tc = compiler.context
    commands = {f.basename: f for f in ctx.attr._tools[DefaultInfo].files.to_list()}
    architecture = None
    for cpu, arch, srcarch, bits in [("x86_64", "x86_64", "x86", 64), ("aarch64", "arm64", "arm64", 64)]:
        if ctx.target_platform_has_constraint(getattr(ctx.attr, "_" + cpu)[platform_common.ConstraintValueInfo]):
            architecture = dict(cpu = cpu, arch = arch, srcarch = srcarch, header_arch = srcarch, subarch = srcarch, bits = bits)
    if architecture == None:
        fail("The measured kernel adapter currently supports x86-64 and ARM64 platforms")
    originals = ctx.attr._source[DefaultInfo].files
    anchor = ctx.file._anchor
    prefix = anchor.dirname + "/"
    logical = {f.path[len(prefix):]: f.path for f in originals.to_list() if f.path.startswith(prefix)}
    prepared = ctx.actions.declare_directory(ctx.label.name + ".sources")
    manifest = ctx.actions.declare_file(ctx.label.name + ".source-inputs.json")
    ctx.actions.write(manifest, json.encode(dict(sources = logical, commands = {n: f.path for n, f in commands.items()})))
    ctx.actions.run(
        executable = commands["python3"],
        arguments = [ctx.file._prepare.path, manifest.path, prepared.path],
        inputs = depset([manifest, ctx.file._prepare], transitive = [originals, ctx.attr._tools[DefaultInfo].files]),
        outputs = [prepared],
        mnemonic = "KernelSource",
    )
    answers = ctx.actions.declare_directory(ctx.label.name + ".kconfig-probes")
    spec = ctx.actions.declare_file(ctx.label.name + ".kconfig-inputs.json")
    ctx.actions.write(spec, json.encode(dict(
        source = prepared.path,
        baseline = ctx.file._baseline.path,
        syntax = ctx.file._syntax.path,
        cc = tc.spec["cc"],
        ld = compiler.roles["LD"],
        host_programs = ctx.file._host_programs.path,
        architecture = architecture,
        roles = compiler.roles,
        host_roles = host.roles,
        compile_env = tc.spec["compile_env"],
        host_compile_flags = host.context.spec["compile_flags"],
        host_compile_env = host.context.spec["compile_env"],
        tool_environment = {"BINDGEN": "false", "HOSTRUSTC": "false"},
        compile_flags = tc.spec["compile_flags"] + compiler.assembler_flags,
    )))
    ctx.actions.run(
        executable = commands["python3"],
        arguments = [ctx.file._probe.path, "kconfig", spec.path, answers.path],
        inputs = depset([prepared, spec, ctx.file._baseline, ctx.file._syntax, ctx.file._probe, ctx.file._host_programs], transitive = [compiler.files, host.files, ctx.attr._tools[DefaultInfo].files]),
        outputs = [answers],
        tools = compiler.tools + host.tools,
        env = {"PATH": "/usr/bin:/bin"},
        mnemonic = "KernelKconfigProbe",
        toolchain = CC_TOOLCHAIN,
    )
    base_tools = {"cmd_" + n: f for n, f in commands.items()}
    compiler_tools = {"compiler_tool_%d" % i: tool for i, tool in enumerate(compiler.tools + host.tools)}
    base_tools.update(compiler_tools)
    base_tools.update(dict(
        python = commands["python3"],
        execution_files = ctx.attr._tools[DefaultInfo].files,
        compiler_files = depset(transitive = [compiler.files, host.files]),
        source_root = prepared,
        emit = ctx.file._emit,
        executor = ctx.file._executor,
        materialize = ctx.file._materialize,
        link_script = ctx.file._link_script,
    ))
    if ctx.attr.rounds < 0:
        fail("rounds must be nonnegative")
    output, metadata = None, None
    for round in range(ctx.attr.rounds + 1):
        final = round == ctx.attr.rounds
        output = ctx.actions.declare_directory(ctx.label.name + ((".tree" if ctx.attr.config_only else ".graph") if final else ".plan%d" % round))
        scratch = ctx.actions.declare_directory(ctx.label.name + ".scratch%d" % round)
        input_directories = {"answers": answers}
        if ctx.attr.probes:
            probes = ctx.attr.probes[AutoconfInfo]
            if probes.toolchain_id != tc.id or probes.toolchain != tc.spec:
                fail("Custom probes must use the same configured toolchain as the kernel")
            input_directories["custom"] = probes.results
        metadata = ctx.actions.declare_directory(ctx.label.name + ".schedule%d" % round)
        ctx.actions.map_directory(
            input_directories = input_directories,
            output_directories = {"kernel": output, "scratch": scratch, "schedule": metadata},
            additional_inputs = {"originals": originals},
            tools = base_tools,
            implementation = _expand,
            mnemonic = "KernelConfig" if ctx.attr.config_only else "KernelArtifact" if final else "KernelPlan",
            additional_params = dict(
                final = final,
                config_only = ctx.attr.config_only,
                mode = ctx.attr.mode,
                requested = json.encode(ctx.attr.requested),
                extra_records = json.encode(ctx.attr.extra_records),
                previous = answers.path,
                logical = json.encode(logical),
                toolchain = json.encode(tc.spec),
                architecture = json.encode(architecture),
                roles = json.encode(compiler.roles),
                host_roles = json.encode({"HOSTCC": host.roles["CC"], "HOSTCXX": host.roles["CXX"]}),
                host_compile_flags = json.encode(host.context.spec["compile_flags"]),
                host_compile_env = json.encode(host.context.spec["compile_env"]),
                host_link_env = json.encode(host.context.spec["link_env"]),
                host_link_flags = json.encode(host.context.spec["link_flags"]),
                builtin_include = compiler.builtin_include,
                ld = compiler.roles["LD"],
            ),
            toolchain = CC_TOOLCHAIN,
        )
        if not final:
            next_answers = ctx.actions.declare_directory(ctx.label.name + ".answers%d" % round)
            ctx.actions.run(
                executable = commands["python3"],
                arguments = [ctx.file._probe.path, "kbuild", output.path + "/requests.json", next_answers.path],
                inputs = depset([output, answers, ctx.file._probe, prepared], transitive = [compiler.files, host.files, ctx.attr._tools[DefaultInfo].files]),
                outputs = [next_answers],
                tools = compiler.tools + host.tools,
                env = {"PATH": "/usr/bin:/bin"},
                mnemonic = "KernelKbuildProbe",
                toolchain = CC_TOOLCHAIN,
            )
            answers = next_answers
    if not ctx.attr.config_only:
        output = schedule(
            ctx,
            output,
            metadata,
            prepared,
            originals,
            anchor.dirname,
            dict(compiler_tools, python = commands["python3"], emit = ctx.file._emit, executor = ctx.file._executor, runner = ctx.file._scheduler, materialize = ctx.file._materialize, link_script = ctx.file._link_script),
            depset(transitive = [compiler.files, host.files]),
            ctx.attr._tools[DefaultInfo].files,
            steps = ctx.attr.schedule_steps,
        )
    return [DefaultInfo(files = depset([output]))]

kernel_build = rule(implementation = _impl, fragments = ["cpp"], toolchains = use_cc_toolchain(), attrs = {
    "compiler": attr.label(default = "//:compiler", providers = [KernelCompilerInfo]),
    "host_compiler": attr.label(default = "//:compiler", cfg = "exec", providers = [KernelCompilerInfo]),
    "_x86_64": attr.label(default = "@platforms//cpu:x86_64"),
    "_aarch64": attr.label(default = "@platforms//cpu:aarch64"),
    "rounds": attr.int(default = 64),
    "schedule_steps": attr.int(default = 32),
    "_scheduler": attr.label(default = "//:scheduler.py", allow_single_file = True),
    "config_only": attr.bool(default = False),
    "mode": attr.string(default = "minimal"),
    "requested": attr.string_dict(),
    "probes": attr.label(providers = [AutoconfInfo]),
    "extra_records": attr.string_list(),
    "_tools": attr.label(default = "@host_tools//:tools"),
    "_source": attr.label(default = "@host_tools//:linux"),
    "_anchor": attr.label(default = "@host_tools//:linux/Makefile", allow_single_file = True),
    "_prepare": attr.label(default = "//rules:prepare_source.py", allow_single_file = True),
    "_probe": attr.label(default = "//:probe_runtime.py", allow_single_file = True),
    "_syntax": attr.label(default = "@kernel_inputs//native:syntax", allow_single_file = True),
    "_host_programs": attr.label(default = "@kernel_inputs//:host-programs.json", allow_single_file = True),
    "_baseline": attr.label(default = "@kernel_inputs//:baseline.json", allow_single_file = True),
    "_emit": attr.label(default = "//:emit.py", allow_single_file = True),
    "_executor": attr.label(default = "//:execute.py", allow_single_file = True),
    "_materialize": attr.label(default = "//rules:materialize.py", allow_single_file = True),
    "_link_script": attr.label(default = "//rules:link-vmlinux.sh", allow_single_file = True),
})
