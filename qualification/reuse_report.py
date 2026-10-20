"""Measure compilation reuse from compact execution logs and aquery inventories.

The workflow uses BuildBuddy's raw decoder and jq to retain only invocation,
output-path and spawn fields. Read these projected JSONL records individually;
input sets and source-tree contents are never expanded into individual spawns.
"""

import argparse
from collections import Counter, deque
from collections.abc import Iterator
from dataclasses import dataclass
import gzip
import hashlib
import json
import os
from pathlib import Path
import shutil
from typing import NotRequired, TypeAlias, TypedDict

Outputs: TypeAlias = tuple[str, ...]
QueryId: TypeAlias = str | int


class PathFragment(TypedDict):
    id: QueryId
    label: str
    parentId: NotRequired[QueryId]


class Artifact(TypedDict):
    id: QueryId
    pathFragmentId: QueryId


class Action(TypedDict):
    mnemonic: str
    outputIds: list[QueryId]
    inputDepSetIds: NotRequired[list[QueryId]]
    targetId: NotRequired[QueryId]


class InputSet(TypedDict):
    id: QueryId
    directArtifactIds: NotRequired[list[QueryId]]
    transitiveDepSetIds: NotRequired[list[QueryId]]


class ActionQuery(TypedDict):
    pathFragments: list[PathFragment]
    artifacts: list[Artifact]
    actions: list[Action]
    depSetOfFiles: NotRequired[list[InputSet]]


class CompilerRecord(TypedDict):
    kind: str
    source: str
    object: str
    frontend: NotRequired[str]
    bitcode: NotRequired[str]
    integrated: NotRequired[str]
    flags: NotRequired[list[str]]


class CompilerDescriptor(TypedDict):
    kind: str
    directory: str
    source: str
    source_file: str


class CompilerJob(TypedDict):
    phase: str
    target: str
    source: str
    kind: str
    outputs: list[str]


class NativeRecipeArtifacts(TypedDict):
    report: str
    compressed: str
    expanded: str
    restored: str


class NativeRecipeReport(TypedDict):
    payload_size: int
    trailer_hex: str
    expanded: str
    roundtrip: bool
    lz4: str


class NativeRecipeEvidence(TypedDict):
    report: NativeRecipeReport
    sha256: dict[str, str]


class KernelManifest(TypedDict):
    compiler: str
    roots: list[str]
    artifacts: dict[str, str]
    compilers: dict[str, CompilerRecord]
    config: str
    config_owner: str
    compiler_inventory: dict[str, CompilerDescriptor]
    compiler_jobs: list[CompilerJob]
    compiler_diagnostics: NotRequired[list[str]]
    compiler_diagnostic_cases: NotRequired[dict[str, str]]
    native_recipe_checks: NotRequired[NativeRecipeArtifacts]


PHASES = ("baseline", "unchanged", "edited", "header", "incbin", "gzip")
PIGGY_OBJECT = "arch/x86/boot/compressed/piggy.o"
PIGGY_SOURCE = "arch/x86/boot/compressed/piggy.S"
GZIP_PAYLOAD = "arch/x86/boot/compressed/vmlinux.bin.gz"
BOOT_HEADERS = ("arch/x86/boot/voffset.h", "arch/x86/boot/zoffset.h")
HEADER_TARGETS = ("init/do_mounts.o", "init/do_mounts_initrd.o", "init/initramfs.o", "init/do_mounts_rd.o")
COMPILER_MNEMONICS = {"KernelArtifact", "KernelLLVMBackend"}
NATIVE_RECIPE_FILES = {"report": "report.json", "compressed": "compressed.lz4", "expanded": "expanded.txt", "restored": "restored.bin"}
NATIVE_RECIPE_EXPANDED = "quoted value|no|$literal"


def mapped(path: str) -> str:
    parts = path.split("/", 2)
    return "bazel-out/mapped/" + parts[2] if len(parts) == 3 and parts[0] == "bazel-out" else path


@dataclass
class Compiler:
    source: str
    outputs: dict[str, Outputs]
    bitcode_digest: str | None
    generated_inputs: dict[str, dict[str, str]]


