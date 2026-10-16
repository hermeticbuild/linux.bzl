"""Write the example's deterministic newc archive without host packaging tools."""
from pathlib import Path
import stat
import sys


def entry(inode, name, mode, content=b'', rmajor=0, rminor=0):
    name = name.encode() + b'\0'
    fields = [inode, mode, 0, 0, 1, 0, len(content), 0, 0, rmajor, rminor, len(name), 0]
    header = b'070701' + b''.join(f'{field:08x}'.encode() for field in fields)
    result = header + name
    result += b'\0' * (-len(result) % 4)
    result += content
    return result + b'\0' * (-len(content) % 4)


def archive(motd):
    contents = b''
    for inode, name in enumerate(['dev', 'etc', 'proc'], 1):
        contents += entry(inode, name, stat.S_IFDIR | 0o755)
    contents += entry(4, 'dev/console', stat.S_IFCHR | 0o600, rmajor=5, rminor=1)
    contents += entry(5, 'dev/null', stat.S_IFCHR | 0o666, rmajor=1, rminor=3)
    contents += entry(6, 'etc/motd', stat.S_IFREG | 0o644, motd)
    contents += entry(7, 'TRAILER!!!', 0)
    return contents + b'\0' * (-len(contents) % 512)


if __name__ == '__main__':
    Path(sys.argv[2]).write_bytes(archive(Path(sys.argv[1]).read_bytes()))
