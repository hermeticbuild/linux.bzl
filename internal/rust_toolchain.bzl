"""Bazel 9 target and execution-scope Rust toolchain accessors."""

visibility("//internal/...")

RUST_TOOLCHAIN_TYPE = str(Label("@rules_rust//rust:toolchain_type"))
RUST_ANALYZER_TOOLCHAIN_TYPE = str(Label("@rules_rust//rust/rust_analyzer:toolchain_type"))
BINDGEN_TOOLCHAIN_TYPE = str(Label("@rules_rs//rs:bindgen_toolchain_type"))

def optional_rust_toolchain_type():
    """Returns an optional Rust toolchain requirement for non-Rust kernels."""
    return config_common.toolchain_type(RUST_TOOLCHAIN_TYPE, mandatory = False)

def optional_rust_analyzer_toolchain_type():
    """Returns the optional source-bearing Rust analyzer toolchain."""
    return config_common.toolchain_type(RUST_ANALYZER_TOOLCHAIN_TYPE, mandatory = False)

def optional_bindgen_toolchain_type():
    """Returns the optional bindgen toolchain used by Rust-enabled kernels."""
    return config_common.toolchain_type(BINDGEN_TOOLCHAIN_TYPE, mandatory = False)

def target_rust_toolchain(ctx):
    """Returns the selected target Rust toolchain, or None when unregistered."""
    return ctx.toolchains[RUST_TOOLCHAIN_TYPE]

def execution_rust_toolchain(ctx, exec_group):
    """Returns the Rust toolchain selected in exec_group, or None."""
    return ctx.exec_groups[exec_group].toolchains[RUST_TOOLCHAIN_TYPE]

def execution_rust_source_toolchain(ctx, exec_group):
    """Returns the source-bearing Rust analyzer toolchain, or None."""
    return ctx.exec_groups[exec_group].toolchains[RUST_ANALYZER_TOOLCHAIN_TYPE]

def execution_bindgen_toolchain(ctx, exec_group):
    """Returns the selected bindgen toolchain, or None."""
    return ctx.exec_groups[exec_group].toolchains[BINDGEN_TOOLCHAIN_TYPE]
