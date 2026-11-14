-- Repeatable sample data for migration/backup verification. Never run in production.
INSERT INTO events (id, source_id, source_event_id, title, start_at, official_url, source_url)
VALUES
  ('quest7-devpost-1', 'devpost', 'q7-1', 'Meeteam Test Hackathon', '2026-12-10T10:00:00Z', 'https://example.org/hackathon-1', 'https://example.org/source-1'),
  ('quest7-hacklist-2', 'hacklist', 'q7-2', 'Meeteam Data Challenge', '2026-12-15T09:00:00Z', 'https://example.org/hackathon-2', 'https://example.org/source-2'),
  ('quest7-devfolio-3', 'devfolio', 'q7-3', 'Meeteam Design Sprint', '2026-12-20T12:00:00Z', 'https://example.org/hackathon-3', 'https://example.org/source-3')
ON CONFLICT (id) DO NOTHING;
