-- Facts about this coachd instance. key 'mode' = 'demo' marks a database
-- holding synthetic demo data (COACHD_DEMO=true); no row means real data.
CREATE TABLE instance (key TEXT PRIMARY KEY, value TEXT NOT NULL);
