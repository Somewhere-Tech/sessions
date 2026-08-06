// Sessions v1 is a native window and tray layer. The v2 lifecycle manager is
// kept separate from this UI code: it may install or kickstart sessionsd, but the
// app process never owns sessionsd or a runner, so quitting it cannot affect a
// durable session.

mod lifecycle;
#[cfg(any(target_os = "windows", test))]
mod windows_cli_path;
mod windows_credentials;
#[cfg(any(target_os = "windows", test))]
mod windows_runner;
#[cfg(target_os = "windows")]
mod windows_runtime;
#[cfg(any(target_os = "windows", test))]
mod windows_supervisor;

use serde::{Deserialize, Serialize};
use serde_json::Value;
#[cfg(desktop)]
use std::fs;
#[cfg(desktop)]
use std::sync::{
    atomic::{AtomicBool, Ordering},
    Arc,
};
use std::{
    collections::HashMap,
    env,
    io::{Read, Write},
    net::IpAddr,
    path::PathBuf,
    process::{Child, Command, ExitStatus, Stdio},
    sync::Mutex,
    thread,
    time::{Duration, Instant},
};
#[cfg(desktop)]
use tauri::{
    menu::{Menu, MenuItem, SubmenuBuilder},
    tray::TrayIconBuilder,
    PhysicalPosition, PhysicalSize, WebviewUrl, WebviewWindow, WebviewWindowBuilder, WindowEvent,
};
use tauri::{AppHandle, Manager};

#[cfg(desktop)]
const TRAY_ID: &str = "sessions-status";
const SOMEWHERE_PACKAGE: &str = "@somewhere-tech/cli";
const SOMEWHERE_LATEST_URL: &str = "https://registry.npmjs.org/@somewhere-tech%2Fcli/latest";
const SUPPORT_TICKET_URL: &str = "https://github.com/Somewhere-Tech/sessions/issues/new/choose";
const SUPPORT_FEEDBACK_URL: &str =
    "https://github.com/Somewhere-Tech/sessions/issues/new?template=feedback.yml";
const SUPPORT_BUG_URL: &str =
    "https://github.com/Somewhere-Tech/sessions/issues/new?template=bug_report.yml";
const SUPPORT_SECURITY_URL: &str =
    "https://github.com/Somewhere-Tech/sessions/security/advisories/new";
const API_PROTOCOL_VERSION: u16 = 1;
// A URL handler either fails fast (unknown scheme, no registered application)
// or takes ownership of the foreground. This is how long we listen for the
// first case before concluding the second.
const EXTERNAL_LAUNCH_TIMEOUT: Duration = Duration::from_secs(5);
// A GUI action that shells out to the bundled CLI must not wait forever: it
// pins a blocking-pool thread and the control that started it. Status, registry
// and discovery calls answer immediately.
const BUNDLED_CLI_TIMEOUT: Duration = Duration::from_secs(120);
// A cross-machine continuation or a first backup legitimately copies a working
// tree over the network, so it gets its own much wider budget.
const BUNDLED_CLI_TRANSFER_TIMEOUT: Duration = Duration::from_secs(30 * 60);
// The CLI's JSON answers are small. Cap what we buffer so a runaway process
// cannot exhaust this process's memory, while still draining its pipes.
const BUNDLED_CLI_MAX_OUTPUT: u64 = 16 * 1024 * 1024;

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
struct TrayServer {
    id: String,
    name: String,
}

#[cfg(desktop)]
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
struct TraySnapshot {
    working: usize,
    idle: usize,
    attention: usize,
    reachable: bool,
}

#[cfg(desktop)]
#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct TraySession {
    working: bool,
    exited: bool,
    exit_code: Option<i32>,
    idle_reason: Option<String>,
    last_user_message_at: Option<i64>,
}

#[cfg(desktop)]
#[derive(Debug, Deserialize)]
struct SessionsResponse {
    sessions: Vec<TraySession>,
}

#[cfg(desktop)]
#[derive(Clone, Debug)]
struct WindowSpec {
    label: String,
    query: String,
    title: String,
    width: f64,
    height: f64,
}

#[cfg(desktop)]
#[derive(Default)]
struct TrayState {
    servers: Mutex<Vec<TrayServer>>,
    snapshot: Mutex<TraySnapshot>,
    server_targets: Mutex<HashMap<String, WindowSpec>>,
}

#[cfg(mobile)]
#[derive(Default)]
struct TrayState;

struct RuntimeState {
    status: Mutex<lifecycle::RuntimeStatus>,
    port: Mutex<u16>,
}

#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct NativeConnectionSettings {
    port: u16,
    runtime: lifecycle::RuntimeStatus,
}

#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct NativeConnectionCommand {
    data: Value,
    detail: String,
}

use windows_credentials::{MachineCredential, MachineCredentialStore};

#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct NativePairingClaim {
    endpoint: String,
    machine_id: String,
    machine_name: String,
    device_id: String,
    token: String,
    name: String,
}

#[derive(Clone, Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
struct NativeAgentMachine {
    alias: Option<String>,
    machine_id: String,
    name: String,
    endpoint: String,
    device_id: Option<String>,
    token: String,
}

#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct NativeTailnetPeer {
    endpoint: String,
    hostname: String,
    os: String,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
struct NativeNearbyPeer {
    endpoint: String,
    hostname: String,
    name: String,
    address: String,
    port: u16,
    transport: String,
    version: String,
    os: String,
    arch: String,
    #[serde(rename = "sessions_loaded")]
    sessions_loaded: usize,
    reachable: bool,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "PascalCase")]
struct TailscalePeer {
    #[serde(default)]
    host_name: String,
    #[serde(default)]
    dns_name: String,
    #[serde(default)]
    os: String,
    #[serde(default)]
    online: bool,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "PascalCase")]
struct NativeTailscaleStatus {
    backend_state: String,
    #[serde(default)]
    peer: HashMap<String, TailscalePeer>,
}

#[derive(Debug, Deserialize)]
struct TailnetRequestResponse {
    request_id: String,
    request_secret: String,
    expires_at: String,
    status: String,
}

#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct NativeTailnetRequest {
    endpoint: String,
    request_id: String,
    request_secret: String,
    expires_at: String,
    status: String,
}

#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct NativeTailnetClaim {
    status: String,
    claim: Option<NativePairingClaim>,
}

#[derive(Debug, Deserialize)]
struct PairingClaimResponse {
    machine_id: Option<String>,
    machine_name: Option<String>,
    device_id: String,
    token: String,
    name: String,
}

#[derive(Debug, Deserialize)]
struct SessionsHealthResponse {
    ok: bool,
    name: String,
    compatibility: Option<SessionsCompatibility>,
}

#[derive(Debug, Deserialize)]
struct SessionsCompatibility {
    api: SessionsApiCompatibility,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct SessionsApiCompatibility {
    minimum_client: u16,
    maximum_client: u16,
}

impl SessionsHealthResponse {
    fn accepts_this_client(&self) -> bool {
        self.compatibility.as_ref().map_or(true, |compatibility| {
            API_PROTOCOL_VERSION >= compatibility.api.minimum_client
                && API_PROTOCOL_VERSION <= compatibility.api.maximum_client
        })
    }
}

#[derive(Debug, PartialEq, Eq)]
struct ParsedPairingLink {
    endpoint: String,
    claim_url: String,
    ticket: String,
}

#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct SomewhereCliStatus {
    installed: bool,
    installed_version: Option<String>,
    latest_version: Option<String>,
    update_available: bool,
    install_command: String,
    update_command: String,
    detail: String,
}

#[cfg(desktop)]
#[derive(Clone, Debug, Deserialize, PartialEq, Eq, Serialize)]
struct WindowBounds {
    x: i32,
    y: i32,
    width: u32,
    height: u32,
    maximized: bool,
}

// Dragging a window emits Moved/Resized tens to hundreds of times per second.
// Coalesce that burst into a single write instead of re-serializing the whole
// map and rewriting the file on every event.
#[cfg(desktop)]
const WINDOW_GEOMETRY_FLUSH_DELAY: Duration = Duration::from_millis(500);

#[cfg(desktop)]
struct WindowGeometryFile {
    path: PathBuf,
    bounds: Mutex<HashMap<String, WindowBounds>>,
    // True while a debounced writer is already armed for the current burst.
    flush_pending: AtomicBool,
}

#[cfg(desktop)]
impl WindowGeometryFile {
    fn write(&self) {
        // Serialize under the lock, then release it before touching the disk.
        // The lock is taken from the window-event handler, so a slow or full
        // volume must never be waited on while holding it.
        let encoded = {
            let Ok(bounds) = self.bounds.lock() else {
                return;
            };
            serde_json::to_vec_pretty(&*bounds)
        };
        let Ok(encoded) = encoded else {
            return;
        };
        if let Some(parent) = self.path.parent() {
            if let Err(error) = fs::create_dir_all(parent) {
                log::warn!(
                    "create window geometry directory {}: {error}",
                    parent.display()
                );
                return;
            }
        }
        // Temp-plus-rename: a crash or a power loss partway through a plain
        // fs::write truncates this file and loses every remembered window.
        if let Err(error) = lifecycle::write_atomic(&self.path, &encoded, 0o600) {
            log::warn!("save window geometry: {error}");
        }
    }
}

#[cfg(desktop)]
struct WindowGeometryStore {
    file: Arc<WindowGeometryFile>,
}

#[cfg(desktop)]
impl WindowGeometryStore {
    fn load(path: PathBuf) -> Self {
        let bounds = fs::read(&path)
            .ok()
            .and_then(|bytes| serde_json::from_slice(&bytes).ok())
            .unwrap_or_default();
        Self {
            file: Arc::new(WindowGeometryFile {
                path,
                bounds: Mutex::new(bounds),
                flush_pending: AtomicBool::new(false),
            }),
        }
    }

    fn get(&self, label: &str) -> Option<WindowBounds> {
        self.file.bounds.lock().ok()?.get(label).cloned()
    }

    fn remember(&self, label: String, bounds: WindowBounds) {
        {
            let Ok(mut all_bounds) = self.file.bounds.lock() else {
                return;
            };
            if all_bounds.get(&label) == Some(&bounds) {
                return;
            }
            all_bounds.insert(label, bounds);
        }
        // The first change of a burst arms one writer; every later change
        // during the window is free and is picked up when that writer wakes.
        if self.file.flush_pending.swap(true, Ordering::SeqCst) {
            return;
        }
        let file = Arc::clone(&self.file);
        thread::spawn(move || {
            thread::sleep(WINDOW_GEOMETRY_FLUSH_DELAY);
            // Clear before writing, so an event that lands mid-write arms a
            // fresh flush instead of being dropped on the floor.
            file.flush_pending.store(false, Ordering::SeqCst);
            file.write();
        });
    }

    // Closing a window or quitting right after a drag must still persist the
    // final position: the debounced writer may well still be sleeping.
    fn flush(&self) {
        self.file.flush_pending.store(false, Ordering::SeqCst);
        self.file.write();
    }
}

#[cfg(desktop)]
fn stable_label_part(value: &str) -> String {
    let cleaned: String = value
        .chars()
        .map(|ch| {
            if ch.is_ascii_alphanumeric() || matches!(ch, '-' | '_' | '.') {
                ch
            } else {
                '-'
            }
        })
        .collect();
    let trimmed = cleaned.trim_matches('-');
    if trimmed.is_empty() {
        "scope".to_string()
    } else {
        trimmed.chars().take(80).collect()
    }
}

#[cfg(desktop)]
fn parse_scoped_window(query: &str, title: String) -> Result<WindowSpec, String> {
    let pairs: Vec<(String, String)> =
        url::form_urlencoded::parse(query.trim().trim_start_matches('?').as_bytes())
            .map(|(key, value)| (key.into_owned(), value.into_owned()))
            .collect();

    let title = if title.trim().is_empty() {
        "Sessions".to_string()
    } else {
        title
    };

    if pairs.len() == 1 && pairs[0].0 == "server" && !pairs[0].1.trim().is_empty() {
        let id = pairs[0].1.trim();
        let query = url::form_urlencoded::Serializer::new(String::new())
            .append_pair("server", id)
            .finish();
        return Ok(WindowSpec {
            label: format!("win-server-{}", stable_label_part(id)),
            query,
            title,
            width: 1100.0,
            height: 760.0,
        });
    }

    if pairs.len() == 1 && pairs[0].0 == "tool" {
        let tool = pairs[0].1.as_str();
        if matches!(tool, "codex" | "claude" | "shell") {
            let query = url::form_urlencoded::Serializer::new(String::new())
                .append_pair("tool", tool)
                .finish();
            return Ok(WindowSpec {
                label: format!("win-tool-{tool}"),
                query,
                title,
                width: 1100.0,
                height: 760.0,
            });
        }
    }

    if pairs.len() == 2 {
        let session_id = pairs
            .iter()
            .find_map(|(key, value)| (key == "session").then_some(value.as_str()));
        let single = pairs
            .iter()
            .any(|(key, value)| key == "mode" && value == "single");
        if let Some(session_id) = session_id.filter(|id| !id.trim().is_empty()) {
            if single {
                let query = url::form_urlencoded::Serializer::new(String::new())
                    .append_pair("session", session_id.trim())
                    .append_pair("mode", "single")
                    .finish();
                return Ok(WindowSpec {
                    label: format!("win-session-{}", stable_label_part(session_id)),
                    query,
                    title,
                    width: 900.0,
                    height: 700.0,
                });
            }
        }
    }

    Err(
        "scope must be server=<id>, tool=codex|claude|shell, or session=<id>&mode=single"
            .to_string(),
    )
}

