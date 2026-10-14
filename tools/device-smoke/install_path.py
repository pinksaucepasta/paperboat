"""Read the executable recorded by a successful `pb install --json`."""
import json
import pathlib
import sys


def windows_executable(receipt):
    value = receipt.get('executable')
    if not isinstance(value, str) or not value or '\n' in value or '\r' in value:
        raise ValueError('install receipt requires an executable path')
    path = pathlib.PureWindowsPath(value)
    if not path.is_absolute() or path.name.lower() != 'pb.exe':
        raise ValueError('install executable must be an absolute Windows pb.exe path')
    return value


if __name__ == '__main__':
    try:
        with open(sys.argv[1]) as source:
            print(windows_executable(json.load(source)))
    except (OSError, ValueError, IndexError, TypeError, AttributeError):
        sys.exit('Cannot read Windows install receipt; pass the saved pb install --json output')
