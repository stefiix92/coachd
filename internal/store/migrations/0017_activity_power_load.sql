-- Activities gain the average power and intervals.icu's own estimates
-- (training load, normalized power, intensity factor), filled from the stored
-- raw JSON. An intervals.icu activity whose original file is not FIT is tried
-- again: the FIT worker now falls back to the FIT file intervals.icu
-- generates. See docs/design/intervals-activities.md §9a.
ALTER TABLE activities ADD COLUMN avg_power_w REAL;
ALTER TABLE activities ADD COLUMN icu_training_load REAL;
ALTER TABLE activities ADD COLUMN icu_np_w REAL;
ALTER TABLE activities ADD COLUMN icu_intensity_factor REAL;
UPDATE activities SET avg_power_w = json_extract(CAST(raw_json AS TEXT), '$.avgPower')
    WHERE source = 'garmin' AND raw_json IS NOT NULL AND json_valid(CAST(raw_json AS TEXT))
      AND json_extract(CAST(raw_json AS TEXT), '$.avgPower') > 0;
UPDATE activities SET
    avg_power_w = CASE WHEN json_extract(CAST(raw_json AS TEXT), '$.icu_average_watts') > 0
        THEN json_extract(CAST(raw_json AS TEXT), '$.icu_average_watts') END,
    icu_training_load = json_extract(CAST(raw_json AS TEXT), '$.icu_training_load'),
    icu_np_w = CASE WHEN json_extract(CAST(raw_json AS TEXT), '$.icu_weighted_avg_watts') > 0
        THEN json_extract(CAST(raw_json AS TEXT), '$.icu_weighted_avg_watts') END,
    icu_intensity_factor = json_extract(CAST(raw_json AS TEXT), '$.icu_intensity') / 100.0
    WHERE source = 'intervals_icu' AND raw_json IS NOT NULL AND json_valid(CAST(raw_json AS TEXT));
UPDATE activities SET fit_status = 'pending', fit_attempts = 0
    WHERE source = 'intervals_icu' AND fit_status = 'unavailable'
      AND raw_json IS NOT NULL AND json_valid(CAST(raw_json AS TEXT))
      AND COALESCE(json_extract(CAST(raw_json AS TEXT), '$.file_type'), '') <> '';
