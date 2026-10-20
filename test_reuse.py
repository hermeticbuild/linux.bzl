"""Qualification rejects missing roles, invented hits and broad invalidation."""

import copy
import io
import json
import os
from pathlib import Path
import tempfile
import unittest

from check_reuse import append_edit, project, records
from qualification import ir_compare, ir_reuse_report, reuse_report


def spawn(output_id: int, *, backend: bool = False, hit: bool = False, digest: str = "action") -> dict:
    return {"spawn": {
        "outputs": [{"outputId": output_id}],
        "mnemonic": "KernelLLVMBackend" if backend else "KernelArtifact",
        "runner": "disk cache hit" if hit else "linux-sandbox",
        "cacheHit": hit, "exitCode": 0, "status": "",
        "digest": {"hash": digest, "sizeBytes": 123},
    }}


def diagnostics() -> list[dict]:
    return [
        {"name": name, "bitcode": name + ".bc", "outcomes": [
            {"phase": phase, "returncode": 0 if phase == "frontend" else 1, "stdout": "", "stderr": "" if phase == "frontend" else error}
            for phase in ("frontend", "integrated", "backend")
        ]}
        for name, error in [
            ("attribute", "error: call to 'fail' declared with 'error' attribute: linux-bzl-ir-attribute-error"),
            ("assembly", "error: linux-bzl-ir-inline-asm"),
        ]
    ]


