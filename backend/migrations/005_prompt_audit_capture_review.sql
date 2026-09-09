ALTER TABLE sub2api_enhance.captures
    DROP CONSTRAINT captures_processing_status_check;

ALTER TABLE sub2api_enhance.captures
    ADD CONSTRAINT enhance_captures_processing_status_check
    CHECK (processing_status IN ('queued', 'processing', 'retry', 'done', 'failed', 'skipped', 'awaiting_review'));
