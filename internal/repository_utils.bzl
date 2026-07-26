"""Shared helpers for Linux repository rules."""

visibility("//internal")

LINUX_SOURCE_REPOSITORY_PROTOCOL = "linux-source-v1"

def repository_prefix(label):
    return "@@" + label.repo_name

def linux_makefile_version(content):
    values = {}
    for raw_line in content.splitlines():
        line = raw_line.strip()
        for key in ["VERSION", "PATCHLEVEL", "SUBLEVEL", "EXTRAVERSION"]:
            prefix = key + " ="
            if line.startswith(prefix):
                values[key] = line[len(prefix):].strip()
    missing = [
        key
        for key in ["VERSION", "PATCHLEVEL", "SUBLEVEL"]
        if key not in values
    ]
    if missing:
        fail("Linux Makefile is missing version fields: %s" % missing)
    return "%s.%s.%s%s" % (
        values["VERSION"],
        values["PATCHLEVEL"],
        values["SUBLEVEL"],
        values.get("EXTRAVERSION", ""),
    )
