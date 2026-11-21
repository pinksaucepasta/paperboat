"""Where a check writes its receipt: SMOKE_RESULTS_DIR when set, else results/ beside the calling script."""
import os
from pathlib import Path


def results_dir(script):
    return Path(os.environ.get('SMOKE_RESULTS_DIR') or Path(script).resolve().parent / 'results')
