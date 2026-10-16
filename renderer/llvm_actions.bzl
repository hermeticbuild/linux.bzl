"""Individual LLVM actions with analysis-resolved flags and configuration."""

load(":commands.bzl", "register")
load(":rules.bzl", "KernelConfigInfo")
load(":toolchain.bzl", "KernelToolchainInfo")

KbuildActionInfo = provider(doc = "Kbuild Action Info.", fields = ["identity", "artifacts", "members"])
_ACTIVE_CONFIG = str(Label("//:active_config"))

def _action_impl(ctx):
    config = ctx.attr._config[KernelConfigInfo]
    setup = ctx.attr._setup[KernelToolchainInfo]
    if setup.identity != config.identity:
        fail("Mixed toolchain setup configuration")
    dependencies = {"include/generated/autoconf.h": config.header}
    members = []
    for dep in ctx.attr.deps:
        info = dep[KbuildActionInfo]
        if info.identity != config.identity:
            fail("Mixed kernel configurations: %s != %s" % (info.identity, config.identity))
        for path, artifact in info.artifacts.items():
            if path in dependencies and dependencies[path] != artifact:
                fail("Multiple producers for Kbuild artifact: " + path)
            dependencies[path] = artifact
        members.extend(info.members)
    artifacts = {path: ctx.actions.declare_file(ctx.label.name + ".artifacts/" + path) for path in ctx.attr.outs}
    register(
        ctx,
        ctx.attr.target,
        ctx.attr.directory,
        {"KbuildHost": "host", "KbuildCompile": "compile"}.get(ctx.attr.mnemonic, "generate"),
        ctx.attr.driver,
        artifacts,
        dependencies,
        ctx.attr._source_tree[DefaultInfo].files,
        ctx.file._source_anchor.dirname,
        config,
        setup,
        [dep[KbuildActionInfo].members[-1].target for dep in ctx.attr.deps if dep[KbuildActionInfo].members[-1].target.endswith(".o")] if ctx.attr.mnemonic == "KbuildHost" and not ctx.attr.target.endswith(".o") else [],
    )
    members.append(struct(
        label = str(ctx.label),
        target = ctx.attr.target,
        identity = config.identity,
        outputs = {p: f.path for p, f in artifacts.items()},
        dependencies = sorted(dependencies),
    ))
    merged = dict(dependencies)
    merged.update(artifacts)
    return [
        KbuildActionInfo(identity = config.identity, artifacts = merged, members = members),
        DefaultInfo(files = depset(artifacts.values())),
    ]

_kbuild_action = rule(
    implementation = _action_impl,
    attrs = {
        "target": attr.string(mandatory = True),
        "directory": attr.string(mandatory = True),
        "outs": attr.string_list(mandatory = True),
        "deps": attr.label_list(providers = [KbuildActionInfo]),
        "driver": attr.string(default = "build", values = ["build", "asm_headers", "arch"]),
        "mnemonic": attr.string(default = "KbuildGenerate"),
        "_config": attr.label(default = "//:active_config", providers = [KernelConfigInfo]),
        "_materialize": attr.label(default = "//rules:materialize.py", allow_single_file = True, cfg = "exec"),
        "_source_tree": attr.label(default = "//:source_tree"),
        "_source_anchor": attr.label(default = "//:source/Makefile", allow_single_file = True),
        "_setup": attr.label(default = "//:toolchain", providers = [KernelToolchainInfo]),
    },
)

def kbuild_action(name, tags = [], **kwargs):
    # These are configured implementation nodes. Wildcard builds start at the
    # kernel roots, which apply the configuration transition to their closure.
    _kbuild_action(name = name, tags = tags + (["manual"] if "manual" not in tags else []), **kwargs)

# Bazel callback signatures include inputs this implementation does not use.
# buildifier: disable=unused-variable
def _select_config(settings, attr):
    return {_ACTIVE_CONFIG: str(attr.config)}

_transition = transition(implementation = _select_config, inputs = [], outputs = [_ACTIVE_CONFIG])

def _build_impl(ctx):
    config = ctx.attr.config[KernelConfigInfo]
    if config.identity != ctx.attr._config[KernelConfigInfo].identity:
        fail("Root config did not propagate")
    artifacts = {}
    members = {}
    for dep in ctx.attr.deps:
        info = dep[KbuildActionInfo]
        if info.identity != config.identity:
            fail("Mixed kernel configurations")
        for path, artifact in info.artifacts.items():
            if path in artifacts and artifacts[path] != artifact:
                fail("Multiple producers for Kbuild artifact: " + path)
            artifacts[path] = artifact
        for member in info.members:
            members[member.label] = member
    out = ctx.actions.declare_file(ctx.label.name + ".json")
    ctx.actions.write(out, json.encode({
        "identity": config.identity,
        "config": config.config.path,
        "config_report": config.report.path,
        "artifacts": {p: f.path for p, f in artifacts.items()},
        "members": members.values(),
        "setup": ctx.attr._setup[KernelToolchainInfo].file.path,
    }))
    return [DefaultInfo(files = depset([out, config.report, ctx.attr._setup[KernelToolchainInfo].file] + artifacts.values()))]

kbuild_build = rule(
    implementation = _build_impl,
    cfg = _transition,
    attrs = {
        "config": attr.label(mandatory = True, providers = [KernelConfigInfo]),
        "_setup": attr.label(default = "//:toolchain", providers = [KernelToolchainInfo]),
        "deps": attr.label_list(providers = [KbuildActionInfo]),
        "_config": attr.label(default = "//:active_config", providers = [KernelConfigInfo]),
        "_allowlist_function_transition": attr.label(default = "@bazel_tools//tools/allowlists/function_transition_allowlist"),
    },
)
