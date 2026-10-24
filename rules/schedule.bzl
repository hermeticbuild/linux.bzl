"""Schedule measured compiler dependencies before declaring compiler spawn inputs."""

load(":compiler_arguments.bzl", "compiler_arguments")
load(":runtime.bzl", "action_tools", "read_payload")

_ASCII = " !\"#$%&'()*+,-./0123456789:;<=>?@ABCDEFGHIJKLMNOPQRSTUVWXYZ[\\]^_`abcdefghijklmnopqrstuvwxyz{|}~"
_ESCAPES = {c: c for c in _ASCII.elems()} | {"%": "%25", "/": "%2F", "\\": "%5C"}

def write_payload(tctx, root, value, report_value = None):
    text = json.encode(value)

    # Preserve the printable-ASCII JSON domain while escaping filename
    # separators and the escape marker. Keep temporary character lists bounded.
    pieces = []
    for start in range(0, len(text), 180):
        pieces.append("".join([_ESCAPES[c] for c in text[start:start + 180].elems()]))
    encoded = "".join(pieces)
    if (len(encoded) + 179) // 180 > 100000000:
        fail("Payload exceeds the eight-digit chunk range")
    report = tctx.declare_file("report.json", directory = root)
    tctx.write(report, text if report_value == None else json.encode(report_value))
    for start in range(0, len(encoded), 180):
        number = ("00000000" + str(start // 180))[-8:]
        path = "/".join([number[:2], number[2:4], number[4:6], number + "=" + encoded[start:start + 180]])
        tctx.write(tctx.declare_file(path, directory = root), "")

def encode_schedule(jobs):
    """Share repeated input strings while preserving each complete ordered list."""
    paths, records = {}, []
    for job in jobs:
        inputs = []
        for path in job["inputs"]:
            if path not in paths:
                paths[path] = len(paths)
            inputs.append(paths[path])
        records.append(dict(job, inputs = inputs))
    return dict(paths = paths.keys(), jobs = records)

def decode_schedule(value):
    """Restore the ordinary records before any readiness or dependency checks."""
    paths = value["paths"]
    jobs = []
    for job in value["jobs"]:
        if any([type(index) != "int" or index < 0 or index >= len(paths) for index in job["inputs"]]):
            fail("Kernel schedule input index is outside its path table")
        jobs.append(dict(job, inputs = [paths[index] for index in job["inputs"]]))
    return jobs

def write_schedule(tctx, root, jobs):
    """Keep the full report while compacting only the callback transport."""
    write_payload(tctx, root, encode_schedule(jobs), report_value = jobs)

def _read_schedule(expanded):
    return decode_schedule(read_payload(expanded))

def _available(input_directories, tools, additional_inputs, params):
    available = {f.path: f for f in input_directories["literals"].children}
    available.update({f.path: f for f in input_directories["config"].children})
    available[tools["config_root"].path] = tools["config_root"]
    for name in ["execution_files", "compiler_files", "backend_files"]:
        available.update({f.path: f for f in tools[name].to_list()})
    available.update({f.path: f for f in additional_inputs["originals"].to_list()})
    available.update({tool.path: tool for tool in tools.values() if type(tool) == "File"})
    for tool in tools.values():
        executable = getattr(tool, "executable", None)
        if executable != None:
            available[executable.path] = executable
    available[params["source_prefix"]] = tools["source_root"]
    for name, prefix in json.decode(params["stage_prefixes"]).items():
        for file in input_directories[name].children:
            if file.path.startswith(prefix + "/artifacts/"):
                available[params["plan_prefix"] + "/" + file.path.removeprefix(prefix + "/artifacts/")] = file
    return available

def _is_backend(job):
    return job.get("compiler", {}).get("phase") == "backend"

def _scan_metadata(expanded, prefix):
    """Index per-job chunks without decoding or retaining every dependency list."""
    paths, jobs = [], {}
    for file in expanded.children:
        if "=" not in file.basename:
            continue
        relative = file.path.removeprefix(prefix + "/")
        if relative.startswith("paths/"):
            paths.append(file)
        elif relative.startswith("jobs/"):
            identity = relative.split("/", 2)[1]
            jobs.setdefault(identity, []).append(file)
        else:
            fail("Unknown dependency metadata path: " + file.path)
    return struct(paths = read_payload(struct(children = paths)), jobs = jobs)

def _scan_dependencies(metadata, identity):
    value = read_payload(struct(children = metadata.jobs[identity]))
    result = {}
    for kind in ["sources", "generated"]:
        indices = value[kind]
        if any([type(index) != "int" or index < 0 or index >= len(metadata.paths) for index in indices]):
            fail("Compiler dependency index is outside its path table")
        result[kind] = [metadata.paths[index] for index in indices]
    return result

def _scan_inputs(job, available, source_files, source_prefix, original_prefix):
    """Separate candidate contents from the mandatory compiler namespace."""
    scan = job["scan"]
    generated = scan.get("generated", {})
    mandatory = {path: True for path in scan.get("mandatory", [])}
    header_tree = scan.get("header_tree")
    narrowed = _compiler_inputs(job, dict(sources = [], generated = []), available, {}, {}, source_prefix)

    def prepared(file):
        if file.path.startswith(original_prefix + "/"):
            return source_files[file.path.removeprefix(original_prefix + "/")]
        return file

    candidates = {}
    inputs = {file.path: prepared(file) for file in narrowed.inputs}
    aliases = {path: inputs[actual].path for path, actual in narrowed.aliases.items()}
    aliases.update({source_prefix: source_prefix, original_prefix: source_prefix})
    for path in generated.values():
        file = prepared(available[path])
        aliases[path] = file.path
        if path not in mandatory:
            if file.is_directory:
                fail("Compiler discovery requires individual generated Files: " + path)
            candidates[file.path] = file
    if header_tree != None:
        for logical, path in generated.items():
            if not logical or logical == "." or logical.startswith("/") or ".." in logical.split("/"):
                fail("Generated compiler input escapes its header tree: " + logical)
            aliases[header_tree + "/" + logical] = prepared(available[path]).path
    return struct(inputs = inputs.values(), candidates = candidates.values(), aliases = aliases, directories = narrowed.empty_directories)

def _compiler_inputs(job, dependencies, available, source_files, originals, source_prefix):
    """Replace the scan's generated closure with measured and mandatory Files."""
    scan = job["scan"]
    generated = scan.get("generated", {})
    header_tree = scan.get("header_tree")
    mandatory = scan.get("mandatory", [])
    if header_tree != None:
        if "generated" not in scan or "mandatory" not in scan or "directories" not in scan:
            fail("Compiler header namespace requires its complete generated mapping and mandatory inputs")
        if any([path not in job["inputs"] for path in generated.values()]):
            fail("Compiler generated mapping contains a File outside the scan input closure")
    elif generated or mandatory:
        fail("Generated compiler dependency pruning requires a header tree namespace")
    removed = {path: True for path in generated.values()}
    removed[source_prefix] = True
    if header_tree != None:
        removed[header_tree] = True
    selected = {path: True for path in job["inputs"] if path not in removed}
    for path in mandatory:
        if path == source_prefix or path == header_tree or path not in available:
            fail("Mandatory compiler input is not an available individual File: " + path)
        if available[path].is_directory:
            fail("Mandatory compiler input must not retain an aggregate tree: " + path)
        selected[path] = True
    measured = {}
    for logical in dependencies["generated"]:
        if logical not in generated:
            fail("Compiler dependency was not declared as a generated input: " + logical)
        measured[logical] = True
        selected[generated[logical]] = True
    if header_tree != None:
        aggregates = [path for path in selected if available[path].is_directory]
        if aggregates:
            fail("Pruned compiler inputs retain an aggregate directory: " + str(aggregates))
    aliases = {path: available[path].path for path in selected}
    empty_directories = []
    if header_tree != None:
        empty_directories.append(header_tree)
        for directory in scan["directories"]:
            if directory.startswith("/") or ".." in directory.split("/"):
                fail("Compiler working directory escapes its header tree: " + directory)
            empty_directories.append(header_tree + "/" + directory)
        for logical, path in generated.items():
            # .incbin and other native prerequisites need their logical names
            # even when preprocessing does not report them as included files.
            if logical not in measured and path not in mandatory:
                continue
            if not logical or logical == "." or logical.startswith("/") or ".." in logical.split("/"):
                fail("Generated compiler input escapes its header tree: " + logical)
            aliases[header_tree + "/" + logical] = available[path].path
    source_inputs = []
    for logical in dependencies["sources"]:
        if logical not in source_files or logical not in originals:
            fail("Compiler dependency was not declared as a kernel source: " + logical)
        source_inputs.extend([source_files[logical], originals[logical]])
    return struct(inputs = [available[path] for path in selected], source_inputs = source_inputs, aliases = aliases, empty_directories = empty_directories)

# Fixed map_directory callback signature.
# buildifier: disable=unused-variable
def _stage(tctx, input_directories, output_directories, tools, additional_inputs, additional_params):
    params = additional_params
    jobs = _read_schedule(input_directories["schedule"])
    available = _available(input_directories, tools, additional_inputs, params)
    source_files = {f.path.removeprefix(params["source_prefix"] + "/"): f for f in input_directories["sources"].children}
    originals = {f.path.removeprefix(params["original_prefix"] + "/"): f for f in additional_inputs["originals"].to_list()}
    scanned = _scan_metadata(input_directories["scans"], params["scan_prefix"]) if "scans" in input_directories else struct(paths = [], jobs = {})
    produced = {path: True for job in jobs for path in job["outputs"]}
    unknown = {path: True for job in jobs for path in job["inputs"] if path not in available and path not in produced}
    if unknown:
        fail("Kernel graph inputs were not supplied as sources, tools or artifacts: " + str(unknown.keys()[:20]))
    for index, job in enumerate(jobs):
        if _is_backend(job):
            continue
        if all([path in available for path in job["outputs"]]):
            continue
        if any([path not in available for path in job["inputs"]]):
            continue
        aliases = {path: available[path].path for path in job["inputs"]}
        if job["scan"] != None and str(index) not in scanned.jobs:
            selected = _scan_inputs(job, available, source_files, params["source_prefix"], params["original_prefix"])
            request = tctx.declare_file("scan-requests/%d.json" % index, directory = output_directories["stage"])
            tctx.write(request, json.encode(dict(manifest = job["manifest"], scan = job["scan"], aliases = selected.aliases, empty_directories = selected.directories, source_prefix = params["source_prefix"], original_prefix = params["original_prefix"])))
            continue
        inputs = [available[path] for path in job["inputs"]]
        source_inputs = []
        empty_directories = []
        if job["scan"] != None:
            narrowed = _compiler_inputs(job, _scan_dependencies(scanned, str(index)), available, source_files, originals, params["source_prefix"])
            inputs = narrowed.inputs
            source_inputs = narrowed.source_inputs
            aliases = narrowed.aliases
            empty_directories = narrowed.empty_directories
        output_files = [tctx.declare_file("artifacts/" + path.removeprefix(params["plan_prefix"] + "/"), directory = output_directories["stage"]) for path in job["outputs"]]
        manifest = tctx.declare_file("actions/%d.json" % index, directory = output_directories["stage"])
        spec = dict(
            manifest = job["manifest"],
            aliases = aliases,
            source_inputs = [f.path for f in source_inputs],
            empty_directories = empty_directories,
            outputs = {path: f.path for path, f in zip(job["outputs"], output_files)},
        )
        tctx.write(manifest, json.encode(spec))
        tctx.run(
            executable = tools["python"],
            arguments = [tools["runner"].path, "execute", manifest.path],
            inputs = depset(inputs + source_inputs + [manifest, available[job["manifest"]], tools["runner"], tools["executor"], tools["payload"]], transitive = [tools["compiler_files"], tools["execution_files"]]),
            outputs = output_files,
            tools = action_tools(tools),
            progress_message = job["message"],
        )
        available.update({path: f for path, f in zip(job["outputs"], output_files)})

# Fixed map_directory callback signature.
# buildifier: disable=unused-variable
def _scan(tctx, input_directories, output_directories, tools, additional_inputs, additional_params):
    params = additional_params
    available = _available(input_directories, tools, additional_inputs, params)
    source_files = {f.path.removeprefix(params["source_prefix"] + "/"): f for f in input_directories["sources"].children}
    source_candidates = depset(input_directories["sources"].children)
    requests = {file.basename.removesuffix(".json"): file for file in input_directories[params["request_directory"]].children if file.path.startswith(params["request_prefix"] + "/scan-requests/")}
    for index, job in enumerate(_read_schedule(input_directories["schedule"])):
        if str(index) not in requests:
            continue
        selected = _scan_inputs(job, available, source_files, params["source_prefix"], params["original_prefix"])
        request = requests[str(index)]
        dependencies = tctx.declare_file("%d/dependencies.json" % index, directory = output_directories["scans"])
        used = tctx.declare_file("%d/used-inputs" % index, directory = output_directories["scans"])
        tctx.run(
            executable = tools["python"],
            arguments = [tools["runner"].path, "scan", request.path, dependencies.path, used.path],
            inputs = depset(selected.inputs + [request, available[job["manifest"]], tools["runner"], tools["executor"], tools["payload"]], transitive = [tools["compiler_files"], tools["execution_files"]]),
            discovery_inputs = depset(selected.candidates, transitive = [source_candidates]),
            used_inputs_list = used,
            outputs = [dependencies, used],
            tools = action_tools(tools),
            progress_message = "Discover dependencies: " + job["message"],
        )

# Fixed map_directory callback signature.
# buildifier: disable=unused-variable
def _backend(tctx, input_directories, output_directories, tools, additional_inputs, additional_params):
    """Compile ready IR directly; only this template supports path mapping."""
    params = additional_params
    available = _available(input_directories, tools, additional_inputs, params)
    environment = json.decode(params["backend_environment"])
    prefix = params["plan_prefix"] + "/"
    for job in _read_schedule(input_directories["schedule"]):
        if not _is_backend(job):
            continue
        if job["scan"] != None or len(job["outputs"]) != 1:
            fail("An LLVM backend job must have one raw object output and no source scan")
        if all([path in available for path in job["outputs"]]):
            continue
        if any([path not in available for path in job["inputs"]]):
            continue
        compiler = job["compiler"]
        if compiler["environment"] != environment:
            fail("LLVM backend environment differs from its action template")
        executable = available.get(compiler["executable"])
        if executable == None or executable.is_directory:
            fail("LLVM backend compiler has no declared executable File: " + compiler["executable"])
        path = job["outputs"][0]
        if not path.startswith(prefix):
            fail("LLVM backend output is outside the planned kernel tree: " + path)
        output = tctx.declare_file("artifacts/" + path.removeprefix(prefix), directory = output_directories["backend"])
        args = compiler_arguments(tctx, compiler["arguments"], available, {path: output}, [params["plan_prefix"], params["source_prefix"], params["original_prefix"]])
        tctx.run(
            executable = executable,
            arguments = [args],
            inputs = depset([available[path] for path in job["inputs"]], transitive = [tools["backend_files"]]),
            tools = [tool for name, tool in tools.items() if name.startswith("backend_tool_")],
            outputs = [output],
            progress_message = job["message"],
        )

        # Keep raw objects unavailable to the current native stage. Its next
        # round receives this tree before scheduling postprocessing, archives,
        # links or generated sources that depend on completed backend objects.

# Fixed map_directory callback signature.
# buildifier: disable=unused-variable
def _export(tctx, input_directories, output_directories, tools, additional_inputs, additional_params):
    available = _available(input_directories, tools, additional_inputs, additional_params)
    jobs = _read_schedule(input_directories["schedule"])
    missing = [path for job in jobs for path in job["outputs"] if path not in available]
    if missing:
        fail("Kernel dependency scheduling did not converge; missing outputs: " + str(missing[:20]))
    scans = {}
    for name, root in json.decode(additional_params["scan_outputs"]).items():
        for file in input_directories[name].children:
            identity, _, kind = file.path.removeprefix(root + "/").partition("/")
            if kind not in ["dependencies.json", "used-inputs"] or not identity.isdigit():
                fail("Unexpected compiler scan output: " + file.path)
            outputs = scans.setdefault(identity, {})
            if kind in outputs:
                fail("Compiler job was scanned more than once: " + identity)
            outputs[kind] = file.path
    expected = {str(index): job for index, job in enumerate(jobs) if job["scan"] != None}
    if sorted(scans) != sorted(expected) or any([sorted(outputs) != ["dependencies.json", "used-inputs"] for outputs in scans.values()]):
        fail("Compiler dependency scan inventory differs from the scheduled jobs")
    compiler_scans = [dict(
        compiler = dict(
            {name: job["compiler"][name] for name in ["phase", "target", "kind"]},
            source = available[job["compiler"]["source"]].path,
        ) if job["compiler"] else {},
        compiler_outputs = [available[path].path for path in job["outputs"]],
        outputs = [scans[identity][kind] for kind in ["dependencies.json", "used-inputs"]],
    ) for identity, job in expected.items()]
    prefix = additional_params["plan_prefix"] + "/"
    mappings = tctx.declare_file("scheduled-paths.json", directory = output_directories["kernel"])
    tctx.write(mappings, json.encode(dict(paths = {path: file.path for path, file in available.items()}, compiler_jobs = [job for job in jobs if job.get("compiler")], compiler_scans = compiler_scans)))
    for path, file in available.items():
        if not path.startswith(prefix):
            continue
        relative = path.removeprefix(prefix)
        if relative.startswith("actions/"):
            continue
        output = tctx.declare_file(relative, directory = output_directories["kernel"])
        tctx.run(
            executable = tools["python"],
            arguments = [tools["runner"].path, "manifest", file.path, mappings.path, output.path] if relative == "kernel.json" else [tools["runner"].path, "copy", file.path, output.path],
            inputs = depset([tools["runner"], tools["executor"], tools["payload"], file] + ([mappings] if relative == "kernel.json" else []), transitive = [tools["python_files"]]),
            outputs = [output],
            progress_message = "Export kernel artifact " + relative,
        )

def schedule(ctx, literals, metadata, sources, originals, original_prefix, tools, compiler_files, execution_files, backend_files, backend_tools, backend_environment, config, steps = 32):
    directories = {"literals": literals, "schedule": metadata, "sources": sources, "config": config}
    prefixes = {}
    scan_directories, scan_outputs = {}, {}
    previous_scans = None
    parameters = dict(plan_prefix = literals.path, source_prefix = sources.path, original_prefix = original_prefix, stage_prefixes = "{}", backend_environment = json.encode(backend_environment))
    tool_files = dict(tools, source_root = sources, config_root = config, compiler_files = compiler_files, execution_files = execution_files, backend_files = backend_files)
    tool_files.update({"backend_tool_%d" % index: tool for index, tool in enumerate(backend_tools)})
    for step in range(steps):
        stage = ctx.actions.declare_directory(ctx.label.name + ".stage%d" % step)
        ctx.actions.map_directory(
            input_directories = dict(directories, **({"scans": previous_scans} if previous_scans != None else {})),
            output_directories = {"stage": stage},
            additional_inputs = {"originals": originals},
            tools = tool_files,
            additional_params = dict(parameters, stage_prefixes = json.encode(prefixes), scan_prefix = previous_scans.path if previous_scans != None else ""),
            implementation = _stage,
            mnemonic = "KernelArtifact",
        )
        directories["stage_%d" % step] = stage
        prefixes["stage_%d" % step] = stage.path
        scans = ctx.actions.declare_directory(ctx.label.name + ".scans%d" % step)
        ctx.actions.map_directory(
            input_directories = dict(directories),
            output_directories = {"scans": scans},
            additional_inputs = {"originals": originals},
            tools = tool_files,
            additional_params = dict(parameters, stage_prefixes = json.encode(prefixes), request_directory = "stage_%d" % step, request_prefix = stage.path),
            implementation = _scan,
            mnemonic = "KernelDependencyScan",
        )
        scan_directories["scan_outputs_%d" % step] = scans
        scan_outputs["scan_outputs_%d" % step] = scans.path
        previous_scans = ctx.actions.declare_directory(ctx.label.name + ".dependencies%d" % step)
        ctx.actions.run(
            executable = tools["python"],
            arguments = [tools["runner"].path, "collect-scans", scans.path, previous_scans.path],
            inputs = depset([scans, tools["runner"], tools["executor"], tools["payload"]], transitive = [tools["python_files"]]),
            outputs = [previous_scans],
            mnemonic = "KernelDependencyMetadata",
        )
        backend = ctx.actions.declare_directory(ctx.label.name + ".backend%d" % step)
        ctx.actions.map_directory(
            input_directories = dict(directories),
            output_directories = {"backend": backend},
            additional_inputs = {"originals": originals},
            tools = tool_files,
            additional_params = dict(parameters, stage_prefixes = json.encode(prefixes)),
            implementation = _backend,
            mnemonic = "KernelLLVMBackend",
            execution_requirements = {"supports-path-mapping": "1"},
            env = backend_environment,
        )
        directories["backend_%d" % step] = backend
        prefixes["backend_%d" % step] = backend.path
    result = ctx.actions.declare_directory(ctx.label.name + ".tree")
    directories.update(scan_directories)
    ctx.actions.map_directory(
        input_directories = directories,
        output_directories = {"kernel": result},
        additional_inputs = {"originals": originals},
        tools = tool_files,
        additional_params = dict(parameters, stage_prefixes = json.encode(prefixes), scan_outputs = json.encode(scan_outputs)),
        implementation = _export,
        mnemonic = "KernelExport",
    )
    manifest = ctx.actions.declare_file(ctx.label.name + ".manifest.json")
    ctx.actions.run(executable = tools["python"], arguments = [tools["runner"].path, "copy", result.path + "/kernel.json", manifest.path], inputs = depset([result, tools["runner"], tools["executor"], tools["payload"]], transitive = [tools["python_files"]]), outputs = [manifest], mnemonic = "KernelManifest")
    return struct(directory = result, manifest = manifest)
