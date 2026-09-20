use collector_core::{CaptureManifest, TimedInputEvent};
use reqwest::{StatusCode, Url, blocking::Client, blocking::RequestBuilder};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use sha2::{Digest, Sha256};
use std::collections::{HashMap, HashSet};
use std::fs::{File, OpenOptions};
use std::io::{BufRead, BufReader, Read, Seek, SeekFrom, Write};
use std::path::{Path, PathBuf};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

#[derive(Deserialize)]
pub struct SubmissionRequest {
    pub api_base: String,
    pub actor: String,
    pub session_id: String,
    pub contributor_id: String,
    pub task: SubmissionTask,
    pub environment: SubmissionEnvironment,
    #[serde(skip)]
    pub access_token: Option<String>,
}

#[derive(Deserialize, Serialize)]
pub struct SubmissionTask {
    pub task_id: String,
    pub template_id: String,
    pub goal: String,
    pub category: String,
    pub difficulty: String,
}

#[derive(Deserialize, Serialize)]
pub struct SubmissionEnvironment {
    pub os: String,
    pub os_version: String,
    pub locale: String,
    pub timezone: String,
    pub display: SubmissionDisplay,
}

#[derive(Deserialize, Serialize)]
pub struct SubmissionDisplay {
    pub width: u32,
    pub height: u32,
    pub scale_factor: f64,
}

#[derive(Serialize)]
pub struct SubmissionReceipt {
    pub session_id: String,
    pub job_id: String,
    pub uploaded_artifacts: usize,
    pub state: String,
}

pub fn submit(directory: &Path, request: SubmissionRequest) -> Result<SubmissionReceipt, String> {
    let api_base = validated_api_base(&request.api_base)?;
    let manifest: CaptureManifest = serde_json::from_reader(
        File::open(directory.join("manifest.json"))
            .map_err(|error| format!("open capture manifest: {error}"))?,
    )
    .map_err(|error| format!("decode capture manifest: {error}"))?;
    if manifest.session_id != request.session_id || manifest.task_id != request.task.task_id {
        return Err("capture manifest does not match the selected assignment".into());
    }
    verify_manifest(directory, &manifest)?;
    let actions = read_actions(directory, &manifest)?;
    validate_timeline(&manifest, &actions)?;
    let trajectory = trajectory_document(&manifest, &request, actions);
    let trajectory_bytes = serde_json::to_vec_pretty(&trajectory)
        .map_err(|error| format!("encode canonical trajectory: {error}"))?;

    let client = Client::builder()
        .connect_timeout(Duration::from_secs(5))
        .timeout(Duration::from_secs(30 * 60))
        .build()
        .map_err(|error| format!("create upload client: {error}"))?;
    let mut uploaded = 0;
    for artifact in &manifest.artifacts {
        upload_file(
            &client,
            &api_base,
            &request,
            &artifact.path,
            &directory.join(&artifact.path),
            &artifact.sha256,
            &artifact.media_type,
        )?;
        uploaded += 1;
    }
    upload_bytes(
        &client,
        &api_base,
        &request,
        "manifest.json",
        &std::fs::read(directory.join("manifest.json"))
            .map_err(|error| format!("read capture manifest: {error}"))?,
        "application/json",
    )?;
    uploaded += 1;
    upload_bytes(
        &client,
        &api_base,
        &request,
        "trajectory.json",
        &trajectory_bytes,
        "application/json",
    )?;
    uploaded += 1;

    let response = authorized(
        client.post(
            api_base
                .join(&format!("v1/sessions/{}/submission", request.session_id))
                .map_err(|error| error.to_string())?,
        ),
        &request,
    )
    .json(&json!({}))
    .send()
    .map_err(|error| format!("submit session: {error}"))?;
    let status = response.status();
    let body: Value = response
        .json()
        .map_err(|error| format!("decode submission response: {error}"))?;
    if status != StatusCode::ACCEPTED {
        return Err(api_error(status, &body));
    }
    let job_id = body
        .pointer("/job/id")
        .and_then(Value::as_str)
        .ok_or_else(|| "submission response is missing job.id".to_string())?;
    let state = body
        .pointer("/session/state")
        .and_then(Value::as_str)
        .unwrap_or("SUBMITTED");
    let receipt = SubmissionReceipt {
        session_id: request.session_id,
        job_id: job_id.into(),
        uploaded_artifacts: uploaded,
        state: state.into(),
    };
    persist_submission_receipt(directory, &receipt)?;
    Ok(receipt)
}

