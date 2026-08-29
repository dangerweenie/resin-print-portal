-- +goose Up
-- +goose StatementBegin

-- Roster mirrored from TinkerAccess (the get_users Leptos endpoint). The PK is
-- TinkerAccess's own user id so re-syncs are plain upserts. Rows are never
-- deleted: certification history and audit rows point at them.
CREATE TABLE members (
    id                   BIGINT PRIMARY KEY,
    name                 TEXT,
    code                 TEXT,
    status               CHAR(1) NOT NULL DEFAULT 'I',   -- 'A' active, 'I' inactive, 'S' active + 24h
    active               BOOLEAN NOT NULL DEFAULT FALSE, -- derived: status IN ('A','S') AND on roster
    first_seen_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_synced_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    source_missing_since TIMESTAMPTZ                     -- set when a previously-seen id drops off the roster
);

-- Admin-maintained mapping from a Tinkermill Slack display name to a member.
-- This is the primary identity path; an exact full-name match is the fallback.
CREATE TABLE slack_identities (
    id                   BIGSERIAL PRIMARY KEY,
    member_id            BIGINT NOT NULL REFERENCES members(id),
    slack_name_normalized TEXT NOT NULL UNIQUE,
    added_by             TEXT NOT NULL,
    added_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX slack_identities_member_id_idx ON slack_identities(member_id);

-- One row per Pi/printer pair. A Pi self-enrolls on first boot (device_id set,
-- approved=false) and an admin approves it once; a printer can also be created
-- by hand in the admin UI (device_id null, approved=true).
CREATE TABLE printers (
    id                 BIGSERIAL PRIMARY KEY,
    slug               TEXT NOT NULL UNIQUE,
    display_name       TEXT NOT NULL,
    model              TEXT NOT NULL DEFAULT '',
    allowed_extensions TEXT[] NOT NULL DEFAULT '{}',      -- empty => allow anything
    safety_checklist   JSONB NOT NULL DEFAULT '[]'::jsonb, -- array of strings
    slack_webhook_url  TEXT NOT NULL DEFAULT '',
    api_key_hash       TEXT NOT NULL,                      -- sha-256 hex of the Pi bearer token
    device_id          TEXT UNIQUE,                        -- stable Pi hardware id; null for hand-made printers
    approved           BOOLEAN NOT NULL DEFAULT FALSE,     -- admin has OK'd this enrolled Pi
    enrolled_at        TIMESTAMPTZ,                        -- when the Pi self-registered
    last_seen_at       TIMESTAMPTZ,                        -- last authenticated request from the Pi
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Resin-printer certification lives here, not in TinkerAccess. Active cert =
-- revoked_at IS NULL.
CREATE TABLE certifications (
    id           BIGSERIAL PRIMARY KEY,
    member_id    BIGINT NOT NULL REFERENCES members(id),
    printer_id   BIGINT NOT NULL REFERENCES printers(id),
    certified_by TEXT NOT NULL,
    certified_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at   TIMESTAMPTZ
);
CREATE UNIQUE INDEX certifications_active_uniq
    ON certifications(member_id, printer_id)
    WHERE revoked_at IS NULL;

CREATE TABLE print_jobs (
    id                    BIGSERIAL PRIMARY KEY,
    printer_id            BIGINT NOT NULL REFERENCES printers(id),
    member_id             BIGINT REFERENCES members(id),
    slack_name_used       TEXT NOT NULL,
    filename              TEXT NOT NULL,
    sliced_for_model      TEXT NOT NULL DEFAULT '',
    checklist_answers     JSONB NOT NULL DEFAULT '[]'::jsonb,
    started_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    estimated_seconds     INTEGER,
    eta_exact             BOOLEAN NOT NULL DEFAULT FALSE,
    estimated_complete_at TIMESTAMPTZ,
    ended_at              TIMESTAMPTZ,
    status                TEXT NOT NULL DEFAULT 'printing', -- printing|ended
    end_reason            TEXT                              -- member_finished|superseded|admin_cleared
);
CREATE INDEX print_jobs_printer_status_idx ON print_jobs(printer_id, status);
CREATE INDEX print_jobs_printer_started_idx ON print_jobs(printer_id, started_at DESC);

-- Every check / upload attempt, approved or denied. Supersedes the old Flask
-- `uploads` table and additionally records denials.
CREATE TABLE decision_log (
    id              BIGSERIAL PRIMARY KEY,
    printer_id      BIGINT REFERENCES printers(id),
    slack_name_used TEXT NOT NULL,
    member_id       BIGINT REFERENCES members(id),
    filename        TEXT NOT NULL DEFAULT '',
    ts              TIMESTAMPTZ NOT NULL DEFAULT now(),
    outcome         TEXT NOT NULL,   -- approved|approved_by_name_match|denied
    reason          TEXT NOT NULL DEFAULT ''
);
CREATE INDEX decision_log_ts_idx ON decision_log(ts DESC);

CREATE TABLE admins (
    id            BIGSERIAL PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS admins;
DROP TABLE IF EXISTS decision_log;
DROP TABLE IF EXISTS print_jobs;
DROP TABLE IF EXISTS certifications;
DROP TABLE IF EXISTS printers;
DROP TABLE IF EXISTS slack_identities;
DROP TABLE IF EXISTS members;
-- +goose StatementEnd
