//! Runtime fixture for the public linux_module rule.

mod rust_test_module_sibling;

use kernel::prelude::*;

module! {
    type: RustTestModule,
    name: "rust_test_module",
    authors: ["linux.bzl contributors"],
    description: "linux.bzl Rust-for-Linux module fixture",
    license: "GPL",
}

struct RustTestModule;

impl kernel::Module for RustTestModule {
    fn init(_module: &'static ThisModule) -> Result<Self> {
        let _sibling_marker = rust_test_module_sibling::SIBLING_MARKER;
        pr_info!("linux.bzl Rust module loaded\n");
        Ok(Self)
    }
}

impl Drop for RustTestModule {
    fn drop(&mut self) {
        pr_info!("linux.bzl Rust module unloaded\n");
    }
}
