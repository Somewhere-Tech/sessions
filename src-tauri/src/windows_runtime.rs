use crate::{
    lifecycle::RuntimeStatus,
    windows_cli_path::merge_user_path,
    windows_supervisor::{require_idle, SupervisorDefinition},
};
use serde::Deserialize;
use sha2::{Digest, Sha256};
use std::{
    collections::BTreeMap,
    fs,
    io::{self, Read},
    net::TcpListener,
    os::windows::process::CommandExt,
    path::Path,
    process::Command,
    thread,
    time::{Duration, Instant},
};
use tauri::{AppHandle, Manager};
use windows_sys::Win32::System::Threading::CREATE_BREAKAWAY_FROM_JOB;
use windows_sys::Win32::UI::WindowsAndMessaging::{
    SendMessageTimeoutW, HWND_BROADCAST, SMTO_ABORTIFHUNG, WM_SETTINGCHANGE,
};
use winreg::{
    enums::{HKEY_CURRENT_USER, KEY_QUERY_VALUE, KEY_SET_VALUE, REG_EXPAND_SZ, REG_SZ},
    types::{FromRegValue, ToRegValue},
    RegKey,
};

const SERVICE_LABEL: &str = "tech.somewhere.sessions.daemon";
const LOGON_RUN_KEY: &str = r"Software\Microsoft\Windows\CurrentVersion\Run";
const LOGON_VALUE_NAME: &str = "Somewhere Sessions";
const REQUIRED_BINARIES: [&str; 3] = ["sessions.exe", "sessionsd.exe", "sessions-runner.exe"];
const CREATE_NEW_PROCESS_GROUP: u32 = 0x0000_0200;
const CREATE_NO_WINDOW: u32 = 0x0800_0000;

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct RuntimeManifest {
    schema_version: u32,
    runtime_version: String,
    target: String,
    binaries: BTreeMap<String, String>,
}

#[derive(Debug, Deserialize)]
struct Health {
    ok: bool,
    name: String,
    version: String,
    #[serde(default)]
    discovering: bool,
}

#[derive(Debug, Deserialize)]
struct SessionIdentity {
    id: String,
}

#[derive(Debug, Deserialize)]
struct SessionEnvelope {
    sessions: Vec<SessionIdentity>,
}

pub fn install(app: &AppHandle, port: u16) -> Result<RuntimeStatus, String> {
    let resources = app
        .path()
        .resource_dir()
        .map_err(|error| format!("resolve Sessions resources: {error}"))?;
    let source = resources.join("runtime");
    let manifest = read_manifest(&source)?;
    verify_manifest(&source, &manifest)?;

    let root = app
        .path()
        .app_local_data_dir()
        .map_err(|error| format!("resolve Windows Sessions data directory: {error}"))?
        .join("runtime");
    fs::create_dir_all(&root)
        .map_err(|error| format!("create Windows runtime root {}: {error}", root.display()))?;
    let destination = root.join(&manifest.runtime_version);
    let installed = destination.exists();
    if installed {
        verify_manifest(&destination, &manifest)?;
    } else {
        stage_runtime(&source, &destination, &manifest)?;
    }

    let definition = supervisor_definition(&destination, port);
    validate_supervisor_definition(&definition, &root)?;
    let previous = read_logon_supervisor()?;
    if let Some(previous) = previous.as_deref() {
        let registered = SupervisorDefinition::parse(previous)?;
        validate_supervisor_definition(&registered, &root)?;
    }
    let handed_off = match health(port)? {
        Some(existing) if runtime_versions_match(&existing.version, &manifest.runtime_version) => {
            wait_for_health(port, &manifest.runtime_version)?;
            write_logon_supervisor(Some(&definition.command_line()))?;
            false
        }
        Some(existing) => {
            let previous = previous.as_deref().ok_or_else(|| {
                format!(
                    "Windows runtime {} is healthy, but its per-user logon definition is missing. Sessions staged {} but refused to replace a service without a rollback definition.",
                    existing.version, manifest.runtime_version
                )
            })?;
            handoff_idle(
                &root,
                previous,
                &definition,
                port,
                port,
                &existing,
                &manifest.runtime_version,
                "switching Windows runtimes",
            )?;
            true
        }
        None => {
            if let Some(previous) = previous.as_deref() {
                let registered = SupervisorDefinition::parse(previous)?;
                if registered != definition {
                    return Err(
                        "The registered Windows supervisor is not healthy, so Sessions cannot prove the host is idle or replace its runtime safely. Sign out and back in to restore the registered supervisor, then reopen Sessions."
                            .to_string(),
                    );
                }
            }
            write_logon_supervisor(Some(&definition.command_line()))?;
            if let Err(start_error) = start_supervisor(&definition)
                .and_then(|_| wait_for_health(port, &manifest.runtime_version).map(drop))
            {
                let _ = stop_supervisor_if_present(&definition.daemon, port);
                let restore = write_logon_supervisor(previous.as_deref());
                return match restore {
                    Ok(()) => Err(format!(
                        "Windows Sessions supervisor did not start; the prior logon definition was restored: {start_error}"
                    )),
                    Err(restore_error) => Err(format!(
                        "Windows Sessions supervisor did not start: {start_error}; restoring the prior logon definition also failed: {restore_error}"
                    )),
                };
            }
            false
        }
    };

    let cli_path = install_cli_path(&root, &destination);
    let mut detail = if handed_off {
        "Windows host runtime changed safely while no sessions were live".to_string()
    } else if installed {
        "Windows host runtime is installed and healthy".to_string()
    } else {
        "Windows host runtime installed; ConPTY runner and per-user supervisor are healthy"
            .to_string()
    };
    match cli_path {
        Ok(()) => {
            detail.push_str(". Open a new terminal to use the versioned sessions CLI from PATH");
        }
        Err(error) => {
            detail.push_str(&format!(
                ". The host remains healthy, but CLI PATH setup needs attention: {error}. Restart Sessions to retry"
            ));
        }
    }

    Ok(RuntimeStatus {
        state: "ready".to_string(),
        detail,
        service_label: SERVICE_LABEL.to_string(),
        runtime_version: Some(manifest.runtime_version),
    })
}