fn authorized(builder: RequestBuilder, request: &SubmissionRequest) -> RequestBuilder {
    match &request.access_token {
        Some(token) => builder.bearer_auth(token),
        None => builder.header("X-Actor-ID", &request.actor),
    }
}

fn persist_submission_receipt(directory: &Path, receipt: &SubmissionReceipt) -> Result<(), String> {
    let destination = directory.join("submission.json");
    let temporary = directory.join("submission.json.tmp");
    let bytes = serde_json::to_vec_pretty(receipt)
        .map_err(|error| format!("encode submission receipt: {error}"))?;
    let mut file =
        File::create(&temporary).map_err(|error| format!("create submission receipt: {error}"))?;
    file.write_all(&bytes)
        .and_then(|_| file.write_all(b"\n"))
        .and_then(|_| file.sync_all())
        .map_err(|error| format!("persist submission receipt: {error}"))?;
    drop(file);
    std::fs::rename(&temporary, &destination)
        .map_err(|error| format!("commit submission receipt: {error}"))
}

fn validated_api_base(value: &str) -> Result<Url, String> {
    let mut url = Url::parse(value.trim()).map_err(|error| format!("invalid API URL: {error}"))?;
    if url.username() != ""
        || url.password().is_some()
        || url.query().is_some()
        || url.fragment().is_some()
    {
        return Err("API URL cannot contain credentials, query parameters, or a fragment".into());
    }
    let host = url.host_str().unwrap_or_default();
    if url.scheme() == "http" && !matches!(host, "127.0.0.1" | "localhost" | "::1") {
        return Err("unencrypted API connections are restricted to the local machine".into());
    }
    if !matches!(url.scheme(), "http" | "https") {
        return Err("API URL must use HTTP or HTTPS".into());
    }
    if !url.path().ends_with('/') {
        url.set_path(&format!("{}/", url.path()));
    }
    Ok(url)
}

fn verify_manifest(directory: &Path, manifest: &CaptureManifest) -> Result<(), String> {
    if manifest.duration_ns == 0 || manifest.video_segments.is_empty() {
        return Err("capture has no usable video timeline".into());
    }
    if !manifest
        .artifacts
        .iter()
        .any(|artifact| artifact.path.starts_with("outputs/"))
    {
        return Err("capture has no attached output".into());
    }
    for artifact in &manifest.artifacts {
        let path = safe_local_artifact(directory, &artifact.path)?;
        let metadata = path
            .metadata()
            .map_err(|error| format!("inspect {}: {error}", artifact.path))?;
        if metadata.len() != artifact.size || sha256_file(&path)? != artifact.sha256 {
            return Err(format!(
                "local artifact no longer matches its manifest: {}",
                artifact.path
            ));
        }
    }
    Ok(())
}

fn read_actions(directory: &Path, manifest: &CaptureManifest) -> Result<Vec<Value>, String> {
    let mut actions = Vec::new();
    let mut event_paths: Vec<&str> = manifest
        .artifacts
        .iter()
        .filter_map(|artifact| {
            (artifact.media_type == "application/x-ndjson").then_some(artifact.path.as_str())
        })
        .collect();
    event_paths.sort_unstable();
    for relative in event_paths {
        let reader = BufReader::new(
            File::open(safe_local_artifact(directory, relative)?)
                .map_err(|error| format!("open {relative}: {error}"))?,
        );
        for (index, line) in reader.lines().enumerate() {
            let line = line.map_err(|error| format!("read {relative}: {error}"))?;
            if line.trim().is_empty() {
                continue;
            }
            let event: TimedInputEvent = serde_json::from_str(&line)
                .map_err(|error| format!("decode {relative} line {}: {error}", index + 1))?;
            actions.push(
                serde_json::to_value(event).map_err(|error| format!("encode action: {error}"))?,
            );
        }
    }
    Ok(actions)
}

