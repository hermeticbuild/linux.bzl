"""Autoconf-driven complete kernel artifact graph, planned in Starlark."""

load("@autoconf_prototype//:autoconf.bzl", "AutoconfInfo", "CC_TOOLCHAIN", "read_results", "toolchain_context")
load("@kernel_inputs//rules:catalog.bzl", "ACTIONS")
load("@kernel_inputs//rules:data.bzl", "PROFILE")
load("@kernel_inputs//rules:flag_program.bzl", "FLAG_PROGRAM")
load("@kernel_inputs//rules:sources.bzl", "SOURCES")
load("@rules_cc//cc:find_cc_toolchain.bzl", "find_cc_toolchain", "use_cc_toolchain")
load(":compiler_inputs.bzl", "bind_compiler_inputs")
load(":host_tools.bzl", "build_objtool")
load(":image.bzl", "build_image")
load(":kconfig_link.bzl", "link_sources")
load(":runtime.bzl", "context", "read_payload", "writer")
load(":solver.bzl", "resolve_options")
load(":toolchain.bzl", "make_setup")

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
    arch = struct(cpu = "x86_64", arch = "x86_64", srcarch = "x86", header_arch = "x86", subarch = "x86", bits = 64)
    records = link_sources(sources, arch)
    custom = read_results(input_directories["custom"]) if "custom" in input_directories else {}
    for encoded in json.decode(additional_params["extra_records"]):
        record = json.decode(encoded)
        records.append(dict(record, order = len(records) + 1, evaluation_rank = len(records) + 1, unit = record["symbol"]))
    custom_inputs = {name: ["y" if value else "n", "tristate"] if type(value) == "bool" else [str(value), "int" if type(value) == "int" else "string"] for name, value in custom.items()}
    records = bind_compiler_inputs(records, custom_inputs)
    requested = dict(PROFILE["base"])
    if additional_params["mode"] != "allno":
        requested.update(PROFILE["requested"])
    else:
        requested["64BIT"] = "y"
    requested.update(json.decode(additional_params["requested"]))
    config = resolve_options(records, requested, list(requested))

    # buildifier: disable=print
    print("Kernel Kconfig:", len(config.values), "symbols;", len(measured["capabilities"]), "measured Kbuild answers; final =", additional_params["final"])
    commands = {name.removeprefix("cmd_"): file for name, file in tools.items() if name.startswith("cmd_")}
    tc = json.decode(additional_params["toolchain"])
    prepared = struct(root = tools["source_root"], files = depset([tools["source_root"]]), logical = {p: True for p in json.decode(additional_params["logical"])})
    runtime_tools = dict(tools, commands = commands)
    params = struct(python = tools["python"], executor = tools["executor"], materialize = tools["materialize"], link_script = tools["link_script"])
    planning = not additional_params["final"]
    ctx = context(tctx, output_directories, runtime_tools, params, planning = planning)

    def file(name):
        return ctx.actions.declare_file(name)

    dotconfig, header, report = file(".config"), file("include/generated/autoconf.h"), file("config-report.json")
    ctx.actions.write(dotconfig, config.config)
    ctx.actions.write(header, config.header)
    ctx.actions.write(report, json.encode(dict(values = config.values, compiler = measured["compiler"], patches = len(measured["patches"]), probes = len(measured["capabilities"]))))
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
    role_tools = {
        "CC": commands["gcc"].path,
        "LD": commands["ld"].path,
        "AR": commands["ar"].path,
        "NM": commands["nm"].path,
        "OBJCOPY": commands["objcopy"].path,
        "OBJDUMP": commands["objdump"].path,
        "READELF": commands["readelf"].path,
        "STRIP": commands["strip"].path,
        "HOSTCC": commands["gcc"].path,
        "HOSTCXX": commands["g++"].path,
    }
    for name, tool in {"CONFIG_SHELL": "sh", "BASH": "bash", "PYTHON3": "python3", "PERL": "perl", "AWK": "awk", "LEX": "flex", "YACC": "bison", "KGZIP": "gzip", "KBZIP2": "bzip2", "KLZMA": "lzma", "LZOP": "lzop", "XZ": "xz", "ZSTD": "zstd"}.items():
        if tool in commands:
            role_tools[name] = commands[tool].path
    requests = {}
    all_files = depset([tools["source_root"]], transitive = [tools["execution_files"], tools["compiler_files"]])
    setup = make_setup(ctx, conf, prepared, ctx.execution, role_tools, additional_params["builtin_include"], tc["compile_flags"], tc["compile_flags"], tc["link_flags"], measured["capabilities"], requests, all_files)
    programs = {p.rpartition("/")[0] or ".": body for p, body in FLAG_PROGRAM["image_programs"].items() if p.rsplit("/", 1)[-1] == "Makefile"}
    programs.update({p.rpartition("/")[0] or ".": body for p, body in FLAG_PROGRAM["image_programs"].items() if p.rsplit("/", 1)[-1] == "Kbuild"})
    catalog = struct(actions = ACTIONS, programs = programs, aliases = {out: p for p, d in ACTIONS.items() for out in d["outs"]})
    objtool = build_objtool(ctx, prepared, arch, tc["compile_flags"], tc["link_flags"], measured["host_capabilities"]) if config.values.get("OBJTOOL") == "y" else struct(executable = None)
    build_image(ctx, conf, setup, prepared, catalog, objtool)

    # buildifier: disable=print
    print("Kernel probe requests:", len(requests))
    if not planning and requests:
        fail("Kbuild probing did not converge: " + str(requests.keys()))
    if planning:
        write = writer(tctx, tools["python"], tools["emit"], output_directories["scratch"])
        manifest = tctx.declare_file("requests.json", directory = output_directories["kernel"])
        write(manifest, json.encode(dict(
            previous = additional_params["previous"],
            requests = requests,
            cc = tc["cc"],
            ld = additional_params["ld"],
            shell = "/bin/sh",
        )))

