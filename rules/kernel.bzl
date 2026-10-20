"""Plan measured Kbuild artifacts from source-owned native configuration."""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("@rules_cc//cc/common:cc_info.bzl", "CcInfo")
load("//execution_tools:defs.bzl", "EXECUTION_TOOLS_TYPE")
load(":config.bzl", "KernelConfigInfo")
load(":host_libraries.bzl", "host_libraries")
load(":host_tools.bzl", "build_objtool", "build_resolve_btfids")
load(":image.bzl", "build_image")
load(":libbpf.bzl", "build_libbpf")
load(":runtime.bzl", "context", "read_payload")
load(":schedule.bzl", "schedule", "write_schedule")
load(":syntax.bzl", "KernelSyntaxInfo", "syntax_for_source")
load(":toolchain.bzl", "make_setup")

KernelBuildInfo = provider("Measured products of one native configuration.", fields = ["directory", "config", "manifest"])

# This generated-syntax callback registers actions, not BUILD targets.
# buildifier: disable=unnamed-macro
def expand_kernel(tctx, input_directories, output_directories, tools, additional_inputs, additional_params, kbuild):
    measured = read_payload(input_directories["answers"])
    if measured.get("converged", False) and not additional_params["final"]:
        manifest = tctx.declare_file("requests.json", directory = output_directories["kernel"])
        tctx.write(manifest, json.encode(dict(previous = additional_params["previous"], requests = {})))
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
    runtime_tools = dict(tools, commands = commands, host_cc = host_roles["HOSTCC"], host_cxx = host_roles["HOSTCXX"], target_cc = tc["cc"], host_roles = json.decode(additional_params["host_toolchain_roles"]), host_spec = json.decode(additional_params["host_toolchain"]), environment = environment, host_compile_env = host_environment, host_link_env = host_link_environment, path_environment = json.decode(additional_params["path_environment"]))
    runtime_tools["backend_environment"] = tc["compile_env"]
    link_script = tctx.declare_file("native/link-vmlinux.sh", directory = output_directories["kernel"])
    params = struct(python = tools["python"], executor = tools["executor"], materialize = tools["materialize"], link_script = link_script, native_recipe = tools["native_recipe"], recipe_check = tools["recipe_check"], diagnostic_driver = tools["diagnostic_driver"], diagnostic_source = tools["diagnostic_source"], link_script_text = kbuild["link_script"], output_prefix = additional_params["output_prefix"])
    planning = not additional_params["final"]
    recorded = [] if additional_params["final"] else None
    ctx = context(tctx, output_directories, runtime_tools, params, planning = planning, recorded = recorded, kernel_roots = json.decode(additional_params["roots"]), required_options = json.decode(additional_params["required_options"]), validate_native_objects = additional_params["validate_native_objects"])
    ctx.actions.write(link_script, kbuild["link_script"])

    native_files = {file.path.removeprefix(input_directories["config"].directory.path + "/"): file for file in input_directories["config"].children}
    dotconfig = native_files[".config"]
    header = native_files["include/generated/autoconf.h"]
    auto_conf = native_files["include/config/auto.conf"]
    conf = struct(
        values = config["values"],
        make_values = config["make_values"],
        architecture = arch,
        arch = arch.arch,
        identity = "kernel",
        config = dotconfig,
        config_owner = input_directories["config"].directory,
        header = header,
        auto_conf = auto_conf,
        pahole = tools["pahole"],
    )
    role_tools = json.decode(additional_params["roles"])
    role_tools.update(host_roles)
    for name, tool in {"CONFIG_SHELL": "sh", "BASH": "bash", "PYTHON3": "python3", "PERL": "perl", "AWK": "awk", "LEX": "flex", "YACC": "bison", "KGZIP": "gzip", "KBZIP2": "bzip2", "KLZMA": "lzma", "LZOP": "lzop", "XZ": "xz", "ZSTD": "zstd"}.items():
        if tool in commands:
            role_tools[name] = commands[tool].path
    requests = {}
    all_files = depset([tools["source_root"]], transitive = [tools["execution_files"], tools["compiler_files"]])
    setup = make_setup(ctx, conf, prepared, ctx.execution, role_tools, additional_params["builtin_include"], json.decode(additional_params["target_flags"]), json.decode(additional_params["host_compile_flags"]), json.decode(additional_params["host_link_flags"]), measured["capabilities"], requests, all_files, kbuild["program"])
    programs = {p.rpartition("/")[0] or ".": body for p, body in kbuild["program"]["image_programs"].items() if p.rsplit("/", 1)[-1] == "Makefile"}
    programs.update({p.rpartition("/")[0] or ".": body for p, body in kbuild["program"]["image_programs"].items() if p.rsplit("/", 1)[-1] == "Kbuild"})
    catalog = struct(actions = kbuild["actions"], programs = programs, aliases = {out: p for p, d in kbuild["actions"].items() for out in d["outs"]})
    if not (planning and requests):
        objtool = build_objtool(ctx, prepared, setup, measured["capabilities"], requests, tools["host_package"]) if any([config["values"].get(name) == "y" for name in ["OBJTOOL", "STACK_VALIDATION"]]) else struct(executable = None)
        resolver = None
        if not (planning and requests) and config["values"].get("DEBUG_INFO_BTF") == "y":
            libraries = struct(cflags = json.decode(additional_params["library_cflags"]), libs = json.decode(additional_params["library_flags"]), files = tools["library_files"])
            libbpf = build_libbpf(ctx, prepared, struct(roles = ctx.execution.host_roles, spec = ctx.execution.host_spec), libraries, tools["host_package"], tools["libbpf_builder"], depset([tools["native_configure"], tools["native_spec"], tools["probe"], tools["payload"]]))
            resolver = build_resolve_btfids(ctx, prepared, setup, measured["capabilities"], requests, tools["host_package"], libraries, libbpf)
        if not (planning and requests):
            build_image(ctx, conf, setup, prepared, catalog, objtool, resolver)
    if not planning and requests:
        fail("Kbuild probing did not converge: " + str(requests.keys()))
    if recorded != None:
        write_schedule(tctx, output_directories["schedule"], recorded)
    if planning:
        manifest = tctx.declare_file("requests.json", directory = output_directories["kernel"])
        tctx.write(manifest, json.encode(dict(previous = additional_params["previous"], requests = requests, config = config["config"], header = config["header"])))

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
    package_command = ctx.actions.declare_file(ctx.label.name + ".host-tools/pkg-config")
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
        inputs = depset([spec, ctx.file._probe, ctx.file._payload], transitive = [execution.python_files]),
        outputs = [answers],
        mnemonic = "KernelProbeSeed",
    )
    compiler_tools = {"compiler_tool_%d" % i: tool for i, tool in enumerate(compiler.tools + host.tools + [config.pahole.files_to_run])}
    action_tool_providers = dict(compiler_tools)
    action_tool_providers.update({"execution_tool_%d" % i: tool for i, tool in enumerate(execution.build_tools)})
    base_tools = {"cmd_" + n: f for n, f in commands.items()}
    base_tools.update(action_tool_providers)
    base_tools.update(dict(
        python = execution.python,
        python_files = execution.python_files,
        execution_files = execution_files,
        compiler_files = depset([package_spec, package_command, ctx.file._host_package], transitive = [compiler.files, host.files, config.pahole.files, libraries.files]),
        target_compiler_files = compiler.files,
        library_files = libraries.files,
        libbpf_builder = ctx.file._libbpf_builder,
        native_configure = ctx.file._native_configure,
        native_spec = ctx.file._native_spec,
        probe = ctx.file._probe,
        payload = ctx.file._payload,
        host_package = package_command,
        pahole = config.pahole.files_to_run.executable,
        source_root = prepared,
        executor = ctx.file._executor,
        materialize = ctx.file._materialize,
        native_recipe = ctx.file._native_recipe,
        recipe_check = ctx.file._recipe_check,
        diagnostic_driver = ctx.file._diagnostic_driver,
        diagnostic_source = ctx.file._diagnostic_source,
    ))
    if ctx.attr.rounds < 0:
        fail("rounds must be nonnegative")
    final_output = ctx.actions.declare_directory(ctx.label.name + ".graph")
    encoded_parameters = dict(
        logical = json.encode(logical),
        toolchain = json.encode(tc.spec),
        architecture = json.encode(config.architecture),
        roles = json.encode(dict(compiler.roles, PAHOLE = config.pahole.files_to_run.executable.path)),
        target_flags = json.encode(compiler.target_flags),
        host_roles = json.encode({"HOSTCC": host.roles["CC"], "HOSTCXX": host.roles["CXX"]}),
        host_toolchain_roles = json.encode(host.roles),
        host_toolchain = json.encode(host.context.spec),
        library_cflags = json.encode(libraries.cflags),
        library_flags = json.encode(libraries.libs),
        host_compile_flags = json.encode(host.context.spec["compile_flags"]),
        host_compile_env = json.encode(host.context.spec["compile_env"]),
        host_link_env = json.encode(host.context.spec["link_env"]),
        host_link_flags = json.encode(host.context.spec["link_flags"]),
        execution_environment = json.encode(dict(execution.bootstrap_environment, PATH = execution.bin_dir, PYTHONDONTWRITEBYTECODE = "1")),
        path_environment = json.encode(execution.bootstrap_path_environment),
        builtin_include = compiler.builtin_include,
        roots = json.encode(ctx.attr.roots),
        required_options = json.encode(ctx.attr.required_options),
        validate_native_objects = ctx.attr._validate_native_objects[BuildSettingInfo].value,
    )
    output, metadata = None, None
    for round in range(ctx.attr.rounds + 1):
        final = round == ctx.attr.rounds
        output = final_output if final else ctx.actions.declare_directory(ctx.label.name + ".plan%d" % round)
        metadata = ctx.actions.declare_directory(ctx.label.name + ".schedule%d" % round)
        ctx.actions.map_directory(
            input_directories = {"answers": answers, "config": config.directory},
            output_directories = {"kernel": output, "schedule": metadata},
            additional_inputs = {"originals": originals},
            tools = base_tools,
            implementation = expand,
            mnemonic = "KernelArtifact" if final else "KernelPlan",
            additional_params = dict(
                final = final,
                output_prefix = final_output.path,
                previous = answers.path,
                **encoded_parameters
            ),
        )
        if not final:
            next_answers = ctx.actions.declare_directory(ctx.label.name + ".answers%d" % round)
            ctx.actions.run(
                executable = execution.python,
                arguments = ["-B", ctx.file._probe.path, "kbuild", output.path + "/requests.json", next_answers.path, json.encode(round + 1 == ctx.attr.rounds)],
                inputs = depset([output, answers, ctx.file._probe, ctx.file._payload, ctx.file._native_recipe, prepared], transitive = [base_tools["compiler_files"], execution_files]),
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
        dict(action_tool_providers, python = execution.python, python_files = execution.python_files, executor = ctx.file._executor, runner = ctx.file._scheduler, payload = ctx.file._payload, materialize = ctx.file._materialize, native_recipe = ctx.file._native_recipe, recipe_check = ctx.file._recipe_check, diagnostic_driver = ctx.file._diagnostic_driver, diagnostic_source = ctx.file._diagnostic_source, libbpf_builder = ctx.file._libbpf_builder, native_configure = ctx.file._native_configure, native_spec = ctx.file._native_spec, probe = ctx.file._probe),
        base_tools["compiler_files"],
        execution_files,
        config = config.directory,
        backend_files = compiler.files,
        backend_tools = compiler.tools,
        backend_environment = tc.spec["compile_env"],
        steps = ctx.attr.schedule_steps,
    )
    return [KernelBuildInfo(directory = output.directory, config = config, manifest = output.manifest), DefaultInfo(files = depset([output.directory])), OutputGroupInfo(qualification_manifest = depset([output.manifest]), boot = depset([output.directory]))]

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
        "_validate_native_objects": attr.label(default = "//:validate_native_objects", providers = [BuildSettingInfo]),
        "_libbpf_builder": attr.label(default = "//rules:build_libbpf.py", allow_single_file = True),
        "_native_configure": attr.label(default = "//native:configure.py", allow_single_file = True),
        "_native_spec": attr.label(default = "//native:spec.py", allow_single_file = True),
        "_libelf": attr.label(default = "@elfutils//:libelf", cfg = "exec", providers = [CcInfo]),
        "_host_package": attr.label(default = "//rules:host_package.py", allow_single_file = True),
        "_scheduler": attr.label(default = "//:scheduler.py", allow_single_file = True),
        "_prepare": attr.label(default = "//rules:prepare_source.py", allow_single_file = True),
        "_probe": attr.label(default = "//:probe_runtime.py", allow_single_file = True),
        "_payload": attr.label(default = "//:payload.py", allow_single_file = True),
        "_native_recipe": attr.label(default = "//:native_recipe.py", allow_single_file = True),
        "_recipe_check": attr.label(default = "//:qualification/native_recipe_check.py", allow_single_file = True),
        "_diagnostic_driver": attr.label(default = "//:qualification/ir_diagnostics.py", allow_single_file = True),
        "_diagnostic_source": attr.label(default = "//:qualification/ir_diagnostics.c", allow_single_file = True),
        "_executor": attr.label(default = "//:execute.py", allow_single_file = True),
        "_materialize": attr.label(default = "//rules:materialize.py", allow_single_file = True),
    },
)
