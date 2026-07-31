use serde::{Deserialize, Serialize};
use std::{
    collections::{BTreeMap, BTreeSet},
    env, fs,
    io::Write,
    net::{SocketAddr, TcpListener, TcpStream},
    path::{Path, PathBuf},
    process::{Command, Output},
    thread,
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};
use tauri::{AppHandle, Manager};

const SERVICE_LABEL: &str = "tech.somewhere.sessions.daemon";
const LOOPBACK_HOST: &str = "127.0.0.1";
const DEFAULT_LOOPBACK_PORT: u16 = 8787;
const REQUIRED_BINARIES: [&str; 3] = ["sessions", "sessionsd", "sessions-runner"];

type LifecycleResult<T> = Result<T, String>;

#[derive(Clone, Debug, Default, Deserialize, Serialize)]
struct NativePreferences {
    port: Option<u16>,
}

#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct RuntimeStatus {
    pub state: String,
    pub detail: String,
    pub service_label: String,
    pub runtime_version: Option<String>,
}

impl RuntimeStatus {
    pub fn menu_label(&self) -> String {
        match self.state.as_str() {
            "ready" => "Background service: ready".to_string(),
            "starting" => "Background service: reconnecting".to_string(),
            "development" => "Background service: external (development)".to_string(),
            "client-only" => "Background service: runs on your Mac".to_string(),
            "disabled" => "Background service: automatic install disabled".to_string(),
            _ => "Background service: needs attention".to_string(),
        }
    }

    fn ready(outcome: InstallOutcome, runtime_version: String) -> Self {
        let detail = match outcome {
            InstallOutcome::Installed => "installed and healthy",
            InstallOutcome::Updated { preserved } => {
                return Self {
                    state: "ready".to_string(),
                    detail: format!("updated safely; {preserved} live sessions re-adopted"),
                    service_label: SERVICE_LABEL.to_string(),
                    runtime_version: Some(runtime_version),
                };
            }
            InstallOutcome::Current => "already installed and healthy",
        };
        Self {
            state: "ready".to_string(),
            detail: detail.to_string(),
            service_label: SERVICE_LABEL.to_string(),
            runtime_version: Some(runtime_version),
        }
    }

    fn informational(state: &str, detail: &str) -> Self {
        Self {
            state: state.to_string(),
            detail: detail.to_string(),
            service_label: SERVICE_LABEL.to_string(),
            runtime_version: None,
        }
    }

    fn failed(error: String) -> Self {
        Self {
            state: "error".to_string(),
            detail: error,
            service_label: SERVICE_LABEL.to_string(),
            runtime_version: None,
        }
    }
}

// Return a truthful first-frame status without touching launchd or waiting for
// runner adoption. The native window must be able to paint before a release
// update reconciles sessionsd: a large retained fleet can take minutes to
// re-adopt, while every runner continues independently in the background.
pub fn startup_status() -> RuntimeStatus {
    if cfg!(debug_assertions) && cfg!(target_os = "macos") {
        return RuntimeStatus::informational(
            "development",
            "debug builds use the separately managed development daemon",
        );
    }
    if cfg!(mobile) {
        return RuntimeStatus::informational(
            "client-only",
            "mobile clients connect to a Mac-hosted background service",
        );
    }
    if cfg!(not(any(target_os = "macos", target_os = "windows"))) {
        return RuntimeStatus::informational(
            "client-only",
            "this platform connects to a Sessions host",
        );
    }
    if env::var_os("SESSIONS_DISABLE_RUNTIME_INSTALL").is_some() {
        return RuntimeStatus::informational("disabled", "SESSIONS_DISABLE_RUNTIME_INSTALL is set");
    }
    RuntimeStatus::informational(
        "starting",
        "checking the local background service; agent sessions keep running",
    )
}

pub fn needs_background_reconcile(status: &RuntimeStatus) -> bool {
    status.state == "starting"
}

pub fn install_for_app(app: &AppHandle) -> RuntimeStatus {
    if cfg!(debug_assertions) && cfg!(target_os = "macos") {
        return RuntimeStatus::informational(
            "development",
            "debug builds use the separately managed development daemon",
        );
    }
    if cfg!(mobile) {
        return RuntimeStatus::informational(
            "client-only",
            "mobile clients connect to a Mac-hosted background service",
        );
    }
    if env::var_os("SESSIONS_DISABLE_RUNTIME_INSTALL").is_some() {
        return RuntimeStatus::informational("disabled", "SESSIONS_DISABLE_RUNTIME_INSTALL is set");
    }

    #[cfg(target_os = "macos")]
    {
        match RuntimeConfig::from_app(app).and_then(|config| install_runtime(&config)) {
            Ok((outcome, version)) => RuntimeStatus::ready(outcome, version),
            Err(error) => RuntimeStatus::failed(error),
        }
    }
    #[cfg(target_os = "windows")]
    {
        match crate::windows_runtime::install(
            app,
            configured_port(app).unwrap_or(DEFAULT_LOOPBACK_PORT),
        ) {
            Ok(status) => status,
            Err(error) => RuntimeStatus::failed(error),
        }
    }
    #[cfg(not(any(target_os = "macos", target_os = "windows")))]
    {
        let _ = app;
        RuntimeStatus::informational(
            "client-only",
            "this platform connects to a Mac-hosted background service",
        )
    }
}

pub fn default_port() -> u16 {
    DEFAULT_LOOPBACK_PORT
}

pub fn configured_port(app: &AppHandle) -> LifecycleResult<u16> {
    let path = preferences_path(app)?;
    let encoded = match fs::read(&path) {
        Ok(encoded) => encoded,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            return Ok(DEFAULT_LOOPBACK_PORT)
        }
        Err(error) => {
            return Err(format!(
                "read native connection settings {}: {error}",
                path.display()
            ))
        }
    };
    let preferences: NativePreferences = serde_json::from_slice(&encoded).map_err(|error| {
        format!(
            "parse native connection settings {}: {error}",
            path.display()
        )
    })?;
    let port = preferences.port.unwrap_or(DEFAULT_LOOPBACK_PORT);
    validate_port(port)?;
    Ok(port)
}

pub fn reconfigure_port(app: &AppHandle, port: u16) -> LifecycleResult<RuntimeStatus> {
    validate_port(port)?;
    if cfg!(debug_assertions) {
        return Err("port changes are available in an installed Sessions desktop app; development builds use a separately managed daemon".to_string());
    }
    if cfg!(mobile) {
        return Err("mobile clients do not own a local background-service port".to_string());
    }

    #[cfg(target_os = "macos")]
    {
        let old = RuntimeConfig::from_app(app)?;
        if old.port == port {
            let (outcome, version) = install_runtime(&old)?;
            return Ok(RuntimeStatus::ready(outcome, version));
        }
        ensure_port_available(port)?;
        let mut new = old.clone();
        new.port = port;
        let installed = stage_runtime(&new)?;
        let old_plist = fs::read(&old.plist_path).map_err(|error| {
            format!(
                "read existing background-service definition {}: {error}",
                old.plist_path.display()
            )
        })?;
        if !service_is_loaded(&old)? {
            return Err(format!(
                "{} is not loaded; reopen Sessions to repair the background service before changing its port",
                old.label
            ));
        }
        let new_plist = daemon_plist(&new, &installed.directory).into_bytes();
        let baseline = capture_baseline(&old)?;
        migrate_loaded_service(&old, &new, &old_plist, &new_plist, &baseline)?;
        if let Err(save_error) = save_configured_port(app, port) {
            let rollback = migrate_loaded_service(&new, &old, &new_plist, &old_plist, &baseline);
            return match rollback {
                Ok(()) => Err(format!("could not save the new Sessions port and rolled back safely: {save_error}")),
                Err(rollback_error) => Err(format!(
                    "could not save the new Sessions port: {save_error}; rolling the service back also failed: {rollback_error}"
                )),
            };
        }
        Ok(RuntimeStatus::ready(
            InstallOutcome::Updated {
                preserved: baseline.len(),
            },
            installed.manifest.runtime_version,
        ))
    }
    #[cfg(target_os = "windows")]
    {
        let current = configured_port(app)?;
        let status = crate::windows_runtime::reconfigure_port(app, current, port)?;
        if current == port {
            return Ok(status);
        }
        if let Err(save_error) = save_configured_port(app, port) {
            let rollback = crate::windows_runtime::reconfigure_port(app, port, current);
            return match rollback {
                Ok(_) => Err(format!(
                    "could not save the new Windows Sessions port and restored localhost:{current}: {save_error}"
                )),
                Err(rollback_error) => Err(format!(
                    "could not save the new Windows Sessions port: {save_error}; restoring localhost:{current} also failed: {rollback_error}"
                )),
            };
        }
        Ok(status)
    }
    #[cfg(not(any(target_os = "macos", target_os = "windows")))]
    {
        let _ = app;
        Err("this platform is a client and does not own a local Sessions daemon".to_string())
    }
}

