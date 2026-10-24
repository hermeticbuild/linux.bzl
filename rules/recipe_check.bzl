"""Execute the source-selected size/LZ4 recipe and Make capture controls."""

load(":commands.bzl", "native_recipe", "shell_quote", "value")
load(":execution_tools.bzl", "execution_tools", "run_recipe")
load(":expressions.bzl", "evaluate", "program")

def native_recipe_check(ctx, setup, source_files):
    execution = execution_tools(ctx)
    outputs = {name: ctx.actions.declare_file("kernel.validation/recipe/" + name) for name in [
        "staged",
        "trailer",
        "report.json",
        "marker",
        "expanded",
        "compressed",
        "frame",
        "restored",
    ]}
    payload = ctx.actions.declare_file("kernel.validation/recipe/payload")
    ctx.actions.write(payload, "x" * 257)
    size_script = source_files["scripts/file-size.sh"]
    variables = {name: {"value": text} for name, text in {
        "real-prereqs": outputs["staged"].path,
        "@": outputs["trailer"].path,
        "CONFIG_SHELL": setup.tools["CONFIG_SHELL"],
        "LZ4": setup.tools["LZ4"],
        "srctree": size_script.path.removesuffix("/scripts/file-size.sh"),
    }.items()}
    sources = {"__shell_commands__": []}
    definitions = [node for node in setup.program["image_command_helpers"] if node.get("name") in ["size_append", "cmd_file_size", "cmd_lz4", "cmd_lz4_with_size"]]
    variables = program(definitions, variables, sources, cells = True).variables
    if "size_append" not in variables:
        fail("Native recipe check requires the source-owned size_append producer")
    command = value("cmd_file_size", variables, sources) if "cmd_file_size" in variables else value("size_append", variables, sources) + " > " + shell_quote(outputs["trailer"].path)
    files = []
    invocation = native_recipe(ctx, "recipe-check-size", [{"command": command, "strict": True, "ignore_errors": False}], sources, setup, files)
    deferred = {"__shell_commands__": []}
    slots = [evaluate([{"op": "function", "name": "shell", "arguments": [[{"op": "literal", "value": text}]]}], {}, deferred) for text in [
        "printf '%s' '\"quoted value\"'; printf '\\n\\n'; false",
        "test -e " + shell_quote(outputs["marker"].path) + " && printf yes || printf no",
    ]]
    lines = [
        {"command": "touch " + shell_quote(outputs["marker"].path), "strict": False, "ignore_errors": False},
        {"command": "false", "strict": False, "ignore_errors": True},
        {"command": "printf '%s|%s|%s' " + slots[0] + " " + slots[1] + " '$literal' > " + shell_quote(outputs["expanded"].path), "strict": False, "ignore_errors": False},
    ]
    invocation += "\n" + native_recipe(ctx, "recipe-check-lines", lines, deferred, setup, files)
    variables["@"] = {"value": outputs["compressed"].path}
    compression_sources = {"__shell_commands__": []}
    compression = value("cmd_lz4_with_size" if "cmd_lz4_with_size" in variables else "cmd_lz4", variables, compression_sources)
    invocation += "\n" + native_recipe(ctx, "recipe-check-lz4", [{"command": compression, "strict": True, "ignore_errors": False}], compression_sources, setup, files)
    checker = [execution.python.path, "-B", ctx.file._recipe_check.path]
    frame = " ".join([shell_quote(word) for word in checker + ["frame", outputs["compressed"].path, outputs["frame"].path]])
    check = " ".join([shell_quote(word) for word in checker + ["check", payload.path, outputs["trailer"].path, outputs["expanded"].path, outputs["restored"].path, setup.tools["LZ4"], outputs["report.json"].path]])
    run_recipe(
        ctx,
        execution,
        # file-size.sh must measure the regular staged input, not its sandbox link.
        command = "cp -L " + shell_quote(payload.path) + " " + shell_quote(outputs["staged"].path) + "\n" + invocation + "\n" + frame + "\n" +
                  shell_quote(execution.commands["lz4"].path) + " -dc " + shell_quote(outputs["frame"].path) + " > " + shell_quote(outputs["restored"].path) + "\n" + check,
        inputs = [payload, size_script, ctx.file._recipe_check] + files,
        outputs = outputs.values(),
        mnemonic = "NativeShellCheck",
    )
    return {role: outputs[name] for role, name in {
        "report": "report.json",
        "compressed": "compressed",
        "expanded": "expanded",
        "restored": "restored",
    }.items()}
