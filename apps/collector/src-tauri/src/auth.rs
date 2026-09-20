use keyring::Entry;
use openidconnect::core::{CoreAuthenticationFlow, CoreClient, CoreProviderMetadata};
use openidconnect::{
    AccessTokenHash, AuthorizationCode, ClientId, CsrfToken, IssuerUrl, Nonce, OAuth2TokenResponse,
    PkceCodeChallenge, RedirectUrl, RefreshToken, Scope, TokenResponse,
};
use openidconnect::{reqwest as oidc_reqwest, url::Url};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use std::io::{BufRead, BufReader, Read, Write};
use std::net::{IpAddr, TcpListener, TcpStream};
use std::sync::Mutex;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};
use tauri::AppHandle;
use tauri_plugin_opener::OpenerExt;

const KEYRING_SERVICE: &str = "com.datatrains.collector";

#[derive(Clone)]
struct ProductionConfiguration {
    api_base: Url,
    issuer: IssuerUrl,
    client_id: ClientId,
    audience: String,
    connection: Option<String>,
    revocation_url: Url,
}

#[derive(Clone)]
struct TokenSession {
    access_token: String,
    refresh_token: String,
    expires_at_unix_ms: u64,
    profile: AuthProfile,
}

#[derive(Default)]
pub struct AuthRuntime {
    session: Mutex<Option<TokenSession>>,
}

#[derive(Clone, Deserialize, Serialize)]
pub struct AuthProfile {
    pub account_id: String,
    pub display_name: String,
    pub email: Option<String>,
    pub contributor_id: Option<String>,
}

#[derive(Serialize)]
pub struct CollectorConfiguration {
    pub auth_mode: String,
    pub api_base: String,
}

#[derive(Serialize)]
pub struct SignOutResult {
    pub warning: Option<String>,
}

#[derive(Deserialize)]
pub struct AuthenticatedAPIRequest {
    pub method: String,
    pub path: String,
    pub body: Option<Value>,
}

pub fn production_enabled() -> bool {
    configured("DATATRAINS_AUTH_MODE", option_env!("DATATRAINS_AUTH_MODE")).as_deref()
        == Some("oidc")
}

#[tauri::command]
pub fn collector_configuration() -> Result<CollectorConfiguration, String> {
    if !production_enabled() {
        return Ok(CollectorConfiguration {
            auth_mode: "local".into(),
            api_base: "http://127.0.0.1:8080".into(),
        });
    }
    let configuration = production_configuration()?;
    Ok(CollectorConfiguration {
        auth_mode: "oidc".into(),
        api_base: configuration
            .api_base
            .to_string()
            .trim_end_matches('/')
            .into(),
    })
}