def _impl(ctx):
    tc = toolchain_context(ctx)
    cc = find_cc_toolchain(ctx)
    commands = {f.basename: f for f in ctx.attr._tools[DefaultInfo].files.to_list()}
    if tc.spec["cc"] != "/usr/bin/gcc":
        fail("The full local prototype currently requires the selected /usr/bin/gcc toolchain")
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
        ld = "/usr/bin/ld",
        host_programs = ctx.file._host_programs.path,
        tool_environment = {"AS": "/usr/bin/as", "AR": "/usr/bin/ar", "NM": "/usr/bin/nm", "OBJCOPY": "/usr/bin/objcopy", "OBJDUMP": "/usr/bin/objdump", "READELF": "/usr/bin/readelf", "STRIP": "/usr/bin/strip", "BINDGEN": "/bin/false", "HOSTRUSTC": "/bin/false"},
        compile_flags = tc.spec["compile_flags"],
    )))
    ctx.actions.run(
        executable = commands["python3"],
        arguments = [ctx.file._probe.path, "kconfig", spec.path, answers.path],
        inputs = depset([prepared, spec, ctx.file._baseline, ctx.file._syntax, ctx.file._probe, ctx.file._host_programs], transitive = [tc.files, ctx.attr._tools[DefaultInfo].files]),
        outputs = [answers],
        env = {"PATH": "/usr/bin:/bin"},
        mnemonic = "KernelKconfigProbe",
        toolchain = CC_TOOLCHAIN,
    )
    base_tools = {"cmd_" + n: f for n, f in commands.items()}
    base_tools.update(dict(
        python = commands["python3"],
        execution_files = ctx.attr._tools[DefaultInfo].files,
        compiler_files = tc.files,
        source_root = prepared,
        emit = ctx.file._emit,
        executor = ctx.file._executor,
        materialize = ctx.file._materialize,
        link_script = ctx.file._link_script,
    ))
    if ctx.attr.rounds < 0:
        fail("rounds must be nonnegative")
    output = None
    for round in range(ctx.attr.rounds + 1):
        final = round == ctx.attr.rounds
        output = ctx.actions.declare_directory(ctx.label.name + (".tree" if final else ".plan%d" % round))
        scratch = ctx.actions.declare_directory(ctx.label.name + ".scratch%d" % round)
        input_directories = {"answers": answers}
        if ctx.attr.probes:
            probes = ctx.attr.probes[AutoconfInfo]
            if probes.toolchain_id != tc.id or probes.toolchain != tc.spec:
                fail("Custom probes must use the same configured toolchain as the kernel")
            input_directories["custom"] = probes.results
        ctx.actions.map_directory(
            input_directories = input_directories,
            output_directories = {"kernel": output, "scratch": scratch},
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
                logical = json.encode(list(logical)),
                toolchain = json.encode(tc.spec),
                builtin_include = cc.built_in_include_directories[0],
                ld = "/usr/bin/ld",
            ),
            toolchain = CC_TOOLCHAIN,
        )
        if not final:
            next_answers = ctx.actions.declare_directory(ctx.label.name + ".answers%d" % round)
            ctx.actions.run(
                executable = commands["python3"],
                arguments = [ctx.file._probe.path, "kbuild", output.path + "/requests.json", next_answers.path],
                inputs = depset([output, answers, ctx.file._probe, prepared], transitive = [tc.files, ctx.attr._tools[DefaultInfo].files]),
                outputs = [next_answers],
                env = {"PATH": "/usr/bin:/bin"},
                mnemonic = "KernelKbuildProbe",
                toolchain = CC_TOOLCHAIN,
            )
            answers = next_answers
    return [DefaultInfo(files = depset([output]))]

kernel_build = rule(implementation = _impl, fragments = ["cpp"], toolchains = use_cc_toolchain(), attrs = {
    "rounds": attr.int(default = 5),
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