#[cfg(desktop)]
fn main_window_spec() -> WindowSpec {
    WindowSpec {
        label: "main".to_string(),
        query: String::new(),
        title: "Sessions".to_string(),
        width: 1200.0,
        height: 800.0,
    }
}

#[cfg(desktop)]
fn focus_window(window: &WebviewWindow) -> Result<(), String> {
    window.show().map_err(|error| error.to_string())?;
    window.unminimize().map_err(|error| error.to_string())?;
    window.set_focus().map_err(|error| error.to_string())
}

#[cfg(desktop)]
fn restore_window(window: &WebviewWindow) {
    let Some(saved) = window
        .app_handle()
        .state::<WindowGeometryStore>()
        .get(window.label())
    else {
        return;
    };
    if saved.width >= 400 && saved.height >= 300 {
        let _ = window.set_size(PhysicalSize::new(saved.width, saved.height));
    }
    let _ = window.set_position(PhysicalPosition::new(saved.x, saved.y));
    if saved.maximized {
        let _ = window.maximize();
    }
}

#[cfg(desktop)]
fn remember_window(window: &WebviewWindow) {
    let (Ok(position), Ok(size), Ok(maximized)) = (
        window.outer_position(),
        window.outer_size(),
        window.is_maximized(),
    ) else {
        return;
    };
    if size.width < 400 || size.height < 300 {
        return;
    }
    window.app_handle().state::<WindowGeometryStore>().remember(
        window.label().to_string(),
        WindowBounds {
            x: position.x,
            y: position.y,
            width: size.width,
            height: size.height,
            maximized,
        },
    );
}

#[cfg(desktop)]
fn track_window(window: &WebviewWindow) {
    let tracked = window.clone();
    window.on_window_event(move |event| match event {
        WindowEvent::Moved(_) | WindowEvent::Resized(_) => remember_window(&tracked),
        // Do not let the debounce outlive the window it is remembering.
        // try_state, not state: Destroyed can arrive during teardown, and a
        // panic in a window-event handler is not worth a saved position.
        WindowEvent::CloseRequested { .. } | WindowEvent::Destroyed => {
            if let Some(store) = tracked.app_handle().try_state::<WindowGeometryStore>() {
                store.flush();
            }
        }
        _ => {}
    });
}

#[cfg(desktop)]
fn open_window(app: &AppHandle, spec: WindowSpec) -> Result<(), String> {
    if let Some(existing) = app.get_webview_window(&spec.label) {
        return focus_window(&existing);
    }

    let path = if spec.query.is_empty() {
        "index.html".to_string()
    } else {
        format!("index.html?{}", spec.query)
    };
    let window = WebviewWindowBuilder::new(app, &spec.label, WebviewUrl::App(path.into()))
        .title(&spec.title)
        .inner_size(spec.width, spec.height)
        .resizable(true)
        .build()
        .map_err(|error| error.to_string())?;
    restore_window(&window);
    track_window(&window);
    focus_window(&window)
}

#[tauri::command]
#[cfg(desktop)]
fn open_scoped_window(app: AppHandle, query: String, title: String) -> Result<(), String> {
    open_window(&app, parse_scoped_window(&query, title)?)
}

#[tauri::command]
#[cfg(mobile)]
fn open_scoped_window(_app: AppHandle, _query: String, _title: String) -> Result<(), String> {
    Err("separate session windows are not available on mobile".to_string())
}

#[tauri::command]
fn set_tray_servers(app: AppHandle, servers: Vec<TrayServer>) -> Result<(), String> {
    #[cfg(mobile)]
    {
        let _ = app;
        let _ = servers;
        return Ok(());
    }

    #[cfg(desktop)]
    {
        if servers.len() > 100 {
            return Err("too many configured servers".to_string());
        }
        let servers: Vec<TrayServer> = servers
            .into_iter()
            .filter_map(|server| {
                let id = server.id.trim();
                if id.is_empty() {
                    return None;
                }
                let name = server.name.trim();
                Some(TrayServer {
                    id: id.to_string(),
                    name: if name.is_empty() { id } else { name }.to_string(),
                })
            })
            .collect();
        *app.state::<TrayState>()
            .servers
            .lock()
            .map_err(|e| e.to_string())? = servers;

        let app_for_menu = app.clone();
        app.run_on_main_thread(move || {
            if let Err(error) = refresh_tray(&app_for_menu) {
                log::warn!("update tray servers: {error}");
            }
        })
        .map_err(|error| error.to_string())
    }
}

#[tauri::command]
fn runtime_status(app: AppHandle) -> Result<lifecycle::RuntimeStatus, String> {
    app.state::<RuntimeState>()
        .status
        .lock()
        .map(|status| status.clone())
        .map_err(|error| error.to_string())
}

// Reconcile the local service without touching runner lifetimes. The
// lifecycle installer repairs or restarts sessionsd; existing runners remain
// independently supervised and are re-adopted by the daemon when it returns.
#[tauri::command]
async fn recover_runtime(app: AppHandle) -> Result<lifecycle::RuntimeStatus, String> {
    let worker = app.clone();
    let status = tauri::async_runtime::spawn_blocking(move || lifecycle::install_for_app(&worker))
        .await
        .map_err(|error| format!("runtime recovery worker failed: {error}"))?;
    *app.state::<RuntimeState>()
        .status
        .lock()
        .map_err(|error| error.to_string())? = status.clone();
    Ok(status)
}

#[tauri::command]
fn native_connection_settings(app: AppHandle) -> Result<NativeConnectionSettings, String> {
    let state = app.state::<RuntimeState>();
    let port = *state.port.lock().map_err(|error| error.to_string())?;
    let runtime = state
        .status
        .lock()
        .map_err(|error| error.to_string())?
        .clone();
    Ok(NativeConnectionSettings { port, runtime })
}

#[tauri::command]
async fn set_runtime_port(app: AppHandle, port: u16) -> Result<NativeConnectionSettings, String> {
    let worker = app.clone();
    let status =
        tauri::async_runtime::spawn_blocking(move || lifecycle::reconfigure_port(&worker, port))
            .await
            .map_err(|error| format!("port-change worker failed: {error}"))??;
    {
        let state = app.state::<RuntimeState>();
        *state.port.lock().map_err(|error| error.to_string())? = port;
        *state.status.lock().map_err(|error| error.to_string())? = status.clone();
    }
    let app_for_menu = app.clone();
    app.run_on_main_thread(move || {
        if let Err(error) = refresh_tray(&app_for_menu) {
            log::warn!("refresh tray after port change: {error}");
        }
    })
    .map_err(|error| error.to_string())?;
    Ok(NativeConnectionSettings {
        port,
        runtime: status,
    })
}

#[tauri::command]
async fn native_connection_action(
    app: AppHandle,
    kind: String,
    action: String,
    name: Option<String>,
) -> Result<NativeConnectionCommand, String> {
    tauri::async_runtime::spawn_blocking(move || {
        run_connection_action(&app, &kind, &action, name.as_deref())
    })
    .await
    .map_err(|error| format!("connection worker failed: {error}"))?
}

#[tauri::command]
async fn native_pairing_claim(pair_url: String) -> Result<NativePairingClaim, String> {
    tauri::async_runtime::spawn_blocking(move || claim_native_pairing_link(&pair_url))
        .await
        .map_err(|error| format!("pairing worker failed: {error}"))?
}

#[tauri::command]
async fn native_tailnet_discover() -> Result<Vec<NativeTailnetPeer>, String> {
    tauri::async_runtime::spawn_blocking(discover_tailnet_peers)
        .await
        .map_err(|error| format!("tailnet discovery worker failed: {error}"))?
}

#[tauri::command]
async fn native_tailnet_request(
    endpoint: String,
    client_id: String,
    name: String,
) -> Result<NativeTailnetRequest, String> {
    tauri::async_runtime::spawn_blocking(move || {
        request_tailnet_access(&endpoint, &client_id, &name)
    })
    .await
    .map_err(|error| format!("tailnet access worker failed: {error}"))?
}

#[tauri::command]
async fn native_tailnet_claim(
    endpoint: String,
    request_id: String,
    request_secret: String,
) -> Result<NativeTailnetClaim, String> {
    tauri::async_runtime::spawn_blocking(move || {
        claim_tailnet_access(&endpoint, &request_id, &request_secret)
    })
    .await
    .map_err(|error| format!("tailnet claim worker failed: {error}"))?
}

#[tauri::command]
async fn native_nearby_discover(app: AppHandle) -> Result<Vec<NativeNearbyPeer>, String> {
    #[cfg(not(target_os = "macos"))]
    {
        let _ = app;
        return Err(
            "Nearby discovery is currently available in Sessions for macOS; use Tailscale on this platform."
                .to_string(),
        );
    }

    #[cfg(target_os = "macos")]
    tauri::async_runtime::spawn_blocking(move || {
        let result = run_bundled_sessions_json(
            &app,
            &[
                "machines".to_string(),
                "discover".to_string(),
                "--timeout".to_string(),
                "3s".to_string(),
            ],
            "nearby discovery",
        )?;
        let machines = result
            .data
            .get("machines")
            .cloned()
            .ok_or_else(|| "Sessions returned invalid nearby discovery data".to_string())?;
        serde_json::from_value(machines)
            .map_err(|error| format!("Sessions returned invalid nearby machines: {error}"))
    })
    .await
    .map_err(|error| format!("nearby discovery worker failed: {error}"))?
}

#[tauri::command]
async fn native_nearby_request(
    endpoint: String,
    client_id: String,
    name: String,
) -> Result<NativeTailnetRequest, String> {
    tauri::async_runtime::spawn_blocking(move || {
        request_nearby_access(&endpoint, &client_id, &name)
    })
    .await
    .map_err(|error| format!("nearby access worker failed: {error}"))?
}

#[tauri::command]
async fn native_nearby_claim(
    endpoint: String,
    request_id: String,
    request_secret: String,
) -> Result<NativeTailnetClaim, String> {
    tauri::async_runtime::spawn_blocking(move || {
        claim_nearby_access(&endpoint, &request_id, &request_secret)
    })
    .await
    .map_err(|error| format!("nearby claim worker failed: {error}"))?
}

#[tauri::command]
async fn native_backup_action(
    app: AppHandle,
    action: String,
    project: Option<String>,
) -> Result<NativeConnectionCommand, String> {
    tauri::async_runtime::spawn_blocking(move || {
        run_backup_action(&app, &action, project.as_deref())
    })
    .await
    .map_err(|error| format!("backup worker failed: {error}"))?
}

#[tauri::command]
async fn native_support_preview(app: AppHandle) -> Result<NativeConnectionCommand, String> {
    tauri::async_runtime::spawn_blocking(move || {
        run_bundled_sessions_json(
            &app,
            &["support".to_string(), "--diagnostics".to_string()],
            "support",
        )
    })
    .await
    .map_err(|error| format!("support preview worker failed: {error}"))?
}

#[tauri::command]
async fn native_move_machines(app: AppHandle) -> Result<NativeConnectionCommand, String> {
    tauri::async_runtime::spawn_blocking(move || {
        run_bundled_sessions_json(&app, &["machines".to_string()], "saved machines")
    })
    .await
    .map_err(|error| format!("saved-machine worker failed: {error}"))?
}