#[tauri::command]
pub async fn sign_in(
    app: AppHandle,
    runtime: tauri::State<'_, AuthRuntime>,
) -> Result<AuthProfile, String> {
    let configuration = production_configuration()?;
    let listener = TcpListener::bind(("127.0.0.1", 0))
        .map_err(|error| format!("start secure sign-in callback: {error}"))?;
    let callback = RedirectUrl::new(format!(
        "http://127.0.0.1:{}/callback",
        listener
            .local_addr()
            .map_err(|error| error.to_string())?
            .port()
    ))
    .map_err(|error| format!("build sign-in callback URL: {error}"))?;
    let http_client = oidc_http_client()?;
    let provider = CoreProviderMetadata::discover_async(configuration.issuer.clone(), &http_client)
        .await
        .map_err(|error| format!("discover OpenID provider: {error}"))?;
    let client =
        CoreClient::from_provider_metadata(provider, configuration.client_id.clone(), None)
            .set_redirect_uri(callback);
    let (challenge, verifier) = PkceCodeChallenge::new_random_sha256();
    let authorization_request = client
        .authorize_url(
            CoreAuthenticationFlow::AuthorizationCode,
            CsrfToken::new_random,
            Nonce::new_random,
        )
        .add_scope(Scope::new("profile".into()))
        .add_scope(Scope::new("email".into()))
        .add_scope(Scope::new("offline_access".into()))
        .add_extra_param("audience", configuration.audience.clone())
        .set_pkce_challenge(challenge);
    let authorization_request = match configuration.connection.as_ref() {
        Some(connection) => authorization_request
            .add_extra_param("connection", connection.clone())
            .add_extra_param("prompt", "login"),
        None => authorization_request,
    };
    let (authorization_url, state, nonce) = authorization_request.url();
    app.opener()
        .open_url(authorization_url.as_str(), None::<&str>)
        .map_err(|error| format!("open system browser for sign-in: {error}"))?;

    let callback_url = tauri::async_runtime::spawn_blocking(move || wait_for_callback(listener))
        .await
        .map_err(|error| format!("sign-in callback task failed: {error}"))??;
    let parameters: std::collections::HashMap<_, _> =
        callback_url.query_pairs().into_owned().collect();
    if let Some(provider_error) = parameters.get("error") {
        return Err(format!(
            "OpenID provider rejected sign-in: {provider_error}"
        ));
    }
    let returned_state = CsrfToken::new(
        parameters
            .get("state")
            .cloned()
            .ok_or_else(|| "sign-in callback is missing state".to_string())?,
    );
    if returned_state != state {
        return Err("sign-in callback state did not match".into());
    }
    let code = parameters
        .get("code")
        .cloned()
        .ok_or_else(|| "sign-in callback is missing code".to_string())?;
    let tokens = client
        .exchange_code(AuthorizationCode::new(code))
        .map_err(|error| format!("prepare OpenID token exchange: {error}"))?
        .set_pkce_verifier(verifier)
        .request_async(&http_client)
        .await
        .map_err(|error| format!("exchange OpenID authorization code: {error}"))?;
    let id_token = tokens
        .id_token()
        .ok_or_else(|| "OpenID provider did not return an ID token".to_string())?;
    let verifier = client.id_token_verifier();
    let claims = id_token
        .claims(&verifier, &nonce)
        .map_err(|error| format!("verify OpenID ID token: {error}"))?;
    if let Some(expected_hash) = claims.access_token_hash() {
        let signing_algorithm = id_token
            .signing_alg()
            .map_err(|error| format!("read ID token algorithm: {error}"))?;
        let signing_key = id_token
            .signing_key(&verifier)
            .map_err(|error| format!("read ID token key: {error}"))?;
        let actual_hash =
            AccessTokenHash::from_token(tokens.access_token(), signing_algorithm, signing_key)
                .map_err(|error| format!("verify OpenID access token binding: {error}"))?;
        if actual_hash != *expected_hash {
            return Err("OpenID access token binding did not match".into());
        }
    }
    let refresh_token = tokens
        .refresh_token()
        .map(|token| token.secret().to_owned())
        .ok_or_else(|| {
            "OpenID provider did not return a refresh token; offline_access is required".to_string()
        })?;
    let access_token = tokens.access_token().secret().to_owned();
    let profile = fetch_profile(&configuration, &access_token).await?;
    if profile.contributor_id.is_none() {
        return Err("this signed-in account is not an active DataTrains contributor".into());
    }
    persist_refresh_token(configuration.client_id.as_str(), &refresh_token).await?;
    let session = TokenSession {
        access_token,
        refresh_token,
        expires_at_unix_ms: expiry(tokens.expires_in()),
        profile: profile.clone(),
    };
    *runtime
        .session
        .lock()
        .map_err(|_| "identity state lock is poisoned".to_string())? = Some(session);
    Ok(profile)
}

#[tauri::command]
pub async fn restore_session(
    runtime: tauri::State<'_, AuthRuntime>,
) -> Result<Option<AuthProfile>, String> {
    if !production_enabled() {
        return Ok(None);
    }
    if let Some(profile) = runtime
        .session
        .lock()
        .map_err(|_| "identity state lock is poisoned".to_string())?
        .as_ref()
        .map(|session| session.profile.clone())
    {
        return Ok(Some(profile));
    }
    let configuration = production_configuration()?;
    let Some(refresh_token) = load_refresh_token(configuration.client_id.as_str()).await? else {
        return Ok(None);
    };
    let session = refresh_session(&configuration, refresh_token).await?;
    let profile = session.profile.clone();
    *runtime
        .session
        .lock()
        .map_err(|_| "identity state lock is poisoned".to_string())? = Some(session);
    Ok(Some(profile))
}

