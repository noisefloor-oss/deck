#!/usr/bin/env python3
"""Verify SQLite writes survive a fresh image-home container restart."""
import argparse
import json
import os
from pathlib import Path

from mempalace.backends import SQLiteExactBackend

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("mode", choices=("write", "verify"))
args = parser.parse_args()
if os.getuid() == 0:
    raise SystemExit("fresh-home qualification must run as the runtime user")
palace = str(Path.home() / ".mempalace" / "image-storage-probe")
backend = SQLiteExactBackend()
try:
    if args.mode == "write":
        collection = backend.get_or_create_collection(palace, "drawers")
        assert collection.count() == 0, "qualification needs a fresh store"
        collection.upsert(
            ids=["image-home-fixture"],
            documents=["synthetic image storage probe"],
            metadatas=[{"qualification": True}],
            embeddings=[[1.0, 0.0]],
        )
    else:
        collection = backend.get_collection(palace, "drawers")
    result = collection.get(include=["documents", "metadatas", "embeddings"])
    assert result.ids == ["image-home-fixture"]
    assert result.documents == ["synthetic image storage probe"]
    assert result.metadatas == [{"qualification": True}]
    assert result.embeddings == [[1.0, 0.0]]
    print(json.dumps({"mode": args.mode, "uid": os.getuid(), "count": collection.count(), "status": "passed"}))
finally:
    backend.close()
