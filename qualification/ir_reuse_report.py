"""Measure native IR backend reuse from declared artifacts and compact logs."""

import argparse
import hashlib
import itertools
import json
import os
import shutil
from pathlib import Path
from typing import TypedDict

from qualification.reuse_report import (
    ActionGraph,
    COMPILER_MNEMONICS,
    CompilerRecord as NativeCompilerRecord,
    KernelManifest,
    authenticate_compilers,
    mapped,
    read_manifest,
    read_native_recipe_checks,
    records,
    snapshot_native_recipe_checks,
)

FAMILY = ("base", "debug", "btf", "lz4")
FAMILY_PHASES = ("base", "unchanged", "lz4", "debug", "btf", "changed")


class ArtifactRecord(TypedDict):
    paths: dict[str, str]
    digests: dict[str, str]


class CompilerRecord(TypedDict):
    source: str
    frontend: str
    bitcode: str
    integrated: str
    object: str
    flags: list[str]


class ActionDigest(TypedDict):
    hash: str
    size_bytes: int


class Outcome(TypedDict):
    cache_hit: bool
    runner: str
    action_digest: ActionDigest


class FrontendOutcome(Outcome):
    output: str


class Execution(TypedDict):
    invocation: str
    hash_function: str
    outcomes: dict[str, Outcome]
    frontend: list[FrontendOutcome]


def string_map(value: object) -> dict[str, str]:
    if not isinstance(value, dict) or any(not isinstance(key, str) or not isinstance(item, str) for key, item in value.items()):
        raise ValueError(f"Expected a string map, got {value}")
    return value


def read_snapshot(path: Path) -> dict[str, ArtifactRecord]:
    value = json.loads(path.read_text())
    if not isinstance(value, dict):
        raise ValueError(f"Expected compiler records in {path}")
    result = {}
    for target, record in value.items():
        match target, record:
            case str(target), {"paths": paths, "digests": digests}:
                paths, digests = string_map(paths), string_map(digests)
                expected = {"frontend", "bitcode", "integrated", "object"}
                if paths.keys() != expected or digests.keys() != expected:
                    raise ValueError(f"Incomplete compiler roles in {path}: {target}")
                result[target] = ArtifactRecord(paths=paths, digests=digests)
            case _:
                raise ValueError(f"Invalid compiler record in {path}: {target}, {record}")
    return result


def snapshot(directory: Path, name: str, workspace: Path) -> None:
    query_path = directory / f"{name}.aquery.json"
    graph = ActionGraph.read(query_path)
    manifest = read_manifest(directory, name, workspace, graph)
    reached, inventory = authenticate_compilers(manifest, graph)
    snapshot_native_recipe_checks(directory, name, workspace, manifest, graph)
    report: dict[str, CompilerRecord] = {}
    for target, compiler in inventory.items():
        if compiler["kind"] != "c":
            continue
        match compiler:
            case {"source": str(source), "frontend": str(frontend), "bitcode": str(bitcode), "integrated": str(integrated), "object": str(obj), "flags": list(flags)}:
                report[target] = CompilerRecord(source=source, frontend=frontend, bitcode=bitcode, integrated=integrated, object=obj, flags=flags)
            case _:
                raise ValueError(f"Native C object lacks an integrated reference: {target}")
    assert report, "No compiler IR artifacts were requested"
    authenticate(directory, name, workspace, manifest, graph, reached, inventory)
    artifacts = set(graph.identities)
    (directory / f"{name}.compiler.json").write_text(json.dumps(report, indent=2) + "\n")
    del graph
    result = {}
    for target, record in report.items():
        match record:
            case {"frontend": str(frontend), "bitcode": str(bitcode), "integrated": str(integrated), "object": str(obj)}:
                paths = {"frontend": frontend, "bitcode": bitcode, "integrated": integrated, "object": obj}
                missing = set(paths.values()) - artifacts
                assert not missing, (target, missing)
            case _:
                raise ValueError(f"Invalid compiler artifact record: {record}")
        digests = {role: hashlib.sha256((workspace / path).read_bytes()).hexdigest() for role, path in paths.items()}
        result[target] = {"paths": paths, "digests": digests}
        if digests["integrated"] != digests["object"]:
            for role, path in paths.items():
                destination = directory / "mismatches" / name / (target + "." + role)
                destination.parent.mkdir(parents=True, exist_ok=True)
                shutil.copyfile(workspace / path, destination)
    (directory / f"{name}.ir.json").write_text(json.dumps(result, indent=2) + "\n")


