"""Write all device Python fixtures/helpers, excluding workspace tests, as bounded gzip tar."""
import io
from pathlib import Path
import sys
import tarfile

MAX_FILES = 128
MAX_FILE_BYTES = 256 << 10
MAX_ARCHIVE_BYTES = 8 << 20


def main():
    root = Path(sys.argv[1])
    sources = sorted(path for path in root.glob('*.py') if not path.name.startswith('test_'))
    if not sources or len(sources) > MAX_FILES:
        raise ValueError('device fixture count is outside its archive bound')
    entries = []
    total = 0
    for path in sources:
        if path.is_symlink() or not path.is_file():
            raise ValueError('device fixture source must be a regular Python file')
        with path.open('rb') as source:
            body = source.read(MAX_FILE_BYTES + 1)
        total += len(body)
        if len(body) > MAX_FILE_BYTES or total > MAX_ARCHIVE_BYTES:
            raise ValueError('device fixture source exceeds its archive bound')
        entries.append((path.name, body))
    with tarfile.open(fileobj=sys.stdout.buffer, mode='w|gz') as archive:
        for name, body in entries:
            metadata = tarfile.TarInfo(name)
            metadata.size = len(body)
            metadata.mode = 0o600
            archive.addfile(metadata, io.BytesIO(body))


if __name__ == '__main__':
    main()