#[tauri::command]
async fn native_agent_machines_sync(
    app: AppHandle,
    machines: Vec<NativeAgentMachine>,
) -> Result<NativeConnectionCommand, String> {
    #[cfg(mobile)]
    {
        let _ = app;
        let _ = machines;
        return Ok(NativeConnectionCommand {
            data: serde_json::json!({ "machines": [] }),
            detail: String::new(),
        });
    }

    #[cfg(desktop)]
    tauri::async_runtime::spawn_blocking(move || {
        let input = serde_json::to_vec(&serde_json::json!({ "machines": machines }))
            .map_err(|error| format!("encode native machine registry: {error}"))?;
        run_bundled_sessions_json_with_input(
            &app,
            &["machines".to_string(), "sync-native".to_string()],
            "agent machine sync",
            Some(&input),
            BUNDLED_CLI_TIMEOUT,
        )
    })
    .await
    .map_err(|error| format!("agent machine sync worker failed: {error}"))?
}

#[tauri::command]
async fn native_move_session(
    app: AppHandle,
    session_id: String,
    machine: String,
    source_machine: Option<String>,
    dry_run: bool,
    allow_dirty: bool,
    runtime_mode: String,
) -> Result<NativeConnectionCommand, String> {
    let local_port = *app
        .state::<RuntimeState>()
        .port
        .lock()
        .map_err(|error| error.to_string())?;
    tauri::async_runtime::spawn_blocking(move || {
        let session_id = session_id.trim();
        let machine = machine.trim();
        if session_id.is_empty() || machine.is_empty() {
            return Err("session id and saved machine are required".to_string());
        }
        if session_id.chars().any(char::is_control) || machine.chars().any(char::is_control) {
            return Err(
                "session id and saved machine must not contain control characters".to_string(),
            );
        }
        let source_machine = source_machine.unwrap_or_default();
        if source_machine.chars().any(char::is_control) {
            return Err("source machine must not contain control characters".to_string());
        }
        let mut args = Vec::new();
        if !source_machine.trim().is_empty() {
            args.extend(["--machine".to_string(), source_machine.trim().to_string()]);
        }
        args.extend(["move".to_string(), session_id.to_string()]);
        if machine == "__local__" {
            args.extend(["--to".to_string(), format!("http://127.0.0.1:{local_port}")]);
        } else {
            args.extend(["--machine".to_string(), machine.to_string()]);
        }
        if dry_run {
            args.push("--dry-run".to_string());
        }
        if allow_dirty {
            args.push("--allow-dirty".to_string());
        }
        if runtime_mode == "terminal" {
            args.push("--terminal".to_string());
        } else if runtime_mode != "rich" {
            return Err("runtime mode must be rich or terminal".to_string());
        }
        // A continuation copies a working tree between machines; hold it to the
        // transfer budget rather than the interactive one.
        run_bundled_sessions_json_with_timeout(
            &app,
            &args,
            "cross-machine continuation",
            BUNDLED_CLI_TRANSFER_TIMEOUT,
        )
    })
    .await
    .map_err(|error| format!("cross-machine continuation worker failed: {error}"))?
}

#[tauri::command]
async fn native_machine_credentials_load(app: AppHandle) -> Result<MachineCredentialStore, String> {
    #[cfg(target_os = "windows")]
    {
        return tauri::async_runtime::spawn_blocking(move || {
            let local_data = app
                .path()
                .app_local_data_dir()
                .map_err(|error| format!("locate the Windows app data directory: {error}"))?;
            windows_credentials::load(&windows_credentials::vault_path(local_data))
        })
        .await
        .map_err(|error| format!("Windows credential worker failed: {error}"))?;
    }

    #[cfg(not(target_os = "windows"))]
    {
        let _ = app;
        Ok(MachineCredentialStore::unsupported())
    }
}

#[tauri::command]
async fn native_machine_credentials_save(
    app: AppHandle,
    credentials: Vec<MachineCredential>,
) -> Result<MachineCredentialStore, String> {
    #[cfg(target_os = "windows")]
    {
        return tauri::async_runtime::spawn_blocking(move || {
            let local_data = app
                .path()
                .app_local_data_dir()
                .map_err(|error| format!("locate the Windows app data directory: {error}"))?;
            windows_credentials::save(&windows_credentials::vault_path(local_data), credentials)
        })
        .await
        .map_err(|error| format!("Windows credential worker failed: {error}"))?;
    }

    #[cfg(not(target_os = "windows"))]
    {
        let _ = app;
        let _ = credentials;
        Ok(MachineCredentialStore::unsupported())
    }
}

fn support_page_url(kind: &str) -> Result<&'static str, String> {
    match kind {
        "choose" => Ok(SUPPORT_TICKET_URL),
        "feedback" => Ok(SUPPORT_FEEDBACK_URL),
        "bug" => Ok(SUPPORT_BUG_URL),
        "security" => Ok(SUPPORT_SECURITY_URL),
        _ => Err("unsupported support page".to_string()),
    }
}

fn validate_external_url(value: &str) -> Result<url::Url, String> {
    let value = value.trim();
    if value.is_empty() || value.len() > 8192 || value.chars().any(char::is_control) {
        return Err("invalid external URL".to_string());
    }
    let parsed = url::Url::parse(value).map_err(|_| "invalid external URL".to_string())?;
    match parsed.scheme() {
        "http" | "https" if parsed.host_str().is_some() => Ok(parsed),
        "mailto" if !parsed.path().is_empty() => Ok(parsed),
        "vscode" if !parsed.path().is_empty() => Ok(parsed),
        _ => Err("unsupported external URL scheme".to_string()),
    }
}

fn open_external_target(url: &str) -> Result<(), String> {
    let parsed = validate_external_url(url)?;
    let target = parsed.as_str();
    #[cfg(target_os = "macos")]
    let mut command = {
        let mut command = Command::new("/usr/bin/open");
        command.arg(target);
        command
    };
    #[cfg(target_os = "windows")]
    let mut command = {
        let mut command = Command::new("rundll32.exe");
        command.args(["url.dll,FileProtocolHandler", target]);
        command
    };
    #[cfg(all(not(target_os = "macos"), not(target_os = "windows")))]
    let mut command = {
        let mut command = Command::new("xdg-open");
        command.arg(target);
        command
    };
    // Never adopt the launched application's lifetime. `/usr/bin/open` returns
    // as soon as LaunchServices accepts the request, but a cold `vscode://`
    // handler can take seconds and several xdg-open backends stay in the
    // foreground for the entire life of the browser they started. Give the
    // launcher a bounded window to report a real failure (no handler for the
    // scheme, bad arguments) and hand it to a reaper if it outlives that
    // window. Output stays inherited so the launcher's own diagnostics still
    // reach the app log; only stdin is closed, because a detached process must
    // never end up waiting on this process's input.
    command.stdin(Stdio::null());
    let mut child = command
        .spawn()
        .map_err(|error| format!("open external URL: {error}"))?;
    match wait_bounded(&mut child, EXTERNAL_LAUNCH_TIMEOUT) {
        Ok(Some(status)) if status.success() => Ok(()),
        Ok(Some(status)) => Err(format!("open external URL failed with {status}")),
        // Still running means the handoff happened and the handler simply owns
        // the foreground now. That is a successful open, not an unknown state.
        Ok(None) => {
            reap_in_background(child);
            Ok(())
        }
        Err(error) => {
            reap_in_background(child);
            Err(format!("open external URL: {error}"))
        }
    }
}

// Wait for a child up to `timeout`. Ok(None) means it is still running; the
// caller decides whether that is success or a failure, because "still running"
// means opposite things for a URL handler and for a CLI we need output from.
fn wait_bounded(child: &mut Child, timeout: Duration) -> std::io::Result<Option<ExitStatus>> {
    let deadline = Instant::now() + timeout;
    loop {
        if let Some(status) = child.try_wait()? {
            return Ok(Some(status));
        }
        let remaining = deadline.saturating_duration_since(Instant::now());
        if remaining.is_zero() {
            return Ok(None);
        }
        thread::sleep(remaining.min(Duration::from_millis(25)));
    }
}

// Child::drop does not wait, so anything we stop waiting for would stay a
// zombie for the life of the app. One short-lived thread owns the final wait.
fn reap_in_background(mut child: Child) {
    thread::spawn(move || {
        let _ = child.wait();
    });
}

#[tauri::command]
async fn open_external_url(url: String) -> Result<(), String> {
    // Sync commands run on Tauri v2's main thread, where any wait freezes every
    // webview. Follow the same spawn_blocking pattern as the other commands.
    tauri::async_runtime::spawn_blocking(move || open_external_target(&url))
        .await
        .map_err(|error| format!("external link worker failed: {error}"))?
}

#[tauri::command]
async fn open_support_page(kind: String) -> Result<(), String> {
    tauri::async_runtime::spawn_blocking(move || {
        let url = support_page_url(kind.trim())?;
        open_external_target(url)
    })
    .await
    .map_err(|error| format!("support page worker failed: {error}"))?
}

#[tauri::command]
async fn somewhere_cli_status() -> Result<SomewhereCliStatus, String> {
    tauri::async_runtime::spawn_blocking(inspect_somewhere_cli)
        .await
        .map_err(|error| format!("Somewhere CLI status worker failed: {error}"))
}

fn inspect_somewhere_cli() -> SomewhereCliStatus {
    let install_command = format!("npm install -g {SOMEWHERE_PACKAGE}");
    let update_command = "somewhere update".to_string();
    let Some(executable) = find_executable("somewhere") else {
        return SomewhereCliStatus {
            installed: false,
            installed_version: None,
            latest_version: fetch_somewhere_latest(),
            update_available: false,
            install_command,
            update_command,
            detail: "Somewhere CLI is not installed".to_string(),
        };
    };
    let installed_version = Command::new(executable)
        .arg("--version")
        .output()
        .ok()
        .filter(|output| output.status.success())
        .and_then(|output| clean_version(&String::from_utf8_lossy(&output.stdout)));
    let latest_version = fetch_somewhere_latest();
    let update_available = installed_version
        .as_deref()
        .zip(latest_version.as_deref())
        .is_some_and(|(installed, latest)| version_is_newer(latest, installed));
    let detail = match (&installed_version, &latest_version, update_available) {
        (Some(installed), Some(latest), true) => {
            format!("Somewhere CLI {installed} is installed; {latest} is available")
        }
        (Some(installed), Some(_), false) => {
            format!("Somewhere CLI {installed} is installed and up to date")
        }
        (Some(installed), None, _) => {
            format!("Somewhere CLI {installed} is installed; update check unavailable")
        }
        (None, _, _) => "Somewhere CLI was found but did not report a valid version".to_string(),
    };
    SomewhereCliStatus {
        installed: true,
        installed_version,
        latest_version,
        update_available,
        install_command,
        update_command,
        detail,
    }
}

fn find_executable(name: &str) -> Option<PathBuf> {
    if let Some(path) = env::var_os("PATH") {
        for directory in env::split_paths(&path) {
            let candidate = directory.join(name);
            if candidate.is_file() {
                return Some(candidate);
            }
            #[cfg(target_os = "windows")]
            {
                let candidate = directory.join(format!("{name}.exe"));
                if candidate.is_file() {
                    return Some(candidate);
                }
            }
        }
    }
    let home = env::var_os("HOME").map(PathBuf::from);
    [
        Some(PathBuf::from("/opt/homebrew/bin").join(name)),
        Some(PathBuf::from("/usr/local/bin").join(name)),
        home.map(|directory| directory.join(".local/bin").join(name)),
    ]
    .into_iter()
    .flatten()
    .find(|candidate| candidate.is_file())
}

fn tailscale_executable() -> Option<PathBuf> {
    find_executable("tailscale").or_else(|| {
        #[cfg(target_os = "macos")]
        {
            let app_binary = PathBuf::from("/Applications/Tailscale.app/Contents/MacOS/Tailscale");
            app_binary.is_file().then_some(app_binary)
        }
        #[cfg(target_os = "windows")]
        {
            return ["ProgramFiles", "LOCALAPPDATA"]
                .into_iter()
                .filter_map(env::var_os)
                .map(PathBuf::from)
                .map(|directory| directory.join("Tailscale").join("tailscale.exe"))
                .find(|candidate| candidate.is_file());
        }
        #[cfg(all(not(target_os = "macos"), not(target_os = "windows")))]
        {
            None
        }
    })
}

fn fetch_somewhere_latest() -> Option<String> {
    let client = reqwest::blocking::Client::builder()
        .connect_timeout(Duration::from_secs(2))
        .timeout(Duration::from_secs(4))
        .build()
        .ok()?;
    let response = client
        .get(SOMEWHERE_LATEST_URL)
        .header("accept", "application/json")
        .send()
        .ok()?
        .error_for_status()
        .ok()?;
    let body = response.json::<Value>().ok()?;
    clean_version(body.get("version")?.as_str()?)
}

