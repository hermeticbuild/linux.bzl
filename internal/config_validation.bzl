"""Validation for the intentionally small supported Linux config surface."""

visibility("//...")

def validate_config_features(config, description):
    """Rejects config selections that do not have a cache-safe native graph."""
    if config.get("CONFIG_X86_NATIVE_CPU", "n") == "y":
        fail(
            "%s enables CONFIG_X86_NATIVE_CPU, whose -march=native output is not cache-safe" %
            description,
        )
    for symbol in [
        "CONFIG_EXTENDED_MODVERSIONS",
        "CONFIG_GENDWARFKSYMS",
        "CONFIG_TRIM_UNUSED_KSYMS",
    ]:
        if config.get(symbol, "n") in ["y", "m"]:
            fail("%s enables unsupported module versioning path %s" % (description, symbol))

    for symbol in [
        "CONFIG_CFI_CLANG",
        "CONFIG_GCOV_KERNEL",
    ]:
        if config.get(symbol, "n") in ["y", "m"]:
            fail("%s enables %s, whose module metadata instrumentation is not modeled" % (description, symbol))

    for symbol in [
        "CONFIG_MODULE_SIG",
        "CONFIG_MODULE_SIG_ALL",
        "CONFIG_IMA_APPRAISE_MODSIG",
        "CONFIG_SYSTEM_REVOCATION_LIST",
    ]:
        if config.get(symbol, "n") in ["y", "m"]:
            fail("%s enables unsupported certificate or signing path %s" % (description, symbol))

    for symbol in [
        "CONFIG_SYSTEM_BLACKLIST_HASH_LIST",
        "CONFIG_SYSTEM_REVOCATION_KEYS",
        "CONFIG_SYSTEM_TRUSTED_KEYS",
    ]:
        value = config.get(symbol, "")
        if value not in ["", "\"\""]:
            fail("%s sets unsupported certificate input %s=%s" % (description, symbol, value))
