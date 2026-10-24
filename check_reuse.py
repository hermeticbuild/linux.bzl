#!/usr/bin/env python3
"""Run native compiler-family or source-mutation reuse qualification."""

import argparse
from collections.abc import Iterator
from contextlib import ExitStack, contextmanager
from datetime import UTC, datetime
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
from typing import TextIO
import uuid

from qualification import ir_reuse_report, reuse_report


FAMILY_TARGETS = {
    "base": "//examples:x86_64_kernel",
    "unchanged": "//examples:x86_64_kernel",
    "lz4": "//examples:x86_64_lz4_kernel",
    "debug": "//examples:x86_64_debug_kernel",
    "btf": "//examples:x86_64_btf_vmlinux",
    "changed": "//examples:x86_64_changed_kernel",
}


@contextmanager
def phase(directory: Path, name: str, stage: str, *, target: str | None = None) -> Iterator[dict[str, int]]:
    """Record the last reached operation without changing qualification results."""
    def record(status: str, **details: object) -> None:
        line = json.dumps({
            "time": datetime.now(UTC).isoformat(),
            "directory": str(directory),
            "phase": name,
            "stage": stage,
            "target": target,
            "status": status,
            **details,
        })
        write_error: OSError | None = None
        try:
            with (directory / "progress.jsonl").open("a") as output:
                output.write(line + "\n")
        except OSError as error:
            write_error = error
        try:
            print(line, flush=True)
            if write_error is not None:
                print(f"Could not retain qualification progress: {write_error}", file=sys.stderr, flush=True)
        except OSError:
            # A full filesystem or closed console must not replace the
            # measured operation's result, including an existing exception.
            pass

    result: dict[str, int] = {}
    record("started")
    try:
        yield result
    except BaseException as error:
        details: dict[str, object] = {"error": type(error).__name__}
        if isinstance(error, subprocess.CalledProcessError):
            details["returncode"] = error.returncode
        elif isinstance(error, SystemExit):
            details["exit"] = error.code
        record("failed", **details)
        raise
    else:
        record("failed" if result.get("returncode", 0) else "completed", **result)


def records(stream: TextIO) -> Iterator[dict[str, object]]:
    """Read the decoder's concatenated JSON without retaining its input trees."""
    decoder = json.JSONDecoder()
    buffer = ""
    read_size = 65536
    while True:
        chunk = stream.read(read_size)
        buffer += chunk
        while buffer.strip():
            buffer = buffer.lstrip()
            try:
                value, end = decoder.raw_decode(buffer)
            except json.JSONDecodeError:
                if not chunk:
                    raise
                read_size = min(read_size * 2, 16 * 1024 * 1024)
                break
            if not isinstance(value, dict):
                raise ValueError("Compact log entry is not an object")
            yield value
            buffer = buffer[end:]
            read_size = 65536
        if not chunk:
            return


def project(entry: dict[str, object]) -> dict[str, object] | None:
    """Keep the original reporter projection, including native cache digests."""
    match entry:
        case {"invocation": {"id": str(invocation), "hashFunctionName": str(algorithm)}}:
            return {"invocation": invocation, "hashFunctionName": algorithm}
        case {"spawn": dict(spawn)}:
            digest = spawn.get("digest", {})
            if not isinstance(digest, dict):
                raise ValueError(f"Invalid compact action digest: {digest}")
            return {"spawn": {
                "outputs": spawn.get("outputs", []),
                "mnemonic": spawn["mnemonic"],
                "runner": spawn.get("runner", ""),
                "cacheHit": spawn.get("cacheHit", False),
                "exitCode": spawn.get("exitCode", 0),
                "status": spawn.get("status", ""),
                "digest": {"hash": digest.get("hash", ""), "sizeBytes": int(digest.get("sizeBytes", 0))},
            }}
    for kind in ("file", "directory", "unresolvedSymlink"):
        match entry.get(kind):
            case {"path": str(path)}:
                return {"id": int(entry["id"]), "path": path}
    if "invocation" in entry:
        raise ValueError(f"Invalid compact invocation: {entry}")
    return None


def decode(decoder: str, log: Path, output: Path) -> None:
    command = [decoder, "print", "--compact_execution_log=" + str(log), "--raw=true", "--sort=false"]
    with output.open("w") as stream, subprocess.Popen(command, stdout=subprocess.PIPE, text=True) as process:
        assert process.stdout is not None
        try:
            for entry in records(process.stdout):
                if (record := project(entry)) is not None:
                    stream.write(json.dumps(record) + "\n")
        except BaseException:
            process.terminate()
            raise
        if process.wait():
            raise RuntimeError(f"Compact execution log decoder failed: {log}")


