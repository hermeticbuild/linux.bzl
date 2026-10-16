"""Grouped Kconfig data and Starlark resolution, without native preparation."""

load("@rules_cc//cc:find_cc_toolchain.bzl", "find_cc_toolchain", "use_cc_toolchain")
load(":architecture.bzl", "KernelArchitectureInfo")
load(":compiler_capabilities.bzl", "KernelCompilerCapabilitiesInfo")
load(":compiler_inputs.bzl", "bind_compiler_inputs", "compiler_inputs")
load(":compiler_metadata.bzl", "KernelCompilerMetadataInfo")
load(":kconfig_link.bzl", "link_sources")
load(":solver.bzl", "resolve_options")

KernelConfigInfo = provider(doc = "Kernel Config Info.", fields = ["identity", "values", "make_values", "config", "header", "headers", "includes", "arch", "architecture", "report"])

KernelOptionsInfo = provider(doc = "Kernel Options Info.", fields = ["records", "sources"])

def _options_impl(ctx):
    return [KernelOptionsInfo(sources = {}, records = depset(
        ctx.attr.records,
        transitive = [d[KernelOptionsInfo].records for d in ctx.attr.deps],
    ))]

_options = rule(
    implementation = _options_impl,
    attrs = {"records": attr.string_list(), "deps": attr.label_list(providers = [KernelOptionsInfo])},
)

def kernel_options(name, options = [], deps = [], **kwargs):
    _options(name = name, records = [json.encode(r) for r in options], deps = deps, **kwargs)

def compiled_expression(instructions):
    """Flat expression encoding for data too deeply nested for BUILD syntax."""
    return instructions

def literal(value, kind = None):
    return [["lit", value, kind if kind != None else "tristate" if value in ("n", "m", "y") else "unknown"]]

def symbol(name):
    return [["sym", name]]

def _combine(op, expressions):
    result = list(expressions[0])
    for expression in expressions[1:]:
        result.extend(expression)
        result.append([op])
    return result

def all_of(*expressions):
    return _combine("and", expressions) if expressions else literal("y")

def any_of(*expressions):
    return _combine("or", expressions) if expressions else literal("n")

def negate(expression):
    return expression + [["not"]]

def equal(left, right):
    return _combine("eq", [left, right])

def not_equal(left, right):
    return _combine("ne", [left, right])

def less(left, right):
    return _combine("lt", [left, right])

def at_most(left, right):
    return _combine("le", [left, right])

def greater(left, right):
    return _combine("gt", [left, right])

def at_least(left, right):
    return _combine("ge", [left, right])

def _property(kind, value, when, text = ""):
    return {"kind": kind, "value": value, "when": when if when != None else literal("y"), "text": text}

def prompt(text, when = None):
    return _property("prompt", literal("y"), when, text)

def default(value, when = None):
    return _property("default", value, when)

def select_symbol(name, when = None):
    return _property("select", symbol(name), when)

def imply_symbol(name, when = None):
    return _property("imply", symbol(name), when)

def value_range(low, high, when = None):
    return _property("range", _combine("range", [low, high]), when)

def _option(
        kind,
        name = "",
        order = 0,
        parent_choice = 0,
        choice = False,
        depends_on = None,
        prompts = [],
        defaults = [],
        selects = [],
        implies = [],
        ranges = [],
        modules = False,
        transitional = False,
        direct = None,
        reverse = None,
        implied = None,
        evaluation_rank = 0,
        unit = ""):
    return {
        "symbol": name,
        "type": kind,
        "order": order,
        "parent_choice": parent_choice,
        "choice": choice,
        "depends_on": depends_on if depends_on != None else literal("y"),
        "modules": modules,
        "transitional": transitional,
        "properties": prompts + defaults + selects + implies + ranges,
        "direct": direct if direct != None else literal("y"),
        "reverse": reverse if reverse != None else literal("n"),
        "implied": implied if implied != None else literal("n"),
        "evaluation_rank": evaluation_rank,
        "unit": unit if unit else name,
    }

def bool_option(name, **kwargs):
    return _option("bool", name = name, **kwargs)

def tristate_option(name, **kwargs):
    return _option("tristate", name = name, **kwargs)

def int_option(name, **kwargs):
    return _option("int", name = name, **kwargs)

def hex_option(name, **kwargs):
    return _option("hex", name = name, **kwargs)

def string_option(name, **kwargs):
    return _option("string", name = name, **kwargs)

def choice_option(**kwargs):
    return _option("bool", choice = True, **kwargs)