fn validate_timeline(manifest: &CaptureManifest, actions: &[Value]) -> Result<(), String> {
    let mut previous = 0;
    for (index, action) in actions.iter().enumerate() {
        let timestamp = action
            .get("timestamp_ns")
            .and_then(Value::as_u64)
            .ok_or_else(|| format!("action {index} has no timestamp"))?;
        if timestamp < previous || timestamp > manifest.duration_ns {
            return Err(format!(
                "action {index} is outside the monotonic capture timeline"
            ));
        }
        previous = timestamp;
    }
    Ok(())
}

fn trajectory_document(
    manifest: &CaptureManifest,
    request: &SubmissionRequest,
    actions: Vec<Value>,
) -> Value {
    let prefix = format!("raw/sessions/{}/", manifest.session_id);
    let videos: Vec<Value> = manifest
        .video_segments
        .iter()
        .map(|segment| {
            json!({
                "segment_id": segment.segment_id, "key": format!("{prefix}{}", segment.path),
                "start_ns": segment.start_ns, "end_ns": segment.end_ns,
                "sha256": segment.sha256, "size": segment.size,
            })
        })
        .collect();
    let outputs: Vec<Value> = manifest
        .artifacts
        .iter()
        .filter(|artifact| artifact.path.starts_with("outputs/"))
        .map(|artifact| {
            json!({
                "key": format!("{prefix}{}", artifact.path), "sha256": artifact.sha256,
                "size": artifact.size, "media_type": artifact.media_type,
            })
        })
        .collect();
    json!({
        "schema_version": "trajectory/v1",
        "trajectory_id": format!("traj_{}", manifest.session_id),
        "task": request.task,
        "environment": request.environment,
        "capture": {
            "started_at": unix_ms_iso(manifest.started_at_unix_ms),
            "duration_ns": manifest.duration_ns,
            "clock": "monotonic_ns_since_session_start",
            "video_segments": videos,
        },
        "actions": actions,
        "outcome": { "status": "success", "output_artifacts": outputs },
        "qa": { "state": "pending", "rubric_version": "rubric-v1", "reviews": [] },
        "privacy": {
            "consent_document_id": manifest.consent.document_id,
            "consent_version": manifest.consent.version,
            "clipboard_captured": false,
            "recording_visible": true,
            "pii_review": "pending",
        },
        "provenance": {
            "session_id": manifest.session_id,
            "contributor_id": request.contributor_id,
            "collector_version": manifest.client_version,
            "created_at": unix_ms_iso(manifest.started_at_unix_ms.saturating_add(manifest.duration_ns / 1_000_000)),
        },
    })
}

fn upload_file(
    client: &Client,
    base: &Url,
    request: &SubmissionRequest,
    relative: &str,
    path: &Path,
    hash: &str,
    media_type: &str,
) -> Result<(), String> {
    let size = path
        .metadata()
        .map_err(|error| format!("inspect {relative} for upload: {error}"))?
        .len();
    if multipart_upload_file(
        client,
        base,
        request,
        MultipartFile {
            relative,
            path,
            hash,
            media_type,
            size,
        },
    )? {
        return Ok(());
    }
    let file = File::open(path).map_err(|error| format!("open {relative} for upload: {error}"))?;
    send_upload(
        client,
        base,
        request,
        relative,
        hash,
        media_type,
        reqwest::blocking::Body::from(file),
    )
}

