"""Real action-template argv overflow and path-mapping regression fixtures."""

load("//internal:template_arguments.bzl", "template_arguments")

def _payload_impl(ctx):
    output = ctx.actions.declare_file(ctx.label.name + ".txt")
    ctx.actions.write(output, ctx.var["COMPILATION_MODE"] + "\n")
    return [DefaultInfo(files = depset([output]))]

transport_payload = rule(implementation = _payload_impl)

def _split_impl(_settings, _attr):
    return {
        "first": {"//command_line_option:compilation_mode": "fastbuild"},
        "second": {"//command_line_option:compilation_mode": "opt"},
    }

_split = transition(implementation = _split_impl, inputs = [], outputs = ["//command_line_option:compilation_mode"])

def _expand(template_ctx, input_directories, output_directories, additional_inputs, tools, additional_params):
    if additional_params:
        fail("argument transport fixture takes no additional parameters")
    output = template_ctx.declare_file("copied.txt", directory = output_directories["out"])
    args = template_arguments(template_ctx)
    args.add("-copy_tree_file")
    args.add("-tree")
    args.add_all([input_directories["tree"].directory], expand_directories = False)
    args.add("-path", "payload.txt")
    args.add("-output", output)

    # >3 MiB before path arguments: exceeds a Linux execve argv limit. Preserve
    # each individual argument, including values which resemble protocol flags.
    for ordinal in range(15000):
        args.add("-action_arg", "%d:" % ordinal + "x" * 240)
    for value in ["", "@literal", "-argument_chunks", "a=b=c", "line\nwith\ttabs\r", "duplicate", "duplicate"]:
        args.add("-action_arg", value)
    for key in sorted(additional_inputs):
        args.add_joined("-action_arg", [key + "=", additional_inputs[key], "=suffix"], expand_directories = False, join_with = "")
    inputs = depset([input_directories["tree"].directory] + additional_inputs.values())
    transport = args.finish(tools["runner"], output_directories["out"], "fixture", inputs, [])
    template_ctx.run(
        executable = tools["runner"],
        inputs = depset(transport.inputs, transitive = [inputs]),
        outputs = [output],
        arguments = transport.arguments,
        progress_message = "Checking large Linux argument transport",
    )

def _fixture_impl(ctx):
    first = ctx.split_attr.payload["first"][DefaultInfo].files.to_list()[0]
    second = ctx.split_attr.payload["second"][DefaultInfo].files.to_list()[0]
    tree = ctx.actions.declare_directory(ctx.label.name + ".input")
    args = ctx.actions.args()
    args.add("-tree_out")
    args.add_all([tree], expand_directories = False)
    args.add_all([first], format_each = "-copy=payload.txt=%s")
    ctx.actions.run(
        executable = ctx.executable._actionfile,
        inputs = [first],
        outputs = [tree],
        arguments = [args],
    )
    output = ctx.actions.declare_directory(ctx.label.name + ".output")
    ctx.actions.map_directory(
        input_directories = {"tree": tree},
        output_directories = {"out": output},
        additional_inputs = dict({"first": first}, **({"second": second} if ctx.attr.collision else {})),
        tools = {"runner": ctx.executable._runner},
        implementation = _expand,
        execution_requirements = {"supports-path-mapping": "1"},
        mnemonic = "LinuxArgumentTransportTest",
    )
    return [DefaultInfo(files = depset([output]), runfiles = ctx.runfiles(files = [output]))]

argument_transport_fixture = rule(
    implementation = _fixture_impl,
    attrs = {
        "collision": attr.bool(),
        "payload": attr.label(cfg = _split, mandatory = True),
        "_actionfile": attr.label(default = "//internal/cmd/actionfile", executable = True, cfg = "exec"),
        "_runner": attr.label(default = "//internal/cmd/mapdirectoryrecipe", executable = True, cfg = "exec"),
        "_allowlist_function_transition": attr.label(default = "@bazel_tools//tools/allowlists/function_transition_allowlist"),
    },
)
