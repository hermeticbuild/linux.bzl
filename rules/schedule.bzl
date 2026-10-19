"""Schedule measured compiler dependencies before declaring compiler spawn inputs."""

load(":runtime.bzl", "read_payload", "writer")

_ASCII = " !\"#$%&'()*+,-./0123456789:;<=>?@ABCDEFGHIJKLMNOPQRSTUVWXYZ[\\]^_`abcdefghijklmnopqrstuvwxyz{|}~"
_HEX = "0123456789abcdef"
_CODES = {c: _HEX[(i + 32) // 16] + _HEX[(i + 32) % 16] for i, c in enumerate(_ASCII.elems())}

def write_payload(tctx, python, emit, root, value, python_files):
    report = tctx.declare_file("report.json", directory = root)
    writer(tctx, python, emit, root, python_files)(report, json.encode(value))
    encoded = "".join([_CODES[c] for c in json.encode(value).elems()])
    outputs = [tctx.declare_file(("00000000" + str(start // 180))[-8:] + "=" + encoded[start:start + 180], directory = root) for start in range(0, len(encoded), 180)]
    for start in range(0, len(outputs), 500):
        batch = outputs[start:start + 500]
        tctx.run(executable = python, inputs = depset([emit], transitive = [python_files]), outputs = batch, arguments = [emit.path, "--touch"] + [f.path for f in batch], progress_message = "Write kernel schedule metadata")

def _available(input_directories, tools, additional_inputs, params):
    available = {f.path: f for f in input_directories["literals"].children}
    for name in ["execution_files", "compiler_files"]:
        available.update({f.path: f for f in tools[name].to_list()})
    available.update({f.path: f for f in additional_inputs["originals"].to_list()})
    available.update({tool.path: tool for tool in tools.values() if type(tool) == "File"})
    available[params["source_prefix"]] = tools["source_root"]
    for name, prefix in json.decode(params["stage_prefixes"]).items():
        for file in input_directories[name].children:
            if file.path.startswith(prefix + "/artifacts/"):
                available[params["plan_prefix"] + "/" + file.path.removeprefix(prefix + "/artifacts/")] = file
    return available

# Fixed map_directory callback signature.
# buildifier: disable=unused-variable
def _stage(tctx, input_directories, output_directories, tools, additional_inputs, additional_params):
    params = additional_params
    jobs = read_payload(input_directories["schedule"])
    available = _available(input_directories, tools, additional_inputs, params)
    source_files = {f.path.removeprefix(params["source_prefix"] + "/"): f for f in input_directories["sources"].children}
    originals = {f.path.removeprefix(params["original_prefix"] + "/"): f for f in additional_inputs["originals"].to_list()}
    scanned = {}
    for name in input_directories:
        if name.startswith("scan_"):
            scanned.update(read_payload(input_directories[name]))
    write = writer(tctx, tools["python"], tools["emit"], output_directories["scratch"], tools["python_files"])
    requests = []
    produced = {path: True for job in jobs for path in job["outputs"]}
    unknown = {path: True for job in jobs for path in job["inputs"] if path not in available and path not in produced}
    if unknown:
        fail("Kernel graph inputs were not supplied as sources, tools or artifacts: " + str(unknown.keys()[:20]))
    for index, job in enumerate(jobs):
        if all([path in available for path in job["outputs"]]):
            continue
        if any([path not in available for path in job["inputs"]]):
            continue
        aliases = {path: available[path].path for path in job["inputs"]}
        if job["scan"] != None and str(index) not in scanned:
            requests.append(dict(
                id = str(index),
                manifest = job["manifest"],
                aliases = aliases,
                scan = job["scan"],
                source_prefix = params["source_prefix"],
                original_prefix = params["original_prefix"],
            ))
            continue
        inputs = [available[path] for path in job["inputs"] if path != params["source_prefix"] or job["scan"] == None]
        source_inputs = []
        if job["scan"] != None:
            aliases.pop(params["source_prefix"], None)
            for logical in scanned[str(index)]:
                if logical not in source_files or logical not in originals:
                    fail("Compiler dependency was not declared as a kernel source: " + logical)
                source_inputs.extend([source_files[logical], originals[logical]])
        output_files = [tctx.declare_file("artifacts/" + path.removeprefix(params["plan_prefix"] + "/"), directory = output_directories["stage"]) for path in job["outputs"]]
        manifest = tctx.declare_file("actions/%d.json" % index, directory = output_directories["stage"])
        spec = dict(
            manifest = job["manifest"],
            aliases = aliases,
            source_inputs = [f.path for f in source_inputs],
            outputs = {path: f.path for path, f in zip(job["outputs"], output_files)},
        )
        write(manifest, json.encode(spec))
        tctx.run(
            executable = tools["python"],
            arguments = [tools["runner"].path, "execute", manifest.path],
            inputs = depset(inputs + source_inputs + [manifest, available[job["manifest"]], tools["runner"], tools["executor"]], transitive = [tools["compiler_files"], tools["execution_files"]]),
            outputs = output_files,
            tools = [tool for name, tool in tools.items() if name.startswith("compiler_tool_")],
            progress_message = job["message"],
        )
        available.update({path: f for path, f in zip(job["outputs"], output_files)})
    requests_file = tctx.declare_file("scan-requests.json", directory = output_directories["stage"])
    write(requests_file, json.encode(requests))

# Fixed map_directory callback signature.
# buildifier: disable=unused-variable
def _export(tctx, input_directories, output_directories, tools, additional_inputs, additional_params):
    available = _available(input_directories, tools, additional_inputs, additional_params)
    jobs = read_payload(input_directories["schedule"])
    missing = [path for job in jobs for path in job["outputs"] if path not in available]
    if missing:
        fail("Kernel dependency scheduling did not converge; missing outputs: " + str(missing[:20]))
    prefix = additional_params["plan_prefix"] + "/"
    for path, file in available.items():
        if not path.startswith(prefix):
            continue
        relative = path.removeprefix(prefix)
        if relative.startswith(("actions/", "write/")):
            continue
        output = tctx.declare_file(relative, directory = output_directories["kernel"])
        tctx.run(
            executable = tools["python"],
            arguments = [tools["runner"].path, "copy", file.path, output.path],
            inputs = depset([tools["runner"], tools["executor"], file], transitive = [tools["python_files"]]),
            outputs = [output],
            progress_message = "Export kernel artifact " + relative,
        )

def schedule(ctx, literals, metadata, sources, originals, original_prefix, tools, compiler_files, execution_files, steps = 32):
    directories = {"literals": literals, "schedule": metadata, "sources": sources}
    prefixes = {}
    parameters = dict(plan_prefix = literals.path, source_prefix = sources.path, original_prefix = original_prefix, stage_prefixes = "{}")
    tool_files = dict(tools, source_root = sources, compiler_files = compiler_files, execution_files = execution_files)
    for step in range(steps):
        stage = ctx.actions.declare_directory(ctx.label.name + ".stage%d" % step)
        scratch = ctx.actions.declare_directory(ctx.label.name + ".stage-scratch%d" % step)
        ctx.actions.map_directory(
            input_directories = dict(directories),
            output_directories = {"stage": stage, "scratch": scratch},
            additional_inputs = {"originals": originals},
            tools = tool_files,
            additional_params = dict(parameters, stage_prefixes = json.encode(prefixes)),
            implementation = _stage,
            mnemonic = "KernelArtifact",
        )
        scans = ctx.actions.declare_directory(ctx.label.name + ".dependencies%d" % step)
        ctx.actions.run(
            executable = tools["python"],
            arguments = [tools["runner"].path, "scan", stage.path + "/scan-requests.json", scans.path],
            inputs = depset([stage, tools["runner"], tools["executor"]] + directories.values(), transitive = [originals, compiler_files, execution_files]),
            outputs = [scans],
            tools = [tool for name, tool in tools.items() if name.startswith("compiler_tool_")],
            mnemonic = "KernelDependencyScan",
        )
        directories["stage_%d" % step] = stage
        directories["scan_%d" % step] = scans
        prefixes["stage_%d" % step] = stage.path
    result = ctx.actions.declare_directory(ctx.label.name + ".tree")
    ctx.actions.map_directory(
        input_directories = directories,
        output_directories = {"kernel": result},
        additional_inputs = {"originals": originals},
        tools = tool_files,
        additional_params = dict(parameters, stage_prefixes = json.encode(prefixes)),
        implementation = _export,
        mnemonic = "KernelExport",
    )
    return result
