#!/usr/bin/env python3
"""Migrate Deck's local MemPalace stores without loading ChromaDB."""

from __future__ import annotations

import argparse
import hashlib
import json
import math
import os
import pickletools
import shutil
import sqlite3
import struct
import tempfile
from pathlib import Path

import numpy as np

from mempalace.backends._sidecar import read_embedder_sidecar
from mempalace.backends.sqlite_exact import SQLiteExactBackend

BATCH_SIZE = 256
DESTINATION = "sqlite_exact.sqlite3"
SOURCE = "chroma.sqlite3"


class MigrationError(RuntimeError):
    pass


def open_read_only(path: Path) -> sqlite3.Connection:
    if (Path(f"{path}-wal").exists() != Path(f"{path}-shm").exists()):
        raise MigrationError(f"incomplete WAL sidecars beside {path}; restore both files")
    try:
        connection = sqlite3.connect(f"{path.resolve().as_uri()}?mode=ro", uri=True)
        connection.execute("PRAGMA query_only=ON")
        connection.execute("PRAGMA busy_timeout=5000")
        connection.execute("BEGIN")
        return connection
    except sqlite3.Error as exc:
        raise MigrationError(f"cannot read {path}: {exc}") from exc


def collections(connection: sqlite3.Connection, palace: Path) -> list[dict]:
    rows = connection.execute("SELECT id, name, dimension FROM collections ORDER BY id").fetchall()
    result = []
    names = set()
    sidecar = palace / "mempalace_embedder.json"
    for collection_id, name, dimension in rows:
        if not isinstance(name, str) or not name or name in names:
            raise MigrationError("Chroma contains an empty or duplicate collection name")
        if dimension is not None and (not isinstance(dimension, int) or dimension <= 0):
            raise MigrationError(f"collection {name!r} has an invalid vector dimension")
        identity = read_embedder_sidecar(str(sidecar), name)
        if identity and dimension and identity.dimension and identity.dimension != dimension:
            raise MigrationError(f"collection {name!r} disagrees with its embedder dimension")
        target_dimension = dimension or (identity.dimension if identity else 0) or None
        names.add(name)
        result.append(
            {
                "id": collection_id,
                "palace": palace,
                "name": name,
                "identity": identity,
                "target_dimension": target_dimension,
            }
        )
    return result


def cell(string_value, int_value, float_value, bool_value):
    values = [
        value
        for value in (string_value, int_value, float_value, bool_value)
        if value is not None
    ]
    if not values:
        return None
    if len(values) != 1:
        raise MigrationError("a Chroma metadata row contains multiple values")
    value = values[0]
    if bool_value is not None and bool_value not in (0, 1):
        raise MigrationError("invalid Chroma boolean metadata")
    return bool(value) if bool_value is not None else value


def metadata(connection: sqlite3.Connection, row_ids: list[int]) -> dict[int, tuple[str, dict]]:
    records = {row_id: [None, {}] for row_id in row_ids}
    marks = ",".join("?" for _ in row_ids)
    rows = connection.execute(
        f"SELECT id, key, string_value, int_value, float_value, bool_value "
        f"FROM embedding_metadata WHERE id IN ({marks}) ORDER BY id, key",
        row_ids,
    )
    for row_id, key, *values in rows:
        if row_id not in records or not isinstance(key, str):
            raise MigrationError("Chroma contains invalid or orphaned metadata")
        value = cell(*values)
        if key == "chroma:document":
            if value is not None and not isinstance(value, str):
                raise MigrationError("a Chroma document is not text")
            records[row_id][0] = value
        elif not key.startswith("chroma:") and value is not None:
            records[row_id][1][key] = value
    array_table = connection.execute(
        "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'embedding_metadata_array'"
    ).fetchone()
    if array_table:
        arrays = {}
        rows = connection.execute(
            f"SELECT rowid, id, key, string_value, int_value, float_value, bool_value "
            f"FROM embedding_metadata_array WHERE id IN ({marks}) ORDER BY rowid",
            row_ids,
        )
        for _, row_id, key, *values in rows:
            if row_id not in records or not isinstance(key, str) or key.startswith("chroma:"):
                raise MigrationError("Chroma contains invalid array metadata")
            value = cell(*values)
            if value is not None:
                arrays.setdefault((row_id, key), []).append(value)
        for (row_id, key), values in arrays.items():
            if key in records[row_id][1]:
                raise MigrationError("Chroma metadata has both scalar and array values for one key")
            records[row_id][1][key] = values
    return {row_id: (document, fields) for row_id, (document, fields) in records.items()}


