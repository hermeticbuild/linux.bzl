"""Bazel 9 execution-configured optional Rust toolchain accessors."""

load(":execution_platform.bzl", "linux_execution_platform_attr", "linux_execution_platform_label")

visibility("//internal/...")

RUST_TOOLCHAIN_TYPE = str(Label("@rules_rust//rust:toolchain_type"))
RUST_ANALYZER_TOOLCHAIN_TYPE = str(Label("@rules_rust//rust/rust_analyzer:toolchain_type"))
BINDGEN_TOOLCHAIN_TYPE = str(Label("@rules_rs//rs:bindgen_toolchain_type"))

_PLATFORMS_OPTION = "//command_line_option:platforms"
_EXTRA_EXECUTION_PLATFORMS_OPTION = "//command_line_option:extra_execution_platforms"

ExecutionOptionalToolchainInfo = provider(
    doc = "An optional toolchain resolved after entering an execution configuration.",
    fields = {
        "execution_platform": "The execution platform on which the selected toolchain is runnable.",
        "toolchain": "The selected ToolchainInfo, or None.",
    },
)

def optional_rust_toolchain_type():
    """Returns an optional Rust toolchain requirement for non-Rust kernels."""
    return config_common.toolchain_type(RUST_TOOLCHAIN_TYPE, mandatory = False)

def optional_rust_analyzer_toolchain_type():
    """Returns the optional source-bearing Rust analyzer toolchain."""
    return config_common.toolchain_type(RUST_ANALYZER_TOOLCHAIN_TYPE, mandatory = False)

def optional_bindgen_toolchain_type():
    """Returns the optional bindgen toolchain used by Rust-enabled kernels."""
    return config_common.toolchain_type(BINDGEN_TOOLCHAIN_TYPE, mandatory = False)

def _pin_execution_platform_impl(settings, _attr):
    platforms = settings[_PLATFORMS_OPTION]
    if len(platforms) != 1:
        fail("optional-toolchain execution bridge requires exactly one transitioned platform, got %s" % platforms)
    return {
        _EXTRA_EXECUTION_PLATFORMS_OPTION: [str(platforms[0])],
    }

# config.exec first turns the consuming group's execution platform into this
# dependency's target platform. Pinning that same label to the head of the
# dependency's execution-platform candidates makes the second toolchain
# resolution diagonal: the optional toolchain's target and execution platforms
# are identical. Each resolver below requests exactly one optional type so an
# unrelated optional tool cannot influence its platform selection.
_pin_execution_platform = transition(
    implementation = _pin_execution_platform_impl,
    inputs = [_PLATFORMS_OPTION],
    outputs = [_EXTRA_EXECUTION_PLATFORMS_OPTION],
)

def _execution_optional_toolchain_bridge_impl(ctx):
    if len(ctx.attr.resolver) != 1:
        fail("optional-toolchain execution bridge produced %d resolver configurations, want 1" % len(ctx.attr.resolver))
    resolved = ctx.attr.resolver[0][ExecutionOptionalToolchainInfo]
    target_platform = ctx.fragments.platform.platform
    return [ExecutionOptionalToolchainInfo(
        execution_platform = resolved.execution_platform,
        toolchain = _matching_execution_toolchain(resolved, target_platform),
    )]

execution_optional_toolchain_bridge = rule(
    implementation = _execution_optional_toolchain_bridge_impl,
    attrs = {
        "_allowlist_function_transition": attr.label(
            default = Label("@bazel_tools//tools/allowlists/function_transition_allowlist"),
        ),
        "resolver": attr.label(
            cfg = _pin_execution_platform,
            mandatory = True,
            providers = [ExecutionOptionalToolchainInfo],
        ),
    },
    fragments = ["platform"],
)

def _execution_rust_toolchain_resolver_impl(ctx):
    return [ExecutionOptionalToolchainInfo(
        execution_platform = linux_execution_platform_label(ctx.attr._execution_platform),
        toolchain = ctx.toolchains[RUST_TOOLCHAIN_TYPE],
    )]

