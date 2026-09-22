-- 0002: speed up the exceptions queue and turnaround queries.
CREATE INDEX IF NOT EXISTS request_views_open_exceptions_idx
    ON request_views ((jsonb_array_length(COALESCE(doc->'exceptions', '[]'::jsonb))))
    WHERE state NOT IN ('approved', 'closed');
CREATE INDEX IF NOT EXISTS request_views_decided_idx
    ON request_views (((doc->>'decided_at')::timestamptz))
    WHERE doc ? 'decided_at';
