# Consent model

Consent documents are immutable, content-hashed, versioned records. An acceptance stores the contributor, document/version, timestamp, and collector version so later audits can prove exactly which terms applied.

The local V1 database ships with `platform-consent-v1`. The collector always retrieves the currently effective record from the API, renders the stored body, and places its exact ID, version, SHA-256, and acceptance time into the capture manifest. Existing acceptances remain reusable for later assignments governed by the same document.
