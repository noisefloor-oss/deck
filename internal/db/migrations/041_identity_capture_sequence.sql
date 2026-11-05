-- 041_identity_capture_sequence: delayed older hook deliveries cannot roll
-- back the newest conversation a generation has already accepted.

ALTER TABLE provider_identity_capture_grants
  ADD COLUMN accepted_sequence INTEGER NOT NULL DEFAULT 0
  CONSTRAINT chk_capture_accepted_sequence
  CHECK (accepted_sequence >= 0
    AND accepted_sequence <= 9007199254740991);
