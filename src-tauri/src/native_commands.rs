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
async fn native_mobile_bonjour_discover() -> Result<Vec<NativeMobileBonjourPeer>, String> {
    #[cfg(desktop)]
    return Err("Phone Bonjour discovery is available only on iOS and Android.".to_string());

    #[cfg(mobile)]
    tauri::async_runtime::spawn_blocking(mobile_discovery::browse_sessions)
        .await
        .map_err(|error| format!("Bonjour discovery worker failed: {error}"))?
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
