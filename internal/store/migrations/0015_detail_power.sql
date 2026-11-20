-- Laps and series gain power (avg_power_w, power_w). They are written with the
-- activity detail, so the detail cache is reset to fill them from the stored
-- FIT files.
DELETE FROM activity_detail;
