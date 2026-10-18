"""Reuse measurements distinguish actual compiles from object postprocessing."""
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from check_reuse import affected_targets, executed_objects, inventory, records


class ReuseTest(unittest.TestCase):
    def test_streamed_records(self):
        payload = [{'file': {'path': 'a' * 70000}}, {'spawn': {'outputs': []}}]
        self.assertEqual(list(records(io.StringIO('\n'.join(json.dumps(row) for row in payload)))), payload)

    def test_generated_compilers_follow_real_dependency_edges(self):
        graph = dict(members=[
            dict(target='generated.o', dependencies=['generated.c']),
            dict(target='control.o', dependencies=['headers']),
            dict(target='generated.c', dependencies=['linked.o']),
            dict(target='linked.o', dependencies=['main.o']),
        ])
        self.assertEqual(affected_targets(graph, 'main.o'), {'main.o', 'linked.o', 'generated.c', 'generated.o'})

    def test_raw_compiles_exclude_postprocessing_and_cache_hits(self):
        jobs = [
            dict(scan={'argument_start': 3}, outputs=['bin/kernel.graph/main.o.raw/init/main.o']),
            dict(scan={'argument_start': 3}, outputs=['bin/kernel.graph/fork.o.raw/kernel/fork.o']),
            dict(scan=None, outputs=['bin/kernel.graph/kernel.artifacts/kernel/fork.o']),
        ]
        with tempfile.TemporaryDirectory() as temporary:
            metadata = Path(temporary)
            (metadata / 'report.json').write_text(json.dumps(jobs))
            objects = inventory(metadata, 'kernel.artifacts/')
        entries = [
            {'id': 1, 'file': {'path': 'bin/kernel.stage1/artifacts/main.o.raw/init/main.o'}},
            {'id': 2, 'file': {'path': 'bin/kernel.stage2/artifacts/kernel.artifacts/kernel/fork.o'}},
            {'id': 3, 'file': {'path': 'bin/kernel.stage1/artifacts/fork.o.raw/kernel/fork.o'}},
            {'spawn': {'mnemonic': 'KernelArtifact', 'runner': 'linux-sandbox', 'outputs': [{'outputId': 1}]}},
            {'spawn': {'mnemonic': 'KernelArtifact', 'runner': 'linux-sandbox', 'outputs': [{'outputId': 2}]}},
            {'spawn': {'mnemonic': 'KernelArtifact', 'runner': 'remote cache hit', 'cacheHit': True, 'outputs': [{'outputId': 3}]}},
        ]
        process = unittest.mock.MagicMock()
        process.__enter__.return_value = process
        process.stdout = io.StringIO('\n'.join(json.dumps(entry) for entry in entries))
        process.wait.return_value = 0
        with patch('check_reuse.subprocess.Popen', return_value=process):
            self.assertEqual(executed_objects('decoder', Path('log'), objects), ['init/main.o'])


if __name__ == '__main__':
    unittest.main()
