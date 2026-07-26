"""Hermetic Linux source repository rule."""

load(
    ":repository_utils.bzl",
    "LINUX_SOURCE_REPOSITORY_PROTOCOL",
    "linux_makefile_version",
    "repository_prefix",
)

visibility("//...")

_KERNEL_RELEASES = {
    "6.12.96": struct(
        integrity = "sha256-fS4bXVqzazoBhW5xeC2tKlTmNPsrN8CkKZje87v5V8E=",
        strip_prefix = "linux-6.12.96",
        urls = [
            "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-6.12.96.tar.xz",
        ],
    ),
    "6.18.39": struct(
        integrity = "sha256-p6fj0q6dledBlyI6jU619r56rCG25t4n6WhdABwfjLA=",
        strip_prefix = "linux-6.18.39",
        urls = [
            "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-6.18.39.tar.xz",
        ],
    ),
}

def _linux_source_repository_impl(rctx):
    catalog = _KERNEL_RELEASES.get(rctx.attr.version)
    has_urls = len(rctx.attr.urls) != 0
    has_integrity = rctx.attr.integrity != ""
    if has_urls != has_integrity:
        fail("linux_source_repository %s requires urls and integrity together" % rctx.original_name)

    if has_urls:
        urls = rctx.attr.urls
        integrity = rctx.attr.integrity
        strip_prefix = rctx.attr.strip_prefix or (catalog.strip_prefix if catalog != None else "")
    elif catalog != None:
        urls = catalog.urls
        integrity = catalog.integrity
        strip_prefix = rctx.attr.strip_prefix or catalog.strip_prefix
    else:
        fail(
            "Linux %s is not in the maintained source catalog; set both urls and integrity" %
            rctx.attr.version,
        )
    if not integrity.startswith("sha256-"):
        fail("linux_source_repository integrity must be a sha256 SRI digest")

    if rctx.attr.patch_strip < 0:
        fail("patch_strip must be non-negative")

    rctx.download_and_extract(
        url = urls,
        integrity = integrity,
        strip_prefix = strip_prefix,
        canonical_id = "linux.bzl-source-%s-%s" % (rctx.attr.version, integrity),
    )
    for patch in rctx.attr.patches:
        rctx.patch(patch, strip = rctx.attr.patch_strip)

    makefile = rctx.path("Makefile")
    kconfig = rctx.path("Kconfig")
    if not makefile.exists or not kconfig.exists:
        fail(
            "Linux source archive for %s must contain root Makefile and Kconfig files" %
            rctx.attr.version,
        )
    actual_version = linux_makefile_version(rctx.read(makefile))
    if actual_version != rctx.attr.version:
        fail(
            "Linux source archive version mismatch: requested %s, Makefile reports %s" %
            (rctx.attr.version, actual_version),
        )

    source_build = rctx.read(rctx.attr._source_build_file)
    source_build = source_build.replace(
        "@rules_cc",
        repository_prefix(rctx.attr._rules_cc_defs),
    )
    source_build = source_build.replace(
        "@platforms",
        repository_prefix(rctx.attr._platforms_x86_64),
    )
    rctx.file("BUILD.bazel", source_build, executable = False)
    rctx.file(
        ".linux-bzl-source.json",
        json.encode({
            "integrity": integrity,
            "protocol": LINUX_SOURCE_REPOSITORY_PROTOCOL,
            "version": actual_version,
        }) + "\n",
        executable = False,
    )
    return rctx.repo_metadata(reproducible = True)

linux_source_repository = repository_rule(
    implementation = _linux_source_repository_impl,
    attrs = {
        "integrity": attr.string(
            doc = "SHA-256 SRI digest required with explicit urls.",
        ),
        "patch_strip": attr.int(
            default = 1,
            doc = "Number of leading path components stripped from patches.",
        ),
        "patches": attr.label_list(
            allow_files = True,
            doc = "Deterministic unified-diff patches applied with Bazel's native patcher.",
        ),
        "strip_prefix": attr.string(
            doc = "Archive directory prefix to remove. Catalog entries provide a default.",
        ),
        "urls": attr.string_list(
            doc = "Explicit mirrors for a source not selected solely from the catalog.",
        ),
        "version": attr.string(
            mandatory = True,
            doc = "Exact upstream Linux version.",
        ),
        "_source_build_file": attr.label(
            allow_single_file = True,
            default = Label("//:source_repo.BUILD.bazel"),
        ),
        "_rules_cc_defs": attr.label(
            allow_single_file = True,
            default = Label("@rules_cc//cc:defs.bzl"),
        ),
        "_platforms_x86_64": attr.label(
            default = Label("@platforms//cpu:x86_64"),
        ),
    },
    doc = "Downloads an integrity-pinned, complete upstream Linux source tree.",
)
