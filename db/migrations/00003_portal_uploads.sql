-- +goose Up
-- +goose StatementBegin

-- Uploads moved off the Pi. A member now uploads to the central portal, which
-- holds the sliced file here until they tap their fob at the printer to load
-- it. Job lifecycle gains a pre-print state:
--   staged   -> uploaded to the portal, file waiting to be pulled to a printer
--   printing -> physically loaded on the gadget (unchanged meaning)
--   ended    -> done / superseded / admin-cleared (unchanged)

-- One pending file per job. Keyed by job_id (not printer_id) so a second
-- member's upload can't overwrite a file another member has already claimed and
-- is mid-download. Bytes are deleted once the print starts or the job ends.
CREATE TABLE staged_files (
    job_id      BIGINT PRIMARY KEY REFERENCES print_jobs(id) ON DELETE CASCADE,
    printer_id  BIGINT NOT NULL REFERENCES printers(id) ON DELETE CASCADE,
    filename    TEXT   NOT NULL,
    bytes       BYTEA  NOT NULL,
    sha256      TEXT   NOT NULL,
    size_bytes  BIGINT NOT NULL,
    uploaded_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX staged_files_printer_idx ON staged_files(printer_id);

-- Certify-by-tap: an admin arms a short window on a printer from the
-- Certifications page, and the next eligible fob tapped there certifies that
-- member. cert_capture_by is the admin who armed it (used as certified_by).
ALTER TABLE printers
    ADD COLUMN cert_capture_until TIMESTAMPTZ,
    ADD COLUMN cert_capture_by    TEXT NOT NULL DEFAULT '';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS staged_files;
ALTER TABLE printers
    DROP COLUMN cert_capture_until,
    DROP COLUMN cert_capture_by;
-- +goose StatementEnd
