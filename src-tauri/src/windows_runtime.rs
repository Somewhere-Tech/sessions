use crate::{
    lifecycle::{IntegrationRemoval, RuntimeStatus},
    windows_cli_path::{describe_cli_path, merge_user_path, remove_user_path},
    windows_credentials::apply_owner_acl,
    windows_runner::{
        activate_stable_runner, file_sha256, stable_runner_is_present, stable_runner_path,
        sweep_retired_runners,
    },
    windows_supervisor::{
        port_release_timeout, readiness_timeout, require_settled_runtime,
        validate_supervisor_definition, SupervisorDefinition,
    },
};
use serde::Deserialize;
use std::{
    collections::{BTreeMap, BTreeSet},
    fs, io,
    net::TcpListener,
    os::windows::process::CommandExt,
    path::{Path, PathBuf},
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
// tauri.conf.json's identifier, which is what app_local_data_dir() appends to
// %LOCALAPPDATA%. Only uninstall needs it spelled out; every other caller has
// an AppHandle and asks Tauri.
const APP_IDENTIFIER: &str = "tech.somewhere.sessions";
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
    prepare_runtime_root(&root)?;
    // Collect the copies a previous update had to leave behind because a runner
    // was still executing them. Doing it here, before anything new is staged,
    // keeps the sweep away from the runner this run is about to pin.
    sweep_retired_runners(&root);
    let destination = root.join(&manifest.runtime_version);
    let installed = destination.exists();
    if installed {
        verify_manifest(&destination, &manifest)?;
        // A runtime staged by a build that predates the owner-scoped policy
        // still carries the inherited profile ACL. Re-assert it on every launch
        // so an update repairs an exposed installation instead of leaving it.
        apply_owner_acl(&destination, true)?;
    } else {
        stage_runtime(&source, &destination, &manifest)?;
    }

    // The supervisor definition names the pinned runner, so something has to be
    // at that path before the definition is written. If nothing is there yet
    // this is a first install and there is no live runner to protect; the
    // version swap itself is deferred until the new daemon is known healthy.
    let runner_digest = required_runner_digest(&manifest)?;
    if !stable_runner_is_present(&root) {
        activate_stable_runner(
            &root,
            &destination.join("sessions-runner.exe"),
            &runner_digest,
        )?;
    }

    let definition = supervisor_definition(&root, &destination, port);
    validate_supervisor_definition(&definition, &root)?;
    let previous = read_logon_supervisor()?;
    if let Some(previous) = previous.as_deref() {
        let registered = SupervisorDefinition::parse(previous)?;
        validate_supervisor_definition(&registered, &root)?;
    }
    let handed_off = match health(port)? {
        Some(existing) if runtime_versions_match(&existing.version, &manifest.runtime_version) => {
            wait_for_listening(port, &manifest.runtime_version)?;
            write_logon_supervisor(Some(&definition.command_line()))?;
            None
        }
        Some(existing) => {
            let previous = previous.as_deref().ok_or_else(|| {
                format!(
                    "Windows runtime {} is healthy, but its per-user logon definition is missing. Sessions staged {} but refused to replace a service without a rollback definition.",
                    existing.version, manifest.runtime_version
                )
            })?;
            Some(handoff(
                &root,
                previous,
                &definition,
                port,
                port,
                &existing,
                &manifest.runtime_version,
                "switching Windows runtimes",
            )?)
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
                .and_then(|_| wait_for_listening(port, &manifest.runtime_version).map(drop))
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
            None
        }
    };

    // Only now, with the daemon that will use it proven healthy, does the
    // pinned path change version. A failed update therefore leaves the restored
    // daemon paired with the runner it was already using.
    activate_stable_runner(
        &root,
        &destination.join("sessions-runner.exe"),
        &runner_digest,
    )?;

    let mut detail = match handed_off {
        Some(preserved) => {
            format!("Windows host runtime updated safely; {preserved} live sessions re-adopted")
        }
        None if installed => "Windows host runtime is installed and healthy".to_string(),
        None => "Windows host runtime installed; ConPTY runner and per-user supervisor are healthy"
            .to_string(),
    };
    describe_cli_path(&mut detail, install_cli_path(&root, &destination));

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
    move_port(app, current_port, requested_port, PortMove::Requested)
}