def read_compilers(path: Path) -> dict[str, Compiler]:
    records = json.loads(path.read_text())
    if not isinstance(records, dict) or not records:
        raise ValueError(f"Missing compiler inventory: {path}")
    result = {}
    owned = set()
    for target, record in records.items():
        match target, record:
            case str(target), {"source": str(source), "outputs": dict(outputs), "bitcode_digest": digest, "generated_inputs": dict(generated_inputs)} if digest is None or isinstance(digest, str):
                pass
            case _:
                raise ValueError(f"Invalid compiler inventory record: {target}, {record}")
        if set(outputs) not in ({"single"}, {"frontend", "backend"}):
            raise ValueError(f"Invalid compiler phases for {target}: {outputs}")
        phases = {}
        for role, paths in outputs.items():
            if not isinstance(paths, list) or not paths or any(not isinstance(item, str) for item in paths):
                raise ValueError(f"Invalid {target} {role} outputs: {paths}")
            identity = tuple(sorted(paths))
            if owned.intersection(identity):
                raise ValueError(f"Duplicate compiler phase outputs: {identity}")
            owned.update(identity)
            phases[role] = identity
        if ("backend" in phases) != bool(digest):
            raise ValueError(f"Missing or unexpected bitcode digest for {target}")
        if set(generated_inputs) != set(phases):
            raise ValueError(f"Missing generated input phase inventory for {target}")
        for role, inputs in generated_inputs.items():
            if not isinstance(inputs, dict) or any(name not in BOOT_HEADERS or not isinstance(value, str) for name, value in inputs.items()):
                raise ValueError(f"Invalid generated inputs for {target} {role}: {inputs}")
        result[target] = Compiler(source, phases, digest, generated_inputs)
    return result


def records(path: Path) -> Iterator[dict[str, object]]:
    with path.open() as stream:
        for number, line in enumerate(stream, 1):
            record = json.loads(line)
            if not isinstance(record, dict):
                raise ValueError(f"Expected an object at {path}:{number}")
            yield record


def artifact_paths(query: ActionQuery) -> dict[str, str]:
    fragments = {str(item["id"]): item for item in query["pathFragments"]}

    def filename(fragment_id: str) -> str:
        parts = []
        visited = set()
        while fragment_id != "0":
            if fragment_id in visited:
                raise ValueError(f"Cyclic aquery path fragment: {fragment_id}")
            visited.add(fragment_id)
            fragment = fragments[fragment_id]
            parts.append(fragment["label"])
            fragment_id = str(fragment.get("parentId", 0))
        return "/".join(reversed(parts))

    return {
        str(item["id"]): filename(str(item["pathFragmentId"]))
        for item in query["artifacts"]
    }


@dataclass
class ActionGraph:
    artifacts: dict[str, str]
    identities: dict[str, str]
    actions: list[Action]
    producers: dict[str, int]
    dep_sets: dict[str, InputSet]

    @classmethod
    def read(cls, path: Path) -> "ActionGraph":
        query: ActionQuery = json.loads(path.read_text())
        artifacts = artifact_paths(query)
        identities = {name: identity for identity, name in artifacts.items()}
        if len(identities) != len(artifacts):
            raise ValueError("Aquery has duplicate artifact paths")
        dep_sets = {str(item["id"]): item for item in query.get("depSetOfFiles", [])}

        def input_ids(action: Action) -> set[str]:
            pending = [(str(item), False) for item in action.get("inputDepSetIds", [])]
            active, visited, result = set(), set(), set()
            while pending:
                identity, leaving = pending.pop()
                if leaving:
                    active.remove(identity)
                    visited.add(identity)
                    continue
                if identity in active:
                    raise ValueError(f"Cyclic aquery input set: {identity}")
                if identity in visited:
                    continue
                active.add(identity)
                pending.append((identity, True))
                inputs = dep_sets[identity]
                result.update(map(str, inputs.get("directArtifactIds", [])))
                pending.extend((str(item), False) for item in inputs.get("transitiveDepSetIds", []))
            return result

        producers = {}
        actions = []
        for action in query["actions"]:
            outputs = [str(output) for output in action["outputIds"]]
            previous = producers.get(outputs[0]) if outputs else None
            # Aquery can serialize one shared native action more than once,
            # using distinct nested-set IDs for the same input artifacts.
            # Compare every other field before expanding this duplicate's
            # inputs; ordinary actions never need per-action input closures.
            if previous is not None:
                original = actions[previous]
                if original == action or (
                    {k: v for k, v in original.items() if k != "inputDepSetIds"}
                    == {k: v for k, v in action.items() if k != "inputDepSetIds"}
                    and input_ids(original) == input_ids(action)
                ):
                    continue
            index = len(actions)
            for output in action["outputIds"]:
                identity = str(output)
                if identity not in artifacts or identity in producers:
                    raise ValueError(f"Invalid or conflicting aquery producer: {identity}")
                producers[identity] = index
            actions.append(action)
        return cls(artifacts, identities, actions, producers, dep_sets)

    def ancestry(self, roots: list[str]) -> set[str]:
        """Follow actual artifact/action/depset edges without flattening header sets."""
        identities = self.identities
        missing = [path for path in roots if path not in identities or identities[path] not in self.producers]
        if missing:
            raise ValueError(f"Requested outputs lack native producers: {missing}")
        stack = [("artifact", identities[path], False) for path in roots]
        active, visited, reached = set(), set(), set()
        while stack:
            kind, identity, leaving = stack.pop()
            node = kind, identity
            if leaving:
                active.remove(node)
                visited.add(node)
                continue
            if node in active:
                raise ValueError(f"Cyclic native action graph at {node}")
            if node in visited:
                continue
            active.add(node)
            stack.append((kind, identity, True))
            if kind == "artifact":
                reached.add(self.artifacts[identity])
                if identity in self.producers:
                    stack.append(("action", str(self.producers[identity]), False))
            elif kind == "action":
                stack.extend(("depset", str(item), False) for item in self.actions[int(identity)].get("inputDepSetIds", []))
            else:
                inputs = self.dep_sets[identity]
                stack.extend(("artifact", str(item), False) for item in inputs.get("directArtifactIds", []))
                stack.extend(("depset", str(item), False) for item in inputs.get("transitiveDepSetIds", []))
        return reached

    def producer(self, path: str) -> Action:
        return self.actions[self.producers[self.identities[path]]]

    def consumes(self, output: str, required: str) -> bool:
        wanted = self.identities[required]
        pending = list(self.producer(output).get("inputDepSetIds", []))
        visited = set()
        while pending:
            identity = str(pending.pop())
            if identity in visited:
                continue
            visited.add(identity)
            inputs = self.dep_sets[identity]
            if wanted in {str(item) for item in inputs.get("directArtifactIds", [])}:
                return True
            pending.extend(inputs.get("transitiveDepSetIds", []))
        return False

    def derives(self, output: str, required: str) -> bool:
        """Check a producer chain, visiting nearby inputs before tool closures."""
        wanted = self.identities[required]
        pending = deque([self.identities[output]])
        visited, input_sets = set(), set()
        while pending:
            identity = pending.popleft()
            if identity == wanted:
                return True
            if identity in visited or identity not in self.producers:
                continue
            visited.add(identity)
            inputs = list(self.actions[self.producers[identity]].get("inputDepSetIds", []))
            while inputs:
                input_id = str(inputs.pop())
                if input_id in input_sets:
                    continue
                input_sets.add(input_id)
                entry = self.dep_sets[input_id]
                direct = [str(item) for item in entry.get("directArtifactIds", [])]
                if wanted in direct:
                    return True
                pending.extend(direct)
                inputs.extend(entry.get("transitiveDepSetIds", []))
        return False