def snapshot_diagnostics(directory: Path, name: str, workspace: Path, manifest: KernelManifest, graph: ActionGraph) -> None:
    """Resolve native report paths only through the scheduler's explicit map."""
    match manifest.get("compiler_diagnostics"):
        case list(diagnostics) if all(isinstance(path, str) for path in diagnostics):
            pass
        case other:
            raise ValueError(f"Invalid diagnostic artifact inventory: {other}")
    match [path for path in diagnostics if path.endswith(".json")]:
        case [diagnostic_report]:
            pass
        case other:
            raise ValueError(f"Expected one compiler diagnostic report: {other}")
    bitcodes = string_map(manifest.get("compiler_diagnostic_cases"))
    if len(set(bitcodes.values())) != len(bitcodes) or diagnostic_report in bitcodes.values():
        raise ValueError("Compiler diagnostic roles must have distinct actual Files")
    expected = {diagnostic_report, *bitcodes.values()}
    if len(diagnostics) != len(expected) or set(diagnostics) != expected:
        raise ValueError(f"Compiler diagnostic artifact membership differs: {diagnostics}, {expected}")
    missing = [path for path in diagnostics if path not in graph.identities or graph.identities[path] not in graph.producers]
    if missing:
        raise ValueError(f"Compiler diagnostic artifacts lack actual producers: {missing}")
    producers = {graph.producers[graph.identities[path]] for path in diagnostics}
    producer = graph.producer(diagnostic_report)
    outputs = {graph.artifacts[str(item)] for item in producer["outputIds"]}
    if len(producers) != 1 or producer["mnemonic"] != "KernelArtifact" or outputs != expected:
        raise ValueError("Compiler diagnostic artifacts must be the outputs of one KernelArtifact producer")
    compiler_outputs = {path for job in manifest["compiler_jobs"] for path in job["outputs"]}
    if outputs & compiler_outputs:
        raise ValueError("Compiler diagnostic controls share a production compiler action")
    cases = json.loads((workspace / diagnostic_report).read_text())
    if not isinstance(cases, list):
        raise ValueError(f"Invalid diagnostic report: {cases}")
    names, planned = set(), set()
    resolved = []
    for case in cases:
        match case:
            case {"name": str(case_name), "bitcode": str(bitcode), "outcomes": list()} if case_name in ("attribute", "assembly"):
                pass
            case _:
                raise ValueError(f"Invalid diagnostic artifact: {case}")
        if case_name in names or bitcode in planned or bitcode not in bitcodes:
            raise ValueError(f"Unknown or duplicate compiler diagnostic case: {case_name}, {bitcode}")
        names.add(case_name)
        planned.add(bitcode)
        actual = bitcodes[bitcode]
        if (workspace / actual).stat().st_size == 0:
            raise ValueError(f"Empty compiler diagnostic bitcode: {actual}")
        resolved.append(dict(case, bitcode=actual))
    if names != {"attribute", "assembly"} or planned != bitcodes.keys():
        raise ValueError(f"Incomplete compiler diagnostic cases: {names}, {planned}, {bitcodes}")
    destination = directory / "diagnostics" / name
    destination.mkdir(parents=True, exist_ok=True)
    for case in resolved:
        shutil.copyfile(workspace / case["bitcode"], destination / (case["name"] + ".bc"))
    (destination / "report.json").write_text(json.dumps(resolved, indent=2) + "\n")


def authenticate(directory: Path, name: str, workspace: Path, manifest: KernelManifest, graph: ActionGraph, reached: set[str], inventory: dict[str, NativeCompilerRecord]) -> None:
    products = {root: manifest["artifacts"][root] for root in manifest["roots"]}
    snapshot_diagnostics(directory, name, workspace, manifest, graph)
    result = {
        "products": {root: {"path": path, "sha256": hashlib.sha256((workspace / path).read_bytes()).hexdigest()} for root, path in products.items()},
        "compilers": inventory,
        "compiler_counts": {kind: sum(item["kind"] == kind for item in inventory.values()) for kind in ("c", "host", "assembly", "codegen")},
        "config": (workspace / manifest["config"]).read_text(),
        "config_path": manifest["config"],
        "config_owner": manifest["config_owner"],
        "reached_artifacts": len(reached),
    }
    (directory / f"{name}.products.json").write_text(json.dumps(result, indent=2) + "\n")