#[tauri::command]
pub async fn sign_out(runtime: tauri::State<'_, AuthRuntime>) -> Result<SignOutResult, String> {
    if !production_enabled() {
        return Ok(SignOutResult { warning: None });
    }
    let configuration = production_configuration()?;
    let current = runtime
        .session
        .lock()
        .map_err(|_| "identity state lock is poisoned".to_string())?
        .clone();
    let refresh_token = match current {
        Some(session) => Some(session.refresh_token),
        None => load_refresh_token(configuration.client_id.as_str()).await?,
    };
    let warning = match refresh_token.as_deref() {
        Some(token) => revoke_refresh_token(&configuration, token).await.err(),
        None => None,
    };
    delete_refresh_token(configuration.client_id.as_str()).await?;
    runtime
        .session
        .lock()
        .map_err(|_| "identity state lock is poisoned".to_string())?
        .take();
    Ok(SignOutResult { warning })
}

#[tauri::command]
pub async fn authenticated_api_request(
    runtime: tauri::State<'_, AuthRuntime>,
    request: AuthenticatedAPIRequest,
) -> Result<Value, String> {
    let configuration = production_configuration()?;
    let access_token = access_token(&runtime, &configuration).await?;
    let method = reqwest::Method::from_bytes(request.method.as_bytes())
        .map_err(|_| "unsupported API request method".to_string())?;
    let path = safe_api_path(&method, &request.path)?;
    let mut operation = reqwest::Client::new()
        .request(
            method,
            configuration
                .api_base
                .join(path)
                .map_err(|error| error.to_string())?,
        )
        .bearer_auth(access_token)
        .header("X-Request-ID", uuid_like_request_id());
    if let Some(body) = request.body {
        operation = operation.json(&body);
    }
    let response = operation
        .send()
        .await
        .map_err(|error| format!("contact DataTrains API: {error}"))?;
    let status = response.status();
    let payload: Value = response
        .json()
        .await
        .map_err(|error| format!("decode DataTrains API response: {error}"))?;
    if !status.is_success() {
        return Err(payload
            .pointer("/error/message")
            .and_then(Value::as_str)
            .unwrap_or("DataTrains API request failed")
            .into());
    }
    Ok(payload)
}

async fn access_token(
    runtime: &AuthRuntime,
    configuration: &ProductionConfiguration,
) -> Result<String, String> {
    let current = runtime
        .session
        .lock()
        .map_err(|_| "identity state lock is poisoned".to_string())?
        .clone();
    let Some(session) = current else {
        return Err("sign in is required".into());
    };
    if session.expires_at_unix_ms > unix_ms().saturating_add(60_000) {
        return Ok(session.access_token);
    }
    let refreshed = refresh_session(configuration, session.refresh_token).await?;
    let token = refreshed.access_token.clone();
    *runtime
        .session
        .lock()
        .map_err(|_| "identity state lock is poisoned".to_string())? = Some(refreshed);
    Ok(token)
}

pub async fn access_token_for_submission(runtime: &AuthRuntime) -> Result<String, String> {
    access_token(runtime, &production_configuration()?).await
}

async fn refresh_session(
    configuration: &ProductionConfiguration,
    refresh_token: String,
) -> Result<TokenSession, String> {
    let http_client = oidc_http_client()?;
    let provider = CoreProviderMetadata::discover_async(configuration.issuer.clone(), &http_client)
        .await
        .map_err(|error| format!("discover OpenID provider: {error}"))?;
    let client =
        CoreClient::from_provider_metadata(provider, configuration.client_id.clone(), None);
    let tokens = client
        .exchange_refresh_token(&RefreshToken::new(refresh_token.clone()))
        .map_err(|error| format!("prepare OpenID session refresh: {error}"))?
        .request_async(&http_client)
        .await
        .map_err(|error| format!("refresh OpenID session: {error}"))?;
    let next_refresh = tokens
        .refresh_token()
        .map(|token| token.secret().to_owned())
        .unwrap_or(refresh_token);
    persist_refresh_token(configuration.client_id.as_str(), &next_refresh).await?;
    let access = tokens.access_token().secret().to_owned();
    let profile = fetch_profile(configuration, &access).await?;
    Ok(TokenSession {
        access_token: access,
        refresh_token: next_refresh,
        expires_at_unix_ms: expiry(tokens.expires_in()),
        profile,
    })
}