pub fn reconfigure_port(
    app: &AppHandle,
    current_port: u16,
    requested_port: u16,
) -> Result<RuntimeStatus, String> {
    if current_port == requested_port {
        return install(app, current_port);
    }
    let resources = app
        .path()
        .resource_dir()
        .map_err(|error| format!("resolve Sessions resources: {error}"))?;
    let source = resources.join("runtime");
    let manifest = read_manifest(&source)?;
    verify_manifest(&source, &manifest)?;
    let root = app
        .path()
        .app_local_data_dir()
        .map_err(|error| format!("resolve Windows Sessions data directory: {error}"))?
        .join("runtime");
    let destination = root.join(&manifest.runtime_version);
    if !destination.exists() {
        fs::create_dir_all(&root)
            .map_err(|error| format!("create Windows runtime root {}: {error}", root.display()))?;
        stage_runtime(&source, &destination, &manifest)?;
    } else {
        verify_manifest(&destination, &manifest)?;
    }

    ensure_port_available(requested_port)?;
    let existing = health(current_port)?.ok_or_else(|| {
        format!(
            "Windows Sessions is not healthy on localhost:{current_port}. Reopen Sessions to repair the current background service before changing its port."
        )
    })?;
    let previous = read_logon_supervisor()?.ok_or_else(|| {
        "The current Windows supervisor logon definition is missing; refusing a port change without a rollback definition."
            .to_string()
    })?;
    let definition = supervisor_definition(&destination, requested_port);
    validate_supervisor_definition(&definition, &root)?;
    handoff_idle(
        &root,
        &previous,
        &definition,
        current_port,
        requested_port,
        &existing,
        &manifest.runtime_version,
        "changing the Windows host port",
    )?;
    if let Err(error) = install_cli_path(&root, &destination) {
        eprintln!("Sessions CLI PATH integration after port handoff: {error}");
    }
    Ok(RuntimeStatus {
        state: "ready".to_string(),
        detail: format!(
            "Windows background service moved safely to localhost:{requested_port} while no sessions were live"
        ),
        service_label: SERVICE_LABEL.to_string(),
        runtime_version: Some(manifest.runtime_version),
    })
}

fn read_manifest(directory: &Path) -> Result<RuntimeManifest, String> {
    let path = directory.join("runtime-manifest.json");
    let encoded = fs::read(&path)
        .map_err(|error| format!("read bundled Windows runtime {}: {error}", path.display()))?;
    serde_json::from_slice(&encoded)
        .map_err(|error| format!("parse bundled Windows runtime {}: {error}", path.display()))
}

