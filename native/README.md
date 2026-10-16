GNU Make integration is the original prototype patch and its three native export
includes. Linux Kconfig integration is prepared by `prepare.py` from the kernel
checkout at tool-build time. It preserves source directives rather than following
them, exports file-local entries, and keeps the existing symbolic compiler facts
and declared fallback policy. These are generation inputs, not build-action tools.

The launcher invokes each parser in a fresh instance of the same executable.
`build.py` combines each parser's object files with `ld -r`, then prefixes its
defined global symbols with `objcopy --redefine-syms`. Undefined libc references
are untouched. This avoids collisions between Linux, GNU Make and CPython helpers
without altering their parser logic or using dynamic libraries.
