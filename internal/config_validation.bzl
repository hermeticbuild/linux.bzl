"""Validation for the intentionally small supported Linux config surface."""

visibility("//...")

def validate_config_features(config, description):
    """Rejects config selections that do not have a cache-safe native graph."""
    if config.get("CONFIG_MODULES", "n") == "y":
        fail("%s enables CONFIG_MODULES, but loadable modules are not implemented" % description)

    module_symbols = sorted([
        symbol
        for symbol, value in config.items()
        if value == "m"
    ])
    if module_symbols:
        fail(
            "%s contains unsupported loadable-module selections: %s" %
            (description, module_symbols),
        )

    if config.get("CONFIG_X86_NATIVE_CPU", "n") == "y":
        fail(
            "%s enables CONFIG_X86_NATIVE_CPU, whose -march=native output is not cache-safe" %
            description,
        )
    if config.get("CONFIG_BPF_SYSCALL", "n") == "y":
        fail(
            "%s enables CONFIG_BPF_SYSCALL, whose cross-tree source include closure is not implemented" %
            description,
        )
    if config.get("CONFIG_RUST", "n") == "y":
        fail("%s enables CONFIG_RUST, which is not implemented" % description)
    if config.get("CONFIG_DEBUG_INFO_BTF", "n") == "y":
        fail("%s enables CONFIG_DEBUG_INFO_BTF, which is not implemented" % description)

    for symbol in [
        "CONFIG_MODULE_SIG",
        "CONFIG_MODULE_SIG_ALL",
        "CONFIG_SYSTEM_REVOCATION_LIST",
        "CONFIG_SYSTEM_TRUSTED_KEYRING",
    ]:
        if config.get(symbol, "n") in ["y", "m"]:
            fail("%s enables unsupported certificate or signing path %s" % (description, symbol))

    for symbol in [
        "CONFIG_SYSTEM_REVOCATION_KEYS",
        "CONFIG_SYSTEM_TRUSTED_KEYS",
    ]:
        value = config.get(symbol, "")
        if value not in ["", "\"\""]:
            fail("%s sets unsupported certificate input %s=%s" % (description, symbol, value))
