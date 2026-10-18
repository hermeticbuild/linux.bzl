"""Small declared code generator for the scheduler integration fixture."""
from pathlib import Path
import sys

Path(sys.argv[1]).write_text("#define GENERATED 7\n")