fn clean_version(value: &str) -> Option<String> {
    let version = value.lines().next()?.trim().trim_start_matches('v');
    if version.is_empty()
        || version.len() > 40
        || !version.chars().all(|character| {
            character.is_ascii_alphanumeric() || matches!(character, '.' | '-' | '+')
        })
    {
        return None;
    }
    Some(version.to_string())
}

fn version_is_newer(candidate: &str, current: &str) -> bool {
    fn parts(value: &str) -> Option<[u64; 3]> {
        let stable = value.split('-').next()?;
        let parsed = stable
            .split('.')
            .map(str::parse::<u64>)
            .collect::<Result<Vec<_>, _>>()
            .ok()?;
        (parsed.len() == 3).then(|| [parsed[0], parsed[1], parsed[2]])
    }
    parts(candidate)
        .zip(parts(current))
        .is_some_and(|(candidate, current)| candidate > current)
}

fn pairing_http_host_is_private(host: &str) -> bool {
    if host.eq_ignore_ascii_case("localhost") {
        return true;
    }
    host.parse::<IpAddr>().is_ok_and(|address| match address {
        IpAddr::V4(address) => address.is_loopback() || address.is_private(),
        IpAddr::V6(address) => address.is_loopback() || address.segments()[0] & 0xfe00 == 0xfc00,
    })
}

fn valid_remote_uuid(value: &str) -> bool {
    if value.len() != 36 {
        return false;
    }
    value.char_indices().all(|(index, character)| {
        if matches!(index, 8 | 13 | 18 | 23) {
            character == '-'
        } else {
            character.is_ascii_hexdigit() && !character.is_ascii_uppercase()
        }
    }) && value.as_bytes().get(14) == Some(&b'4')
        && value
            .as_bytes()
            .get(19)
            .is_some_and(|value| matches!(*value, b'8' | b'9' | b'a' | b'b'))
}

fn parse_tailnet_endpoint(value: &str) -> Result<String, String> {
    let parsed = url::Url::parse(value.trim())
        .map_err(|_| "Sessions received an invalid tailnet address".to_string())?;
    let host = parsed
        .host_str()
        .ok_or_else(|| "Sessions received an invalid tailnet address".to_string())?;
    if parsed.scheme() != "https"
        || !host.to_ascii_lowercase().ends_with(".ts.net")
        || parsed.port().is_some()
        || !parsed.username().is_empty()
        || parsed.password().is_some()
        || !matches!(parsed.path(), "" | "/")
        || parsed.query().is_some()
        || parsed.fragment().is_some()
    {
        return Err(
            "Sessions discovery accepts only a Tailscale HTTPS machine address".to_string(),
        );
    }
    Ok(format!("https://{}", host.to_ascii_lowercase()))
}

fn parse_nearby_endpoint(value: &str) -> Result<String, String> {
    let parsed = url::Url::parse(value.trim())
        .map_err(|_| "Sessions received an invalid nearby address".to_string())?;
    let host = parsed
        .host_str()
        .ok_or_else(|| "Sessions received an invalid nearby address".to_string())?;
    let private_ipv4 = host
        .parse::<std::net::Ipv4Addr>()
        .is_ok_and(|address| address.is_private() && !address.is_loopback());
    let port = parsed.port();
    if parsed.scheme() != "http"
        || !private_ipv4
        || !port.is_some_and(|port| port >= 1024)
        || !parsed.username().is_empty()
        || parsed.password().is_some()
        || !matches!(parsed.path(), "" | "/")
        || parsed.query().is_some()
        || parsed.fragment().is_some()
    {
        return Err(
            "Nearby discovery accepts only a private IPv4 HTTP address with an explicit port"
                .to_string(),
        );
    }
    Ok(format!("http://{host}:{}", port.unwrap_or_default()))
}

fn tailnet_http_client(timeout: Duration) -> Result<reqwest::blocking::Client, String> {
    reqwest::blocking::Client::builder()
        .connect_timeout(Duration::from_secs(3))
        .timeout(timeout)
        .redirect(reqwest::redirect::Policy::none())
        .build()
        .map_err(|error| format!("prepare Tailscale request: {error}"))
}

fn discover_tailnet_peers() -> Result<Vec<NativeTailnetPeer>, String> {
    let executable = tailscale_executable().ok_or_else(|| {
        "Tailscale is not installed. Install it, sign in, and try again.".to_string()
    })?;
    let output = Command::new(executable)
        .args(["status", "--json"])
        .output()
        .map_err(|error| format!("could not read Tailscale status: {error}"))?;
    if !output.status.success() {
        let detail = String::from_utf8_lossy(&output.stderr).trim().to_string();
        return Err(if detail.is_empty() {
            "Tailscale is not connected. Open Tailscale and sign in.".to_string()
        } else {
            format!("Tailscale is not ready: {detail}")
        });
    }
    let status: NativeTailscaleStatus = serde_json::from_slice(&output.stdout)
        .map_err(|_| "Tailscale returned an unreadable device list".to_string())?;
    if status.backend_state != "Running" {
        return Err("Tailscale is not connected. Open Tailscale and sign in.".to_string());
    }

    let candidates: Vec<NativeTailnetPeer> = status
        .peer
        .into_values()
        .filter(|peer| peer.online)
        .filter_map(|peer| {
            let dns_name = peer.dns_name.trim().trim_end_matches('.');
            let endpoint = parse_tailnet_endpoint(&format!("https://{dns_name}")).ok()?;
            let hostname = peer.host_name.trim();
            Some(NativeTailnetPeer {
                endpoint,
                hostname: if hostname.is_empty() {
                    dns_name
                } else {
                    hostname
                }
                .to_string(),
                os: peer.os.trim().to_string(),
            })
        })
        .take(32)
        .collect();
    let client = tailnet_http_client(Duration::from_secs(5))?;
    let handles: Vec<_> = candidates
        .into_iter()
        .map(|candidate| {
            let client = client.clone();
            thread::spawn(move || {
                let response = client
                    .get(format!("{}/api/health", candidate.endpoint))
                    .header("accept", "application/json")
                    .send()
                    .ok()?;
                if !response.status().is_success() {
                    return None;
                }
                let health = response.json::<SessionsHealthResponse>().ok()?;
                (health.ok && health.name == "sessionsd" && health.accepts_this_client())
                    .then_some(candidate)
            })
        })
        .collect();
    let mut peers: Vec<NativeTailnetPeer> = handles
        .into_iter()
        .filter_map(|handle| handle.join().ok().flatten())
        .collect();
    peers.sort_by(|left, right| {
        left.hostname
            .to_lowercase()
            .cmp(&right.hostname.to_lowercase())
    });
    Ok(peers)
}

fn response_error(body: &[u8], fallback: String) -> String {
    serde_json::from_slice::<Value>(body)
        .ok()
        .and_then(|value| value.get("error")?.as_str().map(str::to_string))
        .filter(|value| !value.trim().is_empty())
        .unwrap_or(fallback)
}

fn local_device_name() -> String {
    #[cfg(target_os = "windows")]
    if let Some(name) = env::var_os("COMPUTERNAME") {
        let value = name.to_string_lossy().trim().to_string();
        if !value.is_empty() && value.chars().count() <= 80 && !value.chars().any(char::is_control)
        {
            return value;
        }
    }
    let candidates = [
        ("/usr/sbin/scutil", &["--get", "ComputerName"][..]),
        ("/bin/hostname", &[][..]),
    ];
    for (executable, args) in candidates {
        if let Ok(output) = Command::new(executable).args(args).output() {
            let value = String::from_utf8_lossy(&output.stdout).trim().to_string();
            if output.status.success()
                && !value.is_empty()
                && value.chars().count() <= 80
                && !value.chars().any(char::is_control)
            {
                return value;
            }
        }
    }
    "Sessions device".to_string()
}

fn request_tailnet_access(
    endpoint: &str,
    client_id: &str,
    name: &str,
) -> Result<NativeTailnetRequest, String> {
    let endpoint = parse_tailnet_endpoint(endpoint)?;
    if !valid_remote_uuid(client_id.trim()) {
        return Err("Sessions could not create a stable identity for this device".to_string());
    }
    let requested_name = name.trim();
    let name = if requested_name.is_empty() {
        local_device_name()
    } else {
        requested_name.to_string()
    };
    if name.chars().count() > 80 || name.chars().any(char::is_control) {
        return Err("device name must be at most 80 characters".to_string());
    }
    let client = tailnet_http_client(Duration::from_secs(12))?;
    let response = client
        .post(format!("{endpoint}/api/tailnet/access/request"))
        .header("accept", "application/json")
        .json(&serde_json::json!({ "client_id": client_id, "name": name }))
        .send()
        .map_err(|error| format!("could not request access from the other Mac: {error}"))?;
    let status = response.status();
    let body = bounded_pairing_response(response)?;
    if status.as_u16() != 202 {
        return Err(response_error(
            &body,
            format!("the other Mac returned HTTP {}", status.as_u16()),
        ));
    }
    let requested: TailnetRequestResponse = serde_json::from_slice(&body)
        .map_err(|_| "the other Mac returned an invalid access request".to_string())?;
    if !valid_remote_uuid(&requested.request_id)
        || requested.request_secret.trim().is_empty()
        || requested.request_secret.len() > 512
        || requested.request_secret.chars().any(char::is_control)
        || requested.status != "pending"
        || requested.expires_at.trim().is_empty()
    {
        return Err("the other Mac returned an invalid access request".to_string());
    }
    Ok(NativeTailnetRequest {
        endpoint,
        request_id: requested.request_id,
        request_secret: requested.request_secret,
        expires_at: requested.expires_at,
        status: requested.status,
    })
}

fn request_nearby_access(
    endpoint: &str,
    client_id: &str,
    name: &str,
) -> Result<NativeTailnetRequest, String> {
    let endpoint = parse_nearby_endpoint(endpoint)?;
    request_machine_access(
        endpoint,
        client_id,
        name,
        "/api/lan/access/request",
        "nearby machine",
    )
}

fn request_machine_access(
    endpoint: String,
    client_id: &str,
    name: &str,
    path: &str,
    machine_label: &str,
) -> Result<NativeTailnetRequest, String> {
    if !valid_remote_uuid(client_id.trim()) {
        return Err("Sessions could not create a stable identity for this device".to_string());
    }
    let requested_name = name.trim();
    let name = if requested_name.is_empty() {
        local_device_name()
    } else {
        requested_name.to_string()
    };
    if name.chars().count() > 80 || name.chars().any(char::is_control) {
        return Err("device name must be at most 80 characters".to_string());
    }
    let client = tailnet_http_client(Duration::from_secs(12))?;
    let response = client
        .post(format!("{endpoint}{path}"))
        .header("accept", "application/json")
        .json(&serde_json::json!({ "client_id": client_id, "name": name }))
        .send()
        .map_err(|error| format!("could not request access from the {machine_label}: {error}"))?;
    let status = response.status();
    let body = bounded_pairing_response(response)?;
    if status.as_u16() != 202 {
        return Err(response_error(
            &body,
            format!("{machine_label} returned HTTP {}", status.as_u16()),
        ));
    }
    let requested: TailnetRequestResponse = serde_json::from_slice(&body)
        .map_err(|_| format!("{machine_label} returned an invalid access request"))?;
    if !valid_remote_uuid(&requested.request_id)
        || requested.request_secret.trim().is_empty()
        || requested.request_secret.len() > 512
        || requested.request_secret.chars().any(char::is_control)
        || requested.status != "pending"
        || requested.expires_at.trim().is_empty()
    {
        return Err(format!(
            "{machine_label} returned an invalid access request"
        ));
    }
    Ok(NativeTailnetRequest {
        endpoint,
        request_id: requested.request_id,
        request_secret: requested.request_secret,
        expires_at: requested.expires_at,
        status: requested.status,
    })
}