fn verify_manifest(directory: &Path, manifest: &RuntimeManifest) -> Result<(), String> {
    if manifest.schema_version != 1 {
        return Err(format!(
            "unsupported Windows runtime manifest schema {}",
            manifest.schema_version
        ));
    }
    if manifest.target != "windows-amd64" {
        return Err(format!(
            "bundled Windows runtime target must be windows-amd64, got {:?}",
            manifest.target
        ));
    }
    if manifest.runtime_version.is_empty()
        || manifest.runtime_version.len() > 128
        || !manifest.runtime_version.chars().all(|character| {
            character.is_ascii_alphanumeric() || matches!(character, '.' | '_' | '-')
        })
    {
        return Err(format!(
            "bundled Windows runtime version is unsafe: {:?}",
            manifest.runtime_version
        ));
    }
    if manifest.binaries.len() != REQUIRED_BINARIES.len() {
        return Err("Windows runtime manifest must contain exactly sessions.exe, sessionsd.exe, and sessions-runner.exe".to_string());
    }
    for binary in REQUIRED_BINARIES {
        let expected = manifest
            .binaries
            .get(binary)
            .ok_or_else(|| format!("Windows runtime manifest is missing {binary}"))?;
        if expected.len() != 64
            || !expected
                .chars()
                .all(|character| character.is_ascii_hexdigit())
        {
            return Err(format!("Windows runtime digest for {binary} is invalid"));
        }
        let actual = file_sha256(&directory.join(binary))?;
        if !actual.eq_ignore_ascii_case(expected) {
            return Err(format!(
                "Windows runtime digest mismatch for {}",
                directory.join(binary).display()
            ));
        }
    }
    Ok(())
}

fn stage_runtime(
    source: &Path,
    destination: &Path,
    manifest: &RuntimeManifest,
) -> Result<(), String> {
    let parent = destination
        .parent()
        .ok_or_else(|| format!("invalid Windows runtime path {}", destination.display()))?;
    let staging = parent.join(format!(
        ".staging-{}-{}",
        manifest.runtime_version,
        std::process::id()
    ));
    if staging.exists() {
        fs::remove_dir_all(&staging)
            .map_err(|error| format!("clear stale Windows runtime staging: {error}"))?;
    }
    fs::create_dir(&staging).map_err(|error| {
        format!(
            "create Windows runtime staging {}: {error}",
            staging.display()
        )
    })?;
    let result = (|| {
        for binary in REQUIRED_BINARIES {
            fs::copy(source.join(binary), staging.join(binary)).map_err(|error| {
                format!("copy bundled Windows runtime binary {binary}: {error}")
            })?;
        }
        fs::copy(
            source.join("runtime-manifest.json"),
            staging.join("runtime-manifest.json"),
        )
        .map_err(|error| format!("copy Windows runtime manifest: {error}"))?;
        verify_manifest(&staging, manifest)?;
        fs::rename(&staging, destination).map_err(|error| {
            format!(
                "activate immutable Windows runtime {}: {error}",
                destination.display()
            )
        })
    })();
    if result.is_err() {
        let _ = fs::remove_dir_all(&staging);
    }
    result
}

fn install_cli_path(managed_root: &Path, runtime: &Path) -> Result<(), String> {
    if !runtime.starts_with(managed_root) || !runtime.join("sessions.exe").is_file() {
        return Err(format!(
            "refusing an unmanaged Windows CLI directory {}",
            runtime.display()
        ));
    }

    let (environment, _) = RegKey::predef(HKEY_CURRENT_USER)
        .create_subkey_with_flags("Environment", KEY_QUERY_VALUE | KEY_SET_VALUE)
        .map_err(|error| format!("open the signed-in user's environment: {error}"))?;
    let (current, value_type) = match environment.get_raw_value("Path") {
        Ok(value) if matches!(value.vtype, REG_SZ | REG_EXPAND_SZ) => (
            String::from_reg_value(&value)
                .map_err(|error| format!("read the signed-in user's PATH: {error}"))?,
            value.vtype,
        ),
        Ok(value) => {
            return Err(format!(
                "the signed-in user's PATH has unsupported registry type {:?}",
                value.vtype
            ));
        }
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            (String::new(), REG_EXPAND_SZ)
        }
        Err(error) => {
            return Err(format!("read the signed-in user's PATH: {error}"));
        }
    };
    let updated = merge_user_path(&current, managed_root, runtime);
    if updated != current {
        if updated.encode_utf16().count() + 1 > 32_767 {
            return Err("the signed-in user's PATH is too long to add Sessions safely".to_string());
        }

        let mut value = updated.to_reg_value();
        value.vtype = value_type;
        environment
            .set_raw_value("Path", &value)
            .map_err(|error| format!("write the signed-in user's PATH: {error}"))?;
    }
    broadcast_environment_change()
}