fn upload_bytes(
    client: &Client,
    base: &Url,
    request: &SubmissionRequest,
    relative: &str,
    bytes: &[u8],
    media_type: &str,
) -> Result<(), String> {
    let hash = sha256_bytes(bytes);
    let nonce = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_err(|error| format!("create resumable upload staging identity: {error}"))?
        .as_nanos();
    let temporary = std::env::temp_dir().join(format!(
        "datatrains-upload-{}-{nonce}.tmp",
        std::process::id()
    ));
    let mut staged = OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(&temporary)
        .map_err(|error| format!("create {relative} resumable upload staging file: {error}"))?;
    staged
        .write_all(bytes)
        .and_then(|_| staged.sync_all())
        .map_err(|error| format!("stage {relative} for resumable upload: {error}"))?;
    drop(staged);
    let multipart_result = multipart_upload_file(
        client,
        base,
        request,
        MultipartFile {
            relative,
            path: &temporary,
            hash: &hash,
            media_type,
            size: bytes.len() as u64,
        },
    );
    let _ = std::fs::remove_file(&temporary);
    if multipart_result? {
        return Ok(());
    }
    send_upload(
        client,
        base,
        request,
        relative,
        &hash,
        media_type,
        bytes.to_vec().into(),
    )
}

#[derive(Deserialize)]
struct MultipartStartResponse {
    state: String,
    upload_id: Option<String>,
    part_size_bytes: Option<u64>,
    total_parts: Option<u32>,
    #[serde(default)]
    uploaded_parts: Vec<MultipartUploadedPart>,
}

#[derive(Deserialize)]
struct MultipartUploadedPart {
    part_number: u32,
}

#[derive(Deserialize)]
struct MultipartPartAuthorization {
    url: String,
    method: String,
    headers: HashMap<String, String>,
}

struct MultipartFile<'a> {
    relative: &'a str,
    path: &'a Path,
    hash: &'a str,
    media_type: &'a str,
    size: u64,
}

struct MultipartPart<'a> {
    source: &'a MultipartFile<'a>,
    upload_id: &'a str,
    number: u32,
    offset: u64,
    size: u64,
}

fn multipart_upload_file(
    client: &Client,
    base: &Url,
    request: &SubmissionRequest,
    source: MultipartFile<'_>,
) -> Result<bool, String> {
    let start_url = base
        .join(&format!(
            "v1/sessions/{}/multipart-uploads",
            request.session_id
        ))
        .map_err(|error| error.to_string())?;
    let response = authorized(client.post(start_url), request)
        .json(&json!({
            "path": source.relative, "sha256": source.hash, "size_bytes": source.size,
            "media_type": source.media_type,
        }))
        .send()
        .map_err(|error| format!("start resumable upload for {}: {error}", source.relative))?;
    if response.status() == StatusCode::NOT_IMPLEMENTED {
        return Ok(false);
    }
    let status = response.status();
    let body: Value = response.json().map_err(|error| {
        format!(
            "decode resumable upload response for {}: {error}",
            source.relative
        )
    })?;
    if !status.is_success() {
        return Err(api_error(status, &body));
    }
    let start: MultipartStartResponse = serde_json::from_value(body).map_err(|error| {
        format!(
            "decode resumable upload plan for {}: {error}",
            source.relative
        )
    })?;
    if start.state == "completed" {
        return Ok(true);
    }
    let upload_id = start.upload_id.ok_or_else(|| {
        format!(
            "resumable upload plan for {} has no upload_id",
            source.relative
        )
    })?;
    let part_size = start
        .part_size_bytes
        .filter(|value| *value > 0)
        .ok_or_else(|| {
            format!(
                "resumable upload plan for {} has no part size",
                source.relative
            )
        })?;
    let total_parts = start
        .total_parts
        .filter(|value| *value > 0)
        .ok_or_else(|| format!("resumable upload plan for {} has no parts", source.relative))?;
    let completed: HashSet<u32> = start
        .uploaded_parts
        .into_iter()
        .map(|part| part.part_number)
        .collect();

    for part_number in 1..=total_parts {
        if completed.contains(&part_number) {
            continue;
        }
        let offset = u64::from(part_number - 1) * part_size;
        let expected_size = std::cmp::min(part_size, source.size.saturating_sub(offset));
        if expected_size == 0 {
            return Err(format!(
                "resumable upload plan for {} exceeds the file size",
                source.relative
            ));
        }
        upload_multipart_part(
            client,
            base,
            request,
            MultipartPart {
                source: &source,
                upload_id: &upload_id,
                number: part_number,
                offset,
                size: expected_size,
            },
        )?;
    }

    let complete_url = base
        .join(&format!(
            "v1/sessions/{}/multipart-uploads/{upload_id}/complete",
            request.session_id
        ))
        .map_err(|error| error.to_string())?;
    let response = authorized(client.post(complete_url), request)
        .json(&json!({}))
        .send()
        .map_err(|error| format!("complete resumable upload for {}: {error}", source.relative))?;
    let status = response.status();
    if status.is_success() {
        return Ok(true);
    }
    let body: Value = response.json().unwrap_or_else(|_| json!({}));
    Err(api_error(status, &body))
}