#[derive(Clone, Debug)]
struct RuntimeConfig {
    source_dir: PathBuf,
    managed_root: PathBuf,
    cli_link_paths: Vec<PathBuf>,
    plist_path: PathBuf,
    log_path: PathBuf,
    label: String,
    domain: String,
    host: String,
    port: u16,
    launchctl: PathBuf,
    codesign: PathBuf,
    shasum: PathBuf,
    verify_signatures: bool,
    daemon_arguments: Vec<String>,
    environment: Vec<(String, String)>,
    health_timeout: Duration,
    health_timeout_per_session: Duration,
    health_timeout_cap: Duration,
    poll_interval: Duration,
}

impl RuntimeConfig {
    #[cfg(target_os = "macos")]
    fn from_app(app: &AppHandle) -> LifecycleResult<Self> {
        let home = env::var_os("HOME")
            .filter(|value| !value.is_empty())
            .map(PathBuf::from)
            .ok_or_else(|| {
                "Sessions cannot install its background service because HOME is unset".to_string()
            })?;
        let uid = command_text(Path::new("/usr/bin/id"), &["-u"])?;
        if uid.is_empty() || !uid.chars().all(|character| character.is_ascii_digit()) {
            return Err(format!(
                "Sessions could not determine the current macOS user id: {uid:?}"
            ));
        }
        let resources = app
            .path()
            .resource_dir()
            .map_err(|error| format!("resolve Sessions resources: {error}"))?;
        Ok(Self {
            source_dir: resources.join("runtime"),
            managed_root: home
                .join("Library")
                .join("Application Support")
                .join("Sessions")
                .join("runtime"),
            cli_link_paths: vec![
                PathBuf::from("/opt/homebrew/bin/sessions"),
                PathBuf::from("/usr/local/bin/sessions"),
                home.join(".local").join("bin").join("sessions"),
            ],
            plist_path: home
                .join("Library")
                .join("LaunchAgents")
                .join(format!("{SERVICE_LABEL}.plist")),
            log_path: home
                .join("Library")
                .join("Logs")
                .join("Sessions")
                .join("sessionsd.log"),
            label: SERVICE_LABEL.to_string(),
            domain: format!("gui/{uid}"),
            host: LOOPBACK_HOST.to_string(),
            port: configured_port(app)?,
            launchctl: PathBuf::from("/bin/launchctl"),
            codesign: PathBuf::from("/usr/bin/codesign"),
            shasum: PathBuf::from("/usr/bin/shasum"),
            verify_signatures: true,
            daemon_arguments: Vec::new(),
            environment: Vec::new(),
            // Existing runners are re-adopted serially. A successful attach
            // may consume a two-second HELLO wait plus the initial ten-second
            // replay window; failed probes also retry. Budget that observed
            // startup work instead of imposing one fixed deadline on every
            // fleet size.
            health_timeout: Duration::from_secs(30),
            health_timeout_per_session: Duration::from_secs(15),
            health_timeout_cap: Duration::from_secs(5 * 60),
            poll_interval: Duration::from_millis(200),
        })
    }

    fn service_target(&self) -> String {
        format!("{}/{}", self.domain, self.label)
    }

    fn health_url(&self) -> String {
        format!("http://{}:{}/api/health", self.host, self.port)
    }

    fn sessions_url(&self) -> String {
        format!("http://{}:{}/api/sessions", self.host, self.port)
    }
}

fn validate_port(port: u16) -> LifecycleResult<()> {
    if port < 1024 {
        return Err("Sessions port must be between 1024 and 65535".to_string());
    }
    Ok(())
}

fn ensure_port_available(port: u16) -> LifecycleResult<()> {
    TcpListener::bind((LOOPBACK_HOST, port))
        .map(drop)
        .map_err(|error| format!("port {port} is already in use on {LOOPBACK_HOST}: {error}"))
}

fn preferences_path(app: &AppHandle) -> LifecycleResult<PathBuf> {
    app.path()
        .app_config_dir()
        .map(|directory| directory.join("connections.json"))
        .map_err(|error| format!("resolve native settings directory: {error}"))
}

