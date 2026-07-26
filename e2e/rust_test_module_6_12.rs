//! Runtime fixture for the Linux 6.12 Rust module API.

use kernel::prelude::*;

module! {
    type: RustTestModule,
    name: "rust_test_module",
    author: "linux.bzl contributors",
    description: "linux.bzl Rust-for-Linux module fixture",
    license: "GPL",
}

struct RustTestModule;

impl kernel::Module for RustTestModule {
    fn init(_module: &'static ThisModule) -> Result<Self> {
        pr_info!("linux.bzl Rust module loaded\n");
        Ok(Self)
    }
}

impl Drop for RustTestModule {
    fn drop(&mut self) {
        pr_info!("linux.bzl Rust module unloaded\n");
    }
}
