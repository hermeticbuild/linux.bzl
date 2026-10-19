"""Plan measured Kbuild artifacts from source-owned native configuration."""

load("@rules_cc//cc/common:cc_info.bzl", "CcInfo")
load("//execution_tools:defs.bzl", "EXECUTION_TOOLS_TYPE")
load(":config.bzl", "KernelConfigInfo")
load(":host_libraries.bzl", "host_libraries")
load(":host_tools.bzl", "build_objtool")
load(":image.bzl", "build_image")
load(":runtime.bzl", "context", "read_payload", "writer")
load(":schedule.bzl", "schedule", "write_payload")
load(":syntax.bzl", "KernelSyntaxInfo", "syntax_for_source")
load(":toolchain.bzl", "make_setup")

KernelBuildInfo = provider("Measured products of one native configuration.", fields = ["directory", "config"])

# This generated-syntax callback registers actions, not BUILD targets.
# buildifier: disable=unnamed-macro
def expand_kernel(tctx, input_directories, output_directories, tools, additional_inputs, additional_params, kbuild):
    measured = read_payload(input_directories["answers"])
    if measured.get("converged", False) and not additional_params["final"]:
        write = writer(tctx, tools["python"], tools["emit"], output_directories["scratch"], tools["python_files"])
        manifest = tctx.declare_file("requests.json", directory = output_directories["kernel"])
        write(manifest, json.encode(dict(previous = additional_params["previous"], requests = {})))
        return
    config = read_payload(input_directories["config"])
    arch = struct(**json.decode(additional_params["architecture"]))
    commands = {name.removeprefix("cmd_"): file for name, file in tools.items() if name.startswith("cmd_")}
    tc = json.decode(additional_params["toolchain"])
    originals = {f.path: f for f in additional_inputs["originals"].to_list()}
    prepared = struct(root = tools["source_root"], files = depset([tools["source_root"]]), logical = {p: originals[path] for p, path in json.decode(additional_params["logical"]).items()})
    host_roles = json.decode(additional_params["host_roles"])
    environment = json.decode(additional_params["execution_environment"])
    environment.update(tc["compile_env"])
    host_environment = json.decode(additional_params["execution_environment"])
    host_environment.update(json.decode(additional_params["host_compile_env"]))
    host_link_environment = json.decode(additional_params["execution_environment"])
    host_link_environment.update(json.decode(additional_params["host_link_env"]))
    runtime_tools = dict(tools, commands = commands, host_cc = host_roles["HOSTCC"], host_cxx = host_roles["HOSTCXX"], target_cc = tc["cc"], environment = environment, host_compile_env = host_environment, host_link_env = host_link_environment, path_environment = json.decode(additional_params["path_environment"]))
    link_script = tctx.declare_file("native/link-vmlinux.sh", directory = output_directories["kernel"])
    params = struct(python = tools["python"], executor = tools["executor"], materialize = tools["materialize"], link_script = link_script)
    planning = not additional_params["final"]
    recorded = [] if additional_params["final"] else None
    ctx = context(tctx, output_directories, runtime_tools, params, planning = planning, recorded = recorded, kernel_roots = json.decode(additional_params["roots"]), required_options = json.decode(additional_params["required_options"]))
    ctx.actions.write(link_script, kbuild["link_script"])

    def file(name):
        return ctx.actions.declare_file(name)

    dotconfig = file(".config")
    header = file("include/generated/autoconf.h")
    auto_conf = file("include/config/auto.conf")
    ctx.actions.write(dotconfig, config["config"])
    ctx.actions.write(header, config["header"])
    ctx.actions.write(auto_conf, config["auto_conf"])
    conf = struct(
        values = config["values"],
        make_values = config["make_values"],
        architecture = arch,
        arch = arch.arch,
        identity = "kernel",
        config = dotconfig,
        header = header,
        auto_conf = auto_conf,
    )
    role_tools = json.decode(additional_params["roles"])
    role_tools.update(host_roles)
    for name, tool in {"CONFIG_SHELL": "sh", "BASH": "bash", "PYTHON3": "python3", "PERL": "perl", "AWK": "awk", "LEX": "flex", "YACC": "bison", "KGZIP": "gzip", "KBZIP2": "bzip2", "KLZMA": "lzma", "LZOP": "lzop", "XZ": "xz", "ZSTD": "zstd"}.items():
        if tool in commands:
            role_tools[name] = commands[tool].path
    requests = {}
    all_files = depset([tools["source_root"]], transitive = [tools["execution_files"], tools["compiler_files"]])
    setup = make_setup(ctx, conf, prepared, ctx.execution, role_tools, additional_params["builtin_include"], tc["compile_flags"], json.decode(additional_params["host_compile_flags"]), json.decode(additional_params["host_link_flags"]), measured["capabilities"], requests, all_files, kbuild["program"])
    programs = {p.rpartition("/")[0] or ".": body for p, body in kbuild["program"]["image_programs"].items() if p.rsplit("/", 1)[-1] == "Makefile"}
    programs.update({p.rpartition("/")[0] or ".": body for p, body in kbuild["program"]["image_programs"].items() if p.rsplit("/", 1)[-1] == "Kbuild"})
    catalog = struct(actions = kbuild["actions"], programs = programs, aliases = {out: p for p, d in kbuild["actions"].items() for out in d["outs"]})
    if not (planning and requests):
        objtool = build_objtool(ctx, prepared, arch, json.decode(additional_params["host_compile_flags"]), json.decode(additional_params["host_link_flags"]), measured["capabilities"], requests, kbuild["program"], tools["host_package"]) if config["values"].get("OBJTOOL") == "y" else struct(executable = None)
        build_image(ctx, conf, setup, prepared, catalog, objtool)
    if not planning and requests:
        fail("Kbuild probing did not converge: " + str(requests.keys()))
    if recorded != None:
        write_payload(tctx, tools["python"], tools["emit"], output_directories["schedule"], recorded, tools["python_files"])
    if planning:
        write = writer(tctx, tools["python"], tools["emit"], output_directories["scratch"], tools["python_files"])
        manifest = tctx.declare_file("requests.json", directory = output_directories["kernel"])
        write(manifest, json.encode(dict(previous = additional_params["previous"], requests = requests, config = config["config"], header = config["header"])))