async fn fetch_profile(
    configuration: &ProductionConfiguration,
    access_token: &str,
) -> Result<AuthProfile, String> {
    let response = reqwest::Client::new()
        .get(
            configuration
                .api_base
                .join("v1/me")
                .map_err(|error| error.to_string())?,
        )
        .bearer_auth(access_token)
        .send()
        .await
        .map_err(|error| format!("load DataTrains contributor profile: {error}"))?;
    let status = response.status();
    let payload: Value = response
        .json()
        .await
        .map_err(|error| format!("decode contributor profile: {error}"))?;
    if !status.is_success() {
        return Err(payload
            .pointer("/error/message")
            .and_then(Value::as_str)
            .unwrap_or("contributor profile is unavailable")
            .into());
    }
    serde_json::from_value(payload).map_err(|error| format!("decode contributor profile: {error}"))
}

fn production_configuration() -> Result<ProductionConfiguration, String> {
    let api = required("DATATRAINS_API_URL", option_env!("DATATRAINS_API_URL"))?;
    let mut api_base =
        Url::parse(&api).map_err(|error| format!("invalid DATATRAINS_API_URL: {error}"))?;
    if api_base.scheme() != "https"
        || api_base.username() != ""
        || api_base.password().is_some()
        || api_base.query().is_some()
        || api_base.fragment().is_some()
    {
        return Err("production DATATRAINS_API_URL must be a clean HTTPS URL".into());
    }
    if !api_base.path().ends_with('/') {
        api_base.set_path(&format!("{}/", api_base.path()));
    }
    let issuer_value = required(
        "DATATRAINS_OIDC_ISSUER",
        option_env!("DATATRAINS_OIDC_ISSUER"),
    )?;
    clean_https_url("DATATRAINS_OIDC_ISSUER", issuer_value.clone())?;
    let issuer = IssuerUrl::new(issuer_value)
        .map_err(|error| format!("invalid DATATRAINS_OIDC_ISSUER: {error}"))?;
    Ok(ProductionConfiguration {
        api_base,
        issuer,
        client_id: ClientId::new(required(
            "DATATRAINS_OIDC_CLIENT_ID",
            option_env!("DATATRAINS_OIDC_CLIENT_ID"),
        )?),
        audience: required(
            "DATATRAINS_OIDC_AUDIENCE",
            option_env!("DATATRAINS_OIDC_AUDIENCE"),
        )?,
        connection: configured(
            "DATATRAINS_OIDC_CONNECTION",
            option_env!("DATATRAINS_OIDC_CONNECTION"),
        )
        .map(validate_connection_name)
        .transpose()?,
        revocation_url: clean_https_url(
            "DATATRAINS_OIDC_REVOCATION_URL",
            required(
                "DATATRAINS_OIDC_REVOCATION_URL",
                option_env!("DATATRAINS_OIDC_REVOCATION_URL"),
            )?,
        )?,
    })
}

fn validate_connection_name(value: String) -> Result<String, String> {
    if value.len() <= 128
        && !value.is_empty()
        && value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-'))
    {
        return Ok(value);
    }
    Err("DATATRAINS_OIDC_CONNECTION must be a valid OpenID connection name".into())
}

fn clean_https_url(name: &str, value: String) -> Result<Url, String> {
    let url = Url::parse(&value).map_err(|error| format!("invalid {name}: {error}"))?;
    if url.scheme() != "https"
        || url.username() != ""
        || url.password().is_some()
        || url.query().is_some()
        || url.fragment().is_some()
    {
        return Err(format!("{name} must be a clean HTTPS URL"));
    }
    Ok(url)
}

fn configured(name: &str, built: Option<&'static str>) -> Option<String> {
    built
        .map(str::to_owned)
        .or_else(|| std::env::var(name).ok())
        .map(|value| value.trim().to_owned())
        .filter(|value| !value.is_empty())
}

fn required(name: &str, built: Option<&'static str>) -> Result<String, String> {
    configured(name, built)
        .ok_or_else(|| format!("{name} is required in production collector builds"))
}

fn oidc_http_client() -> Result<oidc_reqwest::Client, String> {
    oidc_reqwest::ClientBuilder::new()
        .redirect(oidc_reqwest::redirect::Policy::none())
        .connect_timeout(Duration::from_secs(10))
        .timeout(Duration::from_secs(30))
        .build()
        .map_err(|error| format!("create OpenID HTTP client: {error}"))
}

