"""Carry declared host library contexts into measured actions."""

load("@rules_cc//cc/common:cc_common.bzl", "cc_common")
load("@rules_cc//cc/common:cc_info.bzl", "CcInfo")

def _describe(info):
    compile = info.compilation_context
    cflags = ["-D" + value for value in compile.defines.to_list()]
    for option, paths in [("-I", compile.includes), ("-iquote", compile.quote_includes), ("-isystem", compile.system_includes)]:
        for path in paths.to_list():
            cflags.extend([option, path])
    files, flags = [], []
    for linker in info.linking_context.linker_inputs.to_list():
        flags.extend(linker.user_link_flags)
        files.extend(linker.additional_inputs)
        for library in linker.libraries:
            archive = library.static_library or library.pic_static_library
            if archive == None:
                fail("Measured host tools require a declared static library: " + str(linker.owner))
            files.append(archive)
            if library.alwayslink:
                flags.extend(["-Wl,--whole-archive", archive.path, "-Wl,--no-whole-archive"])
            else:
                flags.append(archive.path)
    return struct(cflags = cflags, libs = flags, files = depset(files, transitive = [compile.headers]))

def host_libraries(targets):
    """Use the same public CcInfo contexts for probes and final host actions."""
    packages, infos = {}, []
    for target, name in targets.items():
        if not name or name in packages:
            fail("Host library package names must be unique and nonempty: " + name)
        info = target[CcInfo]
        context = _describe(info)
        packages[name] = dict(cflags = context.cflags, libs = context.libs, owner = str(target.label))
        infos.append(info)
    merged = _describe(cc_common.merge_cc_infos(cc_infos = infos))
    return struct(packages = packages, cflags = merged.cflags, libs = merged.libs, files = merged.files)