def _impl(ctx):
    config = ctx.attr.config[KernelConfigInfo]
    source, compiler, host = config.source, config.compiler, config.host_compiler
    expand = syntax_for_source(ctx.attr.syntax[KernelSyntaxInfo], source)
    tc = compiler.context
    execution = ctx.toolchains[EXECUTION_TOOLS_TYPE].execution
    commands = execution.build_commands
    execution_files = execution.build_files
    libraries = host_libraries({ctx.attr._libelf: "libelf"})
    package_spec = ctx.actions.declare_file(ctx.label.name + ".host-packages.json")
    ctx.actions.write(package_spec, json.encode(libraries.packages))
    package_command = ctx.actions.declare_file(ctx.label.name + ".host-pkg-config")
    ctx.actions.write(package_command, "#!" + commands["sh"].path + '\nexec "${KBUILD_EXEC_ROOT:?}/' + execution.python.path + '" -B "${KBUILD_EXEC_ROOT}/' + ctx.file._host_package.path + '" "${KBUILD_EXEC_ROOT}/' + package_spec.path + '" "$@"\n', is_executable = True)
    originals = source.files
    prefix = source.anchor.dirname + "/"
    logical = {f.path[len(prefix):]: f.path for f in originals.to_list() if f.path.startswith(prefix)}
    prepared = ctx.actions.declare_directory(ctx.label.name + ".sources")
    manifest = ctx.actions.declare_file(ctx.label.name + ".source-inputs.json")
    ctx.actions.write(manifest, json.encode(dict(sources = logical, commands = {n: f.path for n, f in commands.items()})))
    ctx.actions.run(
        executable = execution.python,
        arguments = ["-B", ctx.file._prepare.path, manifest.path, prepared.path],
        inputs = depset([manifest, ctx.file._prepare], transitive = [originals, execution.python_files]),
        outputs = [prepared],
        mnemonic = "KernelSource",
    )
    answers = ctx.actions.declare_directory(ctx.label.name + ".probe-seed")
    spec = ctx.actions.declare_file(ctx.label.name + ".probe-seed.json")
    ctx.actions.write(spec, "{}")
    ctx.actions.run(
        executable = execution.python,
        arguments = ["-B", ctx.file._probe.path, "seed", spec.path, answers.path],
        inputs = depset([spec, ctx.file._probe], transitive = [execution.python_files]),
        outputs = [answers],
        mnemonic = "KernelProbeSeed",
    )
    compiler_tools = {"compiler_tool_%d" % i: tool for i, tool in enumerate(compiler.tools + host.tools + [config.pahole.files_to_run])}
    base_tools = {"cmd_" + n: f for n, f in commands.items()}
    base_tools.update(compiler_tools)
    base_tools.update(dict(
        python = execution.python,
        python_files = execution.python_files,
        execution_files = execution_files,
        compiler_files = depset([package_spec, package_command, ctx.file._host_package], transitive = [compiler.files, host.files, config.pahole.files, libraries.files]),
        host_package = package_command,
        source_root = prepared,
        emit = ctx.file._emit,
        executor = ctx.file._executor,
        materialize = ctx.file._materialize,
    ))
    if ctx.attr.rounds < 0:
        fail("rounds must be nonnegative")
    output, metadata = None, None
    for round in range(ctx.attr.rounds + 1):
        final = round == ctx.attr.rounds
        output = ctx.actions.declare_directory(ctx.label.name + (".graph" if final else ".plan%d" % round))
        scratch = ctx.actions.declare_directory(ctx.label.name + ".scratch%d" % round)
        metadata = ctx.actions.declare_directory(ctx.label.name + ".schedule%d" % round)
        ctx.actions.map_directory(
            input_directories = {"answers": answers, "config": config.directory},
            output_directories = {"kernel": output, "scratch": scratch, "schedule": metadata},
            additional_inputs = {"originals": originals},
            tools = base_tools,
            implementation = expand,
            mnemonic = "KernelArtifact" if final else "KernelPlan",
            additional_params = dict(
                final = final,
                previous = answers.path,
                logical = json.encode(logical),
                toolchain = json.encode(tc.spec),
                architecture = json.encode(config.architecture),
                roles = json.encode(dict(compiler.roles, PAHOLE = config.pahole.files_to_run.executable.path)),
                host_roles = json.encode({"HOSTCC": host.roles["CC"], "HOSTCXX": host.roles["CXX"]}),
                host_compile_flags = json.encode(host.context.spec["compile_flags"]),
                host_compile_env = json.encode(host.context.spec["compile_env"]),
                host_link_env = json.encode(host.context.spec["link_env"]),
                host_link_flags = json.encode(host.context.spec["link_flags"]),
                execution_environment = json.encode(dict(execution.bootstrap_environment, PATH = execution.bin_dir, PYTHONDONTWRITEBYTECODE = "1")),
                path_environment = json.encode(execution.bootstrap_path_environment),
                builtin_include = compiler.builtin_include,
                roots = json.encode(ctx.attr.roots),
                required_options = json.encode(ctx.attr.required_options),
            ),
        )
        if not final:
            next_answers = ctx.actions.declare_directory(ctx.label.name + ".answers%d" % round)
            ctx.actions.run(
                executable = execution.python,
                arguments = ["-B", ctx.file._probe.path, "kbuild", output.path + "/requests.json", next_answers.path],
                inputs = depset([output, answers, ctx.file._probe, prepared], transitive = [base_tools["compiler_files"], execution_files]),
                outputs = [next_answers],
                tools = compiler.tools + host.tools + execution.build_tools,
                mnemonic = "KernelKbuildProbe",
            )
            answers = next_answers
    output = schedule(
        ctx,
        output,
        metadata,
        prepared,
        originals,
        source.anchor.dirname,
        dict(compiler_tools, python = execution.python, python_files = execution.python_files, emit = ctx.file._emit, executor = ctx.file._executor, runner = ctx.file._scheduler, materialize = ctx.file._materialize),
        base_tools["compiler_files"],
        execution_files,
        steps = ctx.attr.schedule_steps,
    )
    return [KernelBuildInfo(directory = output, config = config), DefaultInfo(files = depset([output]))]

kernel_build = rule(
    implementation = _impl,
    toolchains = [EXECUTION_TOOLS_TYPE],
    attrs = {
        "config": attr.label(mandatory = True, providers = [KernelConfigInfo]),
        "syntax": attr.label(mandatory = True, providers = [KernelSyntaxInfo]),
        "roots": attr.string_list(default = ["vmlinux"]),
        "required_options": attr.string_dict(),
        "rounds": attr.int(default = 64),
        "schedule_steps": attr.int(default = 32),
        "_libelf": attr.label(default = "@elfutils//:libelf", cfg = "exec", providers = [CcInfo]),
        "_host_package": attr.label(default = "//rules:host_package.py", allow_single_file = True),
        "_scheduler": attr.label(default = "//:scheduler.py", allow_single_file = True),
        "_prepare": attr.label(default = "//rules:prepare_source.py", allow_single_file = True),
        "_probe": attr.label(default = "//:probe_runtime.py", allow_single_file = True),
        "_emit": attr.label(default = "//:emit.py", allow_single_file = True),
        "_executor": attr.label(default = "//:execute.py", allow_single_file = True),
        "_materialize": attr.label(default = "//rules:materialize.py", allow_single_file = True),
    },
)
