"""Behavioral checks for artifact transport used by the kernel action adapter."""
import io
import json
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest

EXECUTOR=Path(__file__).resolve().parent/'execute.py'

class ExecuteTest(unittest.TestCase):
 def run_action(self,root,inputs,outputs,program,directories=(),directory_outputs=()):
  manifest=root/'action.json'
  manifest.write_text(json.dumps(dict(executable=sys.executable,arguments=['-c',program],inputs=inputs,outputs=outputs,
      directory_inputs=list(directories),directory_outputs=list(directory_outputs),environment={})))
  return subprocess.run([sys.executable,str(EXECUTOR),str(manifest)],cwd=root,capture_output=True,text=True)

 def test_regular_declared_input_and_output(self):
  with tempfile.TemporaryDirectory() as tmp:
   root=Path(tmp);(root/'input').write_text('kernel')
   result=self.run_action(root,['input'],['out/result'],"from pathlib import Path; Path('out/result').write_text(Path('input').read_text()+'-built')")
   self.assertEqual(result.returncode,0,result.stderr)
   self.assertEqual((root/'out/result').read_text(),'kernel-built')

 def test_directory_intermediate_round_trip(self):
  with tempfile.TemporaryDirectory() as tmp:
   root=Path(tmp)
   result=self.run_action(root,[],['headers'],"from pathlib import Path; Path('headers').mkdir(); Path('headers/config.h').write_text('#define CONFIG_TEST 1')",directory_outputs=['headers'])
   self.assertEqual(result.returncode,0,result.stderr)
   result=self.run_action(root,['headers'],['result'],"from pathlib import Path; Path('result').write_text(Path('headers/config.h').read_text())",directories=['headers'])
   self.assertEqual(result.returncode,0,result.stderr)
   self.assertEqual((root/'result').read_text(),'#define CONFIG_TEST 1')

 def test_directory_transport_ignores_timestamps(self):
  with tempfile.TemporaryDirectory() as tmp:
   root=Path(tmp)
   outputs=[]
   for stamp in [100,200]:
    result=self.run_action(root,[],['headers'],"from pathlib import Path; import os; Path('headers').mkdir(); Path('headers/config.h').write_text('constant'); os.utime('headers/config.h', (%d,%d))"%(stamp,stamp),directory_outputs=['headers'])
    self.assertEqual(result.returncode,0,result.stderr)
    outputs.append((root/'headers').read_bytes())
   self.assertEqual(outputs[0],outputs[1])

 def test_failed_command_is_a_build_failure(self):
  with tempfile.TemporaryDirectory() as tmp:
   result=self.run_action(Path(tmp),[],['result'],'raise SystemExit(42)')
   self.assertNotEqual(result.returncode,0)

if __name__=='__main__':unittest.main()