fn validate_native_claim(
    endpoint: String,
    claimed: PairingClaimResponse,
) -> Result<NativePairingClaim, String> {
    let machine_id = claimed
        .machine_id
        .as_deref()
        .map(str::trim)
        .filter(|value| valid_remote_uuid(value))
        .ok_or_else(|| "the other Mac is running an older Sessions daemon".to_string())?
        .to_string();
    let machine_name = claimed
        .machine_name
        .as_deref()
        .map(str::trim)
        .filter(|value| !value.is_empty())
        .map(str::to_string)
        .unwrap_or_else(|| {
            url::Url::parse(&endpoint)
                .ok()
                .and_then(|url| url.host_str().map(str::to_string))
                .unwrap_or_else(|| "Sessions machine".to_string())
        });
    if !valid_remote_uuid(claimed.device_id.trim())
        || claimed.token.trim().is_empty()
        || claimed.token.len() > 512
        || claimed.token.chars().any(char::is_control)
        || claimed.name.trim().is_empty()
        || claimed.name.chars().count() > 80
        || claimed.name.chars().any(char::is_control)
        || machine_name.chars().count() > 80
        || machine_name.chars().any(char::is_control)
    {
        return Err("the other machine returned an invalid device credential".to_string());
    }
    Ok(NativePairingClaim {
        endpoint,
        machine_id,
        machine_name,
        device_id: claimed.device_id,
        token: claimed.token,
        name: claimed.name,
    })
}

fn claim_tailnet_access(
    endpoint: &str,
    request_id: &str,
    request_secret: &str,
) -> Result<NativeTailnetClaim, String> {
    let endpoint = parse_tailnet_endpoint(endpoint)?;
    if !valid_remote_uuid(request_id.trim())
        || request_secret.trim().is_empty()
        || request_secret.len() > 512
        || request_secret.chars().any(char::is_control)
    {
        return Err("the access request is invalid or expired".to_string());
    }
    let client = tailnet_http_client(Duration::from_secs(12))?;
    let response = client
        .post(format!("{endpoint}/api/tailnet/access/claim"))
        .header("accept", "application/json")
        .json(&serde_json::json!({
            "request_id": request_id,
            "request_secret": request_secret
        }))
        .send()
        .map_err(|error| format!("could not check the access request: {error}"))?;
    let status = response.status();
    let body = bounded_pairing_response(response)?;
    match status.as_u16() {
        202 => Ok(NativeTailnetClaim {
            status: "pending".to_string(),
            claim: None,
        }),
        403 => Ok(NativeTailnetClaim {
            status: "denied".to_string(),
            claim: None,
        }),
        410 => Ok(NativeTailnetClaim {
            status: "expired".to_string(),
            claim: None,
        }),
        201 => {
            let claimed: PairingClaimResponse = serde_json::from_slice(&body)
                .map_err(|_| "the other Mac returned an invalid device credential".to_string())?;
            Ok(NativeTailnetClaim {
                status: "accepted".to_string(),
                claim: Some(validate_native_claim(endpoint, claimed)?),
            })
        }
        _ => Err(response_error(
            &body,
            format!("the other Mac returned HTTP {}", status.as_u16()),
        )),
    }
}

fn claim_nearby_access(
    endpoint: &str,
    request_id: &str,
    request_secret: &str,
) -> Result<NativeTailnetClaim, String> {
    let endpoint = parse_nearby_endpoint(endpoint)?;
    claim_machine_access(
        endpoint,
        request_id,
        request_secret,
        "/api/lan/access/claim",
        "nearby machine",
    )
}

fn claim_machine_access(
    endpoint: String,
    request_id: &str,
    request_secret: &str,
    path: &str,
    machine_label: &str,
) -> Result<NativeTailnetClaim, String> {
    if !valid_remote_uuid(request_id.trim())
        || request_secret.trim().is_empty()
        || request_secret.len() > 512
        || request_secret.chars().any(char::is_control)
    {
        return Err("the access request is invalid or expired".to_string());
    }
    let client = tailnet_http_client(Duration::from_secs(12))?;
    let response = client
        .post(format!("{endpoint}{path}"))
        .header("accept", "application/json")
        .json(&serde_json::json!({
            "request_id": request_id,
            "request_secret": request_secret
        }))
        .send()
        .map_err(|error| format!("could not check the access request: {error}"))?;
    let status = response.status();
    let body = bounded_pairing_response(response)?;
    match status.as_u16() {
        202 => Ok(NativeTailnetClaim {
            status: "pending".to_string(),
            claim: None,
        }),
        403 => Ok(NativeTailnetClaim {
            status: "denied".to_string(),
            claim: None,
        }),
        410 => Ok(NativeTailnetClaim {
            status: "expired".to_string(),
            claim: None,
        }),
        201 => {
            let claimed: PairingClaimResponse = serde_json::from_slice(&body)
                .map_err(|_| format!("{machine_label} returned an invalid device credential"))?;
            Ok(NativeTailnetClaim {
                status: "accepted".to_string(),
                claim: Some(validate_native_claim(endpoint, claimed)?),
            })
        }
        _ => Err(response_error(
            &body,
            format!("{machine_label} returned HTTP {}", status.as_u16()),
        )),
    }
}

fn parse_native_pairing_link(value: &str) -> Result<ParsedPairingLink, String> {
    let value = value.trim();
    if value.is_empty() {
        return Err("paste the full pairing link from the other Sessions app".to_string());
    }
    if value.len() > 4096 {
        return Err("pairing link is too long".to_string());
    }

    let mut parsed = url::Url::parse(value)
        .map_err(|_| "paste the full pairing link, including https://".to_string())?;
    if parsed.host_str().is_none() {
        return Err("pairing link is missing a machine address".to_string());
    }
    if !parsed.username().is_empty() || parsed.password().is_some() {
        return Err("pairing links cannot contain a username or password".to_string());
    }
    match parsed.scheme() {
        "https" => {}
        "http" if pairing_http_host_is_private(parsed.host_str().unwrap_or_default()) => {}
        "http" => {
            return Err("unencrypted pairing is allowed only to a private LAN address".to_string())
        }
        _ => return Err("pairing links must use HTTPS or private-LAN HTTP".to_string()),
    }
    if !matches!(parsed.path(), "" | "/") || parsed.query().is_some() {
        return Err("pairing link has an unexpected path or query".to_string());
    }

    let fragment = parsed
        .fragment()
        .ok_or_else(|| "pairing link is missing its one-time ticket".to_string())?;
    let mut ticket: Option<String> = None;
    for (key, value) in url::form_urlencoded::parse(fragment.as_bytes()) {
        if key != "pair" || ticket.is_some() {
            return Err("pairing link has an invalid one-time ticket".to_string());
        }
        let value = value.trim();
        if value.is_empty() || value.len() > 512 || value.chars().any(char::is_control) {
            return Err("pairing link has an invalid one-time ticket".to_string());
        }
        ticket = Some(value.to_string());
    }
    let ticket = ticket.ok_or_else(|| "pairing link is missing its one-time ticket".to_string())?;

    parsed.set_fragment(None);
    parsed.set_path("");
    let endpoint = parsed.as_str().trim_end_matches('/').to_string();
    let claim_url = format!("{endpoint}/api/pair/claim");
    Ok(ParsedPairingLink {
        endpoint,
        claim_url,
        ticket,
    })
}

fn claim_native_pairing_link(value: &str) -> Result<NativePairingClaim, String> {
    let link = parse_native_pairing_link(value)?;
    let client = reqwest::blocking::Client::builder()
        .connect_timeout(Duration::from_secs(5))
        .timeout(Duration::from_secs(12))
        .redirect(reqwest::redirect::Policy::none())
        .build()
        .map_err(|error| format!("prepare secure pairing request: {error}"))?;

    // Match the stronger remote-environment onboarding pattern: identify the
    // endpoint before consuming its one-time credential. A typo or unrelated
    // HTTPS service therefore cannot burn a valid pairing link.
    let health_url = format!("{}/api/health", link.endpoint);
    let health_response = client
        .get(&health_url)
        .header("accept", "application/json")
        .send()
        .map_err(|error| format!("could not reach the other Sessions machine: {error}"))?;
    let health_status = health_response.status();
    let health_body = bounded_pairing_response(health_response)?;
    if !health_status.is_success() {
        return Err(format!(
            "the pairing link does not reach Sessions (HTTP {})",
            health_status.as_u16()
        ));
    }
    let health: SessionsHealthResponse = serde_json::from_slice(&health_body)
        .map_err(|_| "the pairing link does not reach a Sessions daemon".to_string())?;
    if !health.ok || health.name != "sessionsd" {
        return Err("the pairing link does not reach a Sessions daemon".to_string());
    }

    let response = client
        .post(&link.claim_url)
        .header("accept", "application/json")
        .json(&serde_json::json!({ "ticket": link.ticket }))
        .send()
        .map_err(|error| format!("could not reach the other Sessions machine: {error}"))?;
    let status = response.status();
    let body = bounded_pairing_response(response)?;
    if !status.is_success() {
        let detail = serde_json::from_slice::<Value>(&body)
            .ok()
            .and_then(|value| value.get("error")?.as_str().map(str::to_string))
            .filter(|value| !value.trim().is_empty())
            .unwrap_or_else(|| format!("the other machine returned HTTP {}", status.as_u16()));
        return Err(format!(
            "{detail}. Create a new pairing link and try again."
        ));
    }
    let claimed: PairingClaimResponse = serde_json::from_slice(&body)
        .map_err(|_| "the other machine returned an invalid device credential".to_string())?;
    validate_native_claim(link.endpoint, claimed).map_err(|error| {
        if error == "the other Mac is running an older Sessions daemon" {
            format!("{error}; update Sessions there and create a new pairing link")
        } else {
            error
        }
    })
}

fn bounded_pairing_response(response: reqwest::blocking::Response) -> Result<Vec<u8>, String> {
    let mut body = Vec::new();
    response
        .take(64 * 1024 + 1)
        .read_to_end(&mut body)
        .map_err(|error| format!("read pairing response: {error}"))?;
    if body.len() > 64 * 1024 {
        return Err("the other machine returned an oversized pairing response".to_string());
    }
    Ok(body)
}

fn run_connection_action(
    app: &AppHandle,
    kind: &str,
    action: &str,
    name: Option<&str>,
) -> Result<NativeConnectionCommand, String> {
    let mut command_args = match (kind, action) {
        ("lan", "status" | "enable" | "disable") => vec!["lan".to_string(), action.to_string()],
        ("remote", "status" | "enable" | "disable") => {
            vec!["remote".to_string(), action.to_string()]
        }
        ("pair", "create") => vec!["pair".to_string()],
        _ => return Err("unsupported native connection action".to_string()),
    };
    if kind == "pair" {
        if let Some(name) = name.map(str::trim).filter(|value| !value.is_empty()) {
            if name.len() > 80 || name.chars().any(char::is_control) {
                return Err(
                    "device name must be at most 80 characters without control characters"
                        .to_string(),
                );
            }
            command_args.extend(["--name".to_string(), name.to_string()]);
        }
    }
    run_bundled_sessions_json(app, &command_args, "connection")
}

fn run_backup_action(
    app: &AppHandle,
    action: &str,
    project: Option<&str>,
) -> Result<NativeConnectionCommand, String> {
    // A first or catch-up backup uploads real data, so it gets the transfer
    // budget; status and enable only talk to the local daemon.
    let (command_args, timeout) = match action {
        "status" => (
            vec!["backup".to_string(), "status".to_string()],
            BUNDLED_CLI_TIMEOUT,
        ),
        "now" => (
            vec!["backup".to_string(), "now".to_string()],
            BUNDLED_CLI_TRANSFER_TIMEOUT,
        ),
        "enable" => {
            let project = project
                .map(str::trim)
                .filter(|value| !value.is_empty())
                .ok_or_else(|| "choose a Somewhere project for Sessions backups".to_string())?;
            if project.len() > 120
                || matches!(project, "." | "..")
                || !project.chars().all(|character| {
                    character.is_ascii_alphanumeric() || matches!(character, '-' | '_' | '.')
                })
            {
                return Err(
                    "Somewhere project must use only letters, numbers, dots, dashes, or underscores"
                        .to_string(),
                );
            }
            (
                vec![
                    "backup".to_string(),
                    "enable".to_string(),
                    "--project".to_string(),
                    project.to_string(),
                    "--interval".to_string(),
                    "15m".to_string(),
                    "--encrypt".to_string(),
                ],
                BUNDLED_CLI_TIMEOUT,
            )
        }
        _ => return Err("unsupported native backup action".to_string()),
    };
    run_bundled_sessions_json_with_timeout(app, &command_args, "backup", timeout)
}

fn run_bundled_sessions_json(
    app: &AppHandle,
    command_args: &[String],
    response_kind: &str,
) -> Result<NativeConnectionCommand, String> {
    run_bundled_sessions_json_with_input(
        app,
        command_args,
        response_kind,
        None,
        BUNDLED_CLI_TIMEOUT,
    )
}

fn run_bundled_sessions_json_with_timeout(
    app: &AppHandle,
    command_args: &[String],
    response_kind: &str,
    timeout: Duration,
) -> Result<NativeConnectionCommand, String> {
    run_bundled_sessions_json_with_input(app, command_args, response_kind, None, timeout)
}

