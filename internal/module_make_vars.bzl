"""Validation for source-repository module Make variables."""

visibility("//internal/...")

_BACKEND_OWNED_MODULE_MAKE_VARS = {
    "LIBELF_FLAGS": True,
    "LIBELF_LIBS": True,
    "M": True,
    "RUST_LIB_SRC": True,
}

def validate_linux_module_make_vars(values, owner):
    """Rejects variables whose values are supplied by the mapped backend."""
    conflicts = sorted([name for name in values if name in _BACKEND_OWNED_MODULE_MAKE_VARS])
    if conflicts:
        fail("%s cannot set backend-owned module Make variables: %s" % (owner, ", ".join(conflicts)))
