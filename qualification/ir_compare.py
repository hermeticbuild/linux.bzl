"""Compare native object bytes and deferred compiler diagnostics."""

import argparse
import json
from pathlib import Path
import re
import shutil

from qualification.ir_reuse_report import FAMILY, FAMILY_PHASES, read_snapshot, snapshot


def check_diagnostics(cases: list[object]) -> dict[str, dict[str, int]]:
    result = {}
    for case in cases:
        match case:
            case {"name": str(name), "bitcode": str(bitcode), "outcomes": list(outcomes)}:
                pass
            case _:
                raise ValueError(f"Invalid diagnostic result: {case}")
        assert name in {"attribute", "assembly"} and name not in result, case
        phases = {}
        for outcome in outcomes:
            match outcome:
                case {"phase": str(phase), "returncode": int(returncode), "stdout": str(stdout), "stderr": str(stderr)}:
                    pass
                case _:
                    raise ValueError(f"Invalid compiler outcome: {outcome}")
            assert phase in {"integrated", "frontend", "backend"} and phase not in phases, outcome
            # The selected toolchain forces ANSI diagnostic colors even when
            # stderr is captured. Preserve the raw report and match its text.
            diagnostic = re.sub(r"\x1b\[[0-9;]*m", "", stderr)
            if phase == "frontend":
                assert returncode == 0, outcome
            else:
                assert returncode == 1, outcome
                if name == "attribute":
                    assert any("error: call to '" in line and "declared with 'error' attribute: linux-bzl-ir-attribute-error" in line for line in diagnostic.splitlines()), outcome
                else:
                    assert "error: linux-bzl-ir-inline-asm" in diagnostic, outcome
            assert not any(marker in stdout + diagnostic for marker in ["PLEASE submit a bug report", "Stack dump:", "Assertion failed", "LLVM ERROR:", "command failed with exit code", "command failed due to signal"]), outcome
            phases[phase] = returncode
        assert phases.keys() == {"integrated", "frontend", "backend"}, case
        result[name] = phases
    assert result.keys() == {"attribute", "assembly"}, result
    return result


def compare_family(directory: Path) -> None:
    snapshots = {name: read_snapshot(directory / f"{name}.ir.json") for name in FAMILY_PHASES}
    parity = {
        name: [target for target, record in values.items() if record["digests"]["integrated"] != record["digests"]["object"]]
        for name, values in snapshots.items()
    }
    diagnostics, errors = {}, {}
    for name in FAMILY_PHASES:
        cases = json.loads((directory / "diagnostics" / name / "report.json").read_text())
        try:
            diagnostics[name] = check_diagnostics(cases)
        except (AssertionError, ValueError) as error:
            errors[name] = str(error)
    base, changed = snapshots["base"], snapshots["changed"]
    control = "kernel/panic.o"
    result = {
        "objects": {name: len(records) for name, records in snapshots.items()},
        "object_mismatches": parity,
        "diagnostics": diagnostics,
        "diagnostic_errors": errors,
        "panic_ir_changed": base[control]["digests"]["bitcode"] != changed[control]["digests"]["bitcode"],
        "panic_object_changed": base[control]["digests"]["object"] != changed[control]["digests"]["object"],
    }
    (directory / "ir-comparison.json").write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps(result, indent=2))
    assert all(result["objects"][name] >= 4 for name in FAMILY), result["objects"]
    assert all(not mismatches for mismatches in parity.values()), parity
    assert not errors, errors
    assert result["panic_ir_changed"] and result["panic_object_changed"], result
    native = json.loads((directory / "base.compiler.json").read_text())
    flags = native["arch/x86/boot/string.o"]["flags"]
    assert "-g" in flags and "-m16" in flags, flags


def compare_controls(directory: Path, workspace: Path) -> None:
    """Check the two native ctype producers before running the full family."""
    parity, diagnostics, errors = {}, {}, {}
    for name in ("default", "5_10"):
        snapshot(directory, name, workspace)
        records = read_snapshot(directory / f"{name}.ir.json")
        assert "lib/ctype.o" in records, records.keys()
        mismatches = []
        for target, record in records.items():
            for role, path in record["paths"].items():
                destination = directory / "objects" / name / (target + "." + role)
                destination.parent.mkdir(parents=True, exist_ok=True)
                shutil.copyfile(workspace / path, destination)
            integrated = (workspace / record["paths"]["integrated"]).read_bytes()
            obj = (workspace / record["paths"]["object"]).read_bytes()
            if not obj or obj != integrated:
                mismatches.append(target)
        parity[name] = mismatches
        cases = json.loads((directory / "diagnostics" / name / "report.json").read_text())
        try:
            diagnostics[name] = check_diagnostics(cases)
        except (AssertionError, ValueError) as error:
            errors[name] = str(error)
    result = {"object_mismatches": parity, "diagnostics": diagnostics, "diagnostic_errors": errors}
    (directory / "controls-comparison.json").write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps(result, indent=2))
    assert all(not mismatches for mismatches in parity.values()), parity
    assert not errors, errors


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--family-results", type=Path)
    mode.add_argument("--controls-results", type=Path)
    parser.add_argument("--execution-root", type=Path)
    args = parser.parse_args()
    if args.controls_results is not None:
        if args.execution_root is None:
            parser.error("--controls-results requires --execution-root")
        compare_controls(args.controls_results, args.execution_root)
    else:
        compare_family(args.family_results)


if __name__ == "__main__":
    main()
