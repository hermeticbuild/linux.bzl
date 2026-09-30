"""Portable validation for generated Linux image repository paths."""

visibility("//internal/...")

_NAME_CHARS = "abcdefghijklmnopqrstuvwxyz0123456789_-"
_WINDOWS_RESERVED_NAMES = {
    "aux": True,
    "con": True,
    "nul": True,
    "prn": True,
}

def validate_linux_image_name(value, what):
    if not value or value[0] not in "abcdefghijklmnopqrstuvwxyz":
        fail("%s %r must start with a lowercase ASCII letter" % (what, value))
    for character in value.elems():
        if character not in _NAME_CHARS:
            fail("%s %r contains unsupported character %r" % (what, value, character))

def validate_linux_overlay_name(name):
    validate_linux_image_name(name, "Linux overlay name")
    if name == "base":
        fail("Linux overlay name must not be base")
    if name in _WINDOWS_RESERVED_NAMES or (
        len(name) == 4 and
        name[:3] in ["com", "lpt"] and
        name[3] >= "1" and
        name[3] <= "9"
    ):
        fail("Linux overlay name %r is reserved on Windows" % name)
