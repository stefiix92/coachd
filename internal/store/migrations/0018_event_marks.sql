-- Events already fired (webhook and push), one row per event and key, so a
-- restart or a second check does not fire one twice. See docs/design/proactive.md.
CREATE TABLE event_marks (event TEXT NOT NULL, key TEXT NOT NULL, at TEXT NOT NULL, PRIMARY KEY (event, key));