fn wait_for_callback(listener: TcpListener) -> Result<Url, String> {
    listener
        .set_nonblocking(true)
        .map_err(|error| error.to_string())?;
    let deadline = Instant::now() + Duration::from_secs(300);
    while Instant::now() < deadline {
        match listener.accept() {
            Ok((stream, peer)) => {
                if !matches!(peer.ip(), IpAddr::V4(ip) if ip.is_loopback())
                    && !matches!(peer.ip(), IpAddr::V6(ip) if ip.is_loopback())
                {
                    continue;
                }
                if let Some(url) = read_callback(stream)? {
                    return Ok(url);
                }
            }
            Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {
                std::thread::sleep(Duration::from_millis(50))
            }
            Err(error) => return Err(format!("receive sign-in callback: {error}")),
        }
    }
    Err("sign-in timed out after five minutes".into())
}

fn read_callback(mut stream: TcpStream) -> Result<Option<Url>, String> {
    stream
        .set_read_timeout(Some(Duration::from_secs(5)))
        .map_err(|error| error.to_string())?;
    let mut first_line = String::new();
    BufReader::new(stream.try_clone().map_err(|error| error.to_string())?)
        .take(8_193)
        .read_line(&mut first_line)
        .map_err(|error| format!("read sign-in callback: {error}"))?;
    if first_line.len() > 8_192 {
        return Err("sign-in callback request was too large".into());
    }
    let mut parts = first_line.split_whitespace();
    let method = parts.next().unwrap_or_default();
    let target = parts.next().unwrap_or_default();
    if method != "GET" {
        write_browser_response(
            &mut stream,
            "405 Method Not Allowed",
            "Sign-in request was rejected.",
        )?;
        return Ok(None);
    }
    let url = Url::parse(&format!("http://127.0.0.1{target}"))
        .map_err(|error| format!("parse sign-in callback: {error}"))?;
    if url.path() != "/callback" {
        write_browser_response(
            &mut stream,
            "404 Not Found",
            "Return to the DataTrains Collector.",
        )?;
        return Ok(None);
    }
    write_browser_response(
        &mut stream,
        "200 OK",
        "Sign-in received. You can close this browser tab and return to DataTrains.",
    )?;
    Ok(Some(url))
}

fn write_browser_response(
    stream: &mut TcpStream,
    status: &str,
    message: &str,
) -> Result<(), String> {
    let body = format!(
        "<!doctype html><html><head><meta charset=\"utf-8\"><meta http-equiv=\"Content-Security-Policy\" content=\"default-src 'none'; style-src 'unsafe-inline'\"><title>DataTrains sign-in</title></head><body style=\"font:18px system-ui;padding:3rem;background:#0b0d0f;color:#f5f7f8\"><h1>DataTrains</h1><p>{message}</p></body></html>"
    );
    let response = format!(
        "HTTP/1.1 {status}\r\nContent-Type: text/html; charset=utf-8\r\nContent-Length: {}\r\nCache-Control: no-store\r\nConnection: close\r\nX-Content-Type-Options: nosniff\r\n\r\n{body}",
        body.len()
    );
    stream
        .write_all(response.as_bytes())
        .map_err(|error| error.to_string())
}

fn safe_api_path<'a>(method: &reqwest::Method, path: &'a str) -> Result<&'a str, String> {
    if !path.starts_with("/v1/")
        || path.contains("..")
        || path.contains('\\')
        || path.contains('#')
        || path.contains('?')
        || path.contains('%')
        || !path
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'/' | b'_' | b'-'))
    {
        return Err("invalid DataTrains API path".into());
    }
    let segments: Vec<&str> = path.trim_matches('/').split('/').collect();
    let allowed = match (method, segments.as_slice()) {
        (&reqwest::Method::GET, ["v1", "consent-documents", "current"])
        | (&reqwest::Method::POST, ["v1", "consent-acceptances"]) => true,
        (&reqwest::Method::GET, ["v1", "contributors", id, "assignments"])
        | (&reqwest::Method::POST, ["v1", "sessions", id, "preflight"])
        | (&reqwest::Method::POST, ["v1", "sessions", id, "transitions"]) => safe_identifier(id),
        _ => false,
    };
    if !allowed {
        return Err("the collector is not allowed to call this DataTrains API operation".into());
    }
    Ok(path.trim_start_matches('/'))
}

