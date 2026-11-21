-- The runner's check-in (one per date, replaced whole) and the athlete's sport
-- settings as the intervals.icu sync last saw them. See docs/design/checkin-profile.md.
CREATE TABLE checkins (
	date TEXT PRIMARY KEY,
	soreness INTEGER, fatigue INTEGER, stress INTEGER, mood INTEGER, motivation INTEGER, sleep_quality INTEGER,
	pain_json TEXT NOT NULL DEFAULT '[]',
	note TEXT NOT NULL DEFAULT '',
	created_by TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE TABLE sport_settings (
	source TEXT NOT NULL,
	source_id TEXT NOT NULL,
	sports_json TEXT NOT NULL DEFAULT '[]',
	ftp_w INTEGER, indoor_ftp_w INTEGER, threshold_hr_bpm INTEGER, max_hr_bpm INTEGER,
	threshold_speed_mps REAL,
	power_zones_json TEXT NOT NULL DEFAULT '[]',
	hr_zones_json TEXT NOT NULL DEFAULT '[]',
	updated_at TEXT,
	PRIMARY KEY (source, source_id)
);
