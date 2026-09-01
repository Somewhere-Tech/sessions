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

// These files remain in this module so Tauri command registration, desktop /
// mobile cfgs, and the native protocol surface keep exactly the same scope.
// The split is organizational only: each file owns one bounded concern.
include!("native_windows.rs");
include!("native_commands.rs");
include!("native_external.rs");
include!("native_connections.rs");
include!("native_tray.rs");

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

include!("lib_tests.rs");