execution_rust_toolchain_resolver = rule(
    implementation = _execution_rust_toolchain_resolver_impl,
    attrs = {
        "_execution_platform": linux_execution_platform_attr(),
        # The platform probe above observes the default execution group. Keep
        # Rust in that group even when callers enable automatic exec groups.
        "_use_auto_exec_groups": attr.bool(default = False),
    },
    toolchains = [optional_rust_toolchain_type()],
)

def _execution_rust_source_toolchain_resolver_impl(ctx):
    return [ExecutionOptionalToolchainInfo(
        execution_platform = linux_execution_platform_label(ctx.attr._execution_platform),
        toolchain = ctx.toolchains[RUST_ANALYZER_TOOLCHAIN_TYPE],
    )]

execution_rust_source_toolchain_resolver = rule(
    implementation = _execution_rust_source_toolchain_resolver_impl,
    attrs = {
        "_execution_platform": linux_execution_platform_attr(),
        "_use_auto_exec_groups": attr.bool(default = False),
    },
    toolchains = [optional_rust_analyzer_toolchain_type()],
)

def _execution_bindgen_toolchain_resolver_impl(ctx):
    return [ExecutionOptionalToolchainInfo(
        execution_platform = linux_execution_platform_label(ctx.attr._execution_platform),
        toolchain = ctx.toolchains[BINDGEN_TOOLCHAIN_TYPE],
    )]

execution_bindgen_toolchain_resolver = rule(
    implementation = _execution_bindgen_toolchain_resolver_impl,
    attrs = {
        "_execution_platform": linux_execution_platform_attr(),
        "_use_auto_exec_groups": attr.bool(default = False),
    },
    toolchains = [optional_bindgen_toolchain_type()],
)

def _execution_optional_toolchain_attr(bridge, exec_group = None):
    return attr.label(
        cfg = "exec" if exec_group == None else config.exec(exec_group = exec_group),
        default = Label(bridge),
        providers = [ExecutionOptionalToolchainInfo],
    )

def execution_rust_toolchain_attr(exec_group = None):
    """Returns an optional Rust compiler bridge pinned to the execution group."""
    return _execution_optional_toolchain_attr("//internal:execution_rust_toolchain", exec_group)

def execution_rust_source_toolchain_attr(exec_group = None):
    """Returns an optional Rust source bridge pinned to the execution group."""
    return _execution_optional_toolchain_attr("//internal:execution_rust_source_toolchain", exec_group)

def execution_bindgen_toolchain_attr(exec_group = None):
    """Returns an optional bindgen bridge pinned to the execution group."""
    return _execution_optional_toolchain_attr("//internal:execution_bindgen_toolchain", exec_group)

def _matching_execution_toolchain(info, execution_platform):
    if info.toolchain != None and info.execution_platform == execution_platform:
        return info.toolchain
    return None

def execution_rust_toolchain_test_filter(info, execution_platform):
    """Test seam for the bridge's final execution-platform guard."""
    return _matching_execution_toolchain(info, execution_platform)

def _execution_optional_toolchain(ctx, attribute, execution_platform):
    info = getattr(ctx.attr, attribute)[ExecutionOptionalToolchainInfo]
    return _matching_execution_toolchain(info, execution_platform)

def execution_rust_toolchain(ctx, attribute, execution_platform):
    """Returns the execution-configured optional Rust toolchain from attribute."""
    return _execution_optional_toolchain(ctx, attribute, execution_platform)

def execution_rust_source_toolchain(ctx, attribute, execution_platform):
    """Returns the source-bearing Rust analyzer toolchain, or None."""
    return _execution_optional_toolchain(ctx, attribute, execution_platform)

def execution_bindgen_toolchain(ctx, attribute, execution_platform):
    """Returns the selected bindgen toolchain, or None."""
    return _execution_optional_toolchain(ctx, attribute, execution_platform)