def execution(path: Path, targets: dict[str, ArtifactRecord]) -> Execution:
    entries = records(path)
    match next(entries, None):
        case {"invocation": str(invocation), "hashFunctionName": str(hash_function)} if invocation and hash_function:
            pass
        case first:
            raise ValueError(f"Missing compact log invocation: {first}")
    selected = {
        mapped(values["paths"][role]): (target, role)
        for target, values in targets.items()
        for role in ("frontend", "integrated", "object")
    }
    if len(selected) != len(targets) * 3:
        raise ValueError("Compiler roles share output paths in one invocation")
    paths: dict[int, str] = {}
    seen: set[str] = set()
    outcomes: dict[str, Outcome] = {}
    frontend: list[FrontendOutcome] = []
    for record in entries:
        match record:
            case {"id": int(entry_id), "path": str(path)} if entry_id > 0 and path:
                if entry_id in paths:
                    raise ValueError(f"Duplicate compact output ID {entry_id}")
                paths[entry_id] = path
                continue
            case {"spawn": {
                "outputs": list(outputs), "mnemonic": str(mnemonic),
                "runner": str(runner), "cacheHit": bool(hit),
                "exitCode": int(exit_code), "status": str(status),
                "digest": {"hash": str(action_hash), "sizeBytes": int(action_size)},
            }}:
                pass
            case _:
                raise ValueError(f"Unexpected compact projection {record}")
        if exit_code or status:
            raise ValueError(f"Failed {mnemonic}: {exit_code}, {status}")
        if mnemonic not in COMPILER_MNEMONICS:
            continue
        digest = ActionDigest(hash=action_hash, size_bytes=action_size)
        resolved = []
        for output in outputs:
            match output:
                case {"outputId": int(output_id)} if output_id in paths:
                    resolved.append(paths[output_id])
                case _:
                    raise ValueError(f"Unknown compiler output {output}")
        for output in resolved:
            identity = selected.get(mapped(output))
            if identity is not None:
                target, role = identity
                expected_mnemonic = "KernelLLVMBackend" if role == "object" else "KernelArtifact"
                if mnemonic != expected_mnemonic:
                    raise ValueError(f"Wrong compiler role producer for {target} {role}: {mnemonic}")
                key = role + ":" + target
                if key in seen:
                    raise ValueError(f"Duplicate selected compiler {key}")
                seen.add(key)
                if not action_hash or action_size <= 0:
                    raise ValueError(f"Selected compiler {key} has no action-cache digest")
                if role == "frontend":
                    frontend.append({"output": output, "cache_hit": hit, "runner": runner, "action_digest": digest})
                else:
                    outcomes[key] = {"cache_hit": hit, "runner": runner, "action_digest": digest}
    return {"invocation": invocation, "hash_function": hash_function, "outcomes": outcomes, "frontend": frontend}