@contextmanager
def append_edit(path: Path, original: bytes, suffix: bytes) -> Iterator[None]:
    if path.read_bytes() != original:
        raise RuntimeError(f"Source changed concurrently before the reuse edit: {path}")
    path.write_bytes(original + suffix)
    try:
        yield
    finally:
        # Remove only this trial's unique addition, preserving concurrent edits.
        current = path.read_bytes()
        if suffix in current:
            path.write_bytes(current.replace(suffix, b"", 1))


def measure(decoder: str, target: str, options: list[str], directory: Path, name: str, workspace: Path, *, family: bool = False) -> Path:
    options = [*options, "--//:validate_native_objects"]
    # Snapshots read original declared Files, including cached intermediates.
    # Request those files without also requesting the exported tree or
    # the staging copies that native recipes consume remotely.
    capture_options = [
        *options, "--remote_download_outputs=minimal",
        "--remote_download_regex=.*[.](stage|backend)[0-9]+/artifacts/kernel[.](ir|integrated|raw|artifacts|validation)/.*",
        "--remote_download_regex=.*[.]manifest[.]json",
        "--remote_download_regex=.*/[.]config",
        "--output_groups=+boot,+qualification_manifest",
        "--sandbox_block_path=/bin", "--sandbox_block_path=/usr/bin", "--sandbox_block_path=/usr/local/bin",
    ]
    log = directory / f"{name}.execution.pb.zst"
    with phase(directory, name, "build", target=target):
        subprocess.run([
            "bazel", "build", target, *capture_options,
            "--execution_log_compact_file=" + str(log),
            "--profile=" + str(directory / f"{name}.profile.gz"),
            # Capture completed producers before another command checks source
            # changes and can dirty their retained Skyframe owners.
            "--experimental_aquery_dump_after_build_format=jsonproto",
            "--experimental_aquery_dump_after_build_output_file=" + str(directory / f"{name}.aquery.json"),
        ], cwd=workspace, check=True)
    with phase(directory, name, "decode", target=target):
        decode(decoder, log, directory / f"{name}.execution.jsonl")
    with phase(directory, name, "manifest", target=target):
        with (directory / f"{name}.manifest-path").open("w") as output:
            subprocess.run([
                "bazel", "cquery", target, *options,
                "--output=files", "--output_groups=qualification_manifest",
            ], cwd=workspace, stdout=output, check=True)
    with phase(directory, name, "execution_root", target=target):
        execution_root = Path(subprocess.check_output(["bazel", "info", "execution_root"], cwd=workspace, text=True).strip())
    snapshot = ir_reuse_report.snapshot if family else reuse_report.snapshot
    with phase(directory, name, "snapshot", target=target):
        snapshot(directory, name, execution_root)
    return execution_root


def family(decoder: str, options: list[str], directory: Path, workspace: Path, mutation_directory: Path | None = None) -> None:
    for name in ir_reuse_report.FAMILY_PHASES:
        execution_root = measure(decoder, FAMILY_TARGETS[name], options, directory, name, workspace, family=True)
        if mutation_directory is not None and name in ("base", "unchanged"):
            mutation_name = "baseline" if name == "base" else name
            with phase(mutation_directory, mutation_name, "shared_snapshot", target=FAMILY_TARGETS[name]):
                # Both comparisons use this invocation's evidence. A second warm
                # build can omit every local action-cache hit from the spawn log.
                for suffix in ("execution.pb.zst", "execution.jsonl", "profile.gz", "aquery.json", "manifest-path"):
                    shutil.copyfile(directory / f"{name}.{suffix}", mutation_directory / f"{mutation_name}.{suffix}")
                # Snapshot actual stage Files before another configuration can
                # replace them; do not substitute the family reporter's schema.
                reuse_report.snapshot(mutation_directory, mutation_name, execution_root)
    # Each comparator must run even when the other rejects the evidence.
    with phase(directory, "family", "parity") as result:
        comparison = subprocess.run([
            sys.executable, "-m", "qualification.ir_compare", "--family-results", str(directory),
        ], cwd=workspace)
        result["returncode"] = comparison.returncode
    with phase(directory, "family", "reuse") as result:
        reuse = subprocess.run([
            sys.executable, "-m", "qualification.ir_reuse_report", "report", str(directory), "--family",
        ], cwd=workspace)
        result["returncode"] = reuse.returncode
    if comparison.returncode or reuse.returncode:
        raise SystemExit(f"Compiler-family qualification failed: parity={comparison.returncode}, reuse={reuse.returncode}; see {directory}")