fn safe_identifier(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 128
        && value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-'))
}

fn expiry(lifetime: Option<Duration>) -> u64 {
    unix_ms().saturating_add(
        u64::try_from(lifetime.unwrap_or(Duration::from_secs(300)).as_millis()).unwrap_or(300_000),
    )
}

fn unix_ms() -> u64 {
    u64::try_from(
        SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default()
            .as_millis(),
    )
    .unwrap_or(u64::MAX)
}

fn uuid_like_request_id() -> String {
    CsrfToken::new_random().secret().to_owned()
}

async fn persist_refresh_token(client_id: &str, refresh_token: &str) -> Result<(), String> {
    let client_id = client_id.to_owned();
    let refresh_token = refresh_token.to_owned();
    tauri::async_runtime::spawn_blocking(move || {
        Entry::new(KEYRING_SERVICE, &client_id)
            .and_then(|entry| entry.set_password(&refresh_token))
            .map_err(|error| {
                format!("store session in the operating-system credential vault: {error}")
            })
    })
    .await
    .map_err(|error| error.to_string())?
}

async fn load_refresh_token(client_id: &str) -> Result<Option<String>, String> {
    let client_id = client_id.to_owned();
    tauri::async_runtime::spawn_blocking(move || {
        match Entry::new(KEYRING_SERVICE, &client_id).and_then(|entry| entry.get_password()) {
            Ok(token) => Ok(Some(token)),
            Err(keyring::Error::NoEntry) => Ok(None),
            Err(error) => Err(format!(
                "open session from the operating-system credential vault: {error}"
            )),
        }
    })
    .await
    .map_err(|error| error.to_string())?
}

async fn delete_refresh_token(client_id: &str) -> Result<(), String> {
    let client_id = client_id.to_owned();
    tauri::async_runtime::spawn_blocking(move || {
        match Entry::new(KEYRING_SERVICE, &client_id).and_then(|entry| entry.delete_credential()) {
            Ok(()) | Err(keyring::Error::NoEntry) => Ok(()),
            Err(error) => Err(format!(
                "remove session from the operating-system credential vault: {error}"
            )),
        }
    })
    .await
    .map_err(|error| error.to_string())?
}

async fn revoke_refresh_token(
    configuration: &ProductionConfiguration,
    refresh_token: &str,
) -> Result<(), String> {
    let client = reqwest::ClientBuilder::new()
        .redirect(reqwest::redirect::Policy::none())
        .connect_timeout(Duration::from_secs(10))
        .timeout(Duration::from_secs(30))
        .build()
        .map_err(|error| format!("create token revocation client: {error}"))?;
    let response = client
        .post(configuration.revocation_url.clone())
        .form(&[
            ("token", refresh_token),
            ("token_type_hint", "refresh_token"),
            ("client_id", configuration.client_id.as_str()),
        ])
        .send()
        .await
        .map_err(|error| format!("the provider could not revoke the remote session: {error}"))?;
    if !response.status().is_success() {
        return Err(format!(
            "the provider returned {} while revoking the remote session",
            response.status()
        ));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::safe_api_path;
    use reqwest::Method;

    #[test]
    fn api_paths_are_versioned_relative_and_collector_scoped() {
        assert_eq!(
            safe_api_path(&Method::GET, "/v1/consent-documents/current"),
            Ok("v1/consent-documents/current")
        );
        assert_eq!(
            safe_api_path(&Method::GET, "/v1/contributors/contrib_123/assignments"),
            Ok("v1/contributors/contrib_123/assignments")
        );
        assert_eq!(
            safe_api_path(&Method::POST, "/v1/sessions/sess_123/preflight"),
            Ok("v1/sessions/sess_123/preflight")
        );
        for value in [
            "v1/me",
            "/healthz",
            "/v1/../secret",
            "/v1/a\\b",
            "/v1/me#fragment",
            "/v1/me?debug=true",
            "/v1/%2e%2e/secret",
            "/v1/projects",
            "/v1/reviews/claim",
        ] {
            assert!(
                safe_api_path(&Method::GET, value).is_err(),
                "accepted {value}"
            );
        }
        assert!(safe_api_path(&Method::GET, "/v1/consent-acceptances").is_err());
        assert!(safe_api_path(&Method::POST, "/v1/consent-documents/current").is_err());
    }
}