class ReuseTest(unittest.TestCase):
    def setUp(self) -> None:
        temporary = tempfile.TemporaryDirectory(dir=os.environ.get("TEST_TMPDIR"))
        self.addCleanup(temporary.cleanup)
        self.directory = Path(temporary.name)

    def write(self, name: str, value: object) -> Path:
        path = self.directory / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(value))
        return path

    def log(self, name: str, entries: list[dict]) -> None:
        (self.directory / f"{name}.execution.jsonl").write_text(
            "\n".join(json.dumps(entry) for entry in [{"invocation": name, "hashFunctionName": "SHA-256"}, *entries]) + "\n"
        )

    def native_recipe_files(self, directory: Path) -> dict[str, str]:
        directory.mkdir(parents=True, exist_ok=True)
        paths = {role: directory / name for role, name in reuse_report.NATIVE_RECIPE_FILES.items()}
        paths["report"].write_text(json.dumps({"payload_size": 257, "trailer_hex": "01010000", "expanded": "quoted value|no|$literal", "roundtrip": True, "lz4": "declared/bin/lz4c"}))
        paths["compressed"].write_bytes(b"compressed fixture" + b"\x01\x01\x00\x00")
        paths["expanded"].write_bytes(b"quoted value|no|$literal")
        paths["restored"].write_bytes(b"x" * 257)
        return {role: path.relative_to(self.directory).as_posix() for role, path in paths.items()}

    def test_decoder_projection_preserves_real_digest_and_defaults(self) -> None:
        raw = [
            {"file": {"path": "a" * 70000}, "id": 1},
            {"spawn": {"mnemonic": "KernelLLVMBackend", "outputs": [{"outputId": 1}], "digest": {"hash": "real", "sizeBytes": "456"}}},
        ]
        self.assertEqual(list(records(io.StringIO("\n".join(map(json.dumps, raw))))), raw)
        projected = project(raw[1])["spawn"]
        self.assertEqual(projected["digest"], {"hash": "real", "sizeBytes": 456})
        self.assertEqual((projected["cacheHit"], projected["exitCode"], projected["status"]), (False, 0, ""))

    def test_source_restoration_preserves_concurrent_edits(self) -> None:
        path = self.directory / "source.c"
        path.write_bytes(b"original")
        with self.assertRaisesRegex(RuntimeError, "stop"):
            with append_edit(path, b"original", b"\n/* unique */\n"):
                path.write_bytes(path.read_bytes() + b"concurrent")
                raise RuntimeError("stop")
        self.assertEqual(path.read_bytes(), b"originalconcurrent")
        with self.assertRaisesRegex(RuntimeError, "changed concurrently"):
            with append_edit(path, b"original", b"marker"):
                self.fail("entered with stale source bytes")

    def native_graph(self) -> tuple[reuse_report.ActionGraph, dict]:
        names = ["vmlinux", "final.o", "backend.o", "frontend.bc", "reference.o", "source.c", "other.c", "manifest.json", "config", "config/include/generated/autoconf.h"]
        ids = {name: str(index) for index, name in enumerate(names, 1)}
        # A direct IR File is valid; no synthetic Symlink or .o suffix is needed.
        producers = [
            ("vmlinux", "KernelArtifact", ["final.o"]),
            ("final.o", "KernelArtifact", ["backend.o"]),
            ("backend.o", "KernelLLVMBackend", ["frontend.bc"]),
            ("frontend.bc", "KernelArtifact", ["source.c", "config/include/generated/autoconf.h"]),
            ("reference.o", "KernelArtifact", ["source.c", "config/include/generated/autoconf.h"]),
            ("manifest.json", "KernelManifest", []),
            ("config", "NativeKconfig", []),
        ]
        actions, sets = [], []
        for index, (output, mnemonic, inputs) in enumerate(producers, 1):
            actions.append({"mnemonic": mnemonic, "outputIds": [ids[output]], "inputDepSetIds": [str(index)]})
            sets.append({"id": str(index), "directArtifactIds": [ids[path] for path in inputs]})
        query = {
            "pathFragments": [{"id": identity, "label": name} for name, identity in ids.items()],
            "artifacts": [{"id": identity, "pathFragmentId": identity} for identity in ids.values()],
            "actions": actions, "depSetOfFiles": sets,
        }
        graph = reuse_report.ActionGraph.read(self.write("query.json", query))
        record = {"kind": "c", "source": "source.c", "object": "backend.o", "frontend": "frontend.bc", "bitcode": "frontend.bc", "integrated": "reference.o", "flags": ["-g"]}
        manifest = {
            "compiler": "clang", "roots": ["vmlinux"], "config": "config/.config", "config_owner": "config",
            "artifacts": {"vmlinux": "vmlinux", "unit.o": "final.o"},
            "compilers": {"unit.o": record},
            "compiler_inventory": {"unit.o": {"kind": "c", "source": "unit.c", "source_file": "source.c", "directory": ""}},
            "compiler_jobs": [
                {"phase": phase, "target": "unit.o", "kind": "c", "source": "source.c", "outputs": [record[key]]}
                for phase, key in (("frontend", "frontend"), ("backend", "object"), ("integrated", "integrated"))
            ],
        }
        return graph, manifest

    def test_actual_compiler_chain_and_explicit_roles(self) -> None:
        graph, manifest = self.native_graph()
        reached, inventory = reuse_report.authenticate_compilers(manifest, graph)
        self.assertEqual(set(inventory), {"unit.o"})
        self.assertIn("backend.o", reached)
        self.assertNotIn("reference.o", reached)
        self.assertTrue(graph.derives("final.o", "backend.o"))
        for defect in ("missing_backend", "wrong_ir", "wrong_source", "omitted_descriptor", "duplicate_role"):
            with self.subTest(defect=defect):
                altered = copy.deepcopy(manifest)
                if defect == "missing_backend":
                    altered["compiler_jobs"].pop(1)
                elif defect == "wrong_ir":
                    altered["compilers"]["unit.o"]["bitcode"] = "other.c"
                elif defect == "wrong_source":
                    altered["compilers"]["unit.o"]["source"] = "other.c"
                    for job in altered["compiler_jobs"]:
                        job["source"] = "other.c"
                elif defect == "omitted_descriptor":
                    altered["compilers"] = {}
                    altered["compiler_jobs"] = []
                else:
                    altered["compiler_jobs"].append(altered["compiler_jobs"][0])
                with self.assertRaises((AssertionError, ValueError)):
                    reuse_report.authenticate_compilers(altered, graph)

    def test_manifest_denominator_cannot_be_reduced_to_tagged_jobs(self) -> None:
        graph, manifest = self.native_graph()
        manifest["compiler_inventory"]["omitted.o"] = {"kind": "assembly", "source": "omitted.S", "directory": ""}
        self.write("manifest.json", manifest)
        (self.directory / "base.manifest-path").write_text("manifest.json\n")
        with self.assertRaisesRegex(ValueError, "denominator differs"):
            reuse_report.read_manifest(self.directory, "base", self.directory, graph)
        del manifest["compiler_inventory"]["omitted.o"]
        manifest["compiler_inventory"]["unit.o"]["source_file"] = "other.c"
        self.write("manifest.json", manifest)
        with self.assertRaisesRegex(ValueError, "native compiler descriptor"):
            reuse_report.read_manifest(self.directory, "base", self.directory, graph)

    def native_recipe_graph(self) -> tuple[reuse_report.ActionGraph, dict]:
        _, manifest = self.native_graph()
        paths = self.native_recipe_files(self.directory / "controls")
        manifest["native_recipe_checks"] = paths
        query = json.loads((self.directory / "query.json").read_text())
        outputs = []
        for path in paths.values():
            identity = str(len(query["artifacts"]) + 1)
            query["pathFragments"].append({"id": identity, "label": path})
            query["artifacts"].append({"id": identity, "pathFragmentId": identity})
            outputs.append(identity)
        query["actions"].append({"mnemonic": "KernelArtifact", "outputIds": outputs})
        return reuse_report.ActionGraph.read(self.write("query.json", query)), manifest

    def test_native_recipe_controls_are_authenticated_and_not_compiler_work(self) -> None:
        graph, manifest = self.native_recipe_graph()
        evidence = reuse_report.snapshot_native_recipe_checks(self.directory, "base", self.directory, manifest, graph)
        self.assertEqual(evidence, reuse_report.read_native_recipe_checks(self.directory, "base"))
        self.assertEqual(evidence["report"]["payload_size"], 257)
        reached, compilers = reuse_report.authenticate_compilers(manifest, graph)
        self.assertEqual(set(compilers), {"unit.o"})
        self.assertTrue(set(manifest["native_recipe_checks"].values()).isdisjoint(reached))
        self.log("controls", [{"id": 1, "path": manifest["native_recipe_checks"]["report"]}, spawn(1)])
        measured = reuse_report.phase(self.directory / "controls.execution.jsonl", {("backend.o",)})
        self.assertEqual(measured.executed, set())

    def test_native_recipe_controls_reject_missing_or_invented_producers(self) -> None:
        for defect in ("missing", "missing_role", "unknown_file", "shared_roles", "wrong_producer", "different_producers", "compiler_producer"):
            with self.subTest(defect=defect):
                graph, manifest = self.native_recipe_graph()
                checks = manifest["native_recipe_checks"]
                if defect == "missing":
                    del manifest["native_recipe_checks"]
                elif defect == "missing_role":
                    del checks["compressed"]
                elif defect == "unknown_file":
                    checks["compressed"] = "missing.lz4"
                elif defect == "shared_roles":
                    checks["restored"] = checks["expanded"]
                elif defect == "wrong_producer":
                    graph.producer(checks["report"])["mnemonic"] = "FileWrite"
                elif defect == "different_producers":
                    graph.producers[graph.identities[checks["compressed"]]] = graph.producers[graph.identities["frontend.bc"]]
                else:
                    manifest["compiler_jobs"].append({"outputs": [checks["report"]]})
                with self.assertRaises(ValueError):
                    reuse_report.snapshot_native_recipe_checks(self.directory, "bad", self.directory, manifest, graph)

    def test_native_recipe_controls_require_actual_bytes_and_pass_fields(self) -> None:
        for defect in ("literal_octal", "expanded", "restored", "payload_size", "trailer_hex", "report_expanded", "roundtrip", "lz4"):
            with self.subTest(defect=defect):
                graph, manifest = self.native_recipe_graph()
                checks = manifest["native_recipe_checks"]
                if defect == "literal_octal":
                    (self.directory / checks["compressed"]).write_bytes(b"compressed fixture" + br"\001\001\000\000")
                elif defect == "expanded":
                    (self.directory / checks["expanded"]).write_bytes(b"quoted value|yes|$literal")
                elif defect == "restored":
                    (self.directory / checks["restored"]).write_bytes(b"y" * 257)
                else:
                    path = self.directory / checks["report"]
                    report = json.loads(path.read_text())
                    field = "expanded" if defect == "report_expanded" else defect
                    report[field] = {"payload_size": 256, "trailer_hex": "00010100", "report_expanded": "quoted value|yes|$literal", "roundtrip": False, "lz4": "declared/bin/other"}[defect]
                    path.write_text(json.dumps(report))
                with self.assertRaises(ValueError):
                    reuse_report.snapshot_native_recipe_checks(self.directory, "bad", self.directory, manifest, graph)

    def diagnostic_graph(self) -> tuple[reuse_report.ActionGraph, dict]:
        _, manifest = self.native_graph()
        cases = diagnostics()
        bitcodes = {case["bitcode"]: "scheduled/" + case["name"] + ".bc" for case in cases}
        report = "scheduled/diagnostics.json"
        self.write(report, cases)
        for planned, actual in bitcodes.items():
            (self.directory / actual).write_bytes(planned.encode())
        manifest["compiler_diagnostics"] = [report, *bitcodes.values()]
        manifest["compiler_diagnostic_cases"] = bitcodes
        query = json.loads((self.directory / "query.json").read_text())
        outputs = []
        for path in manifest["compiler_diagnostics"]:
            identity = str(len(query["artifacts"]) + 1)
            query["pathFragments"].append({"id": identity, "label": path})
            query["artifacts"].append({"id": identity, "pathFragmentId": identity})
            outputs.append(identity)
        query["actions"].append({"mnemonic": "KernelArtifact", "outputIds": outputs})
        return reuse_report.ActionGraph.read(self.write("query.json", query)), manifest

    def test_diagnostic_paths_use_actual_scheduled_files(self) -> None:
        graph, manifest = self.diagnostic_graph()
        ir_reuse_report.snapshot_diagnostics(self.directory, "base", self.directory, manifest, graph)
        cases = json.loads((self.directory / "diagnostics/base/report.json").read_text())
        for original, retained in zip(diagnostics(), cases, strict=True):
            self.assertEqual(retained, dict(original, bitcode=manifest["compiler_diagnostic_cases"][original["bitcode"]]))
            self.assertEqual((self.directory / "diagnostics/base" / (original["name"] + ".bc")).read_bytes(), original["bitcode"].encode())
        self.assertEqual(ir_compare.check_diagnostics(cases), ir_compare.check_diagnostics(diagnostics()))

    def test_diagnostic_mapping_and_producers_are_required(self) -> None:
        for defect in ("missing_map", "missing_key", "shared_file", "shared_report", "wrong_membership", "missing_producer", "wrong_producer", "different_producers", "extra_output", "compiler_producer", "unknown_case", "duplicate_case", "empty_bitcode"):
            with self.subTest(defect=defect):
                graph, manifest = self.diagnostic_graph()
                paths = manifest["compiler_diagnostics"]
                bitcodes = manifest["compiler_diagnostic_cases"]
                if defect == "missing_map":
                    del manifest["compiler_diagnostic_cases"]
                elif defect == "missing_key":
                    del bitcodes["attribute.bc"]
                elif defect == "shared_file":
                    bitcodes["attribute.bc"] = bitcodes["assembly.bc"]
                elif defect == "shared_report":
                    bitcodes["attribute.bc"] = paths[0]
                elif defect == "wrong_membership":
                    paths.append("source.c")
                elif defect == "missing_producer":
                    del graph.producers[graph.identities[paths[1]]]
                elif defect == "wrong_producer":
                    graph.producer(paths[0])["mnemonic"] = "FileWrite"
                elif defect == "different_producers":
                    graph.producers[graph.identities[paths[1]]] = graph.producers[graph.identities["frontend.bc"]]
                elif defect == "extra_output":
                    graph.producer(paths[0])["outputIds"].append(graph.identities["other.c"])
                elif defect == "compiler_producer":
                    manifest["compiler_jobs"].append({"outputs": [paths[1]]})
                elif defect == "empty_bitcode":
                    (self.directory / paths[1]).write_bytes(b"")
                else:
                    cases = diagnostics()
                    if defect == "unknown_case":
                        cases[0]["bitcode"] = "unmapped.bc"
                    else:
                        cases[1] = cases[0]
                    self.write(paths[0], cases)
                with self.assertRaises(ValueError):
                    ir_reuse_report.snapshot_diagnostics(self.directory, "bad", self.directory, manifest, graph)

    def test_bounded_controls_require_object_bytes_and_diagnostic_outcomes(self) -> None:
        _, manifest = self.diagnostic_graph()
        paths = self.native_recipe_files(self.directory / "controls")
        manifest["native_recipe_checks"] = paths
        query = json.loads((self.directory / "query.json").read_text())
        outputs = []
        for path in paths.values():
            identity = str(len(query["artifacts"]) + 1)
            query["pathFragments"].append({"id": identity, "label": path})
            query["artifacts"].append({"id": identity, "pathFragmentId": identity})
            outputs.append(identity)
        query["actions"].append({"mnemonic": "KernelArtifact", "outputIds": outputs})
        for key in ("compilers", "compiler_inventory", "artifacts"):
            manifest[key]["lib/ctype.o"] = manifest[key].pop("unit.o")
        for job in manifest["compiler_jobs"]:
            job["target"] = "lib/ctype.o"
        manifest["roots"] = ["lib/ctype.o"]
        self.write("manifest.json", manifest)
        (self.directory / "config").mkdir()
        (self.directory / "config/.config").write_text("CONFIG_64BIT=y\n")
        for path in ("reference.o", "backend.o", "final.o", "frontend.bc"):
            (self.directory / path).write_bytes(b"actual compiler output")
        for name in ("default", "5_10"):
            self.write(f"{name}.aquery.json", query)
            (self.directory / f"{name}.manifest-path").write_text("manifest.json\n")
        ir_compare.compare_controls(self.directory, self.directory)
        self.assertEqual((self.directory / "objects/default/lib/ctype.o.object").read_bytes(), b"actual compiler output")
        self.assertEqual((self.directory / "diagnostics/5_10/attribute.bc").read_bytes(), b"attribute.bc")
        for content in (b"different backend output", b""):
            (self.directory / "backend.o").write_bytes(content)
            with self.assertRaises(AssertionError):
                ir_compare.compare_controls(self.directory, self.directory)
        (self.directory / "backend.o").write_bytes(b"actual compiler output")
        cases = diagnostics()
        cases[0]["outcomes"][1]["returncode"] = 0
        self.write("scheduled/diagnostics.json", cases)
        with self.assertRaises(AssertionError):
            ir_compare.compare_controls(self.directory, self.directory)

    def family(self) -> None:
        targets = ["kernel/panic.o", "arch/x86/boot/string.o", "lib/ctype.o", "lib/string.o"]
        for phase in ir_reuse_report.FAMILY_PHASES:
            self.native_recipe_files(self.directory / "native-recipes" / phase)
            selected = targets + (["debug/only.o"] if phase == "debug" else [])
            if phase == "changed":
                selected = targets[:2]
            snapshot, compilers, entries = {}, {}, []
            for target in selected:
                changed = phase == "changed" and target == "kernel/panic.o"
                content = target + ("-changed" if changed else "")
                paths = {role: f"bazel-out/{phase}/bin/{role}/{target}" for role in ("frontend", "integrated", "object")}
                paths["bitcode"] = paths["frontend"]
                snapshot[target] = {"paths": paths, "digests": {role: content for role in paths}}
                compilers[target] = {"source": "source/" + target.removesuffix(".o") + ".c", "object": paths["object"], "kind": "c"}
                if phase == "unchanged":
                    continue
                for role in ("frontend", "integrated", "object"):
                    identity = len(entries) + 1
                    entries.append({"id": identity, "path": paths[role]})
                    entries.append(spawn(identity, backend=role == "object", hit=phase == "lz4", digest=role + content))
            self.write(f"{phase}.ir.json", snapshot)
            self.log(phase, entries)
            config = ["CONFIG_PANIC_TIMEOUT=" + ("37" if phase == "changed" else "0")]
            config += {
                "debug": ["CONFIG_DEBUG_KERNEL=y", 'CONFIG_LOCALVERSION="-debug"'],
                "btf": ["CONFIG_DEBUG_INFO_BTF=y"], "lz4": ["CONFIG_KERNEL_LZ4=y"],
            }.get(phase, [])
            self.write(f"{phase}.products.json", {"compilers": compilers, "products": {"vmlinux": {}, "arch/x86/boot/bzImage": {}}, "config": "\n".join(config)})
            self.write(f"diagnostics/{phase}/report.json", diagnostics())
        self.write("base.compiler.json", {"arch/x86/boot/string.o": {"flags": ["-g", "-m16"]}})

    def test_family_requires_equal_ir_equal_keys_and_actual_hits(self) -> None:
        self.family()
        ir_reuse_report.summarize(self.directory)
        ir_compare.compare_family(self.directory)
        result = json.loads((self.directory / "ir-reuse.json").read_text())
        self.assertEqual(result["base_lz4"]["actual_backend_cache_hits"], 4)
        self.assertEqual(result["counts"]["debug"], 5)
        path = self.directory / "lz4.execution.jsonl"
        original = path.read_text()
        for defect in ("different_key", "executed"):
            with self.subTest(defect=defect):
                entries = [json.loads(line) for line in original.splitlines()]
                backend = next(entry["spawn"] for entry in entries if entry.get("spawn", {}).get("mnemonic") == "KernelLLVMBackend")
                if defect == "different_key":
                    backend["digest"]["hash"] = "different-native-key"
                else:
                    backend["cacheHit"] = False
                path.write_text("\n".join(map(json.dumps, entries)) + "\n")
                with self.assertRaises(AssertionError):
                    ir_reuse_report.summarize(self.directory)
                failed = json.loads((self.directory / "ir-reuse.json").read_text())
                self.assertEqual(failed["base_lz4"]["actual_backend_cache_hits"], 3)

    def test_parity_and_diagnostics_are_independent_required_gates(self) -> None:
        for defect in ("parity", "diagnostics"):
            with self.subTest(defect=defect):
                self.family()
                if defect == "parity":
                    path = self.directory / "debug.ir.json"
                    value = json.loads(path.read_text())
                    value["debug/only.o"]["digests"]["integrated"] = "mismatched"
                    self.write(path.name, value)
                else:
                    self.write("diagnostics/btf/report.json", [])
                with self.assertRaises(AssertionError):
                    ir_compare.compare_family(self.directory)

    def test_family_and_mutation_reports_require_native_recipe_evidence(self) -> None:
        for family in (True, False):
            with self.subTest(family=family):
                if family:
                    self.family()
                    report = ir_reuse_report.summarize
                    phase = "btf"
                else:
                    self.mutation()
                    report = reuse_report.report
                    phase = "incbin"
                path = self.directory / "native-recipes" / phase / "compressed.lz4"
                path.unlink()
                with self.assertRaises(FileNotFoundError):
                    report(self.directory)

    def test_unchanged_backend_execution_and_missing_role_are_not_reuse(self) -> None:
        self.family()
        value = ir_reuse_report.read_snapshot(self.directory / "unchanged.ir.json")
        self.log("unchanged", [{"id": 1, "path": value["kernel/panic.o"]["paths"]["object"]}, spawn(1, backend=True)])
        with self.assertRaises(AssertionError):
            ir_reuse_report.summarize(self.directory)
        path = self.write("inventory.json", {"unit.o": {
            "source": "unit.c", "outputs": {"backend": ["unit.o"]},
            "bitcode_digest": "IR", "generated_inputs": {"backend": {}},
        }})
        with self.assertRaisesRegex(ValueError, "compiler phases"):
            reuse_report.read_compilers(path)

    def mutation(self) -> dict[str, dict[str, tuple[str, ...]]]:
        c_targets = ["init/main.o", "kernel/fork.o", "init/do_mounts.o", "init/do_mounts_initrd.o", "init/initramfs.o", ".vmlinux.export.o"]
        singles = ["usr/initramfs_data.o", reuse_report.PIGGY_OBJECT]
        roles = {target: {role: (f"bazel-out/mapped/bin/{role}/{target}",) for role in ("frontend", "backend")} for target in c_targets}
        roles.update({target: {"single": (f"bazel-out/mapped/bin/single/{target}",)} for target in singles})
        for phase in reuse_report.PHASES:
            self.native_recipe_files(self.directory / "native-recipes" / phase)
            records = {}
            for target, outputs in roles.items():
                changed = target == "init/do_mounts.o" and phase in ("header", "incbin", "gzip")
                records[target] = {
                    "source": target + ".source", "outputs": outputs,
                    "bitcode_digest": ("changed" if changed else "IR") if target in c_targets else None,
                    "generated_inputs": {role: {} for role in outputs},
                }
            self.write(f"{phase}.compilers.json", records)
            self.write(f"{phase}.digests.json", {
                "kernel/fork.o": "unchanged", "init/main.o": "unchanged",
                "usr/initramfs_data.o": "new" if phase in ("incbin", "gzip") else "old",
                "usr/initramfs_inc_data": "new" if phase in ("incbin", "gzip") else "old",
                ".vmlinux.export.c": "unchanged", ".vmlinux.export.o": "unchanged",
                reuse_report.PIGGY_SOURCE: "unchanged",
                reuse_report.PIGGY_OBJECT: "gzip" if phase == "gzip" else "new" if phase == "incbin" else "old",
                reuse_report.GZIP_PAYLOAD: "gzip" if phase == "gzip" else "new" if phase == "incbin" else "old",
                "decompressed_gzip": "unchanged",
                **{header: "unchanged" for header in reuse_report.BOOT_HEADERS},
            })
            self.write(f"{phase}.sizes.json", {reuse_report.GZIP_PAYLOAD: 1024})
            selected = {
                "baseline": [(target, role) for target, outputs in roles.items() for role in outputs],
                "unchanged": [], "edited": [("init/main.o", "frontend"), ("init/main.o", "backend")],
                "header": [(target, "frontend") for target in reuse_report.HEADER_TARGETS if target in roles] + [("init/do_mounts.o", "backend")],
                "incbin": [(target, "single") for target in singles],
                "gzip": [(reuse_report.PIGGY_OBJECT, "single")],
            }[phase]
            entries = []
            for index, (target, role) in enumerate(selected, 1):
                entries.extend([{"id": index, "path": roles[target][role][0]}, spawn(index, backend=role == "backend")])
            self.log(phase, entries)
        return roles

    def test_six_phase_selective_mutation_and_omissions(self) -> None:
        self.mutation()
        result = reuse_report.report(self.directory)
        self.assertEqual(result["compiler_phase_counts"], {"frontend": 6, "backend": 6, "single": 2})
        self.assertTrue(all(result["qualification"][key] for key in (
            "all_unrelated_compiles_reused", "header_edit_recompiled_only_consumers",
            "incbin_edit_recompiled_only_consumer", "gzip_edit_recompiled_only_consumer",
        )))
        for phase in ("baseline", "edited", "header", "incbin", "gzip"):
            with self.subTest(phase=phase):
                self.mutation()
                self.log(phase, [])
                with self.assertRaises(ValueError):
                    reuse_report.report(self.directory)

    def test_generated_source_reuse_depends_on_bytes_not_graph_closure(self) -> None:
        roles = self.mutation()
        path = self.directory / "incbin.execution.jsonl"
        with path.open("a") as stream:
            stream.write(json.dumps({"id": 99, "path": roles[".vmlinux.export.o"]["frontend"][0]}) + "\n")
            stream.write(json.dumps(spawn(99)) + "\n")
        result = reuse_report.report(self.directory)
        self.assertFalse(result["qualification"]["incbin_edit_recompiled_only_consumer"])
        digests = json.loads((self.directory / "incbin.digests.json").read_text())
        digests[".vmlinux.export.c"] = "changed"
        self.write("incbin.digests.json", digests)
        with self.assertRaisesRegex(ValueError, "unexpectedly changed .vmlinux.export.c"):
            reuse_report.report(self.directory)

    def test_gzip_changes_only_header_bytes_and_embedding(self) -> None:
        for field, value in [
            (reuse_report.PIGGY_SOURCE, "changed"),
            ("decompressed_gzip", "changed"),
            (reuse_report.GZIP_PAYLOAD, "new"),
            (reuse_report.PIGGY_OBJECT, "new"),
        ]:
            with self.subTest(field=field):
                self.mutation()
                digests = json.loads((self.directory / "gzip.digests.json").read_text())
                digests[field] = value
                self.write("gzip.digests.json", digests)
                with self.assertRaises(ValueError):
                    reuse_report.report(self.directory)
        self.mutation()
        self.write("gzip.sizes.json", {reuse_report.GZIP_PAYLOAD: 1025})
        with self.assertRaisesRegex(ValueError, "payload size"):
            reuse_report.report(self.directory)


if __name__ == "__main__":
    unittest.main()