fn upload_multipart_part(
    client: &Client,
    base: &Url,
    request: &SubmissionRequest,
    part: MultipartPart<'_>,
) -> Result<(), String> {
    let part_base = format!(
        "v1/sessions/{}/multipart-uploads/{}/parts/{}",
        request.session_id, part.upload_id, part.number
    );
    let authorize_url = base
        .join(&format!("{part_base}/authorize"))
        .map_err(|error| error.to_string())?;
    let response = authorized(client.post(authorize_url), request)
        .json(&json!({"size_bytes": part.size}))
        .send()
        .map_err(|error| {
            format!(
                "authorize part {} of {}: {error}",
                part.number, part.source.relative
            )
        })?;
    let status = response.status();
    let body: Value = response.json().map_err(|error| {
        format!(
            "decode part authorization for {}: {error}",
            part.source.relative
        )
    })?;
    if !status.is_success() {
        return Err(api_error(status, &body));
    }
    let authorization: MultipartPartAuthorization =
        serde_json::from_value(body).map_err(|error| {
            format!(
                "decode part authorization for {}: {error}",
                part.source.relative
            )
        })?;
    if !authorization.method.eq_ignore_ascii_case("PUT") {
        return Err(format!(
            "part authorization for {} did not require PUT",
            part.source.relative
        ));
    }
    let upload_url = Url::parse(&authorization.url)
        .map_err(|error| format!("invalid storage URL for {}: {error}", part.source.relative))?;
    if upload_url.scheme() != "https" {
        return Err(format!(
            "storage URL for {} is not HTTPS",
            part.source.relative
        ));
    }
    let mut file = File::open(part.source.path).map_err(|error| {
        format!(
            "open {} part {}: {error}",
            part.source.relative, part.number
        )
    })?;
    file.seek(SeekFrom::Start(part.offset)).map_err(|error| {
        format!(
            "seek {} part {}: {error}",
            part.source.relative, part.number
        )
    })?;
    let mut builder = client.put(upload_url);
    for (name, value) in authorization.headers {
        if !name.eq_ignore_ascii_case("host") {
            builder = builder.header(name, value);
        }
    }
    let response = builder
        .body(reqwest::blocking::Body::sized(
            file.take(part.size),
            part.size,
        ))
        .send()
        .map_err(|error| {
            format!(
                "upload part {} of {}: {error}",
                part.number, part.source.relative
            )
        })?;
    if !response.status().is_success() {
        return Err(format!(
            "storage rejected part {} of {} ({})",
            part.number,
            part.source.relative,
            response.status()
        ));
    }
    let etag = response
        .headers()
        .get(reqwest::header::ETAG)
        .and_then(|value| value.to_str().ok())
        .filter(|value| !value.trim().is_empty())
        .ok_or_else(|| {
            format!(
                "storage response for part {} of {} has no ETag",
                part.number, part.source.relative
            )
        })?;
    let complete_url = base
        .join(&format!("{part_base}/complete"))
        .map_err(|error| error.to_string())?;
    let response = authorized(client.post(complete_url), request)
        .json(&json!({"size_bytes": part.size, "etag": etag}))
        .send()
        .map_err(|error| {
            format!(
                "record part {} of {}: {error}",
                part.number, part.source.relative
            )
        })?;
    let status = response.status();
    if status.is_success() {
        return Ok(());
    }
    let body: Value = response.json().unwrap_or_else(|_| json!({}));
    Err(api_error(status, &body))
}

