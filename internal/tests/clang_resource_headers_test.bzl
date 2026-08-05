"""Unit tests for Clang resource-header discovery."""

load("@bazel_skylib//lib:unittest.bzl", "asserts", "unittest")
load("//internal:clang_resource_headers.bzl", "clang_resource_headers")

visibility("private")

def _clang_resource_headers_test_impl(ctx):
    env = unittest.begin(ctx)
    resource = "bazel-out/k8-fastbuild/bin/external/llvm/lib/clang/21/include"
    fallback = clang_resource_headers.ensure_include(
        ["--target=aarch64-linux-gnu"],
        [struct(path = resource)],
    )
    asserts.equals(
        env,
        [
            "--target=aarch64-linux-gnu",
            "-Xclang",
            "-internal-isystem",
            "-Xclang",
            resource,
        ],
        fallback,
    )

    existing = [
        "-Xclang",
        "-internal-isystem",
        "-Xclang",
        resource,
        "-nostdinc",
    ]
    asserts.equals(
        env,
        existing,
        clang_resource_headers.ensure_include(
            existing,
            [struct(path = "external/other/lib/clang/20/include")],
        ),
    )
    asserts.equals(
        env,
        ["-nostdinc"],
        clang_resource_headers.ensure_include(
            ["-nostdinc"],
            [struct(path = "external/llvm/bin/clang")],
        ),
    )
    asserts.true(env, clang_resource_headers.is_include("C:\\llvm\\lib\\clang\\21\\include"))
    asserts.false(env, clang_resource_headers.is_include("C:\\llvm\\lib\\clang\\21\\include\\arm_neon.h"))
    return unittest.end(env)

clang_resource_headers_test = unittest.make(_clang_resource_headers_test_impl)
