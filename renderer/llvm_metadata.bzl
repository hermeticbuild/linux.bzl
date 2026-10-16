"""hermetic-llvm adapter; generic Kbuild rules consume only its provider."""

load("@bazel_skylib//rules/directory:subdirectory.bzl", "subdirectory")
load("@kernel_llvm_versions//:versions.bzl", "LLVM_SEED_VERSION", "LLVM_SOURCE_VERSION")
load("@llvm//platforms:common.bzl", "SUPPORTED_EXECS")
load("@llvm//toolchain:selects.bzl", "platform_resource_dir")
load(":compiler_capabilities_data.bzl", "COMPILER_CAPABILITIES")
load(":compiler_metadata.bzl", "exec_compiler_headers", "kernel_compiler_metadata")

def _version(text):
    parts = text.split(".")
    if len(parts) != 3 or not all([p.isdigit() for p in parts]):
        fail("Provide explicit Kconfig numeric metadata for non-release LLVM version: %s" % text)
    return int(parts[0]) * 10000 + int(parts[1]) * 100 + int(parts[2])

# Fixed package fixtures or action helpers, not target-naming macros.
# buildifier: disable=unnamed-macro
def llvm_metadata(kernel_prefix = None, kernel_toolchain = None):
    subdirectory(name = "hosted_header_files", path = "include", parent = select({
        "@llvm//platforms/config:%s_%s" % (os, cpu): platform_resource_dir(os, cpu)
        for os, cpu in SUPPORTED_EXECS
    }))
    exec_compiler_headers(name = "hosted_headers", directory = ":hosted_header_files")
    roles = {
        "clang": "CC",
        "ld.lld": "LD",
        "llvm-nm": "NM",
        "llvm-strip": "STRIP",
        "llvm-objcopy": "OBJCOPY",
        "llvm-objdump": "OBJDUMP",
        "llvm-readelf": "READELF",
    }
    hosted_tools = {"@llvm//tools:" + name: role for name, role in roles.items()}
    toolchain_ids = [str(Label("@llvm//toolchain:%s_%s_cc_toolchain" % (os, cpu))) for os, cpu in SUPPORTED_EXECS]
    versions = {"//conditions:default": _version(LLVM_SEED_VERSION)}
    tools, headers = hosted_tools, ":hosted_headers"
    if kernel_prefix != None:
        if kernel_toolchain == None:
            fail("Source LLVM metadata requires its compiler-only toolchain label")
        toolchain_ids.append(str(Label(kernel_toolchain)))
        native.config_setting(name = "kernel", constraint_values = ["//kernel_toolchains:kernel"])
        tools = select({":kernel": {kernel_prefix + "/bin/" + name: role for name, role in roles.items()}, "//conditions:default": hosted_tools})
        headers = select({":kernel": kernel_prefix + "/clang_resource_include_directory", "//conditions:default": ":hosted_headers"})

        # The custom compiler-only toolchain always uses the source-built stage.
        versions[":kernel"] = _version(LLVM_SOURCE_VERSION)
    version = select(versions)
    kernel_compiler_metadata(
        name = "llvm",
        compiler = "clang",
        compiler_version = version,
        toolchain_ids = toolchain_ids,
        # Native scripts/as-version.sh reports LLVM 0 for the integrated
        # assembler; its compiler version is checked separately.
        assembler = "LLVM",
        assembler_version = 0,
        linker = "LLD",
        linker_version = version,
        tools = tools,
        builtin_headers = headers,
        driver_linker_aliases = ["ld", "ld.lld"],
        capabilities = [f.get("input", f["probe"] + ":" + f["argument"]) for f in COMPILER_CAPABILITIES if "clang" in f["compilers"]],
        visibility = ["//visibility:public"],
    )