fn broadcast_environment_change() -> Result<(), String> {
    let environment: Vec<u16> = "Environment".encode_utf16().chain(Some(0)).collect();
    let mut result = 0_usize;
    let sent = unsafe {
        SendMessageTimeoutW(
            HWND_BROADCAST,
            WM_SETTINGCHANGE,
            0,
            environment.as_ptr() as isize,
            SMTO_ABORTIFHUNG,
            5_000,
            &mut result,
        )
    };
    if sent == 0 {
        return Err(format!(
            "notify Windows that PATH changed: {}",
            std::io::Error::last_os_error()
        ));
    }
    Ok(())
}

fn supervisor_definition(runtime: &Path, port: u16) -> SupervisorDefinition {
    SupervisorDefinition::new(
        &runtime.join("sessionsd.exe"),
        port,
        &runtime.join("sessions-runner.exe"),
    )
}

fn validate_supervisor_definition(
    definition: &SupervisorDefinition,
    managed_root: &Path,
) -> Result<(), String> {
    for (label, path) in [
        ("daemon", definition.daemon.as_path()),
        ("runner", definition.runner.as_path()),
    ] {
        if !path.starts_with(managed_root) || !path.is_file() {
            return Err(format!(
                "the Windows supervisor {label} is not an immutable Sessions-managed file: {}",
                path.display()
            ));
        }
    }
    if definition.daemon.parent() != definition.runner.parent() {
        return Err(
            "the Windows supervisor daemon and runner must use one immutable runtime".to_string(),
        );
    }
    Ok(())
}

fn read_logon_supervisor() -> Result<Option<String>, String> {
    let key = match RegKey::predef(HKEY_CURRENT_USER)
        .open_subkey_with_flags(LOGON_RUN_KEY, KEY_QUERY_VALUE)
    {
        Ok(key) => key,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(error) => {
            return Err(format!(
                "open the signed-in user's logon applications: {error}"
            ))
        }
    };
    match key.get_raw_value(LOGON_VALUE_NAME) {
        Ok(value) if value.vtype == REG_SZ => String::from_reg_value(&value)
            .map(Some)
            .map_err(|error| format!("read the Windows Sessions logon definition: {error}")),
        Ok(value) => Err(format!(
            "the Windows Sessions logon definition has unsupported registry type {:?}",
            value.vtype
        )),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(None),
        Err(error) => Err(format!(
            "read the Windows Sessions logon definition: {error}"
        )),
    }
}

fn write_logon_supervisor(value: Option<&str>) -> Result<(), String> {
    let (key, _) = RegKey::predef(HKEY_CURRENT_USER)
        .create_subkey_with_flags(LOGON_RUN_KEY, KEY_QUERY_VALUE | KEY_SET_VALUE)
        .map_err(|error| format!("open the signed-in user's logon applications: {error}"))?;
    match value {
        Some(value) => key
            .set_value(LOGON_VALUE_NAME, &value)
            .map_err(|error| format!("register Windows per-user Sessions supervisor: {error}")),
        None => match key.delete_value(LOGON_VALUE_NAME) {
            Ok(()) => Ok(()),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
            Err(error) => Err(format!(
                "restore the Windows Sessions logon definition: {error}"
            )),
        },
    }
}

