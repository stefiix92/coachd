-- Activities gain a source (garmin or intervals_icu), the source's own ID, a
-- power flag and a hidden_by mark for the poorer copy of a session recorded by
-- both sources. See docs/design/intervals-activities.md.
ALTER TABLE activities ADD COLUMN source TEXT NOT NULL DEFAULT 'garmin';
ALTER TABLE activities ADD COLUMN source_id TEXT;
ALTER TABLE activities ADD COLUMN has_power INTEGER NOT NULL DEFAULT 0;
ALTER TABLE activities ADD COLUMN hidden_by INTEGER;
CREATE UNIQUE INDEX idx_activities_source_id ON activities(source, source_id) WHERE source_id IS NOT NULL;
UPDATE activities SET has_power = 1
    WHERE raw_json IS NOT NULL AND json_valid(CAST(raw_json AS TEXT))
      AND json_extract(CAST(raw_json AS TEXT), '$.avgPower') > 0;
