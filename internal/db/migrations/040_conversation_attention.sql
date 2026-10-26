CREATE TABLE conversation_attention (
    session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    generation INTEGER NOT NULL CHECK (generation > 0),
    identity_marker TEXT NOT NULL,
    native_id TEXT NOT NULL,
    sequence INTEGER NOT NULL CHECK (sequence > 0 AND sequence <= 9007199254740991),
    state TEXT NOT NULL CHECK (state IN ('unknown', 'working', 'needs_input', 'ended')),
    turn_id TEXT NOT NULL,
    last_ended_turn TEXT NOT NULL,
    ended_sequence INTEGER NOT NULL CHECK (ended_sequence >= 0 AND ended_sequence <= sequence),
    seen_sequence INTEGER NOT NULL CHECK (seen_sequence >= 0 AND seen_sequence <= ended_sequence)
);