def read_manifest(directory: Path, name: str, workspace: Path, graph: ActionGraph) -> KernelManifest:
    """Read the final scheduler manifest, whose role paths name actual producers."""
    match (directory / f"{name}.manifest-path").read_text().splitlines():
        case [manifest_path] if manifest_path:
            pass
        case other:
            raise ValueError(f"Expected one declared kernel manifest, found {other}")
    graph.producer(manifest_path)
    text = (workspace / manifest_path).read_text()
    manifest = json.loads(text)
    match manifest:
        case {"compiler": "clang", "roots": list(roots), "artifacts": dict(artifacts), "compilers": dict(compilers), "config": str(config), "config_owner": str(config_owner)} if roots and compilers:
            pass
        case _:
            raise ValueError(f"Invalid native kernel manifest: {manifest_path}")
    if any(not isinstance(root, str) or root not in artifacts for root in roots):
        raise ValueError(f"Invalid native product roots: {roots}")
    if any(not isinstance(key, str) or not isinstance(value, str) for key, value in artifacts.items()):
        raise ValueError("Native artifact inventory must map logical names to File paths")
    for target, compiler in compilers.items():
        match target, compiler:
            case str(target), {"kind": str(kind), "source": str(source), "object": str(obj)} if kind in ("c", "assembly", "codegen", "host"):
                pass
            case _:
                raise ValueError(f"Invalid native compiler record: {target}: {compiler}")
        if target not in artifacts:
            raise ValueError(f"Compiler has no final native artifact: {target}")
        if kind == "c":
            match compiler:
                case {"frontend": str(frontend), "bitcode": str(bitcode), "flags": list(flags)} if all(isinstance(flag, str) for flag in flags):
                    pass
                case _:
                    raise ValueError(f"Qualification requires split target C with native flags: {target}")
        if "integrated" in compiler and not isinstance(compiler["integrated"], str):
            raise ValueError(f"Invalid reference object path: {target}")
    match manifest:
        case {"compiler_inventory": dict(descriptors), "compiler_jobs": list(jobs)}:
            pass
        case _:
            raise ValueError("Missing native descriptor inventory or scheduled compiler roles")
    if descriptors.keys() != compilers.keys():
        raise ValueError(f"Native compiler denominator differs: {descriptors.keys() ^ compilers.keys()}")
    for target, descriptor in descriptors.items():
        match descriptor:
            case {"kind": str(kind), "directory": str(directory_name), "source": str(source), "source_file": str(source_file)} if kind == compilers[target]["kind"] and source_file == compilers[target]["source"]:
                pass
            case _:
                raise ValueError(f"Invalid native compiler descriptor: {target}: {descriptor}")
    if config != config_owner + "/.config" or graph.producer(config_owner)["mnemonic"] != "NativeKconfig":
        raise ValueError(f"Configuration is not owned by native conf: {config}, {config_owner}")
    (directory / f"{name}.manifest.json").write_text(text)
    return manifest