#[allow(clippy::too_many_arguments)]
fn handoff_idle(
    managed_root: &Path,
    previous_entry: &str,
    next: &SupervisorDefinition,
    current_port: u16,
    requested_port: u16,
    current_health: &Health,
    requested_version: &str,
    operation: &str,
) -> Result<(), String> {
    let previous = SupervisorDefinition::parse(previous_entry)?;
    validate_supervisor_definition(&previous, managed_root)?;
    if previous.port != current_port {
        return Err(format!(
            "the registered Windows supervisor uses port {}, but Sessions is connected to {current_port}; refusing a handoff without an exact rollback definition",
            previous.port
        ));
    }
    if requested_port != current_port {
        ensure_port_available(requested_port)?;
    }
    let live_sessions = fetch_live_sessions(current_port)?;
    require_idle(current_health.discovering, live_sessions.len(), operation)?;

    stop_supervisor(&next.daemon)?;
    wait_for_port_release(current_port)?;
    let next_entry = next.command_line();
    let update_result = write_logon_supervisor(Some(&next_entry))
        .and_then(|_| start_supervisor(next))
        .and_then(|_| wait_for_health(requested_port, requested_version).map(drop));
    if update_result.is_ok() {
        return Ok(());
    }

    let update_error = update_result.unwrap_err();
    let rollback_result = (|| -> Result<(), String> {
        stop_supervisor_if_present(&next.daemon, requested_port)?;
        wait_for_port_release(requested_port)?;
        write_logon_supervisor(Some(previous_entry))?;
        start_supervisor(&previous)?;
        wait_for_health(current_port, &current_health.version)?;
        Ok(())
    })();
    match rollback_result {
        Ok(()) => Err(format!(
            "Sessions rejected the Windows supervisor handoff and restored runtime {} on localhost:{current_port}: {update_error}",
            current_health.version
        )),
        Err(rollback_error) => Err(format!(
            "Windows supervisor handoff failed: {update_error}; restoring runtime {} on localhost:{current_port} also failed: {rollback_error}",
            current_health.version
        )),
    }
}

fn start_supervisor(definition: &SupervisorDefinition) -> Result<(), String> {
    let mut command = Command::new(&definition.daemon);
    command
        .arg("--supervise")
        .arg("--port")
        .arg(definition.port.to_string())
        .arg("--runner")
        .arg(&definition.runner)
        .creation_flags(detached_process_creation_flags())
        .spawn()
        .map(drop)
        .map_err(describe_supervisor_start_error)
}

fn detached_process_creation_flags() -> u32 {
    CREATE_BREAKAWAY_FROM_JOB | CREATE_NEW_PROCESS_GROUP | CREATE_NO_WINDOW
}

fn describe_supervisor_start_error(error: io::Error) -> String {
    if error.raw_os_error() == Some(5) {
        return format!(
            "Windows denied the independent Sessions supervisor start, so no background host was started. A restrictive parent Job Object can forbid the required breakaway: close any installer-launched Sessions window and reopen Sessions from the Start menu, or sign out and back in, before retrying. If it still fails, verify this Windows user can run the staged Sessions runtime: {error}"
        );
    }
    format!("start Windows Sessions supervisor: {error}")
}

fn stop_supervisor(control_daemon: &Path) -> Result<(), String> {
    let output = Command::new(control_daemon)
        .arg("--supervise-stop")
        .creation_flags(CREATE_NO_WINDOW)
        .output()
        .map_err(|error| format!("request an idle Windows supervisor handoff: {error}"))?;
    if output.status.success() {
        return Ok(());
    }
    let detail = String::from_utf8_lossy(&output.stderr).trim().to_string();
    Err(if detail.is_empty() {
        "the Windows supervisor refused the idle handoff".to_string()
    } else {
        format!("the Windows supervisor refused the idle handoff: {detail}")
    })
}

fn stop_supervisor_if_present(control_daemon: &Path, port: u16) -> Result<(), String> {
    match stop_supervisor(control_daemon) {
        Ok(()) => Ok(()),
        Err(_) if port_is_available(port) => Ok(()),
        Err(error) => Err(error),
    }
}

fn wait_for_health(port: u16, runtime_version: &str) -> Result<Health, String> {
    let deadline = Instant::now() + Duration::from_secs(30);
    let mut last_error = "no response".to_string();
    loop {
        match health(port) {
            Ok(Some(response))
                if runtime_versions_match(&response.version, runtime_version)
                    && !response.discovering =>
            {
                return Ok(response)
            }
            Ok(Some(response)) if !runtime_versions_match(&response.version, runtime_version) => {
                return Err(format!(
                    "Windows Sessions daemon version is {}, expected {}",
                    response.version, runtime_version
                ));
            }
            Ok(Some(_)) => {
                last_error = "daemon is healthy but runner discovery is still active".to_string()
            }
            Ok(None) => last_error = "no response".to_string(),
            Err(error) => last_error = error,
        }
        if Instant::now() >= deadline {
            return Err(format!(
                "Windows Sessions background service did not become ready on 127.0.0.1:{port}: {last_error}"
            ));
        }
        thread::sleep(Duration::from_millis(200));
    }
}