// The rollback half of a port change, for when the move succeeded but saving
// the new port did not.
//
// It deliberately does not re-run the gate reconfigure_port applies. That gate
// answers "may Sessions start changing this host?", and the answer was already
// yes moments ago; asking it again about a daemon that is mid-adoption can
// refuse, and a refusal here would strand the user with a saved port that does
// not match the daemon they are talking to — the wedged management plane the
// rest of this file exists to prevent. macOS rolls back from the baseline it
// already captured for the same reason.
pub fn restore_port(
    app: &AppHandle,
    current_port: u16,
    requested_port: u16,
) -> Result<RuntimeStatus, String> {
    move_port(app, current_port, requested_port, PortMove::Rollback)
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum PortMove {
    Requested,
    Rollback,
}

fn move_port(
    app: &AppHandle,
    current_port: u16,
    requested_port: u16,
    intent: PortMove,
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
        prepare_runtime_root(&root)?;
        stage_runtime(&source, &destination, &manifest)?;
    } else {
        verify_manifest(&destination, &manifest)?;
        apply_owner_acl(&destination, true)?;
    }

    // Same ordering as install(): the definition about to be written names the
    // pinned runner, so it has to exist before the definition is validated, and
    // its version only changes once the daemon that will use it is healthy.
    let runner_digest = required_runner_digest(&manifest)?;
    if !stable_runner_is_present(&root) {
        activate_stable_runner(
            &root,
            &destination.join("sessions-runner.exe"),
            &runner_digest,
        )?;
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
    let definition = supervisor_definition(&root, &destination, requested_port);
    validate_supervisor_definition(&definition, &root)?;
    let preserved = match intent {
        PortMove::Requested => handoff(
            &root,
            &previous,
            &definition,
            current_port,
            requested_port,
            &existing,
            &manifest.runtime_version,
            "changing the Windows host port",
        )?,
        PortMove::Rollback => handoff_settled(
            &root,
            &previous,
            &definition,
            current_port,
            requested_port,
            &existing,
            &manifest.runtime_version,
        )?,
    };

    activate_stable_runner(
        &root,
        &destination.join("sessions-runner.exe"),
        &runner_digest,
    )?;

    // Same failure, same disposition as install(): a CLI PATH problem is
    // something the user can act on and belongs in the status they read, not
    // only in a stderr line no packaged build has anywhere to print.
    let mut detail = format!(
        "Windows background service moved safely to localhost:{requested_port}; {preserved} live sessions re-adopted"
    );
    describe_cli_path(&mut detail, install_cli_path(&root, &destination));
    Ok(RuntimeStatus {
        state: "ready".to_string(),
        detail,
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

// %LOCALAPPDATA%\Sessions\runtime\<version>\sessionsd.exe is the binary the
// per-user logon supervisor launches at every sign-in. Created with a bare
// create_dir_all it simply inherits whatever %LOCALAPPDATA% grants, which on a
// machine with a relaxed or inherited profile ACL can include principals other
// than the signed-in user — anyone who can rewrite that file owns the daemon.
// macOS already narrows the equivalent root to 0o700; do the same here with the
// owner-scoped policy the credential vault uses. The DACL is protected and
// inheritable, so the versioned directories and binaries created underneath it
// come out owner-scoped too.
fn prepare_runtime_root(root: &Path) -> Result<(), String> {
    fs::create_dir_all(root)
        .map_err(|error| format!("create Windows runtime root {}: {error}", root.display()))?;
    apply_owner_acl(root, true)
}

fn stage_runtime(
    source: &Path,
    destination: &Path,
    manifest: &RuntimeManifest,
) -> Result<(), String> {
    let parent = destination
        .parent()
        .ok_or_else(|| format!("invalid Windows runtime path {}", destination.display()))?;
    // Version plus PID plus a monotonic suffix, exactly as lifecycle.rs names
    // its staging directory. A bare PID is not a unique name — Windows recycles
    // PIDs, and two Sessions processes can stage the same version at once — and
    // the old "clear whatever is already there" step made that collision
    // destructive: it deleted a directory another process was mid-copy into and
    // was about to rename onto the live runtime path. A name that cannot
    // collide removes the need to clear anything.
    let staging = parent.join(format!(
        ".staging-{}-{}-{}",
        manifest.runtime_version,
        std::process::id(),
        crate::lifecycle::unique_suffix()
    ));
    fs::create_dir(&staging).map_err(|error| {
        format!(
            "create Windows runtime staging {}: {error}",
            staging.display()
        )
    })?;
    // Narrow the staging directory before any byte is copied into it, so the
    // binaries are never briefly writable by anyone the parent happened to
    // grant. They inherit this policy as they are created.
    let result = apply_owner_acl(&staging, true).and_then(|()| {
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
        })?;
        // The rename carries the staging DACL across, but the activated
        // directory is the one that matters; assert the policy on it directly
        // rather than trusting that it travelled.
        apply_owner_acl(destination, true)
    });
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

// The daemon is versioned; the runner is not. SESSIONS_RUNNER is the path the
// daemon hands to every session it starts, and a live runner holds its own
// executable open for as long as it runs, so naming the versioned copy here
// would tie every future session to a directory that the next update replaces.
// Point at the pinned path instead and let windows_runner swap the bytes.
fn supervisor_definition(managed_root: &Path, runtime: &Path, port: u16) -> SupervisorDefinition {
    SupervisorDefinition::new(
        &runtime.join("sessionsd.exe"),
        port,
        &stable_runner_path(managed_root),
    )
}

fn required_runner_digest(manifest: &RuntimeManifest) -> Result<String, String> {
    manifest
        .binaries
        .get("sessions-runner.exe")
        .cloned()
        .ok_or_else(|| "the Windows runtime manifest is missing sessions-runner.exe".to_string())
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
            // Reached both by a failed-start rollback and by uninstall, so the
            // wording has to be true of "there should be no value here" rather
            // than of either caller's story.
            Err(error) => Err(format!(
                "remove the Windows Sessions logon definition: {error}"
            )),
        },
    }
}

// Replace the registered supervisor, preserving every live session across the
// change.
//
// This used to refuse outright whenever anything was live, which meant a
// Windows host running the workload the product exists for could never take an
// update or move its port. The refusal bought nothing: stopping the supervisor
// does not stop a runner — runtime/cmd/sessionsd/supervisor_windows.go owns only
// the daemon process, and runners are started detached — so the sessions it
// claimed to protect were never at risk from the operation it blocked. They are
// at risk from a daemon that comes back and fails to re-adopt them, and the
// answer to that is the one macOS already uses: capture the exact live baseline
// first, require every one of those IDs back before calling the change good,
// and otherwise restore the previous runtime and say so.
//
// The discovery half of the old gate stays. A baseline captured while the
// daemon is still finding runners is not a baseline, and rolling forward on a
// partial one would license exactly the loss this is here to prevent.
#[allow(clippy::too_many_arguments)]
fn handoff(
    managed_root: &Path,
    previous_entry: &str,
    next: &SupervisorDefinition,
    current_port: u16,
    requested_port: u16,
    current_health: &Health,
    requested_version: &str,
    operation: &str,
) -> Result<usize, String> {
    require_settled_runtime(current_health.discovering, operation)?;
    handoff_settled(
        managed_root,
        previous_entry,
        next,
        current_port,
        requested_port,
        current_health,
        requested_version,
    )
}

#[allow(clippy::too_many_arguments)]
fn handoff_settled(
    managed_root: &Path,
    previous_entry: &str,
    next: &SupervisorDefinition,
    current_port: u16,
    requested_port: u16,
    current_health: &Health,
    requested_version: &str,
) -> Result<usize, String> {
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
    let baseline = fetch_live_sessions(current_port)?;

    stop_supervisor(&next.daemon)?;
    wait_for_port_release(current_port, baseline.len())?;
    let next_entry = next.command_line();
    let update_result = write_logon_supervisor(Some(&next_entry))
        .and_then(|_| start_supervisor(next))
        .and_then(|_| wait_for_ready(requested_port, requested_version, &baseline).map(drop));
    if update_result.is_ok() {
        return Ok(baseline.len());
    }

    let update_error = update_result.unwrap_err();
    let rollback_result = (|| -> Result<(), String> {
        stop_supervisor_if_present(&next.daemon, requested_port)?;
        wait_for_port_release(requested_port, baseline.len())?;
        write_logon_supervisor(Some(previous_entry))?;
        start_supervisor(&previous)?;
        wait_for_ready(current_port, &current_health.version, &baseline)?;
        Ok(())
    })();
    match rollback_result {
        Ok(()) => Err(format!(
            "Sessions rejected the Windows supervisor handoff and restored runtime {} on localhost:{current_port}: {update_error}",
            current_health.version
        )),
        Err(rollback_error) => Err(format!(
            "Windows supervisor handoff failed: {update_error}; restoring runtime {} on localhost:{current_port} also failed: {rollback_error}. Your runners are still running; reopen Sessions to reconnect them.",
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

// A cold start, or a daemon that is already the right version: the service is
// ready as soon as it answers. Runner discovery may still be running and on a
// machine with a large retained fleet it will be, for minutes. Waiting for it
// here used to time out at a flat thirty seconds and then *stop the daemon that
// was mid-recovery* — reporting a healthy service as failed and making the
// report come true. macOS makes the same distinction for the same reason: see
// wait_until_listening in lifecycle.rs.
fn wait_for_listening(port: u16, runtime_version: &str) -> Result<Health, String> {
    let deadline = Instant::now() + Duration::from_secs(30);
    let mut last_error = "no response".to_string();
    loop {
        match health(port) {
            Ok(Some(response)) if runtime_versions_match(&response.version, runtime_version) => {
                return Ok(response)
            }
            Ok(Some(response)) => {
                return Err(format!(
                    "Windows Sessions daemon version is {}, expected {}",
                    response.version, runtime_version
                ));
            }
            Ok(None) => last_error = "no response".to_string(),
            Err(error) => last_error = error,
        }
        if Instant::now() >= deadline {
            return Err(format!(
                "Windows Sessions background service did not start listening on 127.0.0.1:{port} within 30s: {last_error}"
            ));
        }
        thread::sleep(Duration::from_millis(200));
    }
}

// A handoff, where the question is not "is it listening" but "did every session
// that was live before the change come back". The budget scales with the
// baseline because runners are re-adopted serially; a flat deadline calls a
// fleet-sized recovery a failure and triggers a rollback of something that was
// working.
fn wait_for_ready(
    port: u16,
    runtime_version: &str,
    baseline: &BTreeSet<String>,
) -> Result<Health, String> {
    let timeout = readiness_timeout(baseline.len());
    let deadline = Instant::now() + timeout;
    let mut last_error = "no response".to_string();
    loop {
        match health(port) {
            Ok(Some(response)) if runtime_versions_match(&response.version, runtime_version) => {
                match fetch_live_sessions(port) {
                    Ok(current) => {
                        let missing = baseline.difference(&current).cloned().collect::<Vec<_>>();
                        if missing.is_empty() {
                            return Ok(response);
                        }
                        last_error = format!(
                            "{} live sessions were not re-adopted: {}",
                            missing.len(),
                            missing.join(", ")
                        );
                    }
                    Err(error) => last_error = error,
                }
            }
            Ok(Some(response)) => {
                return Err(format!(
                    "Windows Sessions daemon version is {}, expected {}",
                    response.version, runtime_version
                ));
            }
            Ok(None) => last_error = "no response".to_string(),
            Err(error) => last_error = error,
        }
        if Instant::now() >= deadline {
            return Err(format!(
                "Windows Sessions background service did not become ready on 127.0.0.1:{port} within {}s: {last_error}",
                timeout.as_secs()
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

fn fetch_live_sessions(port: u16) -> Result<BTreeSet<String>, String> {
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

fn wait_for_port_release(port: u16, live_sessions: usize) -> Result<(), String> {
    let timeout = port_release_timeout(live_sessions);
    let deadline = Instant::now() + timeout;
    while Instant::now() < deadline {
        if port_is_available(port) {
            return Ok(());
        }
        thread::sleep(Duration::from_millis(50));
    }
    Err(format!(
        "localhost:{port} remained occupied {}s after the Windows supervisor was asked to stand down",
        timeout.as_secs()
    ))
}

// Uninstall. See lifecycle::IntegrationRemoval for what this may and may not
// touch and why; this is only the Windows half of that decision.
pub fn remove_integration() -> IntegrationRemoval {
    let mut removal = IntegrationRemoval::default();
    match read_logon_supervisor() {
        Ok(None) => removal.absent(&format!(
            "per-user logon supervisor ({LOGON_RUN_KEY}\\{LOGON_VALUE_NAME})"
        )),
        Ok(Some(_)) => match write_logon_supervisor(None) {
            Ok(()) => removal.removed(&format!(
                "per-user logon supervisor ({LOGON_RUN_KEY}\\{LOGON_VALUE_NAME})"
            )),
            Err(error) => removal.problem(&error),
        },
        Err(error) => removal.problem(&error),
    }

    match managed_runtime_root() {
        Ok(root) => match remove_cli_path(&root) {
            Ok(true) => removal.removed("Sessions entry in the per-user PATH"),
            Ok(false) => removal.absent("Sessions entry in the per-user PATH"),
            Err(error) => removal.problem(&error),
        },
        Err(error) => removal.problem(&error),
    }

    removal.kept(
        "the staged Sessions runtime, saved port, paired-machine credentials, and every session record",
    );
    removal
}

// Uninstall runs from the NSIS uninstaller, without a Tauri AppHandle to
// resolve paths with. %LOCALAPPDATA%\<identifier> is exactly what
// app_local_data_dir() returns on Windows, so derive it rather than guess at a
// PATH entry's shape — an uninstaller that matched entries loosely would be
// editing components it does not own.
fn managed_runtime_root() -> Result<PathBuf, String> {
    let local = std::env::var_os("LOCALAPPDATA")
        .filter(|value| !value.is_empty())
        .ok_or_else(|| {
            "LOCALAPPDATA is unset, so Sessions could not identify its own PATH entry to remove. Remove the Sessions runtime directory from the user PATH by hand.".to_string()
        })?;
    Ok(PathBuf::from(local).join(APP_IDENTIFIER).join("runtime"))
}

fn remove_cli_path(managed_root: &Path) -> Result<bool, String> {
    let environment = match RegKey::predef(HKEY_CURRENT_USER)
        .open_subkey_with_flags("Environment", KEY_QUERY_VALUE | KEY_SET_VALUE)
    {
        Ok(key) => key,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(false),
        Err(error) => return Err(format!("open the signed-in user's environment: {error}")),
    };
    let (current, value_type) = match environment.get_raw_value("Path") {
        Ok(value) if matches!(value.vtype, REG_SZ | REG_EXPAND_SZ) => (
            String::from_reg_value(&value)
                .map_err(|error| format!("read the signed-in user's PATH: {error}"))?,
            value.vtype,
        ),
        Ok(value) => {
            return Err(format!(
                "the signed-in user's PATH has unsupported registry type {:?}; Sessions left it untouched",
                value.vtype
            ));
        }
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(false),
        Err(error) => return Err(format!("read the signed-in user's PATH: {error}")),
    };
    let updated = remove_user_path(&current, managed_root);
    if updated == current {
        return Ok(false);
    }
    // Writing an empty string back would leave a per-user PATH that exists only
    // to say nothing. If Sessions' entry was the whole value, take the value
    // with it; the machine PATH is untouched either way.
    if updated.is_empty() {
        match environment.delete_value("Path") {
            Ok(()) => {}
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
            Err(error) => return Err(format!("clear the signed-in user's PATH: {error}")),
        }
    } else {
        let mut value = updated.to_reg_value();
        value.vtype = value_type;
        environment
            .set_raw_value("Path", &value)
            .map_err(|error| format!("write the signed-in user's PATH: {error}"))?;
    }
    broadcast_environment_change()?;
    Ok(true)
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
