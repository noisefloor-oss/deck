from __future__ import annotations

import hashlib
import importlib.util
import io
import json
import os
import pickle
import sqlite3
import struct
import tarfile
import tempfile
import threading
import unittest
from pathlib import Path
from unittest.mock import patch

import numpy as np


SCRIPT = Path(
    os.environ.get(
        "NOISEDECK_MEMPALACE_MIGRATION",
        Path(__file__).with_name("migrate-to-sqlite.py"),
    )
)
SPEC = importlib.util.spec_from_file_location("deck_mempalace_migration", SCRIPT)
migration = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(migration)


class MigrationTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.palace = Path(self.temporary.name) / "palace"
        self.palace.mkdir()
        self.source = self.palace / "chroma.sqlite3"
        self.destination = self.palace / "sqlite_exact.sqlite3"
        self.database = sqlite3.connect(self.source)
        self.database.executescript(
            """
            CREATE TABLE collections (id TEXT PRIMARY KEY, name TEXT NOT NULL, dimension INTEGER);
            CREATE TABLE segments (id TEXT PRIMARY KEY, scope TEXT NOT NULL, collection TEXT NOT NULL);
            CREATE TABLE embeddings (
                id INTEGER PRIMARY KEY, segment_id TEXT NOT NULL,
                embedding_id TEXT NOT NULL, seq_id INTEGER NOT NULL
            );
            CREATE TABLE embeddings_queue (
                seq_id INTEGER PRIMARY KEY, topic TEXT NOT NULL,
                id TEXT NOT NULL, vector BLOB, encoding TEXT
            );
            CREATE TABLE embedding_metadata (
                id INTEGER NOT NULL, key TEXT NOT NULL, string_value TEXT,
                int_value INTEGER, float_value REAL, bool_value INTEGER,
                PRIMARY KEY (id, key)
            );
            CREATE TABLE embedding_metadata_array (
                id INTEGER NOT NULL, key TEXT NOT NULL, string_value TEXT,
                int_value INTEGER, float_value REAL, bool_value INTEGER
            );
            """
        )

    def tearDown(self):
        self.database.close()
        self.temporary.cleanup()

    def collection(self, collection_id="collection-one", name="drawers", dimension=2):
        self.database.execute(
            "INSERT INTO collections VALUES (?, ?, ?)", (collection_id, name, dimension)
        )
        self.database.execute(
            "INSERT INTO segments VALUES (?, 'METADATA', ?)",
            (f"segment-{collection_id}", collection_id),
        )

    def document(
        self,
        row_id,
        document_id,
        document,
        vector,
        metadata=None,
        arrays=None,
        collection_id="collection-one",
    ):
        self.database.execute(
            "INSERT INTO embeddings VALUES (?, ?, ?, ?)",
            (row_id, f"segment-{collection_id}", document_id, row_id),
        )
        self.database.execute(
            "INSERT INTO embeddings_queue VALUES (?, ?, ?, ?, 'FLOAT32')",
            (row_id, f"persistent://default/default/{collection_id}", document_id, vector),
        )
        values = dict(metadata or {})
        if document is not None:
            values["chroma:document"] = document
        for key, value in values.items():
            columns = {
                str: "string_value",
                int: "int_value",
                float: "float_value",
                bool: "bool_value",
            }
            column = columns[type(value)]
            self.database.execute(
                f"INSERT INTO embedding_metadata (id, key, {column}) VALUES (?, ?, ?)",
                (row_id, key, value),
            )
        for key, items in (arrays or {}).items():
            for value in items:
                column = {str: "string_value", int: "int_value", bool: "bool_value"}[type(value)]
                self.database.execute(
                    f"INSERT INTO embedding_metadata_array (id, key, {column}) VALUES (?, ?, ?)",
                    (row_id, key, value),
                )
        self.database.commit()

    def persisted(self, records, *, rust=False, dimension=2, watermark=None):
        segment = "vector-segment"
        self.database.execute(
            "INSERT INTO segments VALUES (?, 'VECTOR', 'collection-one')", (segment,)
        )
        self.database.execute(
            "CREATE TABLE max_seq_id (segment_id TEXT PRIMARY KEY, seq_id INTEGER)"
        )
        self.database.execute(
            "INSERT INTO max_seq_id VALUES (?, ?)",
            (segment, watermark or max(value[2] for value in records.values())),
        )
        folder = self.palace / segment
        folder.mkdir()
        ids = {key: i + 1 for i, key in enumerate(records)}
        fields = {
            "dimensionality": None if rust else dimension,
            "total_elements_added": len(ids),
            "id_to_label": ids,
            "label_to_id": {label: key for key, label in ids.items()},
            "id_to_seq_id": {}
            if rust
            else {key: value[2] for key, value in records.items()},
        }
        (folder / "index_metadata.pickle").write_bytes(pickle.dumps(fields, protocol=4))
        vector_offset, label_offset = 132, 132 + dimension * 4
        stride = label_offset + 8
        (folder / "header.bin").write_bytes(
            struct.pack(
                "<iQQQQQQiIQQQdQ",
                1,
                0,
                len(ids),
                len(ids),
                stride,
                label_offset,
                vector_offset,
                0,
                0,
                16,
                32,
                16,
                0.36,
                100,
            )
        )
        with (folder / "data_level0.bin").open("wb") as handle:
            for key, (vector, deleted, seq) in records.items():
                record = bytearray(stride)
                record[2] = int(deleted)
                record[vector_offset:label_offset] = np.asarray(
                    vector, dtype=np.float32
                ).tobytes()
                struct.pack_into("<Q", record, label_offset, ids[key])
                handle.write(record)
        self.database.commit()
        return folder

    def read_vectors(self):
        backend = migration.SQLiteExactBackend()
        try:
            result = backend.get_collection(str(self.palace), "drawers").get(
                include=["embeddings", "documents", "metadatas"]
            )
            return {
                key: (vector, document, fields)
                for key, vector, document, fields in zip(
                    result.ids, result.embeddings, result.documents, result.metadatas
                )
            }
        finally:
            backend.close()

    def test_compacted_python_and_rust_indexes_preserve_vectors_and_source(self):
        for rust in (False, True):
            with self.subTest(rust=rust):
                if rust:
                    self.tearDown()
                    self.setUp()
                self.collection()
                self.document(
                    1,
                    "alpha",
                    "first",
                    np.array([1, 0], dtype=np.float32).tobytes(),
                    {"enabled": True},
                )
                self.document(
                    2, "beta", "second", np.array([0, 1], dtype=np.float32).tobytes()
                )
                self.persisted(
                    {"alpha": ([1, 0], False, 1), "beta": ([0, 1], False, 2)}, rust=rust
                )
                self.database.execute("DELETE FROM embeddings_queue")
                self.database.commit()
                original = migration.source_fingerprint(self.palace)
                self.assertEqual(migration.migrate(self.palace)["documents"], 2)
                self.assertEqual(migration.source_fingerprint(self.palace), original)
                result = self.read_vectors()
                self.assertEqual(result["alpha"], ([1, 0], "first", {"enabled": True}))
                self.assertEqual(result["beta"][0], [0, 1])

    def test_queue_overlays_compacted_vector_and_drops_deleted_records(self):
        self.collection()
        self.document(
            1, "alpha", "updated", np.array([1, 0], dtype=np.float32).tobytes()
        )
        self.document(
            2, "deleted", "removed", np.array([0, 1], dtype=np.float32).tobytes()
        )
        self.persisted({"alpha": ([1, 0], False, 1), "deleted": ([0, 1], False, 2)})
        self.database.execute("DELETE FROM embeddings_queue")
        self.database.execute("DELETE FROM embeddings WHERE embedding_id = 'deleted'")
        self.database.execute(
            "UPDATE embeddings SET seq_id = 3 WHERE embedding_id = 'alpha'"
        )
        self.database.execute(
            "INSERT INTO embeddings_queue VALUES (3, 'persistent://default/default/collection-one', 'alpha', ?, 'FLOAT32')",
            (np.array([0.25, 0.75], dtype=np.float32).tobytes(),),
        )
        self.database.commit()
        migration.migrate(self.palace)
        self.assertEqual(self.read_vectors(), {"alpha": ([0.25, 0.75], "updated", {})})

    def test_cosine_index_reconstructs_native_vector_from_persisted_length(self):
        self.collection()
        self.document(1, "alpha", "first", np.array([3, 4], dtype=np.float32).tobytes())
        folder = self.persisted({"alpha": ([0.6, 0.8], False, 1)})
        self.database.execute(
            "CREATE TABLE segment_metadata (segment_id TEXT, key TEXT, str_value TEXT)"
        )
        self.database.execute(
            "INSERT INTO segment_metadata VALUES ('vector-segment', 'hnsw:space', 'cosine')"
        )
        self.database.commit()
        (folder / "length.bin").write_bytes(struct.pack("<f", 5))
        migration.migrate(self.palace)
        self.assertEqual(self.read_vectors()["alpha"][0], [3, 4])

    def test_cosine_truncated_or_invalid_norm_is_refused(self):
        self.collection()
        self.document(1, "alpha", "first", np.array([3, 4], dtype=np.float32).tobytes())
        folder = self.persisted({"alpha": ([0.6, 0.8], False, 1)})
        self.database.execute(
            "CREATE TABLE segment_metadata (segment_id TEXT, key TEXT, str_value TEXT)"
        )
        self.database.execute(
            "INSERT INTO segment_metadata VALUES ('vector-segment', 'hnsw:space', 'cosine')"
        )
        self.database.commit()
        for norm in (b"short", struct.pack("<f", float("nan")), struct.pack("<f", -1)):
            with self.subTest(norm=norm):
                (folder / "length.bin").write_bytes(norm)
                with self.assertRaises(migration.MigrationError):
                    migration.migrate(self.palace)
                self.assertFalse(self.destination.exists())

    def test_persisted_vector_wins_over_equal_sequence_queue_bytes(self):
        self.collection()
        self.document(1, "alpha", "first", np.array([2, 0], dtype=np.float32).tobytes())
        self.persisted({"alpha": ([1, 0], False, 1)})
        migration.migrate(self.palace)
        self.assertEqual(self.read_vectors()["alpha"][0], [1, 0])

    def test_nondefault_queue_topic_migrates_records(self):
        self.collection()
        self.document(
            1,
            "alpha",
            "first",
            np.array([1, 0], dtype=np.float32).tobytes(),
            {"enabled": True},
        )
        self.database.execute(
            "UPDATE embeddings_queue SET topic = 'persistent://tenant/custom-db/collection-one'"
        )
        self.database.commit()
        migration.migrate(self.palace)
        self.assertEqual(
            self.read_vectors(), {"alpha": ([1, 0], "first", {"enabled": True})}
        )

    def test_multiple_matching_queue_topics_are_refused(self):
        self.collection()
        self.document(1, "alpha", "first", np.array([1, 0], dtype=np.float32).tobytes())
        self.database.execute(
            "INSERT INTO embeddings_queue VALUES (2, 'persistent://tenant/custom-db/collection-one', 'alpha', ?, 'FLOAT32')",
            (np.array([0, 1], dtype=np.float32).tobytes(),),
        )
        self.database.commit()
        with self.assertRaisesRegex(migration.MigrationError, "multiple queue topics"):
            migration.migrate(self.palace)
        self.assertFalse(self.destination.exists())

    def test_wrong_collection_queue_vector_is_refused(self):
        self.collection()
        self.document(1, "alpha", "first", np.array([1, 0], dtype=np.float32).tobytes())
        self.database.execute(
            "UPDATE embeddings_queue SET topic = 'persistent://default/default/other-collection'"
        )
        self.database.commit()
        with self.assertRaisesRegex(migration.MigrationError, "missing a change"):
            migration.migrate(self.palace)
        self.assertFalse(self.destination.exists())

    def test_null_queue_vector_without_persisted_vector_is_refused(self):
        self.collection()
        self.document(1, "alpha", "first", None)
        with self.assertRaisesRegex(migration.MigrationError, "no usable float32 vector"):
            migration.migrate(self.palace)
        self.assertFalse(self.destination.exists())

    def test_pending_add_or_delete_without_metadata_watermark_is_refused(self):
        self.collection()
        self.document(1, "alpha", "first", np.array([1, 0], dtype=np.float32).tobytes())
        self.persisted({"alpha": ([1, 0], False, 1)})
        self.database.execute(
            "ALTER TABLE embeddings_queue ADD COLUMN operation INTEGER DEFAULT 0"
        )
        for topic in (
            "persistent://default/default/collection-one",
            "persistent://tenant/custom-db/collection-one",
        ):
            self.database.execute("UPDATE embeddings_queue SET topic = ?", (topic,))
            for operation, document_id, vector in (
                (3, "alpha", None),
                (0, "new", np.array([0, 1], dtype=np.float32).tobytes()),
            ):
                with self.subTest(topic=topic, operation=operation):
                    self.database.execute(
                        "DELETE FROM embeddings_queue WHERE seq_id = 5"
                    )
                    self.database.execute(
                        "INSERT INTO embeddings_queue VALUES (5, ?, ?, ?, 'FLOAT32', ?)",
                        (topic, document_id, vector, operation),
                    )
                    self.database.commit()
                    with self.assertRaisesRegex(
                        migration.MigrationError, "not consumed its pending queue"
                    ):
                        migration.migrate(self.palace)
                    self.assertFalse(self.destination.exists())

    def test_pending_queue_with_recorded_metadata_watermark_is_refused(self):
        self.collection()
        self.document(1, "alpha", "first", np.array([1, 0], dtype=np.float32).tobytes())
        self.persisted({"alpha": ([1, 0], False, 1)})
        self.database.execute(
            "INSERT INTO max_seq_id VALUES ('segment-collection-one', 1)"
        )
        self.database.execute(
            "INSERT INTO embeddings_queue VALUES (5, 'persistent://default/default/collection-one', 'new', ?, 'FLOAT32')",
            (np.array([0, 1], dtype=np.float32).tobytes(),),
        )
        self.database.commit()
        with self.assertRaisesRegex(
            migration.MigrationError, "not consumed its pending queue"
        ):
            migration.migrate(self.palace)
        self.assertFalse(self.destination.exists())

    def test_source_change_during_verification_refuses_both_cutover_paths(self):
        self.collection()
        self.document(1, "alpha", "first", np.array([1, 0], dtype=np.float32).tobytes())
        folder = self.persisted({"alpha": ([1, 0], False, 1)})
        original_verify = migration.verify
        metadata = folder / "index_metadata.pickle"
        original_metadata = metadata.read_bytes()

        def change_after_verify(*args):
            result = original_verify(*args)
            metadata.write_bytes(original_metadata + b"changed")
            return result

        with (
            patch.object(migration, "verify", side_effect=change_after_verify),
            self.assertRaisesRegex(
                migration.MigrationError, "source changed during migration"
            ),
        ):
            migration.migrate(self.palace)
        self.assertFalse(self.destination.exists())
        metadata.write_bytes(original_metadata)
        migration.migrate(self.palace)
        target = sqlite3.connect(self.destination)
        target.execute("DELETE FROM meta WHERE key = 'noisedeck.chroma_to_sqlite_exact'")
        target.commit()
        target.close()
        with (
            patch.object(migration, "verify", side_effect=change_after_verify),
            self.assertRaisesRegex(
                migration.MigrationError, "source changed during migration"
            ),
        ):
            migration.migrate(self.palace)
        self.assertFalse(migration.cutover_complete(self.destination))

    def test_hnsw_symlinked_directory_or_file_is_refused(self):
        self.collection()
        self.document(1, "alpha", "first", np.array([1, 0], dtype=np.float32).tobytes())
        folder = self.persisted({"alpha": ([1, 0], False, 1)})
        connection = migration.open_read_only(self.source)
        try:
            collection = migration.collections(connection, self.palace)[0]
            backup = folder.with_name("original-index")
            folder.rename(backup)
            folder.symlink_to(backup, target_is_directory=True)
            with self.assertRaisesRegex(
                migration.MigrationError, "must not be a symlink"
            ):
                migration.PersistedVectors(connection, collection)
            folder.unlink()
            backup.rename(folder)
            header = folder / "header.bin"
            backup_header = folder / "original-header.bin"
            header.rename(backup_header)
            header.symlink_to(backup_header)
            with self.assertRaisesRegex(
                migration.MigrationError, "missing or unsafe HNSW"
            ):
                migration.PersistedVectors(connection, collection)
        finally:
            connection.close()
        self.assertFalse(self.destination.exists())

    def test_hnsw_newer_than_metadata_is_refused(self):
        self.collection()
        self.document(1, "alpha", "first", np.array([1, 0], dtype=np.float32).tobytes())
        self.persisted({"alpha": ([1, 0], False, 2)}, watermark=2)
        with self.assertRaisesRegex(
            migration.MigrationError, "newer than the Chroma metadata"
        ):
            migration.migrate(self.palace)
        self.assertFalse(self.destination.exists())

    def test_hnsw_id_sequence_above_watermark_is_refused(self):
        self.collection()
        self.document(1, "alpha", "first", np.array([1, 0], dtype=np.float32).tobytes())
        self.persisted({"alpha": ([1, 0], False, 2)}, watermark=1)
        with self.assertRaisesRegex(
            migration.MigrationError, "exceeds its persisted watermark"
        ):
            migration.migrate(self.palace)
        self.assertFalse(self.destination.exists())

    def test_multiple_vector_segments_are_refused(self):
        self.collection()
        self.document(1, "alpha", "first", np.array([1, 0], dtype=np.float32).tobytes())
        self.persisted({"alpha": ([1, 0], False, 1)})
        self.database.execute(
            "INSERT INTO segments VALUES ('other-vector', 'VECTOR', 'collection-one')"
        )
        self.database.commit()
        with self.assertRaisesRegex(
            migration.MigrationError, "multiple vector segments"
        ):
            migration.migrate(self.palace)
        self.assertFalse(self.destination.exists())

    def test_missing_post_persistence_vector_change_is_refused(self):
        self.collection()
        self.document(1, "alpha", "new", np.array([0, 1], dtype=np.float32).tobytes())
        self.persisted({"alpha": ([1, 0], False, 1)})
        self.database.execute("UPDATE embeddings SET seq_id = 3")
        self.database.execute("DELETE FROM embeddings_queue")
        self.database.commit()
        with self.assertRaisesRegex(migration.MigrationError, "missing a change"):
            migration.migrate(self.palace)
        self.assertFalse(self.destination.exists())

    def test_delete_and_readd_cannot_reuse_the_deleted_vector(self):
        self.collection()
        self.document(1, "alpha", "new", np.array([1, 0], dtype=np.float32).tobytes())
        self.database.execute(
            "ALTER TABLE embeddings_queue ADD COLUMN operation INTEGER DEFAULT 0"
        )
        self.database.execute("UPDATE embeddings SET seq_id = 3")
        self.database.execute(
            "INSERT INTO embeddings_queue VALUES (2, 'persistent://default/default/collection-one', 'alpha', NULL, NULL, 3)"
        )
        self.database.execute(
            "INSERT INTO embeddings_queue VALUES (3, 'persistent://default/default/collection-one', 'alpha', NULL, NULL, 2)"
        )
        self.database.commit()
        with self.assertRaisesRegex(
            migration.MigrationError, "delete without a vector"
        ):
            migration.migrate(self.palace)
        self.assertFalse(self.destination.exists())

    def test_metadata_only_update_uses_compacted_vector(self):
        self.collection()
        self.document(
            1, "alpha", "current", np.array([1, 0], dtype=np.float32).tobytes()
        )
        self.persisted({"alpha": ([1, 0], False, 1)})
        self.database.execute("DELETE FROM embeddings_queue")
        self.database.execute("UPDATE embeddings SET seq_id = 3")
        self.database.execute(
            "INSERT INTO embeddings_queue VALUES (3, 'persistent://default/default/collection-one', 'alpha', NULL, NULL)"
        )
        self.database.commit()
        migration.migrate(self.palace)
        self.assertEqual(self.read_vectors()["alpha"][:2], ([1, 0], "current"))

    def test_corrupt_hnsw_never_leaves_a_destination(self):
        for damage in ("truncated", "deleted", "mapping", "malicious", "oversize"):
            with self.subTest(damage=damage):
                if damage != "truncated":
                    self.tearDown()
                    self.setUp()
                self.collection()
                self.document(
                    1, "alpha", "first", np.array([1, 0], dtype=np.float32).tobytes()
                )
                folder = self.persisted({"alpha": ([1, 0], False, 1)})
                self.database.execute("DELETE FROM embeddings_queue")
                self.database.commit()
                metadata = folder / "index_metadata.pickle"
                if damage == "truncated":
                    (folder / "data_level0.bin").write_bytes(b"short")
                elif damage == "deleted":
                    data = bytearray((folder / "data_level0.bin").read_bytes())
                    data[2] = 1
                    (folder / "data_level0.bin").write_bytes(data)
                elif damage == "mapping":
                    fields = pickle.loads(metadata.read_bytes())
                    fields["label_to_id"] = {2: "alpha"}
                    metadata.write_bytes(pickle.dumps(fields))
                elif damage == "malicious":
                    metadata.write_bytes(
                        b"cos\nsystem\n(S'touch /tmp/never-run-chroma-pickle'\ntR."
                    )
                else:
                    with metadata.open("wb") as handle:
                        handle.truncate(migration.MAX_INDEX_METADATA + 1)
                before = migration.source_fingerprint(self.palace)
                with self.assertRaises(migration.MigrationError):
                    migration.migrate(self.palace)
                self.assertFalse(self.destination.exists())
                self.assertEqual(migration.source_fingerprint(self.palace), before)
                self.assertEqual(
                    list(self.palace.glob(".sqlite_exact-migration-*")), []
                )
        self.assertFalse(Path("/tmp/never-run-chroma-pickle").exists())

    def test_interrupted_verification_can_restart_without_a_partial_cutover(self):
        self.collection()
        self.document(1, "alpha", "first", np.array([1, 0], dtype=np.float32).tobytes())
        self.persisted({"alpha": ([1, 0], False, 1)})
        self.database.execute("DELETE FROM embeddings_queue")
        self.database.commit()
        with (
            patch.object(migration, "verify", side_effect=RuntimeError()),
            self.assertRaises(RuntimeError),
        ):
            migration.migrate(self.palace)
        self.assertFalse(self.destination.exists())
        self.assertEqual(list(self.palace.glob(".sqlite_exact-migration-*")), [])
        self.assertEqual(migration.migrate(self.palace)["documents"], 1)
        self.assertTrue(migration.cutover_complete(self.destination))

    def test_migration_preserves_records_and_supports_search(self):
        original_document = "first document"
        original_metadata = {"note": "snowman ☃"}
        self.collection()
        self.collection("collection-empty", "empty", 2)
        self.document(
            1,
            "alpha",
            original_document,
            np.array([1.0, 0.0], dtype=np.float32).tobytes(),
            {**original_metadata, "count": 2, "ratio": 0.25, "enabled": True},
            {"tags": ["one", "two"]},
        )
        self.document(
            2,
            "beta",
            "second document",
            np.array([0.0, 1.0], dtype=np.float32).tobytes(),
            {"note": "plain"},
        )

        self.assertEqual(migration.migrate(self.palace)["documents"], 2)
        self.assertFalse(self.destination.with_name(self.destination.name + "-wal").exists())
        before = self.destination.read_bytes()
        self.assertEqual(migration.migrate(self.palace)["status"], "already-sqlite")
        self.assertEqual(self.destination.read_bytes(), before)

        backend = migration.SQLiteExactBackend()
        try:
            collection = backend.get_collection(str(self.palace), "drawers")
            result = collection.get(
                ids=["alpha", "beta"], include=["documents", "metadatas", "embeddings"]
            )
            self.assertEqual(result.ids, ["alpha", "beta"])
            self.assertEqual(result.documents[0], "first document")
            self.assertEqual(result.metadatas[0]["note"], "snowman ☃")
            self.assertEqual(result.metadatas[0]["tags"], ["one", "two"])
            self.assertEqual(result.embeddings[0], [1.0, 0.0])
            hits = collection.query(
                query_embeddings=[[0.0, 1.0]],
                n_results=1,
                include=["documents", "metadatas"],
            )
            self.assertEqual(hits.ids, [["beta"]])
            self.assertEqual(hits.documents, [["second document"]])
            self.assertEqual(backend.get_collection(str(self.palace), "empty").count(), 0)
        finally:
            backend.close()

    def test_empty_chroma_database_migrates_to_empty_sqlite(self):
        result = migration.migrate(self.palace)
        self.assertEqual(result["collections"], 0)
        self.assertEqual(result["documents"], 0)
        self.assertTrue(self.destination.is_file())
        self.assertTrue(migration.cutover_complete(self.destination))

    def test_mixed_collection_with_missing_vector_is_refused(self):
        self.collection()
        self.document(1, "alpha", "first", np.array([1, 0], dtype=np.float32).tobytes())
        self.document(2, "beta", "second", np.array([0, 1], dtype=np.float32).tobytes())
        self.database.execute("DELETE FROM embeddings_queue WHERE seq_id = 2")
        self.database.commit()

        with self.assertRaisesRegex(
            migration.MigrationError, "no usable float32 vector|missing a change"
        ):
            migration.migrate(self.palace)
        self.assertFalse(self.destination.exists())

    def test_new_sqlite_writes_survive_repeated_startup(self):
        self.collection()
        self.document(1, "alpha", "first", np.array([1, 0], dtype=np.float32).tobytes())
        migration.migrate(self.palace)
        backend = migration.SQLiteExactBackend()
        try:
            backend.get_collection(str(self.palace), "drawers").upsert(
                ids=["new-memory"],
                documents=["written after cutover"],
                metadatas=[{"wing": "deck_test"}],
                embeddings=[[0.0, 1.0]],
            )
        finally:
            backend.close()
        before = self.destination.read_bytes()

        self.assertEqual(migration.migrate(self.palace)["status"], "already-sqlite")
        self.assertEqual(self.destination.read_bytes(), before)
        backend = migration.SQLiteExactBackend()
        try:
            self.assertEqual(backend.get_collection(str(self.palace), "drawers").count(), 2)
        finally:
            backend.close()

    def test_incomplete_vector_leaves_no_destination(self):
        self.collection()
        self.document(1, "alpha", "first", np.array([1, 0], dtype=np.float32).tobytes())
        self.database.execute("DELETE FROM embeddings_queue WHERE seq_id = 1")
        self.database.commit()

        with self.assertRaisesRegex(migration.MigrationError, "vector|record|missing|extra"):
            migration.migrate(self.palace)
        self.assertFalse(self.destination.exists())
        self.assertTrue(self.source.exists())
        self.assertEqual(list(self.palace.glob(".sqlite_exact-migration-*")), [])

    def test_metadata_only_update_reuses_current_vector(self):
        self.collection()
        self.document(1, "alpha", "document", np.array([1, 0], dtype=np.float32).tobytes())
        self.database.execute("UPDATE embeddings SET seq_id = 2 WHERE id = 1")
        self.database.execute(
            "INSERT INTO embeddings_queue VALUES (2, 'persistent://default/default/collection-one', 'alpha', NULL, NULL)"
        )
        self.database.execute(
            "INSERT INTO embedding_metadata (id, key, string_value) VALUES (1, 'wing', 'moved')"
        )
        self.database.commit()

        migration.migrate(self.palace)
        backend = migration.SQLiteExactBackend()
        try:
            result = backend.get_collection(str(self.palace), "drawers").get(
                ids=["alpha"], include=["metadatas", "embeddings"]
            )
            self.assertEqual(result.metadatas[0]["wing"], "moved")
            self.assertEqual(result.embeddings, [[1.0, 0.0]])
        finally:
            backend.close()

    def test_embedder_identity_is_preserved_and_enforced(self):
        from mempalace.backends.base import (
            EmbedderIdentity,
            EmbedderIdentityMismatchError,
            check_embedder_identity,
        )

        (self.palace / "mempalace_embedder.json").write_text(
            json.dumps({"drawers": {"model_name": "embeddinggemma", "dimension": 2}})
        )
        self.collection()
        self.document(1, "alpha", "document", np.array([1, 0], dtype=np.float32).tobytes())
        migration.migrate(self.palace)

        backend = migration.SQLiteExactBackend()
        try:
            stored = backend.get_collection(str(self.palace), "drawers").get_stored_embedder_identity()
            self.assertEqual(stored.model_name, "embeddinggemma")
            self.assertEqual(stored.dimension, 2)
            self.assertEqual(check_embedder_identity(stored, stored), "known_match")
            with self.assertRaises(EmbedderIdentityMismatchError):
                check_embedder_identity(stored, EmbedderIdentity("minilm", 2))
        finally:
            backend.close()

    def test_current_sequence_wins_over_queue_history(self):
        self.collection()
        self.document(1, "alpha", "old", np.array([1, 0], dtype=np.float32).tobytes())
        self.database.execute("UPDATE embeddings SET seq_id = 2 WHERE id = 1")
        self.database.execute(
            "INSERT INTO embeddings_queue VALUES (2, 'persistent://default/default/collection-one', 'alpha', ?, 'FLOAT32')",
            (np.array([0, 1], dtype=np.float32).tobytes(),),
        )
        self.database.execute(
            "UPDATE embedding_metadata SET string_value = 'current' "
            "WHERE id = 1 AND key = 'chroma:document'"
        )
        self.database.commit()

        migration.migrate(self.palace)
        backend = migration.SQLiteExactBackend()
        try:
            result = backend.get_collection(str(self.palace), "drawers").get(
                ids=["alpha"], include=["documents", "embeddings"]
            )
            self.assertEqual(result.documents, ["current"])
            self.assertEqual(result.embeddings, [[0.0, 1.0]])
        finally:
            backend.close()

    def test_wrong_dimension_leaves_no_destination(self):
        self.collection()
        self.document(1, "alpha", "first", np.array([1, 0, 0], dtype=np.float32).tobytes())
        with self.assertRaisesRegex(migration.MigrationError, "wrong vector dimension"):
            migration.migrate(self.palace)
        self.assertFalse(self.destination.exists())

    def test_existing_destination_is_verified_not_repaired(self):
        self.collection()
        self.document(1, "alpha", "first", np.array([1, 0], dtype=np.float32).tobytes())
        migration.migrate(self.palace)
        before = self.destination.read_bytes()
        target = sqlite3.connect(self.destination)
        target.execute("DELETE FROM meta WHERE key = 'noisedeck.chroma_to_sqlite_exact'")
        target.execute("UPDATE documents SET document = 'changed' WHERE id = 'alpha'")
        target.commit()
        target.close()

        with self.assertRaisesRegex(migration.MigrationError, "changed during migration"):
            migration.migrate(self.palace)
        self.assertNotEqual(self.destination.read_bytes(), before)

    def test_config_selection_preserves_existing_settings(self):
        config = self.palace.parent / "config.json"
        config.write_text(json.dumps({"palace_path": str(self.palace), "backend": "chroma"}))
        migration.select_sqlite(config)
        value = json.loads(config.read_text())
        self.assertEqual(value["palace_path"], str(self.palace))
        self.assertEqual(value["backend"], "sqlite_exact")
        self.assertEqual(config.stat().st_mode & 0o777, 0o600)

    def test_fresh_palace_needs_no_chroma_file(self):
        self.database.close()
        self.source.unlink()
        self.assertEqual(migration.migrate(self.palace)["status"], "new-sqlite")
        self.database = sqlite3.connect(self.source)