def check_native_recipe_files(paths: dict[str, Path]) -> NativeRecipeEvidence:
    """Check retained native recipe bytes independently of the pass report."""
    report = json.loads(paths["report"].read_text())
    match report:
        case {"payload_size": int(size), "trailer_hex": "01010000", "expanded": str(expanded), "roundtrip": True, "lz4": str(lz4)} if size == 257 and expanded == NATIVE_RECIPE_EXPANDED and Path(lz4).name in ("lz4", "lz4c"):
            pass
        case _:
            raise ValueError(f"Native recipe check did not pass: {report}")
    compressed = paths["compressed"].read_bytes()
    if len(compressed) <= 4 or compressed[-4:] != b"\x01\x01\x00\x00":
        raise ValueError("Native LZ4 output lacks the four-byte little-endian 257 trailer")
    if paths["expanded"].read_bytes() != NATIVE_RECIPE_EXPANDED.encode():
        raise ValueError("Native recipe captures did not expand before the first recipe line")
    if paths["restored"].read_bytes() != b"x" * 257:
        raise ValueError("Native LZ4 roundtrip did not restore the 257-byte payload")
    return NativeRecipeEvidence(report=NativeRecipeReport(payload_size=size, trailer_hex="01010000", expanded=expanded, roundtrip=True, lz4=lz4), sha256={role: hashlib.sha256(path.read_bytes()).hexdigest() for role, path in paths.items()})


def snapshot_native_recipe_checks(directory: Path, name: str, workspace: Path, manifest: KernelManifest, graph: ActionGraph) -> NativeRecipeEvidence:
    """Authenticate standalone controls without adding them to compiler work."""
    checks = manifest.get("native_recipe_checks")
    match checks:
        case {"report": str(report), "compressed": str(compressed), "expanded": str(expanded), "restored": str(restored)} if len(checks) == 4:
            paths = {"report": report, "compressed": compressed, "expanded": expanded, "restored": restored}
        case _:
            raise ValueError(f"Missing native recipe qualification artifacts: {checks}")
    if len(set(paths.values())) != 4:
        raise ValueError("Native recipe artifact roles must have distinct Files")
    missing = [path for path in paths.values() if path not in graph.identities or graph.identities[path] not in graph.producers]
    if missing:
        raise ValueError(f"Native recipe artifacts lack actual producers: {missing}")
    producers = {graph.producers[graph.identities[path]] for path in paths.values()}
    if len(producers) != 1 or graph.producer(report)["mnemonic"] != "KernelArtifact":
        raise ValueError("Native recipe artifacts must share one KernelArtifact producer")
    outputs = {graph.artifacts[str(item)] for item in graph.producer(report)["outputIds"]}
    compiler_outputs = {path for job in manifest["compiler_jobs"] for path in job["outputs"]}
    if outputs & compiler_outputs:
        raise ValueError("Native recipe controls share a production compiler action")
    evidence = check_native_recipe_files({role: workspace / path for role, path in paths.items()})
    destination = directory / "native-recipes" / name
    destination.mkdir(parents=True, exist_ok=True)
    for role, path in paths.items():
        shutil.copyfile(workspace / path, destination / NATIVE_RECIPE_FILES[role])
    return evidence


def read_native_recipe_checks(directory: Path, name: str) -> NativeRecipeEvidence:
    destination = directory / "native-recipes" / name
    return check_native_recipe_files({role: destination / filename for role, filename in NATIVE_RECIPE_FILES.items()})