# Chroma's persistent HNSW v1 layout is defined by chroma-core/hnswlib's
# HEADER_FIELDS and getExternalLabel; no executable serializer is loaded here.
MAX_INDEX_METADATA = 64 * 1024 * 1024
MAX_INDEX_ELEMENTS = 2_000_000
MAX_DIMENSION = 65536
PERSISTENT_CLASS = (
    "chromadb.segment.impl.vector.local_persistent_hnsw",
    "PersistentData",
)


def persistent_metadata(path: Path) -> dict:
    if path.stat().st_size > MAX_INDEX_METADATA:
        raise MigrationError("HNSW metadata exceeds the migration limit")
    data = path.read_bytes()
    stack, memo = [], {}
    mark, persistent_class, instance = object(), object(), object()
    state = None
    try:
        for count, (op, arg, position) in enumerate(pickletools.genops(data)):
            if count > MAX_INDEX_ELEMENTS * 24 or len(stack) > MAX_INDEX_ELEMENTS * 4:
                raise MigrationError("HNSW metadata exceeds the migration limit")
            name = op.name
            if name in {"PROTO", "FRAME"}:
                continue
            if name == "MARK":
                stack.append(mark)
            elif name in {
                "BINUNICODE",
                "SHORT_BINUNICODE",
                "BINUNICODE8",
                "UNICODE",
                "BININT",
                "BININT1",
                "BININT2",
                "LONG1",
                "LONG4",
                "INT",
                "BINBYTES",
                "SHORT_BINBYTES",
                "BINBYTES8",
            }:
                if type(arg) not in {str, int, bytes}:
                    raise MigrationError("unsupported HNSW metadata value")
                stack.append(arg)
            elif name == "NONE":
                stack.append(None)
            elif name == "EMPTY_DICT":
                stack.append({})
            elif name == "EMPTY_TUPLE":
                stack.append(())
            elif name in {"BINPUT", "LONG_BINPUT", "PUT", "MEMOIZE"}:
                key = len(memo) if name == "MEMOIZE" else int(arg)
                if key in memo or len(memo) > MAX_INDEX_ELEMENTS * 8:
                    raise MigrationError("invalid HNSW pickle memo")
                memo[key] = stack[-1]
            elif name in {"BINGET", "LONG_BINGET", "GET"}:
                stack.append(memo[int(arg)])
            elif name in {"SETITEM", "SETITEMS"}:
                if name == "SETITEM":
                    value, key = stack.pop(), stack.pop()
                    pairs = [key, value]
                else:
                    start = len(stack) - 1
                    while start >= 0 and stack[start] is not mark:
                        start -= 1
                    if start < 0:
                        raise MigrationError("missing HNSW dictionary marker")
                    pairs = stack[start + 1 :]
                    del stack[start:]
                target = stack[-1]
                if type(target) is not dict or len(pairs) % 2:
                    raise MigrationError("invalid HNSW metadata dictionary")
                for key, value in zip(pairs[::2], pairs[1::2]):
                    if type(key) not in {str, int} or key in target:
                        raise MigrationError("duplicate or invalid HNSW metadata key")
                    target[key] = value
            elif name == "GLOBAL":
                if tuple(arg.split(" ")) != PERSISTENT_CLASS:
                    raise MigrationError("unsupported HNSW metadata class")
                stack.append(persistent_class)
            elif name == "STACK_GLOBAL":
                cls, module = stack.pop(), stack.pop()
                if (module, cls) != PERSISTENT_CLASS:
                    raise MigrationError("unsupported HNSW metadata class")
                stack.append(persistent_class)
            elif name == "NEWOBJ":
                args, cls = stack.pop(), stack.pop()
                if cls is not persistent_class or args != ():
                    raise MigrationError("unsupported HNSW object construction")
                stack.append(instance)
            elif name == "BUILD":
                value = stack.pop()
                if (
                    stack[-1] is not instance
                    or state is not None
                    or type(value) is not dict
                ):
                    raise MigrationError("unsupported HNSW object state")
                state = value
            elif name == "STOP":
                if position != len(data) - 1 or len(stack) != 1:
                    raise MigrationError("invalid HNSW metadata termination")
                result = state if stack[0] is instance else stack[0]
                if type(result) is not dict:
                    raise MigrationError("unsupported HNSW metadata root")
                return result
            else:
                raise MigrationError(f"unsupported HNSW metadata opcode {name}")
    except (ValueError, IndexError, KeyError, TypeError) as exc:
        raise MigrationError("malformed HNSW metadata") from exc
    raise MigrationError("unterminated HNSW metadata")