fn send_upload(
    client: &Client,
    base: &Url,
    request: &SubmissionRequest,
    relative: &str,
    hash: &str,
    media_type: &str,
    body: reqwest::blocking::Body,
) -> Result<(), String> {
    let url = base
        .join(&format!(
            "v1/sessions/{}/artifacts/{relative}",
            request.session_id
        ))
        .map_err(|error| error.to_string())?;
    let response = authorized(client.put(url), request)
        .header("X-Artifact-SHA256", hash)
        .header("Content-Type", media_type)
        .body(body)
        .send()
        .map_err(|error| format!("upload {relative}: {error}"))?;
    let status = response.status();
    if status.is_success() {
        return Ok(());
    }
    let body: Value = response.json().unwrap_or_else(|_| json!({}));
    Err(api_error(status, &body))
}

fn safe_local_artifact(directory: &Path, relative: &str) -> Result<PathBuf, String> {
    let relative_path = Path::new(relative);
    if relative_path.is_absolute()
        || relative_path
            .components()
            .any(|component| !matches!(component, std::path::Component::Normal(_)))
    {
        return Err(format!("unsafe local artifact path: {relative}"));
    }
    Ok(directory.join(relative_path))
}

fn sha256_file(path: &Path) -> Result<String, String> {
    let mut file = File::open(path).map_err(|error| error.to_string())?;
    let mut digest = Sha256::new();
    let mut buffer = [0_u8; 64 * 1024];
    loop {
        let count = file.read(&mut buffer).map_err(|error| error.to_string())?;
        if count == 0 {
            break;
        }
        digest.update(&buffer[..count]);
    }
    Ok(format!("{:x}", digest.finalize()))
}

fn sha256_bytes(bytes: &[u8]) -> String {
    format!("{:x}", Sha256::digest(bytes))
}

fn unix_ms_iso(milliseconds: u64) -> String {
    let seconds = milliseconds / 1_000;
    let millis = milliseconds % 1_000;
    format!("{}.{millis:03}Z", chrono_free_utc(seconds))
}

fn chrono_free_utc(seconds: u64) -> String {
    // Civil date conversion based on Howard Hinnant's public-domain algorithm.
    let days = (seconds / 86_400) as i64;
    let day_seconds = seconds % 86_400;
    let z = days + 719_468;
    let era = if z >= 0 { z } else { z - 146_096 } / 146_097;
    let doe = z - era * 146_097;
    let yoe = (doe - doe / 1_460 + doe / 36_524 - doe / 146_096) / 365;
    let mut year = yoe + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let day = doy - (153 * mp + 2) / 5 + 1;
    let month = mp + if mp < 10 { 3 } else { -9 };
    year += i64::from(month <= 2);
    format!(
        "{year:04}-{month:02}-{day:02}T{:02}:{:02}:{:02}",
        day_seconds / 3_600,
        (day_seconds % 3_600) / 60,
        day_seconds % 60
    )
}

fn api_error(status: StatusCode, body: &Value) -> String {
    body.pointer("/error/message")
        .and_then(Value::as_str)
        .map(str::to_owned)
        .unwrap_or_else(|| format!("API request failed ({status})"))
}