class MiniLMCacheTest(unittest.TestCase):
    def test_interrupted_extraction_is_replaced_atomically(self):
        from mempalace.minilm_onnx import MiniLMONNX

        payload = {name: f"verified {name} contents".encode() for name in MiniLMONNX.FILES}
        archive_bytes = io.BytesIO()
        with tarfile.open(fileobj=archive_bytes, mode="w:gz") as bundle:
            for name, content in payload.items():
                info = tarfile.TarInfo(f"onnx/{name}")
                info.size = len(content)
                bundle.addfile(info, io.BytesIO(content))
        with tempfile.TemporaryDirectory() as temporary:
            model = object.__new__(MiniLMONNX)
            model.root = Path(temporary)
            model.model_dir = model.root / "onnx"
            model._load_lock = threading.Lock()
            model._verified = False
            model.MODEL_SHA256 = hashlib.sha256(archive_bytes.getvalue()).hexdigest()
            (model.root / "onnx.tar.gz").write_bytes(archive_bytes.getvalue())
            model._ensure_model()

            partial = model.model_dir / "model.onnx"
            partial.write_bytes(b"partial")
            model._verified = False
            model._ensure_model()

            self.assertEqual(partial.read_bytes(), payload["model.onnx"])
            for name, content in payload.items():
                self.assertEqual((model.model_dir / name).read_bytes(), content)


if __name__ == "__main__":
    unittest.main()