def _kernel_config_impl(ctx):
    architecture = ctx.attr._architecture[KernelArchitectureInfo]
    options = ctx.attr.options[KernelOptionsInfo]
    records = link_sources(options.sources, architecture) if options.sources else [json.decode(r) for r in options.records.to_list()]
    inputs = compiler_inputs(
        find_cc_toolchain(ctx),
        ctx.attr._compiler[config_common.FeatureFlagInfo].value,
        ctx.attr._compiler_metadata[KernelCompilerMetadataInfo],
    )
    inputs.update(ctx.attr._compiler_capabilities[KernelCompilerCapabilitiesInfo].inputs)
    inputs["__KCONFIG_ARCH"] = [architecture.arch, "string"]
    inputs["__KCONFIG_SRCARCH"] = [architecture.srcarch, "string"]
    inputs["__KCONFIG_SUBARCH"] = [architecture.subarch, "string"]
    inputs["__KCONFIG_HEADER_ARCH"] = [architecture.header_arch, "string"]
    records = bind_compiler_inputs(records, inputs)
    requested = dict(ctx.attr.requested)

    # Width belongs to the platform. Reject explicit requests that contradict it.
    width = "y" if architecture.bits == 64 else "n"
    if "64BIT" in requested and requested["64BIT"] != width:
        fail("CONFIG_64BIT=%s conflicts with platform CPU %s (%s-bit)" % (requested["64BIT"], architecture.cpu, architecture.bits))
    if ctx.attr.allnoconfig:
        minimum = {r["symbol"]: "n" for r in records if r["symbol"] and not r.get("parent_choice") and r["type"] in ("bool", "tristate") and any([p["kind"] == "prompt" for p in r["properties"]])}

        # A CPU platform fixes width even when Kconfig exposes it as a prompt.
        minimum["64BIT"] = width
        minimum.update(requested)
        requested = minimum
    order = [n for n in requested if n not in ctx.attr.request_order] + ctx.attr.request_order
    result = resolve_options(records, requested, order)
    if result.values.get("64BIT", "n") != width:
        fail("Resolved CONFIG_64BIT conflicts with platform CPU %s (%s-bit); request the architecture CPU/ISA choice in kernel_config" % (architecture.cpu, architecture.bits))
    mismatches = {n: [v, result.values.get(n, "n")] for n, v in ctx.attr.requested.items() if result.values.get(n, "n") != v}
    if ctx.attr.strict and mismatches:
        fail("Unsatisfied Kconfig requests: %s" % mismatches)
    config = ctx.actions.declare_file(ctx.label.name + ".config")
    header = ctx.actions.declare_file(ctx.label.name + ".autoconf.h")
    report = ctx.actions.declare_file(ctx.label.name + ".report.json")
    ctx.actions.write(config, result.config)
    ctx.actions.write(header, result.header)
    types = {r["symbol"]: r["type"] for r in records if r["symbol"]}

    # .config quotes strings; include/config/auto.conf exposes their decoded
    # contents to Make. In particular, an empty string must not become a
    # spurious empty compiler argument when a capability takes its fallback.
    written = dict([line.split("=", 1) for line in result.config.splitlines() if line.startswith("CONFIG_")])
    make_values = {
        name: json.decode(v) if types.get(name.removeprefix("CONFIG_")) == "string" else v
        for name, v in written.items()
    }
    ctx.actions.write(report, json.encode({
        "mismatches": mismatches,
        "requested": ctx.attr.requested,
        "compiler_inputs": inputs,
        "make_values": make_values,
        "architecture": {
            "cpu": architecture.cpu,
            "arch": architecture.arch,
            "srcarch": architecture.srcarch,
            "bits": architecture.bits,
            "subarch": architecture.subarch,
            "header_arch": architecture.header_arch,
        },
    }))
    headers = None
    includes = []
    arch = architecture.arch
    return [
        KernelConfigInfo(
            identity = str(ctx.label) + ":" + config.path,
            values = result.values,
            make_values = make_values,
            config = config,
            header = header,
            headers = headers,
            includes = includes,
            arch = arch,
            architecture = architecture,
            report = report,
        ),
        DefaultInfo(files = depset([config, header, report])),
    ]

_kernel_config = rule(
    implementation = _kernel_config_impl,
    fragments = ["cpp"],
    toolchains = use_cc_toolchain(),
    attrs = {
        "_compiler_capabilities": attr.label(default = "//:compiler_capabilities", providers = [KernelCompilerCapabilitiesInfo]),
        "_compiler": attr.label(default = "@rules_cc//cc/compiler:compiler", providers = [config_common.FeatureFlagInfo]),
        "_compiler_metadata": attr.label(default = "//:compiler_metadata", providers = [KernelCompilerMetadataInfo]),
        "options": attr.label(mandatory = True, providers = [KernelOptionsInfo]),
        "requested": attr.string_dict(),
        "request_order": attr.string_list(),
        "strict": attr.bool(default = False),
        "allnoconfig": attr.bool(default = False),
        "_architecture": attr.label(default = "//:architecture", providers = [KernelArchitectureInfo]),
    },
)

def kernel_config(name, options, requested = {}, base = {}, **kwargs):
    effective = dict(base)
    for key, value in requested.items():
        if key in effective:
            effective.pop(key)
        effective[key] = value
    _kernel_config(
        name = name,
        options = options,
        requested = effective,
        request_order = list(effective),
        **kwargs
    )

def _option_sources_impl(ctx):
    return [KernelOptionsInfo(records = depset(), sources = {path: json.decode(body) for path, body in ctx.attr.files.items()})]

kernel_option_sources = rule(implementation = _option_sources_impl, attrs = {"files": attr.string_dict()})