def table_exists(connection: sqlite3.Connection, name: str) -> bool:
    return (
        connection.execute(
            "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?", (name,)
        ).fetchone()
        is not None
    )


def queue_topic(connection: sqlite3.Connection, collection_id: str) -> str | None:
    suffix = "/" + collection_id
    topics = connection.execute(
        "SELECT DISTINCT topic FROM embeddings_queue WHERE substr(topic, -length(?)) = ? LIMIT 2",
        (suffix, suffix),
    ).fetchall()
    if len(topics) > 1:
        raise MigrationError("multiple queue topics for one Chroma collection")
    return topics[0][0] if topics else None


def sequence(value) -> int:
    # Python Chroma <=0.4 stored sequence IDs as big-endian 8-byte blobs.
    if isinstance(value, bytes) and len(value) == 8:
        value = int.from_bytes(value, "big")
    if type(value) is not int or value < 0:
        raise MigrationError("invalid Chroma sequence ID")
    return value


def object_at(mapping, *keys):
    for key in keys:
        if not isinstance(mapping, dict):
            raise MigrationError("invalid Chroma vector configuration")
        mapping = mapping.get(key, {})
    if not isinstance(mapping, dict):
        raise MigrationError("invalid Chroma vector configuration")
    return mapping


def distance_space(
    connection: sqlite3.Connection, collection_id: str, segment: str
) -> str:
    spaces = set()
    tables = {
        row[0]
        for row in connection.execute(
            "SELECT name FROM sqlite_master WHERE type = 'table'"
        )
    }
    for table, key, value in (
        ("segment_metadata", "segment_id", segment),
        ("collection_metadata", "collection_id", collection_id),
    ):
        if table in tables:
            for row in connection.execute(
                f"SELECT str_value FROM {table} WHERE {key} = ? AND key = 'hnsw:space'",
                (value,),
            ):
                spaces.add(row[0])
    columns = {row[1] for row in connection.execute("PRAGMA table_info(collections)")}
    for column in ("config_json_str", "schema_str"):
        if column not in columns:
            continue
        row = connection.execute(
            f"SELECT {column} FROM collections WHERE id = ?", (collection_id,)
        ).fetchone()
        if row[0] is None:
            continue
        if len(row[0]) > MAX_INDEX_METADATA:
            raise MigrationError("oversized Chroma collection configuration")
        value = json.loads(row[0])
        if not isinstance(value, dict):
            raise MigrationError("invalid Chroma collection configuration")
        if column == "config_json_str":
            for key in ("hnsw_configuration", "hnsw"):
                config = value.get(key, {})
                if isinstance(config, dict) and "space" in config:
                    spaces.add(config["space"])
        else:
            for config in (
                object_at(value, "defaults", "float_list"),
                object_at(value, "keys", "#embedding", "float_list"),
            ):
                index = object_at(config, "vector_index", "config")
                if "space" in index:
                    spaces.add(index["space"])
    if len(spaces) > 1 or (spaces and next(iter(spaces)) not in {"l2", "ip", "cosine"}):
        raise MigrationError("ambiguous or unsupported Chroma distance space")
    return next(iter(spaces), "l2")


