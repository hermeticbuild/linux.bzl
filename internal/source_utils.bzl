"""Shared source-label helpers for Linux module internals."""

def source_label(source_repo, path):
    if source_repo.endswith("//"):
        return "%s:%s" % (source_repo, path)
    return "%s//:%s" % (source_repo, path)

def source_labels(source_repo, paths):
    return [source_label(source_repo, path) for path in paths]

def source_label_package(source_repo):
    if source_repo.endswith("//"):
        return source_repo
    return source_repo + "//"

def package_label(name):
    package = native.package_name()
    if package:
        return "//%s:%s" % (package, name)
    return "//:%s" % name

def bindir_path(path):
    package = native.package_name()
    if package:
        return "$(BINDIR)/%s/%s" % (package, path)
    return "$(BINDIR)/%s" % path