#[cfg(test)]
mod tests {
    use super::*;
    use collector_core::{ArtifactManifestEntry, ConsentProof, RecordingPolicy, VideoSegment};
    use std::fs;
    use std::net::{TcpListener, TcpStream};
    use std::thread;

    type MockRequest = (String, Vec<u8>);
    type MockServer = thread::JoinHandle<Vec<MockRequest>>;

    #[test]
    fn local_http_and_https_are_the_only_supported_api_urls() {
        assert!(validated_api_base("http://127.0.0.1:8080").is_ok());
        assert!(validated_api_base("https://collector.example.test/api").is_ok());
        assert!(validated_api_base("http://collector.example.test").is_err());
        assert!(validated_api_base("file:///tmp/api").is_err());
        assert!(validated_api_base("https://user:secret@example.test").is_err());
    }

    #[test]
    fn unix_milliseconds_are_rendered_as_rfc3339() {
        assert_eq!(unix_ms_iso(0), "1970-01-01T00:00:00.000Z");
        assert_eq!(unix_ms_iso(1_788_739_200_123), "2026-09-07T00:00:00.123Z");
    }

    #[test]
    fn sealed_capture_uploads_and_persists_submission_receipt() {
        let directory = temporary_directory("submit");
        let artifacts = [
            ("video/000001.mp4", b"fixture video".as_slice(), "video/mp4"),
            (
                "events/000001.jsonl",
                br#"{"timestamp_ns":1000000,"application":"fixture.exe","type":"mouse_move","x":12,"y":34}
"#,
                "application/x-ndjson",
            ),
            ("outputs/result.txt", b"fixture result".as_slice(), "text/plain"),
        ];
        let mut manifest_artifacts = Vec::new();
        for (relative, contents, media_type) in artifacts {
            let path = directory.join(relative);
            fs::create_dir_all(path.parent().unwrap()).unwrap();
            fs::write(&path, contents).unwrap();
            manifest_artifacts.push(ArtifactManifestEntry {
                path: relative.into(),
                size: contents.len() as u64,
                sha256: sha256_bytes(contents),
                media_type: media_type.into(),
            });
        }
        let manifest = CaptureManifest {
            schema_version: "capture/v1".into(),
            session_id: "sess_submit".into(),
            task_id: "task_submit".into(),
            client_version: "0.1.0".into(),
            started_at_unix_ms: 1_788_739_200_000,
            duration_ns: 2_000_000_000,
            consent: ConsentProof {
                document_id: "consent_submit".into(),
                version: "v1".into(),
                text_hash: "a".repeat(64),
                accepted_at_unix_ms: 1_788_739_100_000,
            },
            privacy: RecordingPolicy::safe_default("fixture.exe"),
            video_segments: vec![VideoSegment {
                segment_id: "000001".into(),
                path: "video/000001.mp4".into(),
                start_ns: 0,
                end_ns: 2_000_000_000,
                size: b"fixture video".len() as u64,
                sha256: sha256_bytes(b"fixture video"),
            }],
            artifacts: manifest_artifacts,
        };
        fs::write(
            directory.join("manifest.json"),
            serde_json::to_vec_pretty(&manifest).unwrap(),
        )
        .unwrap();

        let (api_base, server) = mock_api(11);
        let receipt = submit(
            &directory,
            SubmissionRequest {
                api_base,
                actor: "contributor_submit".into(),
                session_id: "sess_submit".into(),
                contributor_id: "contributor_submit".into(),
                task: SubmissionTask {
                    task_id: "task_submit".into(),
                    template_id: "template_submit".into(),
                    goal: "Complete the fixture task.".into(),
                    category: "desktop.synthetic".into(),
                    difficulty: "beginner".into(),
                },
                environment: SubmissionEnvironment {
                    os: "windows".into(),
                    os_version: "11".into(),
                    locale: "en-US".into(),
                    timezone: "UTC".into(),
                    display: SubmissionDisplay {
                        width: 1920,
                        height: 1080,
                        scale_factor: 1.0,
                    },
                },
                access_token: None,
            },
        )
        .unwrap();
        let requests = server.join().unwrap();

        assert_eq!(receipt.job_id, "job_submit");
        assert_eq!(receipt.uploaded_artifacts, 5);
        assert!(directory.join("submission.json").exists());
        // Each artifact first probes the resumable endpoint, falls back to a
        // direct upload when the mock returns 501, and the capture is then
        // submitted once: 5 probes + 5 uploads + 1 submission.
        assert_eq!(requests.len(), 11);
        assert!(
            requests
                .iter()
                .any(|(line, _)| { line.starts_with("POST /v1/sessions/sess_submit/submission ") })
        );
        let trajectory = requests
            .iter()
            .find(|(line, _)| {
                line.starts_with("PUT /v1/sessions/sess_submit/artifacts/trajectory.json ")
            })
            .map(|(_, body)| serde_json::from_slice::<Value>(body).unwrap())
            .unwrap();
        assert_eq!(trajectory["schema_version"], "trajectory/v1");
        assert_eq!(trajectory["actions"][0]["type"], "mouse_move");
        assert_eq!(trajectory["privacy"]["clipboard_captured"], false);
        fs::remove_dir_all(directory).unwrap();
    }

