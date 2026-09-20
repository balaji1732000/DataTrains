INSERT INTO consent_documents (id, version, text_hash, body, effective_date)
VALUES (
  'consent_platform_v1',
  'platform-consent-v1',
  'fe19c4dc7a26238861619383454939e926d4defef0bd303b9f4eee30b13ccde4',
  'By continuing, I consent to explicit, task-scoped screen and permitted input recording for the assigned task. I understand that a visible recording indicator remains on screen, I can pause or stop at any time, clipboard content is not captured, and leaving the allowed application pauses recording.',
  '2026-01-01T00:00:00Z'
)
ON CONFLICT (version) DO NOTHING;
