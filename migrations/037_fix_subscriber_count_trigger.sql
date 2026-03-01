-- Fix: Replace per-row subscriber count trigger with efficient increment/decrement
-- The old trigger ran COUNT(*) for every INSERT, making bulk imports O(n²).

BEGIN;

-- Drop the old per-row trigger
DROP TRIGGER IF EXISTS trigger_update_list_counts ON mailing_subscribers;
DROP FUNCTION IF EXISTS update_list_counts();

-- New trigger function uses simple +1/-1 instead of COUNT(*)
CREATE OR REPLACE FUNCTION update_list_counts_fast() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        UPDATE mailing_lists SET
            subscriber_count = subscriber_count + 1,
            active_count = CASE WHEN NEW.status = 'confirmed' THEN active_count + 1 ELSE active_count END,
            updated_at = NOW()
        WHERE id = NEW.list_id;
    ELSIF TG_OP = 'DELETE' THEN
        UPDATE mailing_lists SET
            subscriber_count = GREATEST(subscriber_count - 1, 0),
            active_count = CASE WHEN OLD.status = 'confirmed' THEN GREATEST(active_count - 1, 0) ELSE active_count END,
            updated_at = NOW()
        WHERE id = OLD.list_id;
    ELSIF TG_OP = 'UPDATE' AND OLD.status != NEW.status THEN
        -- Only react to status changes
        UPDATE mailing_lists SET
            active_count = active_count
                + CASE WHEN NEW.status = 'confirmed' THEN 1 ELSE 0 END
                - CASE WHEN OLD.status = 'confirmed' THEN 1 ELSE 0 END,
            updated_at = NOW()
        WHERE id = NEW.list_id;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_update_list_counts
    AFTER INSERT OR UPDATE OR DELETE ON mailing_subscribers
    FOR EACH ROW
    EXECUTE FUNCTION update_list_counts_fast();

-- Recalculate counts once to ensure they're correct
UPDATE mailing_lists SET
    subscriber_count = (SELECT COUNT(*) FROM mailing_subscribers WHERE list_id = mailing_lists.id),
    active_count = (SELECT COUNT(*) FROM mailing_subscribers WHERE list_id = mailing_lists.id AND status = 'confirmed'),
    updated_at = NOW();

COMMIT;