    fn mock_api(expected_requests: usize) -> (String, MockServer) {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let address = listener.local_addr().unwrap();
        let server = thread::spawn(move || {
            let mut requests = Vec::new();
            for _ in 0..expected_requests {
                let (mut stream, _) = listener.accept().unwrap();
                let (line, body) = read_request(&mut stream);
                let (status, response) = if line.contains("/multipart-uploads HTTP/") {
                    (
                        "501 Not Implemented",
                        r#"{"error":{"message":"resumable upload unavailable"}}"#,
                    )
                } else if line.starts_with("POST ") {
                    (
                        "202 Accepted",
                        r#"{"job":{"id":"job_submit"},"session":{"state":"SUBMITTED"}}"#,
                    )
                } else {
                    ("201 Created", "{}")
                };
                write!(
                    stream,
                    "HTTP/1.1 {status}\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{response}",
                    response.len()
                )
                .unwrap();
                stream.flush().unwrap();
                requests.push((line, body));
            }
            requests
        });
        (format!("http://{address}"), server)
    }

    fn read_request(stream: &mut TcpStream) -> (String, Vec<u8>) {
        let mut request = Vec::new();
        let mut buffer = [0_u8; 4096];
        let (header_end, content_length) = loop {
            let count = stream.read(&mut buffer).unwrap();
            assert!(count > 0, "request ended before headers were complete");
            request.extend_from_slice(&buffer[..count]);
            if let Some(header_end) = request.windows(4).position(|part| part == b"\r\n\r\n") {
                let header_end = header_end + 4;
                let headers = String::from_utf8_lossy(&request[..header_end]);
                let content_length = headers
                    .lines()
                    .find_map(|line| {
                        line.to_ascii_lowercase()
                            .strip_prefix("content-length: ")
                            .map(str::to_owned)
                    })
                    .and_then(|value| value.parse::<usize>().ok())
                    .unwrap_or(0);
                break (header_end, content_length);
            }
        };
        while request.len() < header_end + content_length {
            let count = stream.read(&mut buffer).unwrap();
            assert!(count > 0, "request body ended early");
            request.extend_from_slice(&buffer[..count]);
        }
        let line = String::from_utf8_lossy(&request[..header_end])
            .lines()
            .next()
            .unwrap()
            .to_string();
        (
            line,
            request[header_end..header_end + content_length].to_vec(),
        )
    }

    fn temporary_directory(label: &str) -> PathBuf {
        let directory = std::env::temp_dir().join(format!(
            "trajectory-submission-{label}-{}-{:?}",
            std::process::id(),
            std::thread::current().id()
        ));
        let _ = fs::remove_dir_all(&directory);
        fs::create_dir_all(&directory).unwrap();
        directory
    }
}
