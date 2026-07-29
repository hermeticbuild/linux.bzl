//! linux.bzl Rust-for-Linux and module BTF integration fixture.

use kernel::prelude::*;

module! {
    type: LinuxBzlRustBtf,
    name: "linux_bzl_rust_btf",
    authors: ["linux.bzl contributors"],
    description: "linux.bzl Rust-for-Linux BTF integration fixture",
    license: "GPL",
}

struct LinuxBzlRustBtf;

impl kernel::Module for LinuxBzlRustBtf {
    fn init(_module: &'static ThisModule) -> Result<Self> {
        pr_info!("linux.bzl Rust-for-Linux BTF fixture loaded\n");
        Ok(Self)
    }
}
