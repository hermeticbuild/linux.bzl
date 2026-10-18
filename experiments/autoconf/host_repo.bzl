"""Explicit local execution prerequisites for this non-hermetic acceptance test."""

def _impl(ctx):
    names = ["sh", "bash", "python3", "perl", "awk", "flex", "bison", "m4", "gzip", "bzip2", "lzma", "xz", "zstd", "lz4", "lzop", "gcc", "g++", "ld", "ar", "nm", "objcopy", "objdump", "readelf", "strip", "as", "bc", "sed", "grep", "find", "xargs", "cat", "cp", "mv", "rm", "mkdir", "ln", "touch", "chmod", "head", "tail", "cut", "tr", "sort", "uniq", "wc", "date", "expr", "printf", "uname", "basename", "dirname", "mktemp", "od", "dd", "stat", "readlink", "realpath", "truncate", "install", "cmp", "diff", "yes", "echo", "test", "true", "false", "sleep"]
    available = []
    for name in names:
        path = ctx.which(name)
        if path:
            ctx.symlink(path, "bin/" + name)
            available.append("bin/" + name)
    ctx.symlink(ctx.path(ctx.attr.linux).realpath.dirname, "linux")
    ctx.file("BUILD.bazel", 'package(default_visibility=["//visibility:public"])\nexports_files(' + repr(available) + ')\nfilegroup(name="tools",srcs=' + repr(available) + ')\nfilegroup(name="linux",srcs=glob(["linux/**"],exclude=["linux/.git/**"]))\nexports_files(["linux/Makefile"])\n')

host_repository = repository_rule(implementation = _impl, local = True, attrs = {"linux": attr.label(mandatory = True, allow_single_file = True)})