def authenticate_compilers(manifest: KernelManifest, graph: ActionGraph) -> tuple[set[str], dict[str, CompilerRecord]]:
    """Authenticate descriptor inventory against product edges, never scan metadata."""
    artifacts = manifest["artifacts"]
    reached = graph.ancestry([artifacts[root] for root in manifest["roots"]])
    config_owner = manifest["config_owner"]
    # Native conf produces one TreeArtifact. Its expanded children may be
    # action inputs without independent producer actions in aquery.
    if config_owner not in reached and not any(path.startswith(config_owner + "/") for path in reached):
        raise ValueError(f"Product ancestry does not consume the native configuration: {config_owner}")
    inventory = {}
    owned = set()
    jobs = {}
    for job in manifest["compiler_jobs"]:
        match job:
            case {"phase": str(phase), "target": str(target), "source": str(source), "kind": str(kind), "outputs": list(outputs)} if outputs and all(isinstance(path, str) for path in outputs):
                pass
            case _:
                raise ValueError(f"Invalid scheduled compiler job: {job}")
        if target not in manifest["compilers"] or (target, phase) in jobs:
            raise ValueError(f"Unknown or duplicate scheduled compiler role: {target} {phase}")
        compiler = manifest["compilers"][target]
        if source != compiler["source"] or kind != compiler["kind"]:
            raise ValueError(f"Scheduled compiler identity differs from native descriptor: {job}")
        actual = {graph.artifacts[str(item)] for item in graph.producer(outputs[0])["outputIds"]}
        if actual != set(outputs):
            raise ValueError(f"Scheduled compiler outputs differ from actual producer: {job}")
        jobs[target, phase] = set(outputs)
    for target, compiler in manifest["compilers"].items():
        obj, source, kind = compiler["object"], compiler["source"], compiler["kind"]
        final = artifacts[target]
        if final not in reached and obj not in reached:
            continue
        assert final in reached and obj in reached, (target, final, obj)
        assert graph.derives(final, obj), (target, final, obj)
        expected = "KernelLLVMBackend" if kind == "c" else "KernelArtifact"
        assert graph.producer(obj)["mnemonic"] == expected, (target, obj, expected)
        if kind == "c":
            frontend, bitcode = compiler["frontend"], compiler["bitcode"]
            assert graph.producer(frontend)["mnemonic"] == "KernelArtifact", (target, frontend)
            assert graph.consumes(obj, bitcode), (target, obj, bitcode)
            assert bitcode == frontend or graph.consumes(bitcode, frontend), (target, bitcode, frontend)
            assert graph.consumes(frontend, source), (target, frontend, source)
            roles = [frontend, obj]
            if "integrated" in compiler:
                integrated = compiler["integrated"]
                assert graph.producer(integrated)["mnemonic"] == "KernelArtifact", (target, integrated)
                assert graph.consumes(integrated, source), (target, integrated, source)
                roles.append(integrated)
        else:
            assert graph.consumes(obj, source), (target, obj, source)
            roles = [obj]
        expected_roles = {"frontend": compiler["frontend"], "backend": obj} if kind == "c" else {"single": obj}
        if "integrated" in compiler:
            expected_roles["integrated"] = compiler["integrated"]
        actual_roles = {phase for selected, phase in jobs if selected == target}
        if actual_roles != expected_roles.keys():
            raise ValueError(f"Missing or unexpected scheduled compiler roles for {target}: {actual_roles}")
        for phase, path in expected_roles.items():
            if path not in jobs[target, phase]:
                raise ValueError(f"Scheduled compiler role does not produce {target} {phase}: {path}")
        assert len(set(roles)) == len(roles) and not owned.intersection(roles), (target, roles)
        owned.update(roles)
        inventory[target] = compiler
    # KernelArtifact also owns links and generators. The dedicated backend
    # mnemonic independently checks that the native C denominator is complete.
    actual = {
        graph.artifacts[str(output)]
        for action in graph.actions if action["mnemonic"] == "KernelLLVMBackend"
        for output in action["outputIds"] if graph.artifacts[str(output)] in reached
    }
    recorded = {record["object"] for record in inventory.values() if record["kind"] == "c"}
    assert actual and actual == recorded, {"unrecorded": sorted(actual - recorded), "unreachable": sorted(recorded - actual)}
    return reached, inventory