def mutations(decoder: str, target: str, options: list[str], directory: Path, workspace: Path, source: Path, *, baseline_captured: bool = False) -> None:
    root = source.parent.parent
    paths = [source, root / "init/do_mounts.h", root / "usr/default_cpio_list", root / "scripts/Makefile.lib"]
    originals = [path.read_bytes() for path in paths]
    for path, content in zip(paths, originals, strict=True):
        (directory / (path.name + ".original")).write_bytes(content)
    nonce = uuid.uuid4()
    octals = "".join(f"\\{byte:03o}" for byte in nonce.bytes[:4])
    edits = [
        f"\n/* kernel source reuse {nonce} */\n".encode(),
        f"\n/* kernel header reuse {nonce} */\n".encode(),
        f"\ndir /linux-bzl-incbin-{nonce} 0755 0 0\n".encode(),
        f'\n# kernel gzip reuse {nonce}\ncmd_gzip += ; printf "{octals}" | dd of=$@ bs=1 seek=4 conv=notrunc status=none\n'.encode(),
    ]
    # Each later mutation is measured on top of the preceding one, exactly as
    # in the original trial. Restore only our additions on every normal exit.
    with ExitStack() as stack:
        for index, name in enumerate(reuse_report.PHASES):
            if baseline_captured and index < 2:
                continue
            if index >= 2:
                edit = index - 2
                stack.enter_context(append_edit(paths[edit], originals[edit], edits[edit]))
            measure(decoder, target, options, directory, name, workspace)
    with phase(directory, "mutations", "reuse"):
        result = reuse_report.report(directory)
        (directory / "reuse-report.json").write_text(json.dumps(result, indent=2) + "\n")
        print(json.dumps(result["qualification"], indent=2))
        if not all(result["qualification"][key] for key in (
            "all_unrelated_compiles_reused", "header_edit_recompiled_only_consumers",
            "incbin_edit_recompiled_only_consumer", "gzip_edit_recompiled_only_consumer",
        )):
            raise SystemExit("Kernel dependency reuse qualification failed; see reuse-report.json")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--decoder", required=True)
    parser.add_argument("--family", action="store_true", help="Qualify base/unchanged/LZ4/debug/BTF/changed configurations")
    parser.add_argument("--source", type=Path, help="Editable native init/main.c; combine with --family to share its baseline capture")
    parser.add_argument("--target", help="Standalone mutation target (default: //examples:x86_64_kernel)")
    parser.add_argument("--out", type=Path, help="Empty evidence directory (default: validation, validation/family, or validation/reuse)")
    parser.add_argument("--bazel-option", action="append", default=[])
    args = parser.parse_args()
    workspace = Path(os.environ.get("BUILD_WORKSPACE_DIRECTORY", os.getcwd()))
    if args.family and args.target is not None:
        parser.error("--target applies only to standalone mutation qualification")
    if not args.family and args.source is None:
        parser.error("mutation qualification requires --source")
    source = (workspace / args.source).resolve() if args.source is not None else None
    if source is not None and (source.parts[-2:] != ("init", "main.c") or not source.is_file()):
        parser.error("--source must identify native init/main.c for the fixed mutation controls")
    combined = args.family and source is not None
    default = "validation" if combined else "validation/family" if args.family else "validation/reuse"
    directory = (workspace / (args.out or Path(default))).resolve()
    directory.mkdir(parents=True, exist_ok=True)
    if any(directory.iterdir()):
        parser.error("--out already contains evidence; choose a separate empty directory")
    family_directory, mutation_directory = directory, directory
    if combined:
        family_directory, mutation_directory = directory / "family", directory / "reuse"
        family_directory.mkdir()
        mutation_directory.mkdir()
    if args.family:
        family(args.decoder, args.bazel_option, family_directory, workspace, mutation_directory if combined else None)
    if source is not None:
        mutations(args.decoder, args.target or FAMILY_TARGETS["base"], args.bazel_option, mutation_directory, workspace, source, baseline_captured=combined)


if __name__ == "__main__":
    main()
