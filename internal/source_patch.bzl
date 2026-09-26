"""Translate logical source patch paths before Bazel's native patcher runs.

Only filename metadata and hunk extents are interpreted here. Native patch owns
syntax checks, matching, permissions, renames, and writing the patched bytes.
"""

visibility("//...")

def _mapped_patch_path(path, strip, source_paths):
    if path == "/dev/null":
        return path
    components = path.split("/")
    if len(components) <= strip:
        return path
    relative = components[strip:]
    if not relative[0]:
        # Absolute paths remain the native patcher's responsibility.
        return path
    normalized = []
    for component in relative:
        if component in ["", "."]:
            continue
        if component == "..":
            if not normalized:
                return path
            normalized.pop()
        else:
            normalized.append(component)
    replacement = source_paths.get("/".join(normalized))
    if replacement == None:
        return path
    return "/".join(components[:strip] + [replacement])

def _mapped_patch_header(line, prefix, strip, source_paths, timestamp = False):
    tail = line[len(prefix):]
    field = tail.split("\t", 1)[0] if timestamp else tail
    path = field.strip()
    mapped = _mapped_patch_path(path, strip, source_paths)
    if mapped == path:
        return line
    start = len(prefix) + len(field) - len(field.lstrip())
    return line[:start] + mapped + line[start + len(path):]

def _patch_hunk_counts(line):
    if not line.startswith("@@"):
        return None
    end = line.find("@@", 2)
    if end == -1:
        return None
    fields = [field for field in line[2:end].split(" ") if field]
    if len(fields) != 2 or not fields[0].startswith("-") or not fields[1].startswith("+"):
        return None
    counts = []
    for field in fields:
        numbers = field[1:].split(",")
        if len(numbers) not in [1, 2] or not all([number.isdigit() for number in numbers]):
            return None
        counts.append(int(numbers[1]) if len(numbers) == 2 else 1)
    return counts

def translate_linux_source_patch(content, strip, source_paths):
    lines = content.split("\n")
    remaining = None
    git_patch = False
    for index in range(len(lines)):
        raw = lines[index]
        carriage_return = "\r" if raw.endswith("\r") else ""
        line = raw[:-1] if carriage_return else raw
        if line.startswith("\\"):
            # Leave patch comments and no-newline markers unchanged.
            continue
        if remaining != None:
            if line.startswith("-"):
                remaining[0] -= 1
            elif line.startswith("+"):
                remaining[1] -= 1
            elif line.startswith(" ") or not line:
                remaining[0] -= 1
                remaining[1] -= 1
            else:
                # A new hunk header or invalid input: let native patch decide.
                remaining = None
            if remaining != None:
                if remaining == [0, 0]:
                    remaining = None
                continue
        if line.startswith("--- ") or line.startswith("+++ "):
            lines[index] = _mapped_patch_header(line, line[:4], strip, source_paths, timestamp = True) + carriage_return
            continue
        if line.startswith("diff --git "):
            git_patch = True
            fields = line.split(" ")
            if len(fields) >= 4:
                fields[2] = _mapped_patch_path(fields[2], strip, source_paths)
                fields[3] = _mapped_patch_path(fields[3], strip, source_paths)
                lines[index] = " ".join(fields) + carriage_return
            continue
        if git_patch:
            if line.startswith("rename from ") or line.startswith("rename to "):
                prefix = "rename from " if line.startswith("rename from ") else "rename to "
                lines[index] = _mapped_patch_header(line, prefix, 0, source_paths) + carriage_return
                continue
            if any([line.startswith(prefix) for prefix in [
                "old mode ",
                "new mode ",
                "deleted file mode ",
                "new file mode ",
                "copy from ",
                "copy to ",
                "rename old ",
                "rename new ",
                "similarity index ",
                "dissimilarity index ",
                "index ",
            ]]):
                continue
        counts = _patch_hunk_counts(line)
        if counts != None:
            remaining = counts
        else:
            git_patch = False
    return "\n".join(lines)
