# Single-binary kernel repository prototype

This experimental implementation is independent of the existing kernel rules.
The repository-generation boundary now consists of **one statically linked
Linux/x86_64 ELF**. It contains GNU Make 4.4.1's patched native syntax exporter,
the checkout's Linux Kconfig parser, an embedded CPython interpreter, and the
renderer and Starlark templates. It extracts data/templates to a private temporary
directory, but never extracts another executable or a shared library. Native
parser subprocesses execute this same ELF, with separate entry points to isolate
the upstream parsers' global state.

Generation exports symbolic Make expressions, Kconfig entries and source edges.
Architecture, requested config, compiler facts, flags and object selection still
resolve during Bazel analysis using the existing platform/toolchain providers.
Kernel build actions do not invoke Make. The accepted conservative compiler-probe
fallback policy and existing architecture/boot limitations are unchanged.

## Build and try it

From the repository root:

```sh
# One-time host bootstrap dependencies (Debian):
sudo apt-get install musl-tools build-essential bison flex patch

python3 tools/kernel_repository/build.py --linux _tmp/linux

_tmp/kernel-repository/kernel-repository generate \
  --linux _tmp/linux --out _tmp/kernel-repository/repo --all-backends

python3 tools/kernel_repository/example.py
cd _tmp/kernel-repository/example
bazel build @kernel//examples:kernel
bazel build @kernel//multi_platform:configs
# All implemented architectures; this bootstraps the configured LLVM backends:
bazel build @kernel//multi_platform:kernels
```

Outputs must be empty directories. The bootstrap caches sources/objects under
`_tmp/kernel-repository/bootstrap`. GNU Make and CPython downloads have pinned
SHA256 digests. Building the tool currently uses host bootstrap tools; **running
it, including fetching the generated repository, needs none of those tools**.
The resulting binary is about 17 MB. `readelf -l -d` must show neither `INTERP`
nor `NEEDED`. No system Python, libc installation, Make, Bison, Flex, patch or
compiler is needed at fetch time. The tool requires Linux `/proc/self/exe`.

The Kconfig C parser is compiled from the supplied checkout. Its parser/support
source digests are embedded and checked before generation: rebuild the tool when
those sources change. Ordinary Kconfig/Makefile changes only require refetching.
`kernel_repository` watches the source tree for that invalidation.

## Repository rule

[`repository.bzl`](repository.bzl) exposes:

```starlark
kernel_repository = use_repo_rule("//:repository.bzl", "kernel_repository")
kernel_repository(
    name = "kernel",
    source = "@linux_source//:Makefile",
    generator = "@generator//:kernel-repository",
    all_backends = True,
)
```

`source` anchors the actual kernel checkout; `generator` is a prebuilt file label,
such as an exported local binary or an executable `http_file`. It cannot refer to
a binary that first needs a Bazel build action: repository fetching precedes
analysis and execution. The rule invokes it with `PATH` empty, and the tool uses
fixed environments for native parsing. It does not discover tools on the host.

Repository rules cannot add Bzlmod dependencies or register toolchains in the
consumer. `example.py` creates a runnable consumer workspace from the generated
MODULE template, relocating its local labels to `@kernel` and copying the two
accepted dependency patches into the root module. The example includes the
existing BCR dependency/toolchain setup, not another copy of the parser tool.
`//...` in a consumer addresses its own packages; use `@kernel//...` to address
the generated repository.

## Checks

```sh
# Optional host tracing tool, used only by this test runner:
sudo apt-get install strace
python3 tools/kernel_repository/test_repository.py \
  --tool _tmp/kernel-repository/kernel-repository \
  --linux _tmp/linux --out _tmp/kernel-repository/check \
  --reference _tmp/kernel-backends/repo --strace
```

This checks static ELF linkage, full generation with empty PATH and poisoned
compiler/Make/Python environment variables, and an exec trace containing only
the same binary. It compares the generated Kconfig and Make programs with the
existing prototype, and checks parser-version/output-directory failures.
Omit `--reference` when the old generated workspace is unavailable.

For a build/link check of the repository rule's output, run in the example:

```sh
bazel build @kernel//examples:kernel @kernel//execution_tools:check \
  --sandbox_block_path=/bin --sandbox_block_path=/usr/bin \
  --sandbox_block_path=/usr/local/bin
```

`native/` owns parser integration and bootstrap inputs; `renderer/` is the
independent renderer/runtime snapshot. `launcher.c` owns the static executable's
entry points, and `build.py` links the three native object groups together.
Upstream parser global symbols are namespaced before linking; libc references
remain shared. No dynamically loaded Python extension modules are required.

## CI

The `Kernel repository prototype` workflow checks out a pinned Linux commit, builds
the tool from scratch, audits generation with an exec trace, exercises the
repository rule, runs seven analysis tests, and builds/links the x86 example with
host tool directories blocked. It runs on this prototype branch, relevant pull
requests, and manual dispatch. The static executable and diagnostics are uploaded
as workflow artifacts. Existing root tests remain independent of local checkouts.