def snapshot(directory: Path, name: str, workspace: Path) -> None:
    graph = ActionGraph.read(directory / f"{name}.aquery.json")
    manifest = read_manifest(directory, name, workspace, graph)
    _, compilers = authenticate_compilers(manifest, graph)
    snapshot_native_recipe_checks(directory, name, workspace, manifest, graph)
    artifacts = manifest["artifacts"]
    inventory = {}
    paths = {}
    boot_headers = {header: hashlib.sha256((workspace / artifacts[header]).read_bytes()).hexdigest() for header in BOOT_HEADERS}
    for target, compiler in compilers.items():
        source, obj, kind = compiler["source"], compiler["object"], compiler["kind"]
        if kind == "host":
            continue
        roles = {"single": obj}
        bitcode_digest = None
        if "frontend" in compiler or "bitcode" in compiler:
            match compiler:
                case {"kind": "c", "frontend": str(frontend), "bitcode": str(bitcode)}:
                    pass
                case _:
                    raise ValueError(f"Invalid split compiler record: {compiler}")
            roles = {"frontend": frontend, "backend": obj}
            bitcode_digest = hashlib.sha256((workspace / bitcode).read_bytes()).hexdigest()
            # This trial edits this exact include. Its only backend effect is
            # through IR; native explicit non-preprocessor reads stay mandatory.
            if target in HEADER_TARGETS:
                header = compilers["init/do_mounts.o"]["source"].removesuffix(".c") + ".h"
                assert not graph.consumes(obj, header), (target, header)
        outputs = {}
        generated_inputs = {}
        for role, path in roles.items():
            action = graph.producer(path)
            assert action["mnemonic"] in COMPILER_MNEMONICS, (target, role)
            outputs[role] = sorted(mapped(graph.artifacts[str(item)]) for item in action["outputIds"])
            generated_inputs[role] = {header: digest for header, digest in boot_headers.items() if graph.consumes(path, artifacts[header])}
        inventory[target] = {"source": source, "outputs": outputs, "bitcode_digest": bitcode_digest, "generated_inputs": generated_inputs}
        paths[target] = obj
    assert inventory, "No product compiler actions"
    for target in ("usr/initramfs_inc_data", ".vmlinux.export.c", PIGGY_SOURCE, GZIP_PAYLOAD):
        paths[target] = artifacts[target]
    # Authenticate the extra boot compiler reached by a changed initramfs.
    assert graph.consumes(compilers["usr/initramfs_data.o"]["object"], artifacts["usr/initramfs_inc_data"]), "usr/initramfs_data.o"
    assert graph.consumes(compilers[PIGGY_OBJECT]["object"], artifacts[GZIP_PAYLOAD]), PIGGY_OBJECT
    assert artifacts["usr/initramfs_data.o"] in graph.ancestry([artifacts[GZIP_PAYLOAD]]), GZIP_PAYLOAD
    controls = ("init/main.o", "kernel/fork.o", "usr/initramfs_data.o", ".vmlinux.export.o", PIGGY_OBJECT,
                "usr/initramfs_inc_data", ".vmlinux.export.c", PIGGY_SOURCE, GZIP_PAYLOAD)
    digests = {target: hashlib.sha256((workspace / paths[target]).read_bytes()).hexdigest() for target in controls}
    digests.update(boot_headers)
    digests["decompressed_gzip"] = hashlib.sha256(gzip.decompress((workspace / paths[GZIP_PAYLOAD]).read_bytes())).hexdigest()
    (directory / f"{name}.compilers.json").write_text(json.dumps(inventory, indent=2) + "\n")
    (directory / f"{name}.digests.json").write_text(json.dumps(digests, indent=2) + "\n")
    sizes = {target: (workspace / paths[target]).stat().st_size for target in controls}
    (directory / f"{name}.sizes.json").write_text(json.dumps(sizes, indent=2) + "\n")


@dataclass
class Phase:
    invocation: str
    expected: set[Outputs]
    executed: set[Outputs]
    cached: set[Outputs]
    runners: Counter[str]
    mnemonics: Counter[tuple[str, str]]

    def outcome(self, outputs: Outputs) -> str:
        if outputs in self.executed:
            return "executed"
        if outputs in self.cached:
            return "logged_cache_hit"
        return "absent_from_execution_log"

    def report(self) -> dict[str, object]:
        return {
            "invocation": self.invocation,
            "total_compiler_actions": len(self.expected),
            "executed": len(self.executed),
            "logged_cache_hits": len(self.cached),
            "absent_from_execution_log": len(self.expected - self.executed - self.cached),
            "runners": dict(self.runners),
            "all_mnemonics": {
                f"{name}/{kind}": count
                for (name, kind), count in sorted(self.mnemonics.items())
            },
            "executed_outputs": sorted(self.executed),
            "logged_cache_hit_outputs": sorted(self.cached),
        }


def phase(path: Path, expected: set[Outputs]) -> Phase:
    entries = records(path)
    match next(entries, None):
        case {"invocation": str(invocation)} if invocation:
            result = Phase(invocation, expected, set(), set(), Counter(), Counter())
        case first:
            raise ValueError(f"Missing initial invocation in {path}: {first}")
    paths: dict[int, str] = {}
    expected_paths = {item for outputs in expected for item in outputs}
    for record in entries:
        match record:
            case {"id": int(entry_id), "path": str(output_path)} if entry_id > 0 and output_path:
                if entry_id in paths:
                    raise ValueError(f"Duplicate output entry ID in {path}: {entry_id}")
                paths[entry_id] = output_path
                continue
            case {"spawn": {
                "outputs": list(outputs), "mnemonic": str(mnemonic),
                "runner": str(runner), "cacheHit": bool(hit),
                "exitCode": int(exit_code), "status": str(status),
            }} if mnemonic:
                pass
            case _:
                raise ValueError(f"Unexpected compact-log projection in {path}: {record}")
        if status or exit_code:
            raise ValueError(f"Failed {mnemonic} spawn in {path}: {status}, exit {exit_code}")
        result.mnemonics[(mnemonic, "cache_hit" if hit else "executed")] += 1
        if mnemonic not in COMPILER_MNEMONICS:
            continue
        resolved = []
        for output in outputs:
            match output:
                case {"outputId": int(output_id)} if output_id in paths:
                    resolved.append(mapped(paths[output_id]))
                case _:
                    raise ValueError(f"Unresolved compiler output in {path}: {output}")
        identity = tuple(sorted(resolved))
        if identity not in expected:
            if expected_paths.intersection(identity):
                raise ValueError(f"Unknown compiler output in {path}: {identity}")
            continue
        if identity in result.executed or identity in result.cached:
            raise ValueError(f"Repeated compiler output in {path}: {identity}")
        (result.cached if hit else result.executed).add(identity)
        result.runners[runner] += 1
    return result


