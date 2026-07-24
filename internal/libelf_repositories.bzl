"""External repositories required by linux.bzl host tools."""

visibility("//...")

def _libelf_repository_impl(rctx):
    rctx.download_and_extract(
        integrity = "sha256-WRqbTsgcHyBCqXqmBWTgy3nQQcUvqnQWrLOLyVvSx20=",
        stripPrefix = "libelf-0.8.13",
        url = [
            "https://www.mirrorservice.org/sites/ftp.netbsd.org/pub/pkgsrc/distfiles/libelf-0.8.13.tar.gz",
            "https://fossies.org/linux/misc/old/libelf-0.8.13.tar.gz",
        ],
    )
    rctx.file("BUILD.bazel", rctx.read(rctx.path(rctx.attr._build_file)))
    rctx.symlink(rctx.path(rctx.attr._config), "lib/config.h")
    rctx.symlink(rctx.path(rctx.attr._sys_elf), "lib/sys_elf.h")
    rctx.symlink(rctx.path(rctx.attr._sys_elf), "include/elf.h")
    rctx.symlink(rctx.path(rctx.attr._gelf), "include/gelf.h")
    rctx.symlink(rctx.path("lib/elf_repl.h"), "include/libelf/elf_repl.h")
    rctx.symlink(rctx.path("lib/libelf.h"), "include/libelf/libelf.h")
    rctx.symlink(rctx.path("lib/nlist.h"), "include/libelf/nlist.h")
    rctx.symlink(rctx.path(rctx.attr._sys_elf), "include/libelf/sys_elf.h")
    rctx.symlink(rctx.path(rctx.attr._gelf_compat), "lib/gelf_shndx_compat.c")
    return rctx.repo_metadata(reproducible = True)

_libelf_repository = repository_rule(
    implementation = _libelf_repository_impl,
    attrs = {
        "_build_file": attr.label(
            allow_single_file = True,
            default = Label("//:libelf.BUILD.bazel"),
        ),
        "_config": attr.label(
            allow_single_file = True,
            default = Label("//internal/libelf_overlay:config.h"),
        ),
        "_gelf": attr.label(
            allow_single_file = True,
            default = Label("//internal/libelf_overlay:gelf.h"),
        ),
        "_gelf_compat": attr.label(
            allow_single_file = True,
            default = Label("//internal/libelf_overlay:gelf_shndx_compat.c"),
        ),
        "_sys_elf": attr.label(
            allow_single_file = True,
            default = Label("//internal/libelf_overlay:sys_elf.h"),
        ),
    },
)

def _linux_bzl_libelf_impl(mctx):
    _libelf_repository(name = "libelf")
    return mctx.extension_metadata(reproducible = True)

linux_bzl_libelf = module_extension(
    implementation = _linux_bzl_libelf_impl,
)