class PersistedVectors:
    def __init__(self, connection: sqlite3.Connection, collection: dict):
        self.vectors = {}
        self.sequences = {}
        self.global_sequence = None
        self.watermark = -1
        self.cosine = False
        palace = collection["palace"]
        segments = connection.execute(
            "SELECT id FROM segments WHERE collection = ? AND scope = 'VECTOR'",
            (collection["id"],),
        ).fetchall()
        if len(segments) > 1:
            raise MigrationError("multiple vector segments for one collection")
        if not segments:
            return
        segment = segments[0][0]
        if (
            not isinstance(segment, str)
            or Path(segment).name != segment
            or segment in {".", ".."}
        ):
            raise MigrationError("invalid Chroma vector segment path")
        folder = palace / segment
        if not folder.exists():
            return
        if folder.is_symlink():
            raise MigrationError("HNSW segment must not be a symlink")
        path = folder / "index_metadata.pickle"
        if not path.exists():
            # An initialized but never synced index has no authoritative vectors.
            return
        for name in ("index_metadata.pickle", "header.bin", "data_level0.bin"):
            if (folder / name).is_symlink() or not (folder / name).is_file():
                raise MigrationError("missing or unsafe HNSW persistence file")
        fields = persistent_metadata(path)
        has_watermarks = table_exists(connection, "max_seq_id")
        watermark = (
            connection.execute(
                "SELECT seq_id FROM max_seq_id WHERE segment_id = ?", (segment,)
            ).fetchone()
            if has_watermarks
            else None
        )
        if watermark is not None:
            self.watermark = sequence(watermark[0])
        elif fields.get("max_seq_id") is not None:
            self.watermark = sequence(fields["max_seq_id"])
        else:
            raise MigrationError("HNSW segment has no persisted sequence watermark")
        ids, labels, seqs = (
            fields.get(key) for key in ("id_to_label", "label_to_id", "id_to_seq_id")
        )
        dimension = fields.get("dimensionality") or collection["target_dimension"]
        if type(ids) is not dict or type(labels) is not dict or type(seqs) is not dict:
            raise MigrationError("invalid HNSW ID or sequence maps")
        rust_metadata = fields.get("dimensionality") is None and seqs == {}
        if rust_metadata:
            self.global_sequence = self.watermark
            seqs = {document_id: self.global_sequence for document_id in ids}
        if (
            len(ids) > MAX_INDEX_ELEMENTS
            or len(ids) != len(labels)
            or set(ids) != set(seqs)
            or type(dimension) is not int
            or not 0 < dimension <= MAX_DIMENSION
            or dimension != collection["target_dimension"]
        ):
            raise MigrationError("invalid HNSW ID, sequence or dimension metadata")
        for document_id, label in ids.items():
            if (
                type(document_id) is not str
                or not document_id
                or type(label) is not int
                or label < 0
                or labels.get(label) != document_id
            ):
                raise MigrationError("HNSW ID/label mappings are not bijective")
            self.sequences[document_id] = sequence(seqs[document_id])
            if self.sequences[document_id] > self.watermark:
                raise MigrationError("HNSW ID sequence exceeds its persisted watermark")
        with (folder / "header.bin").open("rb") as handle:
            header = handle.read(101)
        # Persistent HNSW v1 uses a packed native-size header, 100 bytes on
        # supported 64-bit little-endian Linux/macOS hosts.
        if len(header) != 100:
            raise MigrationError("unsupported HNSW header size")
        (
            version,
            offset,
            capacity,
            count,
            stride,
            label_offset,
            vector_offset,
            level,
            entry,
            maxm,
            maxm0,
            m,
            mult,
            ef,
        ) = struct.unpack("<iQQQQQQiIQQQdQ", header)
        if (
            version != 1
            or offset != 0
            or not 0 <= count <= capacity <= MAX_INDEX_ELEMENTS
            or count < len(ids)
            or not 0 < m <= 32768
            or maxm != m
            or maxm0 != 2 * m
            or vector_offset != 4 + 4 * maxm0
            or label_offset != vector_offset + 4 * dimension
            or stride != label_offset + 8
            or not -1 <= level <= 1024
            or (count > 0 and entry >= count)
            or not math.isfinite(mult)
            or mult <= 0
            or not m <= ef <= 10_000_000
        ):
            raise MigrationError("invalid or unsupported HNSW header")
        self.cosine = distance_space(connection, collection["id"], segment) == "cosine"
        lengths = folder / "length.bin"
        if self.cosine and (
            lengths.is_symlink()
            or not lengths.is_file()
            or lengths.stat().st_size != capacity * 4
        ):
            raise MigrationError("missing or malformed HNSW cosine vector lengths")
        data_path = folder / "data_level0.bin"
        if (
            capacity * stride > 8 * 1024**3
            or data_path.stat().st_size != capacity * stride
        ):
            raise MigrationError("truncated or oversized HNSW vector data")
        with data_path.open("rb") as handle:
            seen = set()
            for _ in range(count):
                record = handle.read(stride)
                label = struct.unpack_from("<Q", record, label_offset)[0]
                if label in seen:
                    raise MigrationError("duplicate HNSW label")
                seen.add(label)
                document_id = labels.get(label)
                if document_id is not None:
                    if record[2] & 1:
                        raise MigrationError(
                            "active HNSW mapping points to a deleted vector"
                        )
                    self.vectors[document_id] = (
                        data_path,
                        handle.tell() - stride + vector_offset,
                        4 * dimension,
                        lengths,
                        (handle.tell() // stride - 1) * 4,
                    )
        if set(self.vectors) != set(ids):
            raise MigrationError("HNSW mapping points to a missing vector")

    def vector(self, document_id, queue_vector, queue_sequence):
        persisted_sequence = self.sequences.get(document_id, -1)
        if queue_vector is not None and queue_sequence > persisted_sequence:
            return queue_vector
        location = self.vectors.get(document_id)
        if location is None:
            return None
        path, offset, size, lengths, length_offset = location
        with path.open("rb") as handle:
            handle.seek(offset)
            result = handle.read(size)
        if len(result) != size:
            raise MigrationError("HNSW vector changed during migration")
        if self.cosine:
            with lengths.open("rb") as handle:
                handle.seek(length_offset)
                raw_length = handle.read(4)
            if len(raw_length) != 4:
                raise MigrationError("truncated HNSW vector length")
            length = struct.unpack("<f", raw_length)[0]
            if not math.isfinite(length) or length < 0:
                raise MigrationError("invalid HNSW cosine vector length")
            result = (np.frombuffer(result, dtype="<f4") * np.float32(length)).tobytes()
        return result


def source_batches(connection: sqlite3.Connection, collection: dict):
    persisted = PersistedVectors(connection, collection)
    rows = connection.execute(
        """
        SELECT e.id, e.embedding_id, e.seq_id
        FROM embeddings e
        JOIN segments s ON e.segment_id = s.id AND s.scope = 'METADATA'
        WHERE s.collection = ?
        ORDER BY e.id
        """,
        (collection["id"],),
    )
    queue_columns = {
        row[1] for row in connection.execute("PRAGMA table_info(embeddings_queue)")
    }
    operation = "operation" if "operation" in queue_columns else "0"
    topic = queue_topic(connection, collection["id"])
    has_watermarks = table_exists(connection, "max_seq_id")
    watermark_join = (
        "LEFT JOIN max_seq_id m ON m.segment_id = s.id" if has_watermarks else ""
    )
    watermark_column = "m.seq_id" if has_watermarks else "NULL"
    metadata_segments = connection.execute(
        f"SELECT s.id, {watermark_column}, MAX(e.seq_id) "
        f"FROM segments s {watermark_join} LEFT JOIN embeddings e ON e.segment_id = s.id "
        "WHERE s.collection = ? AND s.scope = 'METADATA' GROUP BY s.id",
        (collection["id"],),
    ).fetchall()
    queued = connection.execute(
        "SELECT MAX(seq_id) FROM embeddings_queue WHERE topic = ?", (topic,)
    ).fetchone()[0]
    # Without a recorded consumer watermark, active rows provide a conservative
    # lower bound: later deletes cannot be inferred from rows already removed.
    applied = [
        sequence(seq if seq is not None else latest)
        if seq is not None or latest is not None
        else 0
        for _, seq, latest in metadata_segments
    ]
    if queued is not None and (not applied or min(applied) < sequence(queued)):
        raise MigrationError("Chroma metadata has not consumed its pending queue")
    while batch := rows.fetchmany(BATCH_SIZE):
        details = metadata(connection, [row[0] for row in batch])
        ids, documents, metadatas, embeddings = [], [], [], []
        for row_id, document_id, row_sequence in batch:
            if not isinstance(document_id, str) or not document_id:
                raise MigrationError("Chroma contains an empty document ID")
            row_sequence = sequence(row_sequence)
            persisted_sequence = persisted.sequences.get(document_id, -1)
            if (
                row_sequence > persisted.watermark
                and not connection.execute(
                    "SELECT 1 FROM embeddings_queue WHERE topic = ? AND id = ? AND seq_id = ?",
                    (topic, document_id, row_sequence),
                ).fetchone()
            ):
                raise MigrationError(
                    "Chroma queue is missing a change newer than its persisted index"
                )
            if persisted.global_sequence is None and persisted_sequence > row_sequence:
                raise MigrationError("HNSW state is newer than the Chroma metadata")
            latest = connection.execute(
                f"SELECT seq_id, vector, encoding, {operation} FROM embeddings_queue "
                "WHERE topic = ? AND id = ? AND seq_id <= ? "
                "ORDER BY seq_id DESC",
                (topic, document_id, row_sequence),
            )
            vector, vector_sequence = None, -1
            for queue_sequence, queued, encoding, queued_operation in latest:
                queue_sequence = sequence(queue_sequence)
                if queue_sequence < persisted_sequence:
                    break
                if queued_operation == 3:
                    raise MigrationError(
                        "active Chroma document follows a delete without a vector"
                    )
                if queued_operation not in {0, 1, 2}:
                    raise MigrationError("unknown Chroma queue operation")
                if queued is not None:
                    if encoding != "FLOAT32":
                        raise MigrationError("unsupported Chroma vector encoding")
                    vector, vector_sequence = queued, queue_sequence
                    break
            vector = persisted.vector(document_id, vector, vector_sequence)
            if not isinstance(vector, bytes) or not vector or len(vector) % 4:
                raise MigrationError(f"document {document_id!r} has no usable float32 vector")
            array = np.frombuffer(vector, dtype=np.float32)
            if not np.all(np.isfinite(array)):
                raise MigrationError(f"document {document_id!r} has a non-finite vector")
            if collection["target_dimension"] is not None and array.size != collection["target_dimension"]:
                raise MigrationError(f"document {document_id!r} has the wrong vector dimension")
            document, fields = details[row_id]
            if document is None:
                document = ""
            ids.append(document_id)
            documents.append(document)
            metadatas.append(fields)
            embeddings.append(array.tolist())
        yield ids, documents, metadatas, embeddings


def verify_collection(
    source: sqlite3.Connection, destination: sqlite3.Connection, collection: dict
) -> int:
    row = destination.execute(
        "SELECT id, dimension FROM collections WHERE name = ?", (collection["name"],)
    ).fetchone()
    if row is None:
        raise MigrationError(f"collection {collection['name']!r} was not migrated")
    destination_id, dimension = row
    if collection["target_dimension"] is not None and dimension != collection["target_dimension"]:
        raise MigrationError(f"collection {collection['name']!r} lost its vector dimension")
    expected_model = collection["identity"].model_name if collection["identity"] else None
    identity_row = destination.execute(
        "SELECT value FROM meta WHERE key = ?", (f"embedder_model:{collection['name']}",)
    ).fetchone()
    actual_model = identity_row[0] if identity_row else None
    if actual_model != expected_model:
        raise MigrationError(f"collection {collection['name']!r} lost its embedder identity")
    expected_ids = set()
    count = 0
    for ids, documents, metadatas, embeddings in source_batches(source, collection):
        result = destination.execute(
            f"SELECT id, document, metadata_json, embedding, dim FROM documents "
            f"WHERE collection_id = ? AND id IN ({','.join('?' for _ in ids)})",
            (destination_id, *ids),
        ).fetchall()
        actual = {row[0]: row[1:] for row in result}
        for document_id, document, fields, embedding in zip(ids, documents, metadatas, embeddings):
            row = actual.get(document_id)
            expected_vector = np.asarray(embedding, dtype=np.float32).tobytes()
            if row is None or row[2] != expected_vector or row[3] != len(embedding):
                raise MigrationError(f"document {document_id!r} lost its vector during migration")
            actual_document, actual_fields = row[0], json.loads(row[1])
            if actual_document != document or actual_fields != fields:
                raise MigrationError(f"document {document_id!r} changed during migration")
            expected_ids.add(document_id)
            count += 1
    stored_ids = {
        row[0]
        for row in destination.execute(
            "SELECT id FROM documents WHERE collection_id = ?", (destination_id,)
        )
    }
    if stored_ids != expected_ids:
        raise MigrationError(f"collection {collection['name']!r} has missing or extra documents")
    return count


def verify(source_path: Path, destination_path: Path) -> dict:
    source = open_read_only(source_path)
    destination = open_read_only(destination_path)
    try:
        source_collections = collections(source, source_path.parent)
        target_names = {
            row[0] for row in destination.execute("SELECT name FROM collections")
        }
        if target_names != {collection["name"] for collection in source_collections}:
            raise MigrationError("the SQLite palace has missing or extra collections")
        documents = sum(
            verify_collection(source, destination, collection)
            for collection in source_collections
        )
        return {"collections": len(source_collections), "documents": documents}
    finally:
        destination.close()
        source.close()


def cutover_complete(destination_path: Path) -> bool:
    connection = open_read_only(destination_path)
    try:
        row = connection.execute(
            "SELECT value FROM meta WHERE key = 'noisedeck.chroma_to_sqlite_exact'"
        ).fetchone()
    except sqlite3.Error as exc:
        raise MigrationError(f"cannot inspect the migration receipt in {destination_path}: {exc}") from exc
    finally:
        connection.close()
    if row is not None and row[0] != "1":
        raise MigrationError(f"unsupported migration receipt in {destination_path}")
    return row is not None


def mark_cutover(destination_path: Path) -> None:
    try:
        connection = sqlite3.connect(destination_path)
        try:
            connection.execute("PRAGMA busy_timeout=5000")
            connection.execute(
                "INSERT INTO meta(key, value) VALUES ('noisedeck.chroma_to_sqlite_exact', '1') "
                "ON CONFLICT(key) DO UPDATE SET value = excluded.value"
            )
            connection.commit()
        finally:
            connection.close()
    except sqlite3.Error as exc:
        raise MigrationError(f"cannot record the migration receipt in {destination_path}: {exc}") from exc


def source_fingerprint(palace: Path) -> dict:
    paths = [
        palace / SOURCE,
        palace / f"{SOURCE}-wal",
        palace / "mempalace_embedder.json",
    ]
    for folder in palace.iterdir():
        if (
            not folder.is_symlink()
            and folder.is_dir()
            and not folder.name.startswith(".sqlite_exact-migration-")
        ):
            for name in (
                "index_metadata.pickle",
                "header.bin",
                "data_level0.bin",
                "length.bin",
                "link_lists.bin",
            ):
                if (folder / name).exists():
                    paths.append(folder / name)
    result = {}
    for path in paths:
        if not path.exists():
            result[str(path)] = None
            continue
        if path.is_symlink() or not path.is_file():
            raise MigrationError("unsafe Chroma source path")
        digest = hashlib.sha256()
        with path.open("rb") as handle:
            while block := handle.read(1024 * 1024):
                digest.update(block)
        result[str(path)] = digest.hexdigest()
    return result


def migrate(palace: Path) -> dict:
    source_path = palace / SOURCE
    destination_path = palace / DESTINATION
    if not source_path.is_file():
        if destination_path.is_file():
            return {"palace": str(palace), "status": "already-sqlite"}
        return {"palace": str(palace), "status": "new-sqlite"}
    if destination_path.exists():
        if cutover_complete(destination_path):
            return {"palace": str(palace), "status": "already-sqlite"}
        fingerprint = source_fingerprint(palace)
        counts = verify(source_path, destination_path)
        if source_fingerprint(palace) != fingerprint:
            raise MigrationError("Chroma source changed during migration")
        mark_cutover(destination_path)
        return {"palace": str(palace), "status": "verified", **counts}

    fingerprint = source_fingerprint(palace)
    staging = Path(tempfile.mkdtemp(prefix=".sqlite_exact-migration-", dir=palace))
    backend = SQLiteExactBackend()
    try:
        backend._connect(str(staging), create=True)
        source = open_read_only(source_path)
        try:
            integrity = source.execute("PRAGMA integrity_check").fetchone()
            if integrity is None or integrity[0] != "ok":
                raise MigrationError("the Chroma database failed its integrity check")
            source_collections = collections(source, source_path.parent)
            for collection in source_collections:
                target = backend.get_or_create_collection(str(staging), collection["name"])
                if collection["target_dimension"] is not None:
                    # An emptied collection has no vector from which to recover its dimension.
                    with target._cursor(write=True) as cursor:
                        cursor.execute(
                            "UPDATE collections SET dimension = ? WHERE name = ?",
                            (collection["target_dimension"], collection["name"]),
                        )
                if collection["identity"]:
                    target.set_embedder_identity(collection["identity"])
                for ids, documents, metadatas, embeddings in source_batches(source, collection):
                    target.upsert(
                        ids=ids,
                        documents=documents,
                        metadatas=metadatas,
                        embeddings=embeddings,
                    )
        finally:
            source.close()
        counts = verify(source_path, staging / DESTINATION)
        backend.close()
        if source_fingerprint(palace) != fingerprint:
            raise MigrationError("Chroma source changed during migration")
        mark_cutover(staging / DESTINATION)
        os.chmod(staging / DESTINATION, 0o600)
        with (staging / DESTINATION).open("rb") as handle:
            os.fsync(handle.fileno())
        os.replace(staging / DESTINATION, destination_path)
        directory = os.open(palace, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
        return {"palace": str(palace), "status": "migrated", **counts}
    except (sqlite3.Error, ValueError, OSError) as exc:
        raise MigrationError(f"cannot migrate {palace}: {exc}") from exc
    finally:
        backend.close()
        shutil.rmtree(staging, ignore_errors=True)


def select_sqlite(config_path: Path | None) -> None:
    if config_path is None:
        return
    if config_path.exists():
        config = json.loads(config_path.read_text())
        if not isinstance(config, dict):
            raise MigrationError(f"{config_path} must contain a JSON object")
    else:
        config = {}
    config["backend"] = "sqlite_exact"
    config_path.parent.mkdir(parents=True, exist_ok=True)
    temporary = config_path.with_suffix(config_path.suffix + ".migration-tmp")
    temporary.write_text(json.dumps(config, indent=2) + "\n")
    temporary.chmod(0o600)
    os.replace(temporary, config_path)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--palace", action="append", type=Path, required=True)
    parser.add_argument("--config", type=Path)
    args = parser.parse_args()
    if len(set(args.palace)) != len(args.palace):
        raise SystemExit("each palace may be supplied only once")
    try:
        if args.config and args.config.exists():
            value = json.loads(args.config.read_text())
            if not isinstance(value, dict):
                raise MigrationError(f"{args.config} must contain a JSON object")
        results = [migrate(palace) for palace in args.palace]
        select_sqlite(args.config)
    except (MigrationError, sqlite3.Error, OSError, ValueError, json.JSONDecodeError) as exc:
        raise SystemExit(f"MemPalace SQLite migration refused: {exc}") from exc
    print(json.dumps({"backend": "sqlite_exact", "palaces": results}, separators=(",", ":")))


if __name__ == "__main__":
    main()
