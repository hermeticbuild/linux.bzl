"""Exercise native generator exports through the older compile.h script."""

load("//execution_tools:defs.bzl", "EXECUTION_TOOLS_TYPE")
load("//rules:commands.bzl", "native_codegen", "shell_quote")
load("//rules:compiler.bzl", "KernelCompilerInfo")
load("//rules:execution_tools.bzl", "run_recipe")

def _impl(ctx):
    execution = ctx.toolchains[EXECUTION_TOOLS_TYPE].execution
    compiler = ctx.attr.compiler[KernelCompilerInfo]
    scripts = [file for file in ctx.files.sources if file.path.endswith("/scripts/mkcompile_h")]
    if len(scripts) != 1:
        fail("Expected one native mkcompile_h script, got %s" % scripts)
    script = scripts[0]
    config = ctx.actions.declare_file(ctx.label.name + ".config")
    ctx.actions.write(config, "")
    header = ctx.actions.declare_file(ctx.label.name + "/compile.h")
    observed = ctx.actions.declare_file(ctx.label.name + "/exports")
    expected = ctx.actions.declare_file(ctx.label.name + "/expected")
    checked = ctx.actions.declare_file(ctx.label.name + "/checked")
    source = script.dirname.removesuffix("/scripts")
    stamp = "Thu Jan 1 00:00:00 UTC 1970"
    variables = {
        "KBUILD_BUILD_TIMESTAMP": {"value": stamp},
        "KBUILD_BUILD_VERSION": {"value": "17"},
        "DECLARED_NATIVE_VALUE": {"value": "before target assignment"},
        "PRIVATE_NATIVE_VALUE": {"value": "must not be exported"},
        "LD": {"value": "unselected-linker"},
        "__exports__": {name: True for name in ["KBUILD_BUILD_TIMESTAMP", "KBUILD_BUILD_VERSION", "DECLARED_NATIVE_VALUE", "LD"]},
    }
    native = {
        "function": "script",
        "scripts": [[dict(op = "literal", value = command)] for command in [
            '"${CONFIG_SHELL}" scripts/mkcompile_h compile.h x86_64 y "" "" "fixture compiler" "${LD}"',
            'printf "%s\\n" "${DECLARED_NATIVE_VALUE-unset}" "${PRIVATE_NATIVE_VALUE-unset}" > exports',
        ]],
        "arguments": [],
        "variables": variables,
        "prerequisites": [],
        "stem": "",
        "assignments": [dict(
            kind = "target_assignment",
            targets_expression = [dict(op = "literal", value = "compile.h")],
            name_expression = [dict(op = "literal", value = "DECLARED_NATIVE_VALUE")],
            value_expression = [dict(op = "literal", value = "selected __source__/input __object__/output")],
            value = "",
            flavor = "simple",
        )],
    }
    tools = dict(compiler.roles, CONFIG_SHELL = execution.shell.path)
    setup = struct(tools = tools, make_tools = tools, cc_arguments = [tools["CC"]], files = compiler.files, sources = {}, program = {"image_command_helpers": []}, driver_tools = compiler.roles["CC"].rpartition("/")[0])
    native_codegen(
        struct(actions = ctx.actions, label = ctx.label, file = ctx.file, execution = execution),
        "compile.h",
        {"compile.h": header, "exports": observed},
        {"scripts/mkcompile_h": script},
        depset([script]),
        source,
        struct(config = config, auto_conf = config),
        setup,
        native,
    )
    ctx.actions.write(expected, "selected " + source + "/input ./output\nunset\n")
    run_recipe(
        ctx,
        execution,
        command = "grep '^#define UTS_VERSION' %s\ngrep -Fqx %s %s\ncmp %s %s\nprintf passed > %s" % (shell_quote(header.path), shell_quote('#define UTS_VERSION "#17 SMP ' + stamp + '"'), shell_quote(header.path), shell_quote(expected.path), shell_quote(observed.path), shell_quote(checked.path)),
        inputs = [header, observed, expected],
        outputs = [checked],
        mnemonic = "NativeExportsCheck",
    )
    return [DefaultInfo(files = depset([header, observed, checked]))]

native_exports_fixture = rule(
    implementation = _impl,
    toolchains = [EXECUTION_TOOLS_TYPE],
    attrs = {
        "compiler": attr.label(default = "//:compiler", providers = [KernelCompilerInfo]),
        "sources": attr.label(default = "@linux_5_10//:sources"),
        "_link_script": attr.label(default = "//:emit.py", allow_single_file = True),
        "_materialize": attr.label(default = "//rules:materialize.py", allow_single_file = True),
        "_native_recipe": attr.label(default = "//:native_recipe.py", allow_single_file = True),
    },
)
