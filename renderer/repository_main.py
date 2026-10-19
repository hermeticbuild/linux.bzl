"""Export source-selected Kbuild syntax with a declared, generic GNU Make."""

import argparse
from pathlib import Path
import sys
import tempfile

from export import native_events, parse_events
from extract import extract
from inputs import write_inputs


def generate(linux: Path, output: Path, make: list[Path | str]) -> None:
    if output == linux or linux in output.parents or output in linux.parents:
        raise ValueError("Output must be outside the Linux source tree")
    output.mkdir(parents=True, exist_ok=True)
    if list(output.iterdir()):
        raise ValueError("Output directory must be empty: " + str(output))
    with tempfile.TemporaryDirectory(prefix=".generation-", dir=output.parent) as directory:
        work = Path(directory)
        paths = sorted(p for p in linux.rglob("*") if p.is_file() and ".git" not in p.parts and
                       (p.name in ("Kbuild", "Makefile") or p.name.startswith("Makefile.") or p.suffix == ".mk"))
        print(f"Exporting {len(paths)} Make files without configuring a kernel", file=sys.stderr)
        tree = parse_events(native_events(make, paths, expressions=True))
        program, link_script = extract(tree, linux, make)
        data = work / "data"
        write_inputs(data, tree, linux, program, link_script)
        # Publish only the completed static module and its diagnostic report.
        data.replace(output)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--linux", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--make", type=Path)
    args = parser.parse_args()
    make = [args.make.resolve()] if args.make else [Path(sys.executable), "_make"]
    generate(args.linux.resolve(), args.out.resolve(), make)
