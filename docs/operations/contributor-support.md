# Contributor support runbook

## Safe support boundary

Support may request the Collector version, operating system/version, Xorg or
Wayland session type on Linux, task/session ID, visible error text, request ID,
and the time of failure. Never request passwords, refresh/access tokens,
presigned URLs, full raw recordings, unrelated screenshots, or the operating
system credential-vault contents.

Contributors sign in with OpenID Connect. Their internal contributor ID is
resolved server-side and does not need to be copied between sessions. If the
desktop credential is removed, signing in again restores access to the same
account and task history.

## Common cases

### No tasks are ready

1. Choose **Refresh tasks**.
2. Confirm the contributor used the invited account and the invitation was
   accepted rather than a different email/login.
3. An administrator checks assignment state and organization membership. Do
   not create a duplicate contributor to work around an identity mismatch.

### Permission or required-application failure

1. Re-run preflight and read the named failing check.
2. Open the required application before continuing.
3. On Linux, Xorg supports the V1 global input path. Wayland must fail closed
   when required telemetry is unavailable.
4. On macOS or Windows, grant only the requested screen/input permission and
   restart the Collector if the OS requires it.

### Recording pauses when the pointer leaves the task

This is intentional. The Collector pauses when focus leaves the allowed task
application or enters a denied sensitive context. Return to the task and choose
**Resume recording**. Do not ask support to disable the boundary.

### Collector closes or computer restarts

Reopen the Collector and choose **Recover session**. Completed one-minute
segments remain available; recovery opens paused. If capture was already
sealed, choose **Resume submission** instead of starting a new attempt.

### Upload is interrupted

Keep the sealed local capture. Reopen the task and retry submission. Multipart
state is persisted server-side and the Collector uploads only missing parts.
Do not rename or edit sealed files, because hash verification will reject them.

### Rework requested

Task history shows the reviewer explanation. Refresh tasks to open the newly
assigned corrected attempt; the original submission and review remain immutable.
Complete the task again and submit the new attempt.

### Privacy concern

Pause immediately, close sensitive applications, and contact the privacy route
in the contributor notice. Support records the smallest necessary time range
and session ID, escalates it as a potential privacy incident, and does not ask
the contributor to email captured evidence.

## Escalation evidence

Escalate to engineering with: sanitized error, collector/OS version, session
ID, request ID, UTC time, preflight result names, and whether recovery/retry was
attempted. Escalate immediately to security/privacy for unexpected recording,
cross-account data, public object access, credential exposure, or evidence that
a legal hold/deletion did not behave as shown.

