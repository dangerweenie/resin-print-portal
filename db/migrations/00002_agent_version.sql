-- +goose Up
-- +goose StatementBegin

-- Track which pi-agent build each Pi is running, and give an admin per-Pi
-- control over the fleet self-update:
--   agent_version         last version string the Pi reported (X-Agent-Version header)
--   agent_version_at      when that value last changed
--   agent_target_override non-empty pins THIS Pi to a specific version now,
--                         regardless of the global AGENT_AUTO_UPDATE setting
--   agent_update_hold     TRUE keeps this Pi on whatever it is running (auto-update skips it)
ALTER TABLE printers
    ADD COLUMN agent_version         TEXT NOT NULL DEFAULT '',
    ADD COLUMN agent_version_at      TIMESTAMPTZ,
    ADD COLUMN agent_target_override TEXT NOT NULL DEFAULT '',
    ADD COLUMN agent_update_hold     BOOLEAN NOT NULL DEFAULT FALSE;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE printers
    DROP COLUMN agent_version,
    DROP COLUMN agent_version_at,
    DROP COLUMN agent_target_override,
    DROP COLUMN agent_update_hold;
-- +goose StatementEnd