def summarize(directory: Path) -> None:
    names = FAMILY_PHASES
    native_recipes = {name: read_native_recipe_checks(directory, name) for name in names}
    snapshots = {name: read_snapshot(directory / f"{name}.ir.json") for name in names}
    expected = snapshots["base"].keys()
    assert expected and all(snapshots.values()), "Empty native compiler inventory"
    assert snapshots["unchanged"].keys() == expected, "Unchanged compiler inventory differs"
    phases = {name: execution(directory / f"{name}.execution.jsonl", snapshots[name]) for name in names}
    assert len({value["invocation"] for value in phases.values()}) == len(names), "Repeated invocation IDs"
    assert len({value["hash_function"] for value in phases.values()}) == 1, "Different action-cache digest algorithms"
    parity = {
        name: [target for target, record in values.items() if record["digests"]["integrated"] != record["digests"]["object"]]
        for name, values in snapshots.items()
    }
    products = {name: json.loads((directory / f"{name}.products.json").read_text()) for name in names}
    common = expected & snapshots["lz4"].keys()
    common = {target for target in common if mapped(products["base"]["compilers"][target]["source"]) == mapped(products["lz4"]["compilers"][target]["source"])}
    equal = [target for target in sorted(common) if snapshots["base"][target]["digests"]["bitcode"] == snapshots["lz4"][target]["digests"]["bitcode"]]
    paired = []
    hits = []
    for target in equal:
        base = phases["base"]["outcomes"].get("object:" + target)
        lz4 = phases["lz4"]["outcomes"].get("object:" + target)
        if base is not None and lz4 is not None and base["action_digest"] == lz4["action_digest"]:
            paired.append(target)
            if lz4["cache_hit"]:
                hits.append(target)
    control = "kernel/panic.o"
    changed = all(snapshots["base"][control]["digests"][role] != snapshots["changed"][control]["digests"][role] for role in ("bitcode", "object"))
    control_outcome = phases["changed"]["outcomes"].get("object:" + control)
    base_control = phases["base"]["outcomes"].get("object:" + control)
    control_action_changed = base_control is not None and control_outcome is not None and base_control["action_digest"] != control_outcome["action_digest"]
    unchanged_executions = [
        key for key, outcome in phases["unchanged"]["outcomes"].items() if not outcome["cache_hit"]
    ] + [outcome["output"] for outcome in phases["unchanged"]["frontend"] if not outcome["cache_hit"]]
    unchanged_digests = all(snapshots["base"][target]["digests"] == snapshots["unchanged"][target]["digests"] for target in expected)
    result = {
        "native_recipe_checks": native_recipes,
        "phases": phases, "object_mismatches": parity, "equal_base_lz4_ir": equal,
        "lz4_backend_hits": hits, "panic_changed": changed,
        "paired_backend_actions": paired, "panic_action_changed": control_action_changed,
        "panic_backend_outcome": control_outcome,
        "unchanged_executions": unchanged_executions, "unchanged_digests": unchanged_digests,
        "counts": {name: len(values) for name, values in snapshots.items()},
        "base_lz4": {"common_c_compilers": len(common), "equal_ir": len(equal), "equal_backend_actions": len(paired), "actual_backend_cache_hits": len(hits), "hit_fraction_of_lz4_c": len(hits) / len(snapshots["lz4"])},
    }
    memberships = {}
    for size in range(2, len(FAMILY) + 1):
        for members in itertools.combinations(FAMILY, size):
            shared = set.intersection(*(set(snapshots[member]) for member in members))
            identities = []
            for target in sorted(shared):
                outcomes = [phases[member]["outcomes"].get("object:" + target) for member in members]
                if all(outcome is not None for outcome in outcomes):
                    digests = [outcome["action_digest"] for outcome in outcomes if outcome is not None]
                else:
                    continue
                if all(digest == digests[0] for digest in digests) and len({mapped(products[member]["compilers"][target]["source"]) for member in members}) == 1:
                    assert len({snapshots[member][target]["digests"]["bitcode"] for member in members}) == 1, (members, target)
                    identities.append(target)
            memberships["/".join(members)] = {"common_c_compilers": len(shared), "equal_native_backend_actions": identities}
    result["memberships"] = memberships
    result["products"] = products
    result["family_checks"] = {
        "all_vmlinux": all("vmlinux" in products[name]["products"] for name in FAMILY),
        "both_boot_images": all("arch/x86/boot/bzImage" in products[name]["products"] for name in ("base", "lz4")),
        "per_variant_c_floor": all(len(snapshots[name]) >= 4 for name in FAMILY),
        "debug_config": all(line in products["debug"]["config"].splitlines() for line in ('CONFIG_DEBUG_KERNEL=y', 'CONFIG_LOCALVERSION="-debug"')),
        "btf_config": "CONFIG_DEBUG_INFO_BTF=y" in products["btf"]["config"].splitlines(),
        "lz4_config": "CONFIG_KERNEL_LZ4=y" in products["lz4"]["config"].splitlines(),
        "base_panic": "CONFIG_PANIC_TIMEOUT=0" in products["base"]["config"].splitlines(),
        "changed_panic": "CONFIG_PANIC_TIMEOUT=37" in products["changed"]["config"].splitlines(),
    }
    (directory / "ir-reuse.json").write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps({key: value for key, value in result.items() if key not in ("phases", "products")}, indent=2))
    assert all(result["family_checks"].values()), result["family_checks"]
    assert len(hits) >= 4, hits
    assert unchanged_digests and not unchanged_executions, result
    # A previous invocation may have cached the changed configuration too.
    # Preserve its actual outcome instead of treating a cache hit as execution.
    assert changed and control_action_changed, result


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("snapshot", "report"))
    parser.add_argument("directory", type=Path)
    parser.add_argument("phase", choices=FAMILY_PHASES, nargs="?")
    parser.add_argument("--family", action="store_true", required=True)
    args = parser.parse_args()
    if args.command == "snapshot":
        if args.phase is None:
            parser.error("snapshot requires a phase")
        snapshot(args.directory, args.phase, Path(os.environ["BUILD_WORKSPACE_DIRECTORY"]))
    else:
        if args.phase is not None:
            parser.error("report does not take a phase")
        summarize(args.directory)


if __name__ == "__main__":
    main()
