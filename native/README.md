# Native tools

`build_conf.py` asks the selected source's Kconfig Makefile for `conf-objs` and
builds those files with declared compiler, Make, Bison, and Flex tools. The
append-only entry adapter preserves native configuration and serialization and
exposes a complete native symbol/type inventory. The process adapter binds native
shell calls to declared execution tools and distinguishes infrastructure failure
from a normally unsuccessful compiler probe.

`configure.py` merges raw fragment assignments in order, captures the source's
top-level and Kconfig Make environments, and runs native `conf`. Configuration
bytes and semantic values are separate outputs. Diagnostic provenance and timing
do not enter the Kbuild semantic payload.

`make_ast.bzl` builds a generic GNU Make from the declared Make module's POSIX
source/header inventory, applying `gnu-make-4.4.1.patch` and the three export
includes. Syntax export preserves source expressions; setup export performs
native Make expansion but exits before remaking includes or executing targets.
Neither requires a Linux Kconfig parser or a source-specific generator binary.

The exporter producer root applies `make_inputs.patch` to expose Make's POSIX
input target and its native GNU glob fallback for the static-musl host. A consumer
using the published generic exporter does not need that patch. Kernel source
patches are distributed separately and must remain paired with their intended
source versions.

`bootstrap.bzl`, `repositories.bzl`, `../build.py` and `../launcher.c` package one
source-independent Make/Python tool for x86-64 and ARM64 hosts. Linux artifacts
are static ELF executables; macOS artifacts embed their ZIP inside the signed
Mach-O image. Python, Make, their native objects, standard library and licenses
are declared inputs. The launcher isolates Python and permits only Make syntax
export; it cannot evaluate a kernel configuration or execute a Make recipe.