fn save_configured_port(app: &AppHandle, port: u16) -> LifecycleResult<()> {
    let path = preferences_path(app)?;
    let parent = path
        .parent()
        .ok_or_else(|| format!("invalid native settings path: {}", path.display()))?;
    fs::create_dir_all(parent).map_err(|error| {
        format!(
            "create native settings directory {}: {error}",
            parent.display()
        )
    })?;
    set_directory_mode(parent, 0o700)?;
    let encoded = serde_json::to_vec_pretty(&NativePreferences { port: Some(port) })
        .map_err(|error| format!("encode native connection settings: {error}"))?;
    write_atomic(&path, &encoded, 0o600)
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
struct RuntimeManifest {
    schema_version: u32,
    runtime_version: String,
    target: String,
    binaries: BTreeMap<String, String>,
}

#[derive(Clone, Debug)]
struct InstalledRuntime {
    manifest: RuntimeManifest,
    directory: PathBuf,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum InstallOutcome {
    Installed,
    Updated { preserved: usize },
    Current,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct HealthResponse {
    ok: bool,
    name: String,
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

fn install_runtime(config: &RuntimeConfig) -> LifecycleResult<(InstallOutcome, String)> {
    validate_config(config)?;
    let installed = stage_runtime(config)?;
    // launchd receives one stable runner path for the lifetime of the app.
    // Existing runner processes keep their already-open executable when this
    // file is atomically replaced, while future sessions retain a consistent
    // macOS privacy identity across versioned runtime updates.
    let stable_runner = stable_runner_path(config);
    if !runner_is_usable(config, &stable_runner) {
        activate_stable_runner(config, &installed)?;
    }
    let plist = daemon_plist(config, &installed.directory);
    let previous_plist = match fs::read(&config.plist_path) {
        Ok(bytes) => Some(bytes),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => None,
        Err(error) => {
            return Err(format!(
                "read existing background-service definition {}: {error}",
                config.plist_path.display()
            ));
        }
    };
    let loaded = service_is_loaded(config)?;

    let outcome = if loaded {
        if previous_plist.as_deref() == Some(plist.as_bytes()) {
            wait_until_ready(config, &BTreeSet::new())?;
            InstallOutcome::Current
        } else {
            let old_plist = previous_plist.as_ref().ok_or_else(|| {
                format!(
                    "{} is loaded but its plist is missing; refusing an update without a rollback definition",
                    config.label
                )
            })?;
            let baseline = capture_baseline(config)?;
            update_loaded_service(config, old_plist, plist.as_bytes(), &baseline)?;
            InstallOutcome::Updated {
                preserved: baseline.len(),
            }
        }
    } else {
        if health_once(config).is_ok() {
            return Err(format!(
                "{} already answers on {}; Sessions will not replace or restart an unrelated service",
                config.health_url(), config.port
            ));
        }
        install_unloaded_service(config, previous_plist.as_deref(), plist.as_bytes())?;
        InstallOutcome::Installed
    };

    // Defer the normal version swap until the daemon update is known healthy.
    // If rollback was needed, the previously active runner therefore remains
    // paired with the restored daemon. Replacement is atomic and never stops
    // a runner that is already serving a live session.
    activate_stable_runner(config, &installed)?;

    if let Err(error) = install_cli_link(config, &installed.directory) {
        // CLI discoverability is useful but must never turn a healthy daemon
        // update into a rollback or put live sessions at risk.
        eprintln!("Sessions CLI PATH integration: {error}");
    }
    Ok((outcome, installed.manifest.runtime_version))
}

fn stable_runner_path(config: &RuntimeConfig) -> PathBuf {
    config.managed_root.join("sessions-runner")
}

fn runner_is_usable(config: &RuntimeConfig, path: &Path) -> bool {
    let Ok(metadata) = fs::symlink_metadata(path) else {
        return false;
    };
    if !metadata.file_type().is_file() {
        return false;
    }
    !config.verify_signatures
        || run_checked_path(&config.codesign, &["--verify", "--strict"], path).is_ok()
}

fn activate_stable_runner(
    config: &RuntimeConfig,
    installed: &InstalledRuntime,
) -> LifecycleResult<PathBuf> {
    let source = installed.directory.join("sessions-runner");
    let destination = stable_runner_path(config);
    let expected_digest = installed
        .manifest
        .binaries
        .get("sessions-runner")
        .ok_or_else(|| "bundled runtime manifest is missing sessions-runner".to_string())?;
    if runner_is_usable(config, &destination)
        && verify_binary(config, &destination, expected_digest).is_ok()
    {
        return Ok(destination);
    }

    fs::create_dir_all(&config.managed_root).map_err(|error| {
        format!(
            "create Sessions runtime root {}: {error}",
            config.managed_root.display()
        )
    })?;
    set_directory_mode(&config.managed_root, 0o700)?;
    let temporary = config.managed_root.join(format!(
        ".sessions-runner-{}-{}",
        std::process::id(),
        unique_suffix()
    ));
    let activated = (|| -> LifecycleResult<()> {
        fs::copy(&source, &temporary).map_err(|error| {
            format!(
                "stage stable Sessions runner {} from {}: {error}",
                temporary.display(),
                source.display()
            )
        })?;
        set_file_mode(&temporary, 0o755)?;
        fs::File::open(&temporary)
            .and_then(|file| file.sync_all())
            .map_err(|error| format!("sync stable Sessions runner: {error}"))?;
        verify_binary(config, &temporary, expected_digest)?;
        fs::rename(&temporary, &destination).map_err(|error| {
            format!(
                "activate stable Sessions runner {}: {error}",
                destination.display()
            )
        })?;
        verify_binary(config, &destination, expected_digest)
    })();
    if activated.is_err() {
        let _ = fs::remove_file(&temporary);
    }
    activated?;
    Ok(destination)
}

#[cfg(unix)]
fn install_cli_link(config: &RuntimeConfig, runtime_directory: &Path) -> LifecycleResult<PathBuf> {
    let target = runtime_directory.join("sessions");
    let mut skipped = Vec::new();
    let mut managed = Vec::new();
    let mut available = Vec::new();
    for candidate in &config.cli_link_paths {
        let Some(parent) = candidate.parent() else {
            skipped.push(format!("{} has no parent", candidate.display()));
            continue;
        };
        if let Err(error) = fs::create_dir_all(parent) {
            skipped.push(format!("{}: {error}", parent.display()));
            continue;
        }

        match fs::symlink_metadata(candidate) {
            Ok(metadata) if !metadata.file_type().is_symlink() => {
                skipped.push(format!("{} already exists", candidate.display()));
                continue;
            }
            Ok(_) => {
                let existing = match fs::read_link(candidate) {
                    Ok(existing) if existing.is_absolute() => existing,
                    Ok(existing) => parent.join(existing),
                    Err(error) => {
                        skipped.push(format!("{}: {error}", candidate.display()));
                        continue;
                    }
                };
                let sessions_managed = existing.file_name().and_then(|name| name.to_str())
                    == Some("sessions")
                    && (existing.starts_with(&config.managed_root)
                        || existing.starts_with(&config.source_dir));
                if !sessions_managed {
                    skipped.push(format!(
                        "{} points outside Sessions' managed runtime",
                        candidate.display()
                    ));
                    continue;
                }
                managed.push(candidate.clone());
            }
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                available.push(candidate.clone());
            }
            Err(error) => {
                skipped.push(format!("{}: {error}", candidate.display()));
                continue;
            }
        }
    }

    if !managed.is_empty() {
        let mut update_errors = Vec::new();
        for candidate in &managed {
            if let Err(error) = replace_cli_link(candidate, &target) {
                update_errors.push(format!("{}: {error}", candidate.display()));
            }
        }
        if update_errors.is_empty() {
            return Ok(managed[0].clone());
        }
        return Err(format!(
            "could not update every Sessions-managed CLI link ({})",
            update_errors.join("; ")
        ));
    }

    for candidate in available {
        match replace_cli_link(&candidate, &target) {
            Ok(()) => return Ok(candidate),
            Err(error) => skipped.push(format!("{}: {error}", candidate.display())),
        }
    }
    Err(format!(
        "could not expose `sessions` on a standard command path ({})",
        skipped.join("; ")
    ))
}

#[cfg(unix)]
fn replace_cli_link(candidate: &Path, target: &Path) -> LifecycleResult<()> {
    use std::os::unix::fs::symlink;

    let parent = candidate
        .parent()
        .ok_or_else(|| format!("{} has no parent", candidate.display()))?;
    let temporary = parent.join(format!(
        ".sessions-link-{}-{}",
        std::process::id(),
        unique_suffix()
    ));
    let _ = fs::remove_file(&temporary);
    symlink(target, &temporary)
        .map_err(|error| format!("create temporary link {}: {error}", temporary.display()))?;
    if let Err(error) = fs::rename(&temporary, candidate) {
        let _ = fs::remove_file(&temporary);
        return Err(format!("replace link {}: {error}", candidate.display()));
    }
    Ok(())
}

#[cfg(not(unix))]
fn install_cli_link(
    _config: &RuntimeConfig,
    _runtime_directory: &Path,
) -> LifecycleResult<PathBuf> {
    Err("automatic CLI PATH integration is unavailable on this platform".to_string())
}

fn validate_config(config: &RuntimeConfig) -> LifecycleResult<()> {
    validate_port(config.port)?;
    if config.label.is_empty()
        || !config
            .label
            .chars()
            .all(|character| character.is_ascii_alphanumeric() || matches!(character, '.' | '-'))
    {
        return Err(format!(
            "invalid Sessions launchd label: {:?}",
            config.label
        ));
    }
    if config.host != LOOPBACK_HOST {
        return Err("Sessions background-service installer only permits 127.0.0.1".to_string());
    }
    for required in [&config.launchctl, &config.shasum] {
        if !required.is_file() {
            return Err(format!(
                "required macOS tool is missing: {}",
                required.display()
            ));
        }
    }
    if config.verify_signatures && !config.codesign.is_file() {
        return Err(format!(
            "required macOS signing tool is missing: {}",
            config.codesign.display()
        ));
    }
    Ok(())
}

fn stage_runtime(config: &RuntimeConfig) -> LifecycleResult<InstalledRuntime> {
    let manifest_path = config.source_dir.join("runtime-manifest.json");
    let bytes = fs::read(&manifest_path).map_err(|error| {
        format!(
            "read bundled runtime manifest {}: {error}",
            manifest_path.display()
        )
    })?;
    let manifest: RuntimeManifest = serde_json::from_slice(&bytes).map_err(|error| {
        format!(
            "parse bundled runtime manifest {}: {error}",
            manifest_path.display()
        )
    })?;
    validate_manifest(&manifest)?;
    for binary in REQUIRED_BINARIES {
        verify_binary(
            config,
            &config.source_dir.join(binary),
            manifest.binaries.get(binary).unwrap(),
        )?;
    }

    fs::create_dir_all(&config.managed_root).map_err(|error| {
        format!(
            "create Sessions runtime root {}: {error}",
            config.managed_root.display()
        )
    })?;
    set_directory_mode(&config.managed_root, 0o700)?;
    let destination = config.managed_root.join(&manifest.runtime_version);
    if destination.exists() {
        verify_runtime_directory(config, &destination, &manifest)?;
        return Ok(InstalledRuntime {
            manifest,
            directory: destination,
        });
    }

    let staging = config.managed_root.join(format!(
        ".staging-{}-{}-{}",
        manifest.runtime_version,
        std::process::id(),
        unique_suffix()
    ));
    fs::create_dir(&staging).map_err(|error| {
        format!(
            "create runtime staging directory {}: {error}",
            staging.display()
        )
    })?;
    set_directory_mode(&staging, 0o700)?;
    let staged = (|| -> LifecycleResult<()> {
        for binary in REQUIRED_BINARIES {
            let source = config.source_dir.join(binary);
            let target = staging.join(binary);
            fs::copy(&source, &target).map_err(|error| {
                format!(
                    "copy bundled runtime {} to {}: {error}",
                    source.display(),
                    target.display()
                )
            })?;
            set_file_mode(&target, 0o755)?;
            fs::File::open(&target)
                .and_then(|file| file.sync_all())
                .map_err(|error| format!("sync installed runtime {}: {error}", target.display()))?;
        }
        fs::write(staging.join("runtime-manifest.json"), &bytes).map_err(|error| {
            format!(
                "write installed runtime manifest {}: {error}",
                staging.join("runtime-manifest.json").display()
            )
        })?;
        verify_runtime_directory(config, &staging, &manifest)?;
        fs::rename(&staging, &destination).map_err(|error| {
            format!(
                "activate immutable runtime {} at {}: {error}",
                staging.display(),
                destination.display()
            )
        })?;
        Ok(())
    })();
    if staged.is_err() {
        let _ = fs::remove_dir_all(&staging);
    }
    staged?;

    Ok(InstalledRuntime {
        manifest,
        directory: destination,
    })
}

fn validate_manifest(manifest: &RuntimeManifest) -> LifecycleResult<()> {
    if manifest.schema_version != 1 {
        return Err(format!(
            "unsupported bundled runtime manifest schema {}",
            manifest.schema_version
        ));
    }
    if manifest.target != "darwin-arm64" {
        return Err(format!(
            "bundled runtime target must be darwin-arm64, got {:?}",
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
            "bundled runtime version is not a safe path component: {:?}",
            manifest.runtime_version
        ));
    }
    if manifest.binaries.len() != REQUIRED_BINARIES.len() {
        return Err(
            "bundled runtime manifest must name exactly sessions, sessionsd, and sessions-runner"
                .to_string(),
        );
    }
    for binary in REQUIRED_BINARIES {
        let digest = manifest
            .binaries
            .get(binary)
            .ok_or_else(|| format!("bundled runtime manifest is missing {binary}"))?;
        if digest.len() != 64
            || !digest
                .chars()
                .all(|character| character.is_ascii_hexdigit())
        {
            return Err(format!("bundled runtime digest for {binary} is invalid"));
        }
    }
    Ok(())
}

fn verify_runtime_directory(
    config: &RuntimeConfig,
    directory: &Path,
    manifest: &RuntimeManifest,
) -> LifecycleResult<()> {
    for binary in REQUIRED_BINARIES {
        verify_binary(
            config,
            &directory.join(binary),
            manifest.binaries.get(binary).unwrap(),
        )?;
    }
    Ok(())
}

fn verify_binary(
    config: &RuntimeConfig,
    path: &Path,
    expected_digest: &str,
) -> LifecycleResult<()> {
    let metadata = fs::metadata(path)
        .map_err(|error| format!("inspect runtime binary {}: {error}", path.display()))?;
    if !metadata.is_file() {
        return Err(format!(
            "runtime binary is not a regular file: {}",
            path.display()
        ));
    }
    let digest = command_text_path(&config.shasum, &["-a", "256"], path)?
        .split_whitespace()
        .next()
        .unwrap_or_default()
        .to_ascii_lowercase();
    if digest != expected_digest.to_ascii_lowercase() {
        return Err(format!(
            "runtime binary digest mismatch for {}: expected {}, got {}",
            path.display(),
            expected_digest,
            digest
        ));
    }
    if config.verify_signatures {
        run_checked_path(&config.codesign, &["--verify", "--strict"], path).map_err(|error| {
            format!(
                "runtime signature verification failed for {}: {error}",
                path.display()
            )
        })?;
    }
    Ok(())
}

fn daemon_plist(config: &RuntimeConfig, runtime_dir: &Path) -> String {
    let mut arguments = vec![runtime_dir.join("sessionsd").display().to_string()];
    arguments.extend(config.daemon_arguments.clone());
    let mut environment = vec![
        (
            "PATH".to_string(),
            "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin".to_string(),
        ),
        ("SESSIONS_HOST".to_string(), config.host.clone()),
        ("SESSIONS_PORT".to_string(), config.port.to_string()),
        (
            "SESSIONS_RUNNER".to_string(),
            stable_runner_path(config).display().to_string(),
        ),
    ];
    environment.extend(config.environment.clone());

    let argument_xml = arguments
        .iter()
        .map(|argument| format!("    <string>{}</string>", xml_escape(argument)))
        .collect::<Vec<_>>()
        .join("\n");
    let environment_xml = environment
        .iter()
        .map(|(key, value)| {
            format!(
                "    <key>{}</key>\n    <string>{}</string>",
                xml_escape(key),
                xml_escape(value)
            )
        })
        .collect::<Vec<_>>()
        .join("\n");
    format!(
        r#"<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>{label}</string>
  <key>ProgramArguments</key>
  <array>
{arguments}
  </array>
  <key>EnvironmentVariables</key>
  <dict>
{environment}
  </dict>
  <key>WorkingDirectory</key>
  <string>{working_directory}</string>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>ThrottleInterval</key>
  <integer>5</integer>
  <key>StandardOutPath</key>
  <string>{log_path}</string>
  <key>StandardErrorPath</key>
  <string>{log_path}</string>
</dict>
</plist>
"#,
        label = xml_escape(&config.label),
        arguments = argument_xml,
        environment = environment_xml,
        working_directory = xml_escape(&runtime_dir.display().to_string()),
        log_path = xml_escape(&config.log_path.display().to_string())
    )
}

fn install_unloaded_service(
    config: &RuntimeConfig,
    previous_plist: Option<&[u8]>,
    new_plist: &[u8],
) -> LifecycleResult<()> {
    prepare_service_directories(config)?;
    write_atomic(&config.plist_path, new_plist, 0o644)?;
    let start_result = bootstrap(config).and_then(|_| wait_until_ready(config, &BTreeSet::new()));
    if let Err(start_error) = start_result {
        let _ = bootout_if_loaded(config);
        let restore_result = restore_plist(config, previous_plist);
        return match restore_result {
            Ok(()) => Err(format!(
                "Sessions could not start its background service: {start_error}; the previous unloaded definition was restored"
            )),
            Err(restore_error) => Err(format!(
                "Sessions could not start its background service: {start_error}; restoring the previous definition also failed: {restore_error}"
            )),
        };
    }
    Ok(())
}

fn update_loaded_service(
    config: &RuntimeConfig,
    old_plist: &[u8],
    new_plist: &[u8],
    baseline: &BTreeSet<String>,
) -> LifecycleResult<()> {
    prepare_service_directories(config)?;
    let update_result = (|| -> LifecycleResult<()> {
        bootout(config)?;
        wait_for_service_unload(config)?;
        wait_for_port_release(config)?;
        write_atomic(&config.plist_path, new_plist, 0o644)?;
        bootstrap(config)?;
        wait_until_ready(config, baseline)
    })();
    if update_result.is_ok() {
        return Ok(());
    }

    let update_error = update_result.unwrap_err();
    let rollback_result = (|| -> LifecycleResult<()> {
        bootout_if_loaded(config)?;
        wait_for_port_release(config)?;
        write_atomic(&config.plist_path, old_plist, 0o644)?;
        bootstrap(config)?;
        wait_until_ready(config, baseline)
    })();
    match rollback_result {
        Ok(()) => Err(format!(
            "Sessions rejected the background-service update and rolled back safely: {update_error}"
        )),
        Err(rollback_error) => Err(format!(
            "Sessions background-service update failed: {update_error}; rollback also failed: {rollback_error}"
        )),
    }
}

fn migrate_loaded_service(
    old: &RuntimeConfig,
    new: &RuntimeConfig,
    old_plist: &[u8],
    new_plist: &[u8],
    baseline: &BTreeSet<String>,
) -> LifecycleResult<()> {
    prepare_service_directories(new)?;
    let update_result = (|| -> LifecycleResult<()> {
        bootout(old)?;
        wait_for_service_unload(old)?;
        wait_for_port_release(old)?;
        write_atomic(&new.plist_path, new_plist, 0o644)?;
        bootstrap(new)?;
        wait_until_ready(new, baseline)
    })();
    if update_result.is_ok() {
        return Ok(());
    }

    let update_error = update_result.unwrap_err();
    let rollback_result = (|| -> LifecycleResult<()> {
        bootout_if_loaded(new)?;
        // The rollback returns to the old port. Do not let an unrelated
        // process which raced onto the requested new port prevent restoring
        // the known-good service definition.
        write_atomic(&old.plist_path, old_plist, 0o644)?;
        bootstrap(old)?;
        wait_until_ready(old, baseline)
    })();
    match rollback_result {
        Ok(()) => Err(format!(
            "Sessions rejected the port change and rolled back safely: {update_error}"
        )),
        Err(rollback_error) => Err(format!(
            "Sessions port change failed: {update_error}; rollback also failed: {rollback_error}"
        )),
    }
}

fn prepare_service_directories(config: &RuntimeConfig) -> LifecycleResult<()> {
    let plist_parent = config
        .plist_path
        .parent()
        .ok_or_else(|| format!("invalid plist path: {}", config.plist_path.display()))?;
    let log_parent = config
        .log_path
        .parent()
        .ok_or_else(|| format!("invalid log path: {}", config.log_path.display()))?;
    for directory in [plist_parent, log_parent] {
        fs::create_dir_all(directory).map_err(|error| {
            format!("create Sessions directory {}: {error}", directory.display())
        })?;
        set_directory_mode(directory, 0o700)?;
    }
    Ok(())
}

fn capture_baseline(config: &RuntimeConfig) -> LifecycleResult<BTreeSet<String>> {
    wait_until_ready(config, &BTreeSet::new())?;
    fetch_sessions(config)
}

fn wait_until_ready(config: &RuntimeConfig, baseline: &BTreeSet<String>) -> LifecycleResult<()> {
    let timeout = readiness_timeout(config, baseline.len());
    let deadline = Instant::now() + timeout;
    let mut last_error = "no response".to_string();
    while Instant::now() < deadline {
        match health_once(config) {
            Ok(health) if health.ok && health.name == "sessionsd" && !health.discovering => {
                match fetch_sessions(config) {
                    Ok(current) => {
                        let missing = baseline.difference(&current).cloned().collect::<Vec<_>>();
                        if missing.is_empty() {
                            return Ok(());
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
            Ok(health) if health.discovering => {
                last_error = "daemon is healthy but discovery is still running".to_string();
            }
            Ok(health) => {
                last_error = format!("unexpected health response from {:?}", health.name);
            }
            Err(error) => last_error = error,
        }
        thread::sleep(config.poll_interval);
    }
    Err(format!(
        "background service did not become ready at {} within {}s: {} (logs: {})",
        config.health_url(),
        timeout.as_secs(),
        last_error,
        config.log_path.display()
    ))
}

fn readiness_timeout(config: &RuntimeConfig, baseline_count: usize) -> Duration {
    let count = u32::try_from(baseline_count).unwrap_or(u32::MAX);
    let scaled = config
        .health_timeout_per_session
        .checked_mul(count)
        .and_then(|per_session| config.health_timeout.checked_add(per_session))
        .unwrap_or(config.health_timeout_cap);
    scaled.min(config.health_timeout_cap)
}

fn health_once(config: &RuntimeConfig) -> LifecycleResult<HealthResponse> {
    http_client(config)?
        .get(config.health_url())
        .send()
        .and_then(|response| response.error_for_status())
        .and_then(|response| response.json::<HealthResponse>())
        .map_err(|error| format!("health probe failed: {error}"))
}

fn fetch_sessions(config: &RuntimeConfig) -> LifecycleResult<BTreeSet<String>> {
    let response = http_client(config)?
        .get(config.sessions_url())
        .send()
        .and_then(|response| response.error_for_status())
        .and_then(|response| response.json::<SessionEnvelope>())
        .map_err(|error| format!("session-baseline probe failed: {error}"))?;
    Ok(response
        .sessions
        .into_iter()
        .map(|session| session.id)
        .filter(|id| !id.is_empty())
        .collect())
}

fn http_client(config: &RuntimeConfig) -> LifecycleResult<reqwest::blocking::Client> {
    reqwest::blocking::Client::builder()
        .no_proxy()
        .connect_timeout(Duration::from_secs(1))
        .timeout(config.poll_interval.max(Duration::from_secs(1)))
        .build()
        .map_err(|error| format!("build loopback health client: {error}"))
}

fn service_is_loaded(config: &RuntimeConfig) -> LifecycleResult<bool> {
    let target = config.service_target();
    let output = Command::new(&config.launchctl)
        .args(["print", target.as_str()])
        .output()
        .map_err(|error| format!("run launchctl print for {}: {error}", config.label))?;
    if output.status.success() {
        return Ok(true);
    }
    let detail = output_detail(&output).to_ascii_lowercase();
    if detail.contains("could not find service")
        || detail.contains("service not found")
        || detail.contains("no such process")
    {
        return Ok(false);
    }
    Err(format!(
        "launchctl could not inspect {}: {}",
        config.label,
        output_detail(&output)
    ))
}

fn bootstrap(config: &RuntimeConfig) -> LifecycleResult<()> {
    run_launchctl(
        config,
        &[
            "bootstrap",
            config.domain.as_str(),
            path_text(&config.plist_path).as_str(),
        ],
    )
}

fn bootout(config: &RuntimeConfig) -> LifecycleResult<()> {
    run_launchctl(config, &["bootout", config.service_target().as_str()])
}

fn bootout_if_loaded(config: &RuntimeConfig) -> LifecycleResult<()> {
    let bootout_error = if service_is_loaded(config)? {
        bootout(config).err()
    } else {
        None
    };
    match wait_for_service_unload(config) {
        Ok(()) => Ok(()),
        Err(unload_error) => match bootout_error {
            Some(bootout_error) => Err(format!(
                "launchd bootout failed and {} did not unload: {bootout_error}; {unload_error}",
                config.label
            )),
            None => Err(unload_error),
        },
    }
}

fn run_launchctl(config: &RuntimeConfig, arguments: &[&str]) -> LifecycleResult<()> {
    let output = Command::new(&config.launchctl)
        .args(arguments)
        .output()
        .map_err(|error| format!("run launchctl {}: {error}", arguments.join(" ")))?;
    if output.status.success() {
        Ok(())
    } else {
        Err(format!(
            "launchctl {} failed: {}",
            arguments.join(" "),
            output_detail(&output)
        ))
    }
}

fn wait_for_service_unload(config: &RuntimeConfig) -> LifecycleResult<()> {
    let deadline = Instant::now() + Duration::from_secs(3);
    let mut last_error = None;
    while Instant::now() < deadline {
        match service_is_loaded(config) {
            Ok(false) => return Ok(()),
            Ok(true) => {}
            Err(error) => last_error = Some(error),
        }
        thread::sleep(Duration::from_millis(50));
    }
    Err(match last_error {
        Some(error) => format!(
            "{} did not finish unloading from launchd within 3s: {error}",
            config.label
        ),
        None => format!(
            "{} remained loaded in launchd for more than 3s after bootout",
            config.label
        ),
    })
}

fn wait_for_port_release(config: &RuntimeConfig) -> LifecycleResult<()> {
    let address: SocketAddr = format!("{}:{}", config.host, config.port)
        .parse()
        .map_err(|error| format!("parse daemon address: {error}"))?;
    let deadline = Instant::now() + Duration::from_secs(3);
    while Instant::now() < deadline {
        if TcpStream::connect_timeout(&address, Duration::from_millis(100)).is_err() {
            return Ok(());
        }
        thread::sleep(Duration::from_millis(50));
    }
    Err(format!(
        "{}:{} stayed occupied after stopping {}",
        config.host, config.port, config.label
    ))
}

fn restore_plist(config: &RuntimeConfig, previous: Option<&[u8]>) -> LifecycleResult<()> {
    match previous {
        Some(bytes) => write_atomic(&config.plist_path, bytes, 0o644),
        None => match fs::remove_file(&config.plist_path) {
            Ok(()) => Ok(()),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
            Err(error) => Err(format!(
                "remove failed Sessions plist {}: {error}",
                config.plist_path.display()
            )),
        },
    }
}

fn write_atomic(path: &Path, bytes: &[u8], mode: u32) -> LifecycleResult<()> {
    let parent = path
        .parent()
        .ok_or_else(|| format!("invalid destination path: {}", path.display()))?;
    let file_name = path
        .file_name()
        .and_then(|name| name.to_str())
        .ok_or_else(|| format!("invalid destination filename: {}", path.display()))?;
    let temporary = parent.join(format!(
        ".{file_name}.tmp-{}-{}",
        std::process::id(),
        unique_suffix()
    ));
    let result = (|| -> LifecycleResult<()> {
        let mut file = fs::OpenOptions::new()
            .create_new(true)
            .write(true)
            .open(&temporary)
            .map_err(|error| format!("create temporary file {}: {error}", temporary.display()))?;
        file.write_all(bytes)
            .and_then(|_| file.sync_all())
            .map_err(|error| format!("write temporary file {}: {error}", temporary.display()))?;
        set_file_mode(&temporary, mode)?;
        fs::rename(&temporary, path).map_err(|error| {
            format!(
                "atomically replace {} with {}: {error}",
                path.display(),
                temporary.display()
            )
        })
    })();
    if result.is_err() {
        let _ = fs::remove_file(&temporary);
    }
    result
}

#[cfg(unix)]
fn set_file_mode(path: &Path, mode: u32) -> LifecycleResult<()> {
    use std::os::unix::fs::PermissionsExt;
    fs::set_permissions(path, fs::Permissions::from_mode(mode))
        .map_err(|error| format!("set permissions on {}: {error}", path.display()))
}

#[cfg(not(unix))]
fn set_file_mode(_path: &Path, _mode: u32) -> LifecycleResult<()> {
    Ok(())
}

fn set_directory_mode(path: &Path, mode: u32) -> LifecycleResult<()> {
    set_file_mode(path, mode)
}

fn unique_suffix() -> u128 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_nanos()
}

fn xml_escape(value: &str) -> String {
    value
        .replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;")
}

fn command_text(command: &Path, arguments: &[&str]) -> LifecycleResult<String> {
    let output = Command::new(command)
        .args(arguments)
        .output()
        .map_err(|error| format!("run {}: {error}", command.display()))?;
    if !output.status.success() {
        return Err(format!(
            "{} {} failed: {}",
            command.display(),
            arguments.join(" "),
            output_detail(&output)
        ));
    }
    Ok(String::from_utf8_lossy(&output.stdout).trim().to_string())
}

fn command_text_path(command: &Path, arguments: &[&str], path: &Path) -> LifecycleResult<String> {
    let mut process = Command::new(command);
    process.args(arguments).arg(path);
    let output = process
        .output()
        .map_err(|error| format!("run {}: {error}", command.display()))?;
    if !output.status.success() {
        return Err(format!(
            "{} failed for {}: {}",
            command.display(),
            path.display(),
            output_detail(&output)
        ));
    }
    Ok(String::from_utf8_lossy(&output.stdout).trim().to_string())
}

fn run_checked_path(command: &Path, arguments: &[&str], path: &Path) -> LifecycleResult<()> {
    let mut process = Command::new(command);
    process.args(arguments).arg(path);
    let output = process
        .output()
        .map_err(|error| format!("run {}: {error}", command.display()))?;
    if output.status.success() {
        Ok(())
    } else {
        Err(output_detail(&output))
    }
}

fn output_detail(output: &Output) -> String {
    let stderr = String::from_utf8_lossy(&output.stderr).trim().to_string();
    if !stderr.is_empty() {
        return stderr;
    }
    let stdout = String::from_utf8_lossy(&output.stdout).trim().to_string();
    if !stdout.is_empty() {
        return stdout;
    }
    output.status.to_string()
}

fn path_text(path: &Path) -> String {
    path.display().to_string()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn native_port_rejects_privileged_range() {
        assert!(validate_port(1023).is_err());
        assert!(validate_port(1024).is_ok());
        assert!(validate_port(65_535).is_ok());
    }

    #[test]
    fn readiness_budget_scales_with_serial_runner_adoption_and_stays_bounded() {
        let root = env::temp_dir().join("sessions-readiness-budget-test");
        let mut config = fixture_config(&root, "tech.somewhere.sessions.readiness", 47_869);
        config.health_timeout = Duration::from_secs(30);
        config.health_timeout_per_session = Duration::from_secs(15);
        config.health_timeout_cap = Duration::from_secs(5 * 60);
        assert_eq!(readiness_timeout(&config, 0), Duration::from_secs(30));
        assert_eq!(readiness_timeout(&config, 7), Duration::from_secs(135));
        assert_eq!(readiness_timeout(&config, 9), Duration::from_secs(165));
        assert_eq!(readiness_timeout(&config, 19), Duration::from_secs(300));
        assert_eq!(readiness_timeout(&config, 10_000), Duration::from_secs(300));
    }

    #[test]
    fn only_transitional_runtime_status_reconciles_in_background() {
        assert!(needs_background_reconcile(&RuntimeStatus::informational(
            "starting", "checking",
        )));
        for state in ["ready", "development", "client-only", "disabled", "error"] {
            assert!(!needs_background_reconcile(&RuntimeStatus::informational(
                state, "settled",
            )));
        }
    }

    use std::{io::Read, net::TcpListener};

    const HELPER_ENV: &str = "SESSIONS_LAUNCHD_TEST_HELPER";

    #[test]
    fn manifest_rejects_unsafe_versions_and_incomplete_binary_sets() {
        let mut manifest = RuntimeManifest {
            schema_version: 1,
            runtime_version: "v1/escape".to_string(),
            target: "darwin-arm64".to_string(),
            binaries: REQUIRED_BINARIES
                .iter()
                .map(|name| (name.to_string(), "0".repeat(64)))
                .collect(),
        };
        assert!(validate_manifest(&manifest).is_err());
        manifest.runtime_version = "v1-safe".to_string();
        manifest.binaries.remove("sessions-runner");
        assert!(validate_manifest(&manifest).is_err());
    }

    #[test]
    #[cfg(unix)]
    fn cli_link_tracks_the_current_managed_runtime_without_overwriting_other_tools() {
        use std::os::unix::fs::symlink;

        let root = env::temp_dir().join(format!(
            "sessions-cli-link-test-{}-{}",
            std::process::id(),
            unique_suffix()
        ));
        let mut config = fixture_config(&root, "tech.somewhere.sessions.cli-link", 47870);
        let occupied = root.join("occupied").join("sessions");
        fs::create_dir_all(occupied.parent().unwrap()).unwrap();
        fs::write(&occupied, b"unrelated").unwrap();
        config.cli_link_paths = vec![occupied.clone(), root.join("bin").join("sessions")];

        let v1 = config.managed_root.join("v1");
        fs::create_dir_all(&v1).unwrap();
        fs::write(v1.join("sessions"), b"v1").unwrap();
        let installed = install_cli_link(&config, &v1).unwrap();
        assert_eq!(installed, root.join("bin").join("sessions"));
        assert_eq!(fs::read(&occupied).unwrap(), b"unrelated");
        assert_eq!(fs::read_link(&installed).unwrap(), v1.join("sessions"));

        let v2 = config.managed_root.join("v2");
        fs::create_dir_all(&v2).unwrap();
        fs::write(v2.join("sessions"), b"v2").unwrap();
        let newly_available = root.join("preferred").join("sessions");
        config.cli_link_paths = vec![newly_available.clone(), installed.clone()];
        assert_eq!(install_cli_link(&config, &v2).unwrap(), installed);
        assert_eq!(fs::read_link(&installed).unwrap(), v2.join("sessions"));
        assert!(!newly_available.exists());

        let second_managed = root.join("also-managed").join("sessions");
        fs::create_dir_all(second_managed.parent().unwrap()).unwrap();
        symlink(v1.join("sessions"), &second_managed).unwrap();
        config.cli_link_paths = vec![second_managed.clone(), installed.clone()];
        assert_eq!(install_cli_link(&config, &v2).unwrap(), second_managed);
        assert_eq!(fs::read_link(&second_managed).unwrap(), v2.join("sessions"));
        assert_eq!(fs::read_link(&installed).unwrap(), v2.join("sessions"));

        let external = root.join("external-sessions");
        fs::write(&external, b"external").unwrap();
        fs::remove_file(&installed).unwrap();
        symlink(&external, &installed).unwrap();
        config.cli_link_paths = vec![installed.clone()];
        assert!(install_cli_link(&config, &v2).is_err());
        assert_eq!(fs::read_link(&installed).unwrap(), external);
        fs::remove_dir_all(&root).unwrap();
    }

    #[cfg(target_os = "macos")]
    #[test]
    fn plist_escapes_paths_and_keeps_daemon_and_runner_separate() {
        let config = fixture_config(
            Path::new("/tmp/Sessions & tests"),
            "tech.somewhere.sessions.fixture",
            47871,
        );
        let runtime = Path::new("/tmp/Sessions & tests/runtime/v1");
        let plist = daemon_plist(&config, runtime);
        assert!(plist.contains("tech.somewhere.sessions.fixture"));
        assert!(plist.contains("/tmp/Sessions &amp; tests/runtime/v1/sessionsd"));
        assert!(plist.contains("SESSIONS_RUNNER"));
        assert!(plist.contains(
            "/tmp/Sessions &amp; tests/Application Support/Sessions/runtime/sessions-runner"
        ));
        assert!(plist.contains("<key>KeepAlive</key>"));
    }

    #[cfg(target_os = "macos")]
    #[test]
    fn stable_runner_is_atomically_replaced_without_changing_its_path() {
        let root = env::temp_dir().join(format!(
            "sessions-stable-runner-test-{}-{}",
            std::process::id(),
            unique_suffix()
        ));
        let mut config = fixture_config(&root, "tech.somewhere.sessions.stable-runner", 47872);
        config.verify_signatures = false;

        write_fixture_runtime(&config, "v1", None);
        let v1 = stage_runtime(&config).unwrap();
        let stable = activate_stable_runner(&config, &v1).unwrap();
        assert_eq!(stable, config.managed_root.join("sessions-runner"));
        let first_digest = command_text_path(&config.shasum, &["-a", "256"], &stable).unwrap();

        let replacement = root.join("replacement-runner");
        fs::copy("/usr/bin/true", &replacement).unwrap();
        write_fixture_runtime(&config, "v2", Some(&replacement));
        fs::copy(&replacement, config.source_dir.join("sessions-runner")).unwrap();
        set_file_mode(&config.source_dir.join("sessions-runner"), 0o755).unwrap();
        let mut manifest: RuntimeManifest = serde_json::from_slice(
            &fs::read(config.source_dir.join("runtime-manifest.json")).unwrap(),
        )
        .unwrap();
        manifest.binaries.insert(
            "sessions-runner".to_string(),
            command_text_path(
                &config.shasum,
                &["-a", "256"],
                &config.source_dir.join("sessions-runner"),
            )
            .unwrap()
            .split_whitespace()
            .next()
            .unwrap()
            .to_string(),
        );
        fs::write(
            config.source_dir.join("runtime-manifest.json"),
            serde_json::to_vec_pretty(&manifest).unwrap(),
        )
        .unwrap();
        let v2 = stage_runtime(&config).unwrap();
        assert_eq!(activate_stable_runner(&config, &v2).unwrap(), stable);
        let second_digest = command_text_path(&config.shasum, &["-a", "256"], &stable).unwrap();
        assert_ne!(first_digest, second_digest);
        fs::remove_dir_all(&root).unwrap();
    }

    #[test]
    fn launchd_helper() {
        if env::var(HELPER_ENV).ok().as_deref() != Some("1") {
            return;
        }
        let address = format!(
            "127.0.0.1:{}",
            env::var("SESSIONS_PORT").expect("SESSIONS_PORT")
        );
        let listener = TcpListener::bind(&address).expect("bind launchd helper");
        for incoming in listener.incoming() {
            let mut stream = incoming.expect("accept launchd helper request");
            let mut request = [0_u8; 4096];
            let count = stream.read(&mut request).unwrap_or_default();
            let request = String::from_utf8_lossy(&request[..count]);
            let body = if request.starts_with("GET /api/sessions ") {
                let sessions = env::var("SESSIONS_LAUNCHD_TEST_SESSION_IDS")
                    .unwrap_or_default()
                    .split(',')
                    .filter(|id| !id.is_empty())
                    .map(|id| serde_json::json!({ "id": id }))
                    .collect::<Vec<_>>();
                serde_json::json!({ "sessions": sessions }).to_string()
            } else {
                r#"{"ok":true,"name":"sessionsd","discovering":false}"#.to_string()
            };
            let response = format!(
                "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{}",
                body.len(), body
            );
            stream
                .write_all(response.as_bytes())
                .expect("write helper response");
        }
    }

    #[test]
    #[cfg(target_os = "macos")]
    fn scratch_launchd_install_update_and_rollback_preserve_service() {
        let launchctl = Path::new("/bin/launchctl");
        if !launchctl.is_file() {
            return;
        }
        let uid = command_text(Path::new("/usr/bin/id"), &["-u"]).unwrap();
        let domain = format!("gui/{uid}");
        if Command::new(launchctl)
            .args(["print", domain.as_str()])
            .output()
            .map(|output| !output.status.success())
            .unwrap_or(true)
        {
            return;
        }

        let port_probe = TcpListener::bind("127.0.0.1:0").unwrap();
        let port = port_probe.local_addr().unwrap().port();
        drop(port_probe);
        let root = env::temp_dir().join(format!(
            "sessions-launchd-test-{}-{}",
            std::process::id(),
            unique_suffix()
        ));
        fs::create_dir_all(&root).unwrap();
        let label = format!(
            "tech.somewhere.sessions.scratch.{}.{}",
            std::process::id(),
            unique_suffix()
        );
        let mut guard = ScratchGuard {
            launchctl: launchctl.to_path_buf(),
            target: format!("{domain}/{label}"),
            root: root.clone(),
        };
        let mut config = fixture_config(&root, &label, port);
        config.domain = domain;
        config.daemon_arguments = vec![
            "--exact".to_string(),
            "lifecycle::tests::launchd_helper".to_string(),
            "--nocapture".to_string(),
        ];
        config.environment = vec![
            (HELPER_ENV.to_string(), "1".to_string()),
            (
                "SESSIONS_LAUNCHD_TEST_SESSION_IDS".to_string(),
                "alpha,beta".to_string(),
            ),
        ];
        config.verify_signatures = false;
        config.health_timeout = Duration::from_secs(3);
        config.health_timeout_per_session = Duration::ZERO;
        config.health_timeout_cap = Duration::from_secs(3);
        config.poll_interval = Duration::from_millis(50);

        write_fixture_runtime(&config, "v1", None);
        let first = install_runtime(&config).unwrap();
        assert_eq!(first.0, InstallOutcome::Installed);
        assert!(health_once(&config).is_ok());

        let current = install_runtime(&config).unwrap();
        assert_eq!(current.0, InstallOutcome::Current);

        write_fixture_runtime(&config, "v2", None);
        let updated = install_runtime(&config).unwrap();
        assert_eq!(updated.0, InstallOutcome::Updated { preserved: 2 });
        assert!(health_once(&config).is_ok());

        let moved_port_probe = TcpListener::bind("127.0.0.1:0").unwrap();
        let moved_port = moved_port_probe.local_addr().unwrap().port();
        drop(moved_port_probe);
        let mut moved = config.clone();
        moved.port = moved_port;
        let old_plist = fs::read(&config.plist_path).unwrap();
        let moved_plist = daemon_plist(&moved, &config.managed_root.join("v2")).into_bytes();
        let baseline = capture_baseline(&config).unwrap();
        migrate_loaded_service(&config, &moved, &old_plist, &moved_plist, &baseline).unwrap();
        assert!(health_once(&moved).is_ok());
        assert!(health_once(&config).is_err());

        let occupied = TcpListener::bind("127.0.0.1:0").unwrap();
        let mut blocked = moved.clone();
        blocked.port = occupied.local_addr().unwrap().port();
        let blocked_plist = daemon_plist(&blocked, &config.managed_root.join("v2")).into_bytes();
        let error =
            migrate_loaded_service(&moved, &blocked, &moved_plist, &blocked_plist, &baseline)
                .unwrap_err();
        assert!(error.contains("rolled back safely"), "{error}");
        assert!(health_once(&moved).is_ok());
        drop(occupied);
        config = moved;

        write_fixture_runtime(&config, "v3-broken", Some(Path::new("/usr/bin/false")));
        let error = install_runtime(&config).unwrap_err();
        assert!(error.contains("rolled back safely"), "{error}");
        assert!(health_once(&config).is_ok());
        let plist = fs::read_to_string(&config.plist_path).unwrap();
        assert!(plist.contains("/v2/sessionsd"), "{plist}");

        bootout_if_loaded(&config).unwrap();
        guard.target.clear();
        fs::remove_dir_all(&root).unwrap();
    }

    fn fixture_config(root: &Path, label: &str, port: u16) -> RuntimeConfig {
        RuntimeConfig {
            source_dir: root.join("resources").join("runtime"),
            managed_root: root
                .join("Application Support")
                .join("Sessions")
                .join("runtime"),
            cli_link_paths: vec![root.join("bin").join("sessions")],
            plist_path: root.join("LaunchAgents").join(format!("{label}.plist")),
            log_path: root.join("Logs").join("sessionsd.log"),
            label: label.to_string(),
            domain: "gui/0".to_string(),
            host: LOOPBACK_HOST.to_string(),
            port,
            launchctl: PathBuf::from("/bin/launchctl"),
            codesign: PathBuf::from("/usr/bin/codesign"),
            shasum: PathBuf::from("/usr/bin/shasum"),
            verify_signatures: false,
            daemon_arguments: Vec::new(),
            environment: Vec::new(),
            health_timeout: Duration::from_secs(1),
            health_timeout_per_session: Duration::ZERO,
            health_timeout_cap: Duration::from_secs(1),
            poll_interval: Duration::from_millis(25),
        }
    }

    fn write_fixture_runtime(config: &RuntimeConfig, version: &str, daemon: Option<&Path>) {
        let source = &config.source_dir;
        fs::create_dir_all(source).unwrap();
        let test_binary = env::current_exe().unwrap();
        for binary in REQUIRED_BINARIES {
            let origin = if binary == "sessionsd" {
                daemon.unwrap_or(&test_binary)
            } else {
                &test_binary
            };
            fs::copy(origin, source.join(binary)).unwrap();
            set_file_mode(&source.join(binary), 0o755).unwrap();
        }
        let binaries = REQUIRED_BINARIES
            .iter()
            .map(|binary| {
                let digest =
                    command_text_path(&config.shasum, &["-a", "256"], &source.join(binary))
                        .unwrap()
                        .split_whitespace()
                        .next()
                        .unwrap()
                        .to_string();
                (binary.to_string(), digest)
            })
            .collect::<BTreeMap<_, _>>();
        let manifest = serde_json::json!({
            "schemaVersion": 1,
            "runtimeVersion": version,
            "target": "darwin-arm64",
            "binaries": binaries,
        });
        fs::write(
            source.join("runtime-manifest.json"),
            serde_json::to_vec_pretty(&manifest).unwrap(),
        )
        .unwrap();
    }

    struct ScratchGuard {
        launchctl: PathBuf,
        target: String,
        root: PathBuf,
    }

    impl Drop for ScratchGuard {
        fn drop(&mut self) {
            if !self.target.is_empty() {
                let _ = Command::new(&self.launchctl)
                    .args(["bootout", self.target.as_str()])
                    .output();
            }
            let _ = fs::remove_dir_all(&self.root);
        }
    }
}