fn run_bundled_sessions_json_with_input(
    app: &AppHandle,
    command_args: &[String],
    response_kind: &str,
    input: Option<&[u8]>,
    timeout: Duration,
) -> Result<NativeConnectionCommand, String> {
    let port = *app
        .state::<RuntimeState>()
        .port
        .lock()
        .map_err(|error| error.to_string())?;
    let resources = app
        .path()
        .resource_dir()
        .map_err(|error| format!("resolve Sessions resources: {error}"))?;
    let executable = resources.join("runtime").join("sessions");
    if !executable.is_file() {
        return Err(format!(
            "bundled Sessions CLI is missing: {}",
            executable.display()
        ));
    }
    let mut command = Command::new(executable);
    let port_string = port.to_string();
    command.args([
        "--json",
        "--host",
        "127.0.0.1",
        "--port",
        port_string.as_str(),
    ]);
    command.args(command_args);
    let inherited_path = env::var("PATH").unwrap_or_default();
    command.env(
        "PATH",
        format!("/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:{inherited_path}"),
    );
    run_json_command(command, response_kind, input, timeout)
}

// Run one bundled-CLI invocation to completion and decode its JSON answer.
//
// Split out from the argument building so the process handling — which is the
// part with the failure modes — can be exercised directly in tests.
fn run_json_command(
    mut command: Command,
    response_kind: &str,
    input: Option<&[u8]>,
    timeout: Duration,
) -> Result<NativeConnectionCommand, String> {
    command
        .stdin(if input.is_some() {
            Stdio::piped()
        } else {
            Stdio::null()
        })
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    let mut child = command
        .spawn()
        .map_err(|error| format!("run bundled Sessions CLI: {error}"))?;

    // Drain both output pipes on their own threads *before* writing stdin. The
    // CLI writes progress and diagnostics while it reads its input, so an input
    // larger than the ~64 KiB pipe buffer used to deadlock permanently: this
    // side blocked in write_all because the child was not reading, while the
    // child blocked writing output nobody was reading — and the old
    // wait_with_output only began draining after write_all returned. The
    // machine registry that native_agent_machines_sync writes crosses that
    // buffer on any well-used machine.
    let stdout = child
        .stdout
        .take()
        .ok_or_else(|| "read bundled Sessions CLI output".to_string())?;
    let stderr = child
        .stderr
        .take()
        .ok_or_else(|| "read bundled Sessions CLI diagnostics".to_string())?;
    let stdout_reader = thread::spawn(move || drain_bounded(stdout));
    let stderr_reader = thread::spawn(move || drain_bounded(stderr));

    let write_result = match (input, child.stdin.take()) {
        // Dropping the handle at the end of this arm closes the pipe, which is
        // the EOF the CLI is waiting for.
        (Some(input), Some(mut stdin)) => stdin
            .write_all(input)
            .map_err(|error| format!("write bundled Sessions CLI input: {error}")),
        (Some(_), None) => Err("open bundled Sessions CLI input".to_string()),
        (None, _) => Ok(()),
    };

    let finished = if write_result.is_ok() {
        matches!(wait_bounded(&mut child, timeout), Ok(Some(_)))
    } else {
        false
    };
    let status = if finished {
        child.try_wait().ok().flatten()
    } else {
        // Stop the run so the reader threads see EOF and are released. The
        // daemon and every runner are separate processes; ending this CLI
        // invocation cannot touch a live session.
        let _ = child.kill();
        child.wait().ok()
    };
    // Join before reporting anything: these threads hold the pipe ends.
    let stdout = stdout_reader.join().unwrap_or_default();
    let stderr = stderr_reader.join().unwrap_or_default();
    write_result?;

    let stdout = String::from_utf8_lossy(&stdout).trim().to_string();
    let stderr = String::from_utf8_lossy(&stderr).trim().to_string();
    let Some(status) = status.filter(|_| finished) else {
        return Err(format!(
            "the sessions {response_kind} command did not finish within {} seconds and was stopped. Your daemon and every runner keep running; retry, or run the same command from the sessions CLI to watch its progress.",
            timeout.as_secs()
        ));
    };
    if !status.success() {
        let detail = if stderr.is_empty() { stdout } else { stderr };
        return Err(if detail.is_empty() {
            format!("sessions {response_kind} command failed with {status}")
        } else {
            detail
        });
    }
    let data = serde_json::from_str(&stdout)
        .map_err(|error| format!("Sessions returned invalid {response_kind} data: {error}"))?;
    Ok(NativeConnectionCommand {
        data,
        detail: stderr,
    })
}

// Read up to the cap, then keep draining to the void. The child must never
// block on a full pipe just because we stopped being interested in its output.
fn drain_bounded(mut source: impl Read) -> Vec<u8> {
    let mut buffer = Vec::new();
    let _ = source
        .by_ref()
        .take(BUNDLED_CLI_MAX_OUTPUT)
        .read_to_end(&mut buffer);
    let _ = std::io::copy(&mut source, &mut std::io::sink());
    buffer
}

#[cfg(desktop)]
fn tray_tooltip(snapshot: TraySnapshot) -> String {
    let suffix = if snapshot.reachable {
        String::new()
    } else {
        " — daemon unreachable".to_string()
    };
    format!(
        "Sessions — ● {} working, ○ {} idle, ⚠ {} needing attention{}",
        snapshot.working, snapshot.idle, snapshot.attention, suffix
    )
}

#[cfg(desktop)]
fn tray_snapshot(response: SessionsResponse) -> TraySnapshot {
    let mut snapshot = TraySnapshot {
        reachable: true,
        ..TraySnapshot::default()
    };
    for session in response.sessions {
        // These are mutually-exclusive menu buckets. A completed/crashed
        // session, or an idle conversational session that has received a
        // user message, is actionable; untouched idle shells remain idle.
        if session.exited && session.exit_code.unwrap_or_default() != 0 {
            snapshot.attention += 1;
        } else if session.working {
            snapshot.working += 1;
        } else if matches!(
            session.idle_reason.as_deref(),
            Some("needs-input" | "failed")
        ) || (session.idle_reason.is_none() && session.last_user_message_at.is_some())
        {
            snapshot.attention += 1;
        } else {
            snapshot.idle += 1;
        }
    }
    snapshot
}

// The daemon binds 127.0.0.1 (lifecycle::LOOPBACK_HOST). Asking for
// "localhost" invites a resolver that answers ::1 first, and the tray then
// reads "daemon unreachable" forever on a perfectly healthy machine.
#[cfg(desktop)]
fn tray_sessions_url(port: u16) -> String {
    format!("http://127.0.0.1:{port}/api/sessions")
}

// Every other loopback client in this codebase disables proxies; this one used
// not to. With HTTP_PROXY set, the tray's own status poll would be routed
// through the proxy and fail permanently.
#[cfg(desktop)]
fn tray_http_client() -> Result<reqwest::blocking::Client, String> {
    reqwest::blocking::Client::builder()
        .no_proxy()
        .connect_timeout(Duration::from_secs(2))
        .timeout(Duration::from_secs(3))
        .build()
        .map_err(|error| format!("build loopback session client: {error}"))
}

#[cfg(desktop)]
fn fetch_tray_snapshot(client: &reqwest::blocking::Client, port: u16) -> TraySnapshot {
    client
        .get(tray_sessions_url(port))
        .send()
        .and_then(|response| response.error_for_status())
        .and_then(|response| response.json::<SessionsResponse>())
        .map(tray_snapshot)
        .unwrap_or_default()
}

#[cfg(desktop)]
fn build_tray_menu(app: &AppHandle) -> tauri::Result<Menu<tauri::Wry>> {
    let state = app.state::<TrayState>();
    let snapshot = *state.snapshot.lock().unwrap_or_else(|e| e.into_inner());
    let servers = state
        .servers
        .lock()
        .unwrap_or_else(|e| e.into_inner())
        .clone();

    let working = MenuItem::with_id(
        app,
        "status-working",
        format!("● {} working", snapshot.working),
        false,
        None::<&str>,
    )?;
    let idle = MenuItem::with_id(
        app,
        "status-idle",
        format!("○ {} idle", snapshot.idle),
        false,
        None::<&str>,
    )?;
    let attention = MenuItem::with_id(
        app,
        "status-attention",
        format!("⚠ {} needing attention", snapshot.attention),
        false,
        None::<&str>,
    )?;
    let runtime = app
        .state::<RuntimeState>()
        .status
        .lock()
        .unwrap_or_else(|error| error.into_inner())
        .clone();
    let runtime = MenuItem::with_id(
        app,
        "runtime-status",
        runtime.menu_label(),
        false,
        None::<&str>,
    )?;
    let open = MenuItem::with_id(app, "open-main", "Open Sessions", true, None::<&str>)?;

    let mut targets = HashMap::new();
    let mut new_window = SubmenuBuilder::new(app, "New window for…");
    if servers.is_empty() {
        let empty = MenuItem::with_id(
            app,
            "no-servers",
            "No configured servers",
            false,
            None::<&str>,
        )?;
        new_window = new_window.item(&empty);
    } else {
        for (index, server) in servers.iter().enumerate() {
            let menu_id = format!("new-server-{index}");
            let item = MenuItem::with_id(app, &menu_id, &server.name, true, None::<&str>)?;
            let query = url::form_urlencoded::Serializer::new(String::new())
                .append_pair("server", &server.id)
                .finish();
            if let Ok(spec) = parse_scoped_window(&query, format!("{} — Sessions", server.name)) {
                targets.insert(menu_id, spec);
            }
            new_window = new_window.item(&item);
        }
    }
    new_window = new_window
        .separator()
        .text("new-tool-codex", "Codex")
        .text("new-tool-claude", "Claude")
        .text("new-tool-shell", "Shell");
    let new_window = new_window.build()?;
    *state
        .server_targets
        .lock()
        .unwrap_or_else(|e| e.into_inner()) = targets;

    let quit = MenuItem::with_id(
        app,
        "quit-sessions",
        "Quit Sessions (work keeps running)",
        true,
        None::<&str>,
    )?;
    let menu = Menu::new(app)?;
    menu.append(&working)?;
    menu.append(&idle)?;
    menu.append(&attention)?;
    menu.append(&runtime)?;
    menu.append(&tauri::menu::PredefinedMenuItem::separator(app)?)?;
    menu.append(&open)?;
    menu.append(&new_window)?;
    menu.append(&tauri::menu::PredefinedMenuItem::separator(app)?)?;
    menu.append(&quit)?;
    Ok(menu)
}

#[cfg(desktop)]
fn refresh_tray(app: &AppHandle) -> tauri::Result<()> {
    let snapshot = *app
        .state::<TrayState>()
        .snapshot
        .lock()
        .unwrap_or_else(|e| e.into_inner());
    if let Some(tray) = app.tray_by_id(TRAY_ID) {
        tray.set_tooltip(Some(tray_tooltip(snapshot)))?;
        tray.set_menu(Some(build_tray_menu(app)?))?;
    }
    Ok(())
}

#[cfg(mobile)]
fn refresh_tray(_app: &AppHandle) -> tauri::Result<()> {
    Ok(())
}

#[cfg(desktop)]
fn handle_tray_menu(app: &AppHandle, id: &str) {
    let result = match id {
        "open-main" => open_window(app, main_window_spec()),
        "new-tool-codex" => parse_scoped_window("tool=codex", "Codex — Sessions".to_string())
            .and_then(|spec| open_window(app, spec)),
        "new-tool-claude" => parse_scoped_window("tool=claude", "Claude — Sessions".to_string())
            .and_then(|spec| open_window(app, spec)),
        "new-tool-shell" => parse_scoped_window("tool=shell", "Shell — Sessions".to_string())
            .and_then(|spec| open_window(app, spec)),
        "quit-sessions" => {
            app.exit(0);
            Ok(())
        }
        _ => {
            let spec = app
                .state::<TrayState>()
                .server_targets
                .lock()
                .ok()
                .and_then(|targets| targets.get(id).cloned());
            match spec {
                Some(spec) => open_window(app, spec),
                None => Ok(()),
            }
        }
    };
    if let Err(error) = result {
        log::warn!("tray action {id}: {error}");
    }
}

