"""Adapt the artifact-oriented kernel backend to action-template callbacks."""
_ASCII = " !\"#$%&'()*+,-./0123456789:;<=>?@ABCDEFGHIJKLMNOPQRSTUVWXYZ[\\]^_`abcdefghijklmnopqrstuvwxyz{|}~"

def read_payload(expanded):
    chunks = sorted([f.basename for f in expanded.children if "=" in f.basename])
    encoded = "".join([s.split("=", 1)[1] for s in chunks])
    text = "".join([_ASCII[int(encoded[i:i + 2], 16) - 32] for i in range(0, len(encoded), 2)])
    return json.decode(text)

def writer(tctx, python, script, root):
    count = []

    # buildifier: disable=unused-variable
    def write(output, content, **kwargs):
        count.append(0)
        if len(content) < 60000:
            tctx.run(executable = python, inputs = [script], outputs = [output], arguments = [script.path, output.path, content])
            return
        parts = []
        for start in range(0, len(content), 240000):
            piece = content[start:start + 240000]
            part = tctx.declare_file("write/%d-%d" % (len(count), start), directory = root)
            tctx.run(executable = python, inputs = [script], outputs = [part], arguments = [script.path, "--chunks", part.path] + [piece[i:i + 16000] for i in range(0, len(piece), 16000)])
            parts.append(part)
        tctx.run(executable = python, inputs = parts + [script], outputs = [output], arguments = [script.path, "--concat", output.path] + [p.path for p in parts])

    return write

def context(tctx, roots, tools, params, planning = False, recorded = None):
    runtime_tools = tools
    virtual = {}
    sequence = []
    write_file = writer(tctx, tools["python"], tools["emit"], roots["scratch"])
    common_paths, common_manifest = {}, None
    if recorded != None:
        common = depset(transitive = [tools["compiler_files"], tools["execution_files"]]).to_list()
        common_paths = {f.path: True for f in common}
        common_manifest = tctx.declare_file("actions/tools.json", directory = roots["kernel"])
        write_file(common_manifest, json.encode(common_paths.keys()))

    # buildifier: disable=unused-variable
    def declare_file(name, **kwargs):
        return tctx.declare_file(name, directory = roots["kernel"])

    def declare_directory(name):
        file = declare_file(name)
        virtual[file.path] = True
        return file

    # buildifier: disable=unused-variable
    def write(output, content, **kwargs):
        if not planning:
            write_file(output, content)

    def args():
        values = []

        # Adapter accepts the actions.args interface.
        # buildifier: disable=unused-variable
        def add_all(items, **kwargs):
            values.extend([x.path if type(x) == "File" else x for x in items])

        def add(item):
            values.append(item.path if type(item) == "File" else item)

        return struct(values = values, add_all = add_all, add = add)

    # buildifier: disable=unused-variable
    def run(executable, outputs, inputs = [], arguments = [], tools = [], env = {}, **kwargs):
        if planning:
            return
        sequence.append(0)
        files = inputs.to_list() if type(inputs) == "depset" else list(inputs)
        flat = []
        for argument in arguments:
            flat.extend(argument.values if type(argument) == "struct" else [argument])
        all_files = files + [x for x in tools if type(x) == "File"] + runtime_tools["compiler_files"].to_list()
        if type(executable) == "File":
            all_files.append(executable)
            executable = executable.path
        spec = dict(
            executable = executable,
            arguments = flat,
            inputs = [f.path for f in all_files],
            outputs = [f.path for f in outputs],
            directory_inputs = [f.path for f in all_files if f.path in virtual],
            directory_outputs = [f.path for f in outputs if f.path in virtual],
            environment = env,
        )
        if recorded != None:
            spec["inputs"] = [path for path in spec["inputs"] if path not in common_paths] + [common_manifest.path]
            spec["common_inputs"] = common_manifest.path
        manifest = tctx.declare_file("actions/%d.json" % len(sequence), directory = roots["kernel"])
        write_file(manifest, json.encode(spec))
        if recorded != None:
            scan = None
            if kwargs.get("mnemonic") in ["KbuildCC", "KbuildAS"]:
                scan = dict(argument_start = 3)
            elif executable in [runtime_tools["host_cc"], runtime_tools.get("host_cxx", ""), runtime_tools.get("target_cc", "")] and ("-c" in flat or "-S" in flat):
                scan = dict(argument_start = 0)
            recorded.append(dict(
                manifest = manifest.path,
                inputs = spec["inputs"],
                outputs = spec["outputs"],
                scan = scan,
                message = kwargs.get("progress_message", kwargs.get("mnemonic", "Kernel action")),
            ))
            return
        tctx.run(
            executable = params.python,
            inputs = depset([manifest, params.executor] + all_files),
            outputs = outputs,
            tools = [tool for name, tool in runtime_tools.items() if name.startswith("compiler_tool_")],
            arguments = [params.executor.path, manifest.path],
            progress_message = kwargs.get("progress_message", "%s %s" % (kwargs.get("mnemonic", "Kernel action"), outputs[0].path)),
        )

    execution = struct(
        commands = tools["commands"],
        host_cc = tools["host_cc"],
        host_compile_env = tools.get("host_compile_env", {}),
        host_link_env = tools.get("host_link_env", {}),
        files = tools["execution_files"],
        tools = [],
        bin_dir = tools["commands"]["sh"].dirname,
        shell = tools["commands"]["sh"],
        python = tools["python"],
        environment = runtime_tools.get("environment", {}),
        path_environment = {},
    )
    return struct(
        actions = struct(declare_file = declare_file, declare_directory = declare_directory, write = write, args = args, run = run),
        execution = execution,
        file = struct(_materialize = params.materialize, _link_script = params.link_script),
        label = struct(name = "kernel"),
        attr = struct(roots = ["vmlinux"], architecture_roots = {}, required_options = {}),
    )
