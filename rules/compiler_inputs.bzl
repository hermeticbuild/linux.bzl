"""Analysis inputs for a compiler-neutral native Kconfig export."""

def compiler_inputs(cc, compiler, metadata = None):
    """Use selected CcToolchainInfo metadata; missing versions stay unknown/zero."""
    clang = compiler in ("clang", "llvm", "clang-cl")
    gcc = compiler in ("gcc", "gnu", "g++")
    if not clang and not gcc:
        fail("Unsupported kernel C compiler identity in selected toolchain: %r" % compiler)
    version = getattr(cc, "compiler_version", None)
    if metadata != None and metadata.compiler_version != None:
        if version != None and version != metadata.compiler_version:
            fail("Compiler version disagrees between CcToolchainInfo and kernel metadata")
        version = metadata.compiler_version
    if version == None:
        # buildifier: disable=print
        print("WARNING: selected C++ toolchain %r exposes no compiler version; using fallback 0 for Kconfig version constraints" % cc.toolchain_id)
        version = 0

    # CcToolchainInfo has no standard assembler/linker version fields.
    if metadata != None and metadata.assembler != None:
        clang_as = metadata.assembler == "LLVM"
        gnu_as = metadata.assembler == "GNU"
        lld, bfd = metadata.linker == "LLD", metadata.linker == "BFD"
        as_version, ld_version = metadata.assembler_version, metadata.linker_version
    else:
        # Compatibility policy for the older config-only experiments.
        ld = cc.ld_executable.rsplit("/", 1)[-1]
        if ld not in ("ld.lld", "lld", "ld.bfd", "ld"):
            linkers = {f.basename: True for f in cc.all_files.to_list() if f.basename in ("ld.lld", "ld.bfd", "ld")}
            if len(linkers) == 1:
                ld = linkers.keys()[0]
        lld, bfd = ld in ("ld.lld", "lld"), ld == "ld.bfd"

        # buildifier: disable=print
        print("WARNING: selected C++ toolchain exposes no assembler/linker version; using fallback 0")
        clang_as, gnu_as, as_version, ld_version = clang, False, 0, 0
    return {
        "__KCONFIG_CC_NAME": ["Clang" if clang else "GCC", "string"],
        "__KCONFIG_CC_IS_CLANG": ["y" if clang else "n", "tristate"],
        "__KCONFIG_CC_IS_GCC": ["y" if gcc else "n", "tristate"],
        "__KCONFIG_CC_VERSION": [str(version), "int"],
        "__KCONFIG_CC_MAJOR_VERSION": [str(int(version) // 10000), "int"],
        "__KCONFIG_CC_VERSION_TEXT": [compiler + " (Bazel toolchain " + cc.toolchain_id + ")", "string"],
        # Image builds take assembler mode from their paired metadata provider.
        "__KCONFIG_AS_NAME": ["LLVM" if clang_as else "GNU" if gnu_as else "", "string"],
        "__KCONFIG_AS_IS_LLVM": ["y" if clang_as else "n", "tristate"],
        "__KCONFIG_AS_IS_GNU": ["y" if gnu_as else "n", "tristate"],
        "__KCONFIG_AS_VERSION": [str(as_version), "int"],
        "__KCONFIG_LD_NAME": ["LLD" if lld else "BFD" if bfd else "", "string"],
        "__KCONFIG_LD_IS_LLD": ["y" if lld else "n", "tristate"],
        "__KCONFIG_LD_IS_BFD": ["y" if bfd else "n", "tristate"],
        "__KCONFIG_LD_VERSION": [str(ld_version), "int"],
    }

def _bind(code, inputs):
    result = []
    for token in code:
        if token[0] in ("sym", "lit") and token[1] in inputs:
            value, kind = inputs[token[1]]
            result.append(["lit", value, kind])
        else:
            if token[0] in ("sym", "lit") and "__KCONFIG_" in token[1]:
                fail("Unresolved generated Kconfig input: " + token[1])
            result.append(token)
    return result

def bind_compiler_inputs(records, inputs):
    """Bind exported expression leaves before the dependency-ordered solver."""
    result = []
    for record in records:
        bound = dict(record)
        for key in ("depends_on", "direct", "reverse", "implied"):
            bound[key] = _bind(record[key], inputs)
        properties = []
        for prop in record["properties"]:
            p = dict(prop)
            p["value"] = _bind(prop["value"], inputs)
            p["when"] = _bind(prop["when"], inputs)
            properties.append(p)
        bound["properties"] = properties
        result.append(bound)
    return result
