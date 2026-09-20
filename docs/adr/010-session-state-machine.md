# ADR-010: Backend-owned session state machine

The backend validates every session transition and appends an audit event in the same transaction. Clients never assemble workflow state from booleans.

