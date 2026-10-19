"""Identity of a source-only Kbuild export and its statically bound callback."""

KernelSyntaxInfo = provider(
    "Source syntax loaded only by kernel builds, independently of native config.",
    fields = ["anchor", "expand"],
)

def syntax_for_source(syntax, source):
    if syntax.anchor != source.anchor:
        fail("Kbuild syntax anchor %s does not match native configuration source %s" % (syntax.anchor.path, source.anchor.path))
    return syntax.expand
