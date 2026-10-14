#!/usr/bin/env python3
"""Build sqlc's unqualified parser input from the authoritative Goose schema."""
from pathlib import Path

root = Path(__file__).resolve().parents[1]
migrations = root / "internal/db/migrations"
target = root / ".build/sqlc-schema.sql"
target.parent.mkdir(exist_ok=True)
target.write_text("\n".join(path.read_text().split("-- +goose Down", 1)[0].replace("paperboat.", "") for path in sorted(migrations.glob("*.sql"))))