#[cfg(desktop)]
fn start_tray_poll(app: AppHandle) {
    thread::spawn(move || {
        // Building the client must never be fatal here. An .expect() inside
        // this thread would take tray status down silently and permanently for
        // the rest of the app's life; retry on the next tick instead and report
        // the honest "unreachable" state in the meantime.
        let mut client: Option<reqwest::blocking::Client> = None;
        loop {
            if client.is_none() {
                match tray_http_client() {
                    Ok(built) => client = Some(built),
                    Err(error) => log::warn!("tray status client: {error}"),
                }
            }
            let port = *app
                .state::<RuntimeState>()
                .port
                .lock()
                .unwrap_or_else(|error| error.into_inner());
            let next = match client.as_ref() {
                Some(client) => fetch_tray_snapshot(client, port),
                None => TraySnapshot::default(),
            };
            let changed = {
                let state = app.state::<TrayState>();
                let mut snapshot = state.snapshot.lock().unwrap_or_else(|e| e.into_inner());
                if *snapshot == next {
                    false
                } else {
                    *snapshot = next;
                    true
                }
            };
            if changed {
                let app_for_menu = app.clone();
                let _ = app.run_on_main_thread(move || {
                    if let Err(error) = refresh_tray(&app_for_menu) {
                        log::warn!("refresh tray status: {error}");
                    }
                });
            }
            thread::sleep(Duration::from_secs(5));
        }
    });
}

// The uninstall entry point, invoked by the NSIS uninstaller before it deletes
// the package and available by hand on macOS, which ships as a bundle with no
// uninstaller of its own. It runs before any Tauri app is built, so it opens no
// window, starts no tray, and touches no daemon: it removes the per-user
// integration Sessions installed and reports what it deliberately left. See
// lifecycle::IntegrationRemoval for that boundary.
#[cfg(desktop)]
const REMOVE_INTEGRATION_ARGUMENT: &str = "--remove-integration";