def report(directory: Path) -> dict[str, object]:
    native_recipes = {name: read_native_recipe_checks(directory, name) for name in PHASES}
    inventories = {name: read_compilers(directory / f"{name}.compilers.json") for name in PHASES}
    compilers = inventories["baseline"]
    identities = {target: (record.source, record.outputs, {role: sorted(inputs) for role, inputs in record.generated_inputs.items()}) for target, record in compilers.items()}
    if any({target: (record.source, record.outputs, {role: sorted(inputs) for role, inputs in record.generated_inputs.items()}) for target, record in value.items()} != identities for value in inventories.values()):
        raise ValueError("Kernel compile output inventory changed between builds")
    expected = {outputs for record in compilers.values() for outputs in record.outputs.values()}
    controls = {target: compilers[target].outputs for target in (
        "init/main.o", "kernel/fork.o", "init/do_mounts.o", "init/do_mounts_initrd.o", "init/initramfs.o", "usr/initramfs_data.o", PIGGY_OBJECT,
    )}
    phases = {
        name: phase(directory / f"{name}.execution.jsonl", expected)
        for name in inventories
    }
    if len({value.invocation for value in phases.values()}) != len(phases):
        raise ValueError("Build phases reused an invocation log")
    if expected - phases["baseline"].executed - phases["baseline"].cached:
        raise ValueError("Baseline did not observe every compiler action; inventory includes unmeasured actions")
    if phases["unchanged"].executed:
        raise ValueError("Unchanged build executed kernel compiles; edit comparison is not isolated")
    edited = phases["edited"]
    source_compiles = set(controls["init/main.o"].values())
    if missing := source_compiles - edited.executed:
        raise ValueError(f"Edited-source compile did not execute; trial is inconclusive: {sorted(missing)}")
    unrelated = expected - source_compiles
    unrelated_executed = unrelated & edited.executed
    header_required, header_allowed = set(), set()
    for target in HEADER_TARGETS:
        if target not in compilers:
            continue
        roles = compilers[target].outputs
        frontend = roles["frontend"] if "frontend" in roles else roles["single"]
        header_required.add(frontend)
        header_allowed.add(frontend)
        if "backend" in roles and inventories["edited"][target].bitcode_digest != inventories["header"][target].bitcode_digest:
            backend = roles["backend"]
            header_allowed.add(backend)
            if backend not in phases["header"].executed | phases["header"].cached:
                raise ValueError(f"Changed header IR has no observed backend: {target}")
    if missing := header_required - phases["header"].executed:
        raise ValueError(f"Included-header consumers did not execute: {sorted(missing)}")
    incbin_consumers = set(controls["usr/initramfs_data.o"].values()) | set(controls[PIGGY_OBJECT].values())
    if missing := incbin_consumers - phases["incbin"].executed:
        raise ValueError(f"Assembly embedding changed initramfs or boot payload did not execute: {sorted(missing)}")
    incbin_allowed = set(incbin_consumers)
    for target, previous in inventories["header"].items():
        current = inventories["incbin"][target]
        changed_roles = {role for role, inputs in current.generated_inputs.items() if inputs != previous.generated_inputs[role]}
        if not changed_roles:
            continue
        if "backend" in current.outputs and previous.bitcode_digest != current.bitcode_digest:
            changed_roles.add("backend")
        for role in changed_roles:
            outputs = current.outputs[role]
            incbin_allowed.add(outputs)
            if outputs not in phases["incbin"].executed | phases["incbin"].cached:
                raise ValueError(f"Changed native boot header has no observed compiler: {target} {role}")
    digests = {name: json.loads((directory / f"{name}.digests.json").read_text()) for name in PHASES}
    for target in ("usr/initramfs_inc_data", "usr/initramfs_data.o", GZIP_PAYLOAD, PIGGY_OBJECT):
        if digests["header"][target] == digests["incbin"][target]:
            raise ValueError(f"The initramfs edit did not change {target}")
    if len({value["kernel/fork.o"] for value in digests.values()}) != 1:
        raise ValueError("Unrelated kernel/fork.o bytes changed during the trial")
    # The directory-record edit changes vmlinux.o, but not its exported-symbol
    # C source. Rebuilding that source's ancestors must not recompile it.
    for target in (".vmlinux.export.c", ".vmlinux.export.o"):
        if digests["header"][target] != digests["incbin"][target]:
            raise ValueError(f"The initramfs edit unexpectedly changed {target}")
    gzip_consumers = set(controls[PIGGY_OBJECT].values())
    if not gzip_consumers <= phases["gzip"].executed:
        raise ValueError(
            "Assembly embedding the changed gzip payload did not execute; "
            f"the trial is inconclusive: {sorted(gzip_consumers - phases['gzip'].executed)}"
        )
    sizes = {name: json.loads((directory / f"{name}.sizes.json").read_text()) for name in PHASES}
    if sizes["incbin"][GZIP_PAYLOAD] != sizes["gzip"][GZIP_PAYLOAD]:
        raise ValueError("The gzip header edit changed the payload size")
    if digests["incbin"][PIGGY_SOURCE] != digests["gzip"][PIGGY_SOURCE]:
        raise ValueError("The gzip header edit changed generated piggy.S")
    if digests["incbin"]["decompressed_gzip"] != digests["gzip"]["decompressed_gzip"]:
        raise ValueError("The gzip header edit changed the decompressed payload")
    for target in (GZIP_PAYLOAD, PIGGY_OBJECT):
        if digests["incbin"][target] == digests["gzip"][target]:
            raise ValueError(f"The gzip header edit did not change {target}")
    for target in ("init/main.o", "usr/initramfs_data.o", ".vmlinux.export.c", ".vmlinux.export.o", *BOOT_HEADERS):
        if digests["incbin"][target] != digests["gzip"][target]:
            raise ValueError(f"The gzip header edit unexpectedly changed {target}")
    return {
        "measurement_complete": True,
        "native_recipe_checks": native_recipes,
        "compile_inventory_unchanged": True,
        "qualification": {
            "unrelated_logical_objects": len(compilers) - 1,
            "unrelated_compiler_actions": len(unrelated),
            "unrelated_executed_after_edit": len(unrelated_executed),
            "unrelated_not_executed_after_edit": len(unrelated - edited.executed),
            "unrelated_control_reused": not set(controls["kernel/fork.o"].values()) & edited.executed,
            "all_unrelated_compiles_reused": not unrelated_executed,
            "header_edit_recompiled_only_consumers": header_required <= phases["header"].executed <= header_allowed,
            "incbin_edit_recompiled_only_consumer": incbin_consumers <= phases["incbin"].executed <= incbin_allowed,
            "gzip_edit_recompiled_only_consumer": phases["gzip"].executed == gzip_consumers,
        },
        "logical_kernel_objects": len(compilers),
        "compiler_phase_counts": dict(Counter(role for record in compilers.values() for role in record.outputs)),
        "phases": {name: dict(value.report(), compiler_phases={
            role: {outcome: sum(value.outcome(record.outputs[role]) == outcome for record in compilers.values() if role in record.outputs)
                   for outcome in ("executed", "logged_cache_hit", "absent_from_execution_log")}
            for role in ("frontend", "backend", "single")
        }) for name, value in phases.items()},
        "controls": {
            source: {role: {"outputs": outputs, "after_edit": edited.outcome(outputs)} for role, outputs in roles.items()}
            for source, roles in controls.items()
        },
        "digests": digests,
        "sizes": sizes,
    }


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("snapshot", "report"))
    parser.add_argument("directory", type=Path)
    parser.add_argument("phase", choices=PHASES, nargs="?")
    args = parser.parse_args()
    if args.command == "snapshot":
        if args.phase is None:
            parser.error("snapshot requires a phase")
        snapshot(args.directory, args.phase, Path(os.environ["BUILD_WORKING_DIRECTORY"]))
        return
    if args.phase is not None:
        parser.error("report does not take a phase")
    result = report(args.directory)
    (args.directory / "reuse-report.json").write_text(json.dumps(result, indent=2) + "\n")
    # Keep every output identity in the artifact, while making CI logs readable.
    result["phases"] = {
        name: {key: value for key, value in measured.items() if not key.endswith("_outputs")}
        for name, measured in result["phases"].items()
    }
    print(json.dumps(result, indent=2))
    if not all(result["qualification"][key] for key in (
        "all_unrelated_compiles_reused", "header_edit_recompiled_only_consumers", "incbin_edit_recompiled_only_consumer", "gzip_edit_recompiled_only_consumer",
    )):
        raise SystemExit("Kernel dependency reuse qualification failed; see reuse-report.json")


if __name__ == "__main__":
    main()