fn runtime_versions_match(daemon: &str, manifest: &str) -> bool {
    daemon == manifest || manifest.starts_with(&format!("{daemon}-bin."))
}

fn health(port: u16) -> Result<Option<Health>, String> {
    let client = reqwest::blocking::Client::builder()
        .no_proxy()
        .connect_timeout(Duration::from_secs(1))
        .timeout(Duration::from_secs(2))
        .build()
        .map_err(|error| format!("build Windows Sessions health client: {error}"))?;
    let response = match client
        .get(format!("http://127.0.0.1:{port}/api/health"))
        .send()
    {
        Ok(response) => response,
        Err(error) if error.is_connect() => return Ok(None),
        Err(error) => {
            return Err(format!(
                "probe Windows Sessions on localhost:{port}: {error}"
            ))
        }
    };
    let response = response.error_for_status().map_err(|error| {
        format!("localhost:{port} answered, but not as a healthy Sessions service: {error}")
    })?;
    let health: Health = response.json().map_err(|error| {
        format!("localhost:{port} answered with an invalid Sessions health response: {error}")
    })?;
    if !health.ok || health.name != "sessionsd" {
        return Err(format!(
            "localhost:{port} is not a healthy Sessions daemon; refusing to replace it"
        ));
    }
    Ok(Some(health))
}

fn fetch_live_sessions(port: u16) -> Result<Vec<String>, String> {
    let client = reqwest::blocking::Client::builder()
        .no_proxy()
        .connect_timeout(Duration::from_secs(1))
        .timeout(Duration::from_secs(2))
        .build()
        .map_err(|error| format!("build Windows Sessions session client: {error}"))?;
    let response = client
        .get(format!("http://127.0.0.1:{port}/api/sessions"))
        .send()
        .and_then(|response| response.error_for_status())
        .and_then(|response| response.json::<SessionEnvelope>())
        .map_err(|error| format!("check live Windows sessions before handoff: {error}"))?;
    Ok(response
        .sessions
        .into_iter()
        .map(|session| session.id)
        .filter(|id| !id.is_empty())
        .collect())
}

fn ensure_port_available(port: u16) -> Result<(), String> {
    TcpListener::bind(("127.0.0.1", port))
        .map(drop)
        .map_err(|error| {
            format!("localhost:{port} is already in use. Choose another Windows host port: {error}")
        })
}

fn port_is_available(port: u16) -> bool {
    TcpListener::bind(("127.0.0.1", port)).is_ok()
}

fn wait_for_port_release(port: u16) -> Result<(), String> {
    let deadline = Instant::now() + Duration::from_secs(5);
    while Instant::now() < deadline {
        if port_is_available(port) {
            return Ok(());
        }
        thread::sleep(Duration::from_millis(50));
    }
    Err(format!(
        "localhost:{port} remained occupied after the idle Windows supervisor stopped"
    ))
}

fn file_sha256(path: &Path) -> Result<String, String> {
    let mut file =
        fs::File::open(path).map_err(|error| format!("open {}: {error}", path.display()))?;
    let mut hasher = Sha256::new();
    let mut buffer = [0_u8; 64 * 1024];
    loop {
        let count = file
            .read(&mut buffer)
            .map_err(|error| format!("read {}: {error}", path.display()))?;
        if count == 0 {
            break;
        }
        hasher.update(&buffer[..count]);
    }
    Ok(format!("{:x}", hasher.finalize()))
}

#[cfg(test)]
mod tests {
    use super::{
        describe_supervisor_start_error, detached_process_creation_flags,
        CREATE_BREAKAWAY_FROM_JOB, CREATE_NEW_PROCESS_GROUP, CREATE_NO_WINDOW,
    };
    use std::io;

    #[test]
    fn supervisor_start_requests_job_breakaway() {
        let flags = detached_process_creation_flags();
        assert_ne!(flags & CREATE_BREAKAWAY_FROM_JOB, 0);
        assert_ne!(flags & CREATE_NEW_PROCESS_GROUP, 0);
        assert_ne!(flags & CREATE_NO_WINDOW, 0);
    }

    #[test]
    fn denied_job_breakaway_has_an_instructional_error() {
        let message = describe_supervisor_start_error(io::Error::from_raw_os_error(5));
        assert!(message.contains("parent Job Object"));
        assert!(message.contains("Start menu"));
        assert!(message.contains("sign out and back in"));
    }
}