#[cfg(desktop)]
fn requested_integration_removal() -> bool {
    env::args_os().skip(1).any(|argument| {
        argument
            .to_str()
            .is_some_and(|argument| argument == REMOVE_INTEGRATION_ARGUMENT)
    })
}

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    #[cfg(desktop)]
    if requested_integration_removal() {
        let removal = lifecycle::remove_integration();
        println!("{}", removal.report());
        // A non-zero exit is the only way this can tell an uninstaller that
        // something is now the user's to clean up by hand; the uninstaller
        // continues either way, because a package that refuses to uninstall is
        // worse than one that leaves a registry value behind.
        std::process::exit(if removal.is_complete() { 0 } else { 1 });
    }

    let app = tauri::Builder::default()
        .plugin(tauri_plugin_process::init())
        .plugin(tauri_plugin_notification::init())
        .plugin(tauri_plugin_updater::Builder::new().build())
        .manage(TrayState::default())
        .invoke_handler(tauri::generate_handler![
            open_scoped_window,
            set_tray_servers,
            runtime_status,
            recover_runtime,
            native_connection_settings,
            set_runtime_port,
            native_connection_action,
            native_pairing_claim,
            native_tailnet_discover,
            native_tailnet_request,
            native_tailnet_claim,
            native_nearby_discover,
            native_nearby_request,
            native_nearby_claim,
            native_backup_action,
            native_support_preview,
            native_move_machines,
            native_agent_machines_sync,
            native_move_session,
            native_machine_credentials_load,
            native_machine_credentials_save,
            open_external_url,
            open_support_page,
            somewhere_cli_status
        ])
        .setup(|app| {
            // One answer to a corrupt connections.json, shared with every
            // reconcile path: fall back to the default port so the management
            // plane keeps working, and carry the reason into the status the
            // tray and settings screen display instead of only into the log.
            let resolved_port = lifecycle::resolve_port(app.handle());
            if let Some(problem) = resolved_port.problem() {
                log::error!("Sessions native connection settings: {problem}");
            }
            let configured_port = resolved_port.port();
            // Never run launchd reconciliation on Tauri's setup thread. Until
            // setup returns WebKit cannot draw even the recovery shell, and a
            // machine with many retained runners can need minutes to re-adopt
            // them after a daemon update. Publish a truthful transitional state
            // first, then reconcile without blocking the viewer.
            let runtime_status = resolved_port.annotate(lifecycle::startup_status());
            let reconcile_runtime = lifecycle::needs_background_reconcile(&runtime_status);
            let owns_local_runtime = runtime_status.state != "client-only";
            app.manage(RuntimeState {
                status: Mutex::new(runtime_status),
                port: Mutex::new(configured_port),
            });

            #[cfg(desktop)]
            {
                let geometry_path = app.path().app_config_dir()?.join("window-geometry.json");
                app.manage(WindowGeometryStore::load(geometry_path));

                if let Some(main) = app.get_webview_window("main") {
                    restore_window(&main);
                    track_window(&main);
                }

                let menu = build_tray_menu(app.handle())?;
                let mut tray = TrayIconBuilder::with_id(TRAY_ID)
                    .menu(&menu)
                    .tooltip(tray_tooltip(TraySnapshot::default()))
                    .icon_as_template(true)
                    .on_menu_event(|app, event| handle_tray_menu(app, event.id().as_ref()));
                if let Some(icon) = app.default_window_icon().cloned() {
                    tray = tray.icon(icon);
                }
                tray.build(app.handle())?;
                if owns_local_runtime {
                    start_tray_poll(app.handle().clone());
                }
            }

            if reconcile_runtime {
                let worker = app.handle().clone();
                thread::spawn(move || {
                    let status = lifecycle::install_for_app(&worker);
                    if status.state == "error" {
                        log::error!("Sessions background service: {}", status.detail);
                    }
                    if let Ok(mut current) = worker.state::<RuntimeState>().status.lock() {
                        *current = status;
                    }
                    #[cfg(desktop)]
                    {
                        let app_for_menu = worker.clone();
                        let _ = worker.run_on_main_thread(move || {
                            if let Err(error) = refresh_tray(&app_for_menu) {
                                log::warn!("refresh tray after runtime reconciliation: {error}");
                            }
                        });
                    }
                });
            }

            #[cfg(mobile)]
            let _ = owns_local_runtime;

            if cfg!(debug_assertions) {
                app.handle().plugin(
                    tauri_plugin_log::Builder::default()
                        .level(log::LevelFilter::Info)
                        .build(),
                )?;
            }
            Ok(())
        })
        .build(tauri::generate_context!())
        .expect("error while building tauri application");

    app.run(|_app, _event| {
        // Quitting right after moving a window must still keep that position:
        // the debounced geometry writer may be mid-sleep. Quitting never ends
        // the daemon or a runner, so this is the only state worth saving here.
        #[cfg(desktop)]
        if matches!(_event, tauri::RunEvent::Exit) {
            if let Some(store) = _app.try_state::<WindowGeometryStore>() {
                store.flush();
            }
        }
        #[cfg(target_os = "macos")]
        if let tauri::RunEvent::Reopen { .. } = _event {
            let _ = open_window(_app, main_window_spec());
        }
    });
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::net::{TcpListener, TcpStream};

    fn read_test_http_request(stream: &mut TcpStream) -> String {
        stream
            .set_read_timeout(Some(Duration::from_secs(2)))
            .unwrap();
        let mut request = Vec::new();
        loop {
            let mut chunk = [0_u8; 2048];
            let count = stream.read(&mut chunk).unwrap();
            if count == 0 {
                break;
            }
            request.extend_from_slice(&chunk[..count]);
            let Some(headers_end) = request.windows(4).position(|window| window == b"\r\n\r\n")
            else {
                continue;
            };
            let headers_end = headers_end + 4;
            let headers = String::from_utf8_lossy(&request[..headers_end]);
            let content_length = headers
                .lines()
                .find_map(|line| {
                    line.to_ascii_lowercase()
                        .strip_prefix("content-length:")
                        .and_then(|value| value.trim().parse::<usize>().ok())
                })
                .unwrap_or(0);
            if request.len() >= headers_end + content_length {
                break;
            }
        }
        String::from_utf8(request).unwrap()
    }

    fn write_test_http_json(stream: &mut TcpStream, status: &str, body: &str) {
        write!(
            stream,
            "HTTP/1.1 {status}\r\ncontent-type: application/json\r\ncontent-length: {}\r\nconnection: close\r\n\r\n{body}",
            body.len()
        )
        .unwrap();
    }

    fn unique_test_suffix() -> u128 {
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap_or_default()
            .as_nanos()
    }

    #[cfg(desktop)]
    #[test]
    fn window_geometry_is_debounced_and_replaced_atomically() {
        let root = env::temp_dir().join(format!(
            "sessions-window-geometry-test-{}-{}",
            std::process::id(),
            unique_test_suffix()
        ));
        let path = root.join("window-geometry.json");
        let store = WindowGeometryStore::load(path.clone());
        let at = |x: i32| WindowBounds {
            x,
            y: 10,
            width: 1200,
            height: 800,
            maximized: false,
        };

        // One drag. The old code re-serialized the whole map and truncated the
        // file once per event, with the mutex held for the entire write.
        for x in 0..300 {
            store.remember("main".to_string(), at(x));
        }
        assert!(
            !path.exists(),
            "window geometry was written synchronously during a drag"
        );

        thread::sleep(WINDOW_GEOMETRY_FLUSH_DELAY + Duration::from_millis(500));
        let saved: HashMap<String, WindowBounds> =
            serde_json::from_slice(&fs::read(&path).unwrap()).unwrap();
        assert_eq!(saved.get("main"), Some(&at(299)));
        assert_eq!(store.get("main"), Some(at(299)));

        // Temp-plus-rename must not leave a partial file to be loaded next time.
        let leftovers: Vec<String> = fs::read_dir(&root)
            .unwrap()
            .filter_map(|entry| entry.ok())
            .map(|entry| entry.file_name().to_string_lossy().into_owned())
            .filter(|name| name != "window-geometry.json")
            .collect();
        assert!(
            leftovers.is_empty(),
            "atomic write left files behind: {leftovers:?}"
        );

        // Quitting or closing right after a move must still keep that position.
        store.remember("main".to_string(), at(900));
        store.flush();
        let saved: HashMap<String, WindowBounds> =
            serde_json::from_slice(&fs::read(&path).unwrap()).unwrap();
        assert_eq!(saved.get("main"), Some(&at(900)));

        thread::sleep(WINDOW_GEOMETRY_FLUSH_DELAY + Duration::from_millis(500));
        fs::remove_dir_all(&root).unwrap();
    }

    #[cfg(unix)]
    #[test]
    fn bundled_cli_input_and_output_stream_together_instead_of_deadlocking() {
        // The child writes far past a pipe buffer before it reads a byte of
        // stdin. The old code wrote all of stdin first and only began draining
        // in wait_with_output afterwards, so a machine registry over ~64 KiB
        // hung this call and its blocking-pool thread forever.
        let mut command = Command::new("/bin/sh");
        command.args([
            "-c",
            "dd if=/dev/zero bs=1024 count=200 2>/dev/null | tr '\\0' 'd' >&2; cat >/dev/null; printf '{\"machines\":[]}'",
        ]);
        let registry = vec![b'x'; 512 * 1024];
        let answer = run_json_command(
            command,
            "agent machine sync",
            Some(&registry),
            Duration::from_secs(60),
        )
        .unwrap();
        assert_eq!(answer.data, serde_json::json!({ "machines": [] }));
        assert_eq!(answer.detail.len(), 200 * 1024);
    }

    #[cfg(unix)]
    #[test]
    fn a_bundled_cli_run_that_outlives_its_budget_is_stopped_and_explained() {
        let mut command = Command::new("/bin/sh");
        command.args(["-c", "sleep 60"]);
        let started = Instant::now();
        let error = run_json_command(command, "support", None, Duration::from_secs(1)).unwrap_err();
        assert!(error.contains("did not finish within 1 seconds"), "{error}");
        // Rule 4: say what is still true and what to do next.
        assert!(error.contains("keep running"), "{error}");
        assert!(error.contains("sessions CLI"), "{error}");
        assert!(
            started.elapsed() < Duration::from_secs(20),
            "the timeout did not release the caller"
        );
    }

    #[cfg(unix)]
    #[test]
    fn external_launch_waits_only_long_enough_to_see_an_immediate_failure() {
        let detached = |program: &str| {
            Command::new("/bin/sh")
                .args(["-c", program])
                .stdin(Stdio::null())
                .stdout(Stdio::null())
                .stderr(Stdio::null())
                .spawn()
                .unwrap()
        };

        // A scheme with no registered handler still has to be reported.
        let mut refused = detached("exit 3");
        let status = wait_bounded(&mut refused, Duration::from_secs(10))
            .unwrap()
            .expect("an immediate failure was not observed");
        assert_eq!(status.code(), Some(3));

        // An xdg-open backend that stays in the foreground for the life of the
        // browser must not hold the caller — the old code called status().
        let mut lingering = detached("sleep 60");
        let started = Instant::now();
        assert!(wait_bounded(&mut lingering, Duration::from_millis(200))
            .unwrap()
            .is_none());
        assert!(
            started.elapsed() < Duration::from_secs(10),
            "a long-lived URL handler blocked the caller"
        );
        let _ = lingering.kill();
        reap_in_background(lingering);
    }

    #[cfg(desktop)]
    #[test]
    fn tray_status_polls_the_loopback_address_the_daemon_actually_binds() {
        // Not "localhost": a resolver that answers ::1 first leaves the tray
        // permanently stuck on "daemon unreachable".
        assert_eq!(
            tray_sessions_url(8787),
            "http://127.0.0.1:8787/api/sessions"
        );

        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let port = listener.local_addr().unwrap().port();
        let server = std::thread::spawn(move || {
            let (mut stream, _) = listener.accept().unwrap();
            let request = read_test_http_request(&mut stream);
            assert!(request.starts_with("GET /api/sessions HTTP/1.1"));
            write_test_http_json(
                &mut stream,
                "200 OK",
                r#"{"sessions":[{"working":true,"exited":false,"exitCode":null,"idleReason":null,"lastUserMessageAt":null}]}"#,
            );
        });

        let client = tray_http_client().expect("tray client must build");
        let snapshot = fetch_tray_snapshot(&client, port);
        assert_eq!(snapshot.working, 1);
        assert!(snapshot.reachable);
        server.join().unwrap();

        // A daemon that is not there reads as unreachable, not as idle.
        let closed = TcpListener::bind("127.0.0.1:0").unwrap();
        let closed_port = closed.local_addr().unwrap().port();
        drop(closed);
        assert!(!fetch_tray_snapshot(&client, closed_port).reachable);
    }

    #[test]
    fn scoped_queries_are_validated_and_stable() {
        let server = parse_scoped_window("?server=studio%20mac", "Studio".to_string()).unwrap();
        assert_eq!(server.label, "win-server-studio-mac");
        assert_eq!(server.query, "server=studio+mac");

        let tool = parse_scoped_window("tool=claude", "Claude".to_string()).unwrap();
        assert_eq!(tool.label, "win-tool-claude");

        let session =
            parse_scoped_window("session=abc-123&mode=single", "Session".to_string()).unwrap();
        assert_eq!(session.label, "win-session-abc-123");
        assert!(parse_scoped_window("tool=unknown", String::new()).is_err());
        assert!(parse_scoped_window("server=x&tool=codex", String::new()).is_err());
    }

    #[test]
    fn tray_counts_are_mutually_exclusive() {
        let snapshot = tray_snapshot(SessionsResponse {
            sessions: vec![
                TraySession {
                    working: true,
                    exited: false,
                    exit_code: None,
                    idle_reason: None,
                    last_user_message_at: Some(1),
                },
                TraySession {
                    working: false,
                    exited: false,
                    exit_code: None,
                    idle_reason: Some("completed".to_string()),
                    last_user_message_at: None,
                },
                TraySession {
                    working: false,
                    exited: false,
                    exit_code: None,
                    idle_reason: Some("needs-input".to_string()),
                    last_user_message_at: Some(1),
                },
                TraySession {
                    working: false,
                    exited: true,
                    exit_code: Some(1),
                    idle_reason: Some("failed".to_string()),
                    last_user_message_at: None,
                },
            ],
        });
        assert_eq!(snapshot.working, 1);
        assert_eq!(snapshot.idle, 1);
        assert_eq!(snapshot.attention, 2);
        assert!(snapshot.reachable);
    }

    #[test]
    fn somewhere_versions_compare_numerically() {
        assert!(version_is_newer("0.27.3", "0.21.0"));
        assert!(version_is_newer("1.10.0", "1.9.9"));
        assert!(!version_is_newer("1.9.0", "1.10.0"));
        assert!(!version_is_newer("invalid", "1.0.0"));
        assert_eq!(clean_version("v0.27.3\n"), Some("0.27.3".to_string()));
    }

    #[test]
    fn support_pages_are_fixed_https_destinations() {
        assert_eq!(support_page_url("choose").unwrap(), SUPPORT_TICKET_URL);
        assert_eq!(support_page_url("feedback").unwrap(), SUPPORT_FEEDBACK_URL);
        assert_eq!(support_page_url("bug").unwrap(), SUPPORT_BUG_URL);
        assert_eq!(support_page_url("security").unwrap(), SUPPORT_SECURITY_URL);
        assert!(support_page_url("https://attacker.example").is_err());
        for url in [
            SUPPORT_TICKET_URL,
            SUPPORT_FEEDBACK_URL,
            SUPPORT_BUG_URL,
            SUPPORT_SECURITY_URL,
        ] {
            let parsed = url::Url::parse(url).unwrap();
            assert_eq!(parsed.scheme(), "https");
            assert_eq!(parsed.host_str(), Some("github.com"));
        }
    }

    #[test]
    fn external_links_allow_only_explicit_non_executable_schemes() {
        for valid in [
            "https://somewhere.tech/sessions",
            "http://127.0.0.1:8787/docs",
            "mailto:support@somewhere.tech",
            "vscode://file/Users/test/project/main.go:12",
        ] {
            assert!(
                validate_external_url(valid).is_ok(),
                "external URL was rejected: {valid}"
            );
        }
        for invalid in [
            "javascript:alert(1)",
            "data:text/html,hello",
            "file:///Users/test/.ssh/id_ed25519",
            "https://example.com/\nnext",
            "relative/path",
        ] {
            assert!(
                validate_external_url(invalid).is_err(),
                "unsafe URL was accepted: {invalid}"
            );
        }
    }

    #[test]
    fn native_pairing_links_keep_tickets_out_of_the_request_url() {
        let parsed = parse_native_pairing_link(
            "https://mac-mini.example.ts.net/#pair=ticket-id.ticket-secret",
        )
        .unwrap();
        assert_eq!(
            parsed,
            ParsedPairingLink {
                endpoint: "https://mac-mini.example.ts.net".to_string(),
                claim_url: "https://mac-mini.example.ts.net/api/pair/claim".to_string(),
                ticket: "ticket-id.ticket-secret".to_string(),
            }
        );

        let lan = parse_native_pairing_link("http://192.168.1.25:8787/#pair=one%2Etwo").unwrap();
        assert_eq!(lan.endpoint, "http://192.168.1.25:8787");
        assert_eq!(lan.ticket, "one.two");
    }

    #[test]
    fn native_pairing_rejects_unsafe_or_ambiguous_links() {
        for invalid in [
            "ticket-only",
            "http://example.com/#pair=secret",
            "ftp://192.168.1.25/#pair=secret",
            "https://user:password@example.com/#pair=secret",
            "https://example.com/other#pair=secret",
            "https://example.com/?query=1#pair=secret",
            "https://example.com/#pair=one&pair=two",
            "https://example.com/#other=secret",
        ] {
            assert!(
                parse_native_pairing_link(invalid).is_err(),
                "unsafe link was accepted: {invalid}"
            );
        }
    }

    #[test]
    fn native_pairing_http_accepts_only_private_or_loopback_hosts() {
        for valid in [
            "localhost",
            "127.0.0.1",
            "192.168.1.25",
            "::1",
            "fc00::1",
            "fd12:3456:789a::1",
        ] {
            assert!(
                pairing_http_host_is_private(valid),
                "private host was rejected: {valid}"
            );
        }
        for invalid in [
            "example.com",
            "8.8.8.8",
            "169.254.1.1",
            "fe80::1",
            "2001:db8::1",
        ] {
            assert!(
                !pairing_http_host_is_private(invalid),
                "public or link-local host was accepted: {invalid}"
            );
        }
    }

    #[test]
    fn remote_machine_ids_are_strict_v4_uuids() {
        assert!(valid_remote_uuid("11111111-1111-4111-8111-111111111111"));
        assert!(!valid_remote_uuid("11111111-1111-5111-8111-111111111111"));
        assert!(!valid_remote_uuid("11111111-1111-4111-C111-111111111111"));
        assert!(!valid_remote_uuid("../machine"));
    }

    #[test]
    fn tailnet_discovery_accepts_only_https_machine_names() {
        assert_eq!(
            parse_tailnet_endpoint("https://Mac-Mini.tail1234.ts.net/").unwrap(),
            "https://mac-mini.tail1234.ts.net"
        );
        for invalid in [
            "http://mac-mini.tail1234.ts.net",
            "https://example.com",
            "https://mac-mini.tail1234.ts.net:8443",
            "https://mac-mini.tail1234.ts.net/api/health",
            "https://user@mac-mini.tail1234.ts.net",
        ] {
            assert!(
                parse_tailnet_endpoint(invalid).is_err(),
                "unsafe tailnet endpoint was accepted: {invalid}"
            );
        }
    }

    #[test]
    fn nearby_discovery_accepts_only_private_ipv4_http_origins() {
        assert_eq!(
            parse_nearby_endpoint("http://192.168.1.25:8787/").unwrap(),
            "http://192.168.1.25:8787"
        );
        for invalid in [
            "https://192.168.1.25:8787",
            "http://127.0.0.1:8787",
            "http://example.com:8787",
            "http://192.168.1.25",
            "http://192.168.1.25:80",
            "http://192.168.1.25:8787/api/health",
            "http://user@192.168.1.25:8787",
        ] {
            assert!(
                parse_nearby_endpoint(invalid).is_err(),
                "unsafe nearby endpoint was accepted: {invalid}"
            );
        }
    }

    #[test]
    fn health_compatibility_accepts_legacy_and_current_but_rejects_explicit_skew() {
        let legacy: SessionsHealthResponse =
            serde_json::from_str(r#"{"ok":true,"name":"sessionsd"}"#).unwrap();
        assert!(legacy.accepts_this_client());

        let current: SessionsHealthResponse = serde_json::from_str(
            r#"{"ok":true,"name":"sessionsd","compatibility":{"api":{"minimumClient":1,"maximumClient":1}}}"#,
        )
        .unwrap();
        assert!(current.accepts_this_client());

        for incompatible in [
            r#"{"ok":true,"name":"sessionsd","compatibility":{"api":{"minimumClient":2,"maximumClient":3}}}"#,
            r#"{"ok":true,"name":"sessionsd","compatibility":{"api":{"minimumClient":0,"maximumClient":0}}}"#,
        ] {
            let health: SessionsHealthResponse = serde_json::from_str(incompatible).unwrap();
            assert!(!health.accepts_this_client());
        }
    }

    #[test]
    fn tailscale_status_parses_online_peer_metadata() {
        let status: NativeTailscaleStatus = serde_json::from_str(
            r#"{
              "BackendState":"Running",
              "Peer":{
                "nodekey:example":{
                  "HostName":"Studio Mac",
                  "DNSName":"studio-mac.tail1234.ts.net.",
                  "OS":"macOS",
                  "Online":true
                }
              }
            }"#,
        )
        .unwrap();
        assert_eq!(status.backend_state, "Running");
        let peer = status.peer.get("nodekey:example").unwrap();
        assert_eq!(peer.host_name, "Studio Mac");
        assert!(peer.online);
    }

    #[test]
    fn native_pairing_probes_before_it_consumes_the_ticket() {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let address = listener.local_addr().unwrap();
        let server = std::thread::spawn(move || {
            let (mut health, _) = listener.accept().unwrap();
            let health_request = read_test_http_request(&mut health);
            assert!(health_request.starts_with("GET /api/health HTTP/1.1"));
            write_test_http_json(&mut health, "200 OK", r#"{"ok":true,"name":"sessionsd"}"#);

            let (mut claim, _) = listener.accept().unwrap();
            let claim_request = read_test_http_request(&mut claim);
            assert!(claim_request.starts_with("POST /api/pair/claim HTTP/1.1"));
            assert!(claim_request.contains(r#""ticket":"ticket-id.ticket-secret""#));
            write_test_http_json(
                &mut claim,
                "201 Created",
                r#"{"machine_id":"11111111-1111-4111-8111-111111111111","machine_name":"Studio Mac","device_id":"22222222-2222-4222-8222-222222222222","token":"device-token","name":"MacBook"}"#,
            );
        });

        let paired =
            claim_native_pairing_link(&format!("http://{address}/#pair=ticket-id.ticket-secret"))
                .unwrap();
        assert_eq!(paired.machine_name, "Studio Mac");
        assert_eq!(paired.name, "MacBook");
        assert_eq!(paired.endpoint, format!("http://{address}"));
        server.join().unwrap();
    }
}
