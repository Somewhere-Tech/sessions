use serde::{Deserialize, Serialize};
use std::{
    collections::{BTreeMap, BTreeSet},
    env, fs,
    io::Write,
    net::{SocketAddr, TcpListener, TcpStream},
    path::{Path, PathBuf},
    process::{Command, Output},
    sync::{
        atomic::{AtomicU64, Ordering},
        Mutex, MutexGuard, TryLockError,
    },
    thread,
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};
use tauri::{AppHandle, Manager};

const SERVICE_LABEL: &str = "tech.somewhere.sessions.daemon";
const LOOPBACK_HOST: &str = "127.0.0.1";
const DEFAULT_LOOPBACK_PORT: u16 = 8787;
const REQUIRED_BINARIES: [&str; 3] = ["sessions", "sessionsd", "sessions-runner"];

pub(crate) type LifecycleResult<T> = Result<T, String>;

// Every mutation of the per-user background service — startup reconciliation,
// the Recover button, and a port change — funnels through install_runtime or
// migrate_loaded_service, each of which boots the daemon out, writes a new
// definition, boots it back in, and rolls back on failure. Two of those
// interleaved can make one call's rollback boot out the service the other just
// started, leaving the management plane wedged until the app is relaunched.
// The UI's guards are per-webview React booleans and capabilities/default.json
// grants this surface to "main" *and* every "win-*" window, so the only place
// that can actually serialize the three callers is here.
static SERVICE_MUTATION_LOCK: Mutex<()> = Mutex::new(());
static UNIQUE_SUFFIX_SEQUENCE: AtomicU64 = AtomicU64::new(0);

// Refuse rather than queue. A second Recover click that silently waited would
// look like a hang, and the honest answer — the first attempt is still running
// and re-adopting a large fleet is slow — is something the user can act on.
fn lock_service_mutation() -> LifecycleResult<MutexGuard<'static, ()>> {
    match SERVICE_MUTATION_LOCK.try_lock() {
        Ok(guard) => Ok(guard),
        // The guard protects launchd, not shared Rust state: a panicked holder
        // leaves nothing inconsistent to observe here, and refusing every
        // later repair behind a poisoned lock would wedge exactly the plane
        // this lock exists to protect.
        Err(TryLockError::Poisoned(poisoned)) => Ok(poisoned.into_inner()),
        Err(TryLockError::WouldBlock) => Err(
            "Sessions is already reconciling its background service. Wait for that attempt to finish — re-adopting a large set of live sessions can take a few minutes — and then try again. Your sessions keep running either way."
                .to_string(),
        ),
    }
}

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

    // Nothing above this point touches launchd, so the lock is taken only once
    // a real mutation is about to start.
    let _guard = match lock_service_mutation() {
        Ok(guard) => guard,
        // A concurrent reconcile is not a fault: it is the same repair already
        // in flight. Report the calm transitional state and let the running
        // attempt publish the settled one when it finishes.
        Err(busy) => return RuntimeStatus::informational("starting", &busy),
    };
    let resolved = resolve_port(app);

    #[cfg(target_os = "macos")]
    {
        // RuntimeConfig::from_app resolves the same port; this call only needs
        // the reason so it can reach the status the user reads.
        let status = match RuntimeConfig::from_app(app).and_then(|config| install_runtime(&config))
        {
            Ok((outcome, version)) => RuntimeStatus::ready(outcome, version),
            Err(error) => RuntimeStatus::failed(error),
        };
        resolved.annotate(status)
    }
    #[cfg(target_os = "windows")]
    {
        let status = match crate::windows_runtime::install(app, resolved.port()) {
            Ok(status) => status,
            Err(error) => RuntimeStatus::failed(error),
        };
        resolved.annotate(status)
    }
    #[cfg(not(any(target_os = "macos", target_os = "windows")))]
    {
        let _ = app;
        let _ = resolved;
        RuntimeStatus::informational(
            "client-only",
            "this platform connects to a Mac-hosted background service",
        )
    }
}

// Prefer resolve_port over this: it is the only caller that decides what an
// unreadable settings file means, and every other caller must agree with it.
fn configured_port(app: &AppHandle) -> LifecycleResult<u16> {
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

// One answer for an unreadable or unparseable connections.json.
//
// Three callers used to disagree: the Tauri setup hook logged and fell back to
// 8787, RuntimeConfig::from_app treated the same file as fatal, and the
// Windows installer fell back silently. The viewer therefore reported port
// 8787 in Settings while every reconcile failed against it, and no surface
// told the user which file was wrong or how to repair it.
//
// The chosen answer is "not fatal, but never silent". Refusing to reconcile
// would hand a single corrupt preferences file the power to wedge the
// management plane with no in-app way out, which is exactly what this app must
// not do. So fall back to the default loopback port and carry the reason into
// the RuntimeStatus the tray and settings screen already display.
pub struct ResolvedPort {
    port: u16,
    problem: Option<String>,
}

impl ResolvedPort {
    pub fn port(&self) -> u16 {
        self.port
    }

    pub fn problem(&self) -> Option<&str> {
        self.problem.as_deref()
    }

    // Keep the reason attached to whatever the install actually reported, so a
    // healthy daemon on the fallback port still says why it is on that port.
    pub fn annotate(&self, status: RuntimeStatus) -> RuntimeStatus {
        let Some(problem) = self.problem.as_deref() else {
            return status;
        };
        RuntimeStatus {
            detail: format!("{}; {problem}", status.detail),
            ..status
        }
    }
}

pub fn resolve_port(app: &AppHandle) -> ResolvedPort {
    resolved_port(configured_port(app))
}

fn resolved_port(configured: LifecycleResult<u16>) -> ResolvedPort {
    match configured {
        Ok(port) => ResolvedPort {
            port,
            problem: None,
        },
        Err(error) => ResolvedPort {
            port: DEFAULT_LOOPBACK_PORT,
            problem: Some(format!(
                "Sessions could not read its saved connection port ({error}), so it is using localhost:{DEFAULT_LOOPBACK_PORT}. Set the port again in Settings to rewrite that file."
            )),
        },
    }
}

pub fn reconfigure_port(app: &AppHandle, port: u16) -> LifecycleResult<RuntimeStatus> {
    validate_port(port)?;
    if cfg!(debug_assertions) {
        return Err("port changes are available in an installed Sessions desktop app; development builds use a separately managed daemon".to_string());
    }
    if cfg!(mobile) {
        return Err("mobile clients do not own a local background-service port".to_string());
    }

    // A port change boots the service out and back in exactly like an install
    // does, so it shares the install lock. Without it a reconcile racing a port
    // change can roll back onto the definition the other one just replaced.
    let _guard = lock_service_mutation()?;

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
        // A corrupt connections.json must not block the one action that
        // rewrites it. Fall back to the default port as the presumed current
        // one; if the daemon is not actually there, windows_runtime says so.
        let current = resolve_port(app).port();
        let status = crate::windows_runtime::reconfigure_port(app, current, port)?;
        if current == port {
            return Ok(status);
        }
        if let Err(save_error) = save_configured_port(app, port) {
            // restore_port, not reconfigure_port: the rollback must not be
            // refused by a gate that already said yes to this exact move a
            // moment ago, or the saved port and the running daemon end up
            // disagreeing with no way back. macOS rolls back from its captured
            // baseline for the same reason.
            let rollback = crate::windows_runtime::restore_port(app, port, current);
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

// Uninstall.
//
// Sessions' own integration points are the things the package wrote *outside*
// itself: the per-user service definition that brings the daemon back at every
// login, and the `sessions` command it published on a shared command path.
// Those come out. Everything else stays, and the two boundaries are worth
// stating because both are easy to get wrong in the direction that costs work:
//
// Nothing is stopped. A runner is not owned by the viewer, and the daemon is
// the only process that can still record what a live runner produces; ending
// either during an uninstall would turn live sessions into orphans, which is
// the one outcome this product refuses. Removing the definition is enough — the
// daemon simply does not come back after the next sign-out, by which time the
// user's own session has ended anyway. This is the same boundary as "quitting
// the viewer never ends work", applied to the last thing the viewer does.
//
// Nothing of the user's is deleted. Session records, the ledger, the saved
// port, window geometry, paired-machine credentials, and the staged runtime
// bytes a live daemon and its runners are executing right now all survive, and
// an uninstall reports that it left them rather than pretending it was
// thorough. Provider credentials — Claude, Codex, Git, Somewhere — were never
// Sessions' to begin with and are not touched or read.
#[derive(Debug, Default)]
pub struct IntegrationRemoval {
    removed: Vec<String>,
    absent: Vec<String>,
    kept: Vec<String>,
    problems: Vec<String>,
}

impl IntegrationRemoval {
    pub(crate) fn removed(&mut self, what: &str) {
        self.removed.push(what.to_string());
    }

    pub(crate) fn absent(&mut self, what: &str) {
        self.absent.push(what.to_string());
    }

    pub(crate) fn kept(&mut self, what: &str) {
        self.kept.push(what.to_string());
    }

    pub(crate) fn problem(&mut self, what: &str) {
        self.problems.push(what.to_string());
    }

    // An uninstaller that cannot finish must say which piece it left behind and
    // where, because that piece is now the user's to remove by hand.
    pub fn is_complete(&self) -> bool {
        self.problems.is_empty()
    }

    pub fn report(&self) -> String {
        let mut lines = vec!["Sessions integration removal".to_string()];
        for (label, entries) in [
            ("removed", &self.removed),
            ("already absent", &self.absent),
            ("kept on purpose", &self.kept),
            ("needs attention", &self.problems),
        ] {
            for entry in entries {
                lines.push(format!("  {label}: {entry}"));
            }
        }
        lines.join("\n")
    }
}

pub fn remove_integration() -> IntegrationRemoval {
    #[cfg(target_os = "macos")]
    {
        let mut removal = IntegrationRemoval::default();
        let Some(home) = env::var_os("HOME")
            .filter(|value| !value.is_empty())
            .map(PathBuf::from)
        else {
            removal.problem(
                "HOME is unset, so Sessions could not locate the login service definition or CLI links it installed",
            );
            return removal;
        };
        remove_macos_integration(&macos_integration(&home))
    }
    #[cfg(target_os = "windows")]
    {
        crate::windows_runtime::remove_integration()
    }
    #[cfg(not(any(target_os = "macos", target_os = "windows")))]
    {
        let mut removal = IntegrationRemoval::default();
        removal.absent("this platform is a client and installs no local Sessions integration");
        removal
    }
}

// The paths install and uninstall must agree on, in one place so they cannot
// drift into an uninstaller that misses what the installer wrote.
#[cfg(target_os = "macos")]
struct MacosIntegration {
    managed_root: PathBuf,
    plist_path: PathBuf,
    cli_link_paths: Vec<PathBuf>,
}

#[cfg(target_os = "macos")]
fn macos_integration(home: &Path) -> MacosIntegration {
    MacosIntegration {
        managed_root: home
            .join("Library")
            .join("Application Support")
            .join("Sessions")
            .join("runtime"),
        plist_path: home
            .join("Library")
            .join("LaunchAgents")
            .join(format!("{SERVICE_LABEL}.plist")),
        cli_link_paths: vec![
            PathBuf::from("/opt/homebrew/bin/sessions"),
            PathBuf::from("/usr/local/bin/sessions"),
            home.join(".local").join("bin").join("sessions"),
        ],
    }
}

#[cfg(target_os = "macos")]
fn remove_macos_integration(integration: &MacosIntegration) -> IntegrationRemoval {
    let mut removal = IntegrationRemoval::default();
    let plist = integration.plist_path.display().to_string();
    // Deliberately no launchctl bootout: see the note above. Deleting the
    // definition stops the daemon from returning at the next login without
    // taking the current one, and its runners, down with it.
    match fs::remove_file(&integration.plist_path) {
        Ok(()) => removal.removed(&format!("login service definition {plist}")),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            removal.absent(&format!("login service definition {plist}"))
        }
        Err(error) => removal.problem(&format!("remove {plist}: {error}")),
    }

    for candidate in &integration.cli_link_paths {
        let shown = candidate.display().to_string();
        match managed_cli_link(candidate, &integration.managed_root) {
            Ok(true) => match fs::remove_file(candidate) {
                Ok(()) => removal.removed(&format!("`sessions` command link {shown}")),
                Err(error) => removal.problem(&format!("remove {shown}: {error}")),
            },
            // A real file, or a link into something that is not Sessions'
            // managed runtime, belongs to whoever put it there.
            Ok(false) => {}
            Err(error) => removal.problem(&error),
        }
    }

    removal.kept(&format!(
        "the staged runtime, sessions, ledger, saved port, and paired-machine credentials under {}",
        integration
            .managed_root
            .parent()
            .unwrap_or(&integration.managed_root)
            .display()
    ));
    removal.kept("every running daemon, runner, and provider process");
    removal
}

// The same ownership test install_cli_link applies before it replaces a link:
// a symlink, named `sessions`, resolving into Sessions' managed runtime.
// Anything else is someone else's tool and is left exactly as found.
#[cfg(target_os = "macos")]
fn managed_cli_link(candidate: &Path, managed_root: &Path) -> LifecycleResult<bool> {
    let metadata = match fs::symlink_metadata(candidate) {
        Ok(metadata) => metadata,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(false),
        Err(error) => {
            return Err(format!(
                "inspect {}: {error}; remove it by hand if Sessions installed it",
                candidate.display()
            ))
        }
    };
    if !metadata.file_type().is_symlink() {
        return Ok(false);
    }
    let parent = candidate
        .parent()
        .ok_or_else(|| format!("{} has no parent", candidate.display()))?;
    let target = match fs::read_link(candidate) {
        Ok(target) if target.is_absolute() => target,
        Ok(target) => parent.join(target),
        Err(error) => {
            return Err(format!(
                "read {}: {error}; remove it by hand if Sessions installed it",
                candidate.display()
            ))
        }
    };
    Ok(
        target.file_name().and_then(|name| name.to_str()) == Some("sessions")
            && target.starts_with(managed_root),
    )
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
        // Shared with remove_integration, so an uninstall cannot look in a
        // different place than the install wrote to.
        let integration = macos_integration(&home);
        Ok(Self {
            source_dir: resources.join("runtime"),
            managed_root: integration.managed_root,
            cli_link_paths: integration.cli_link_paths,
            plist_path: integration.plist_path,
            log_path: home
                .join("Library")
                .join("Logs")
                .join("Sessions")
                .join("sessionsd.log"),
            label: SERVICE_LABEL.to_string(),
            domain: format!("gui/{uid}"),
            host: LOOPBACK_HOST.to_string(),
            // Deliberately not fatal: see resolve_port. install_for_app carries
            // the reason into the status the user reads.
            port: resolve_port(app).port(),
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
            // A manager machine can legitimately retain dozens of live
            // runners. Keep a hard bound, but do not cut off the measured
            // per-session budget before a fleet-sized restart can finish.
            health_timeout_cap: Duration::from_secs(15 * 60),
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
            // Nothing is being replaced in this branch. The daemon can serve
            // the UI as soon as it is listening while retained runners finish
            // re-attaching in the background.
            wait_until_listening(config)?;
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
    verify_runtime_directory(config, &config.source_dir, &manifest)?;

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

// Every present caller happens to run validate_manifest first, but this
// function accepts an arbitrary &RuntimeManifest. An unwrap() here would turn
// a manifest that is merely corrupt — the exact case a verifier exists to
// catch — into an abort of the whole app, taking the recovery UI with it. Say
// what is wrong and what repairs it instead.
fn verify_runtime_directory(
    config: &RuntimeConfig,
    directory: &Path,
    manifest: &RuntimeManifest,
) -> LifecycleResult<()> {
    for binary in REQUIRED_BINARIES {
        let expected = manifest.binaries.get(binary).ok_or_else(|| {
            format!(
                "the runtime manifest for {} is missing {binary}; reinstall Sessions to restore a complete runtime. Your daemon and runners keep running in the meantime.",
                directory.display()
            )
        })?;
        verify_binary(config, &directory.join(binary), expected)?;
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
    // A cold boot has no captured pre-update baseline to preserve. Treat the
    // daemon as installed once its API is listening; discovery may take
    // minutes on a machine with a large retained fleet and must not make a
    // healthy service time out, get booted out, and disappear again.
    let start_result = bootstrap(config).and_then(|_| wait_until_listening(config));
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

fn wait_until_listening(config: &RuntimeConfig) -> LifecycleResult<()> {
    let timeout = config.health_timeout;
    let deadline = Instant::now() + timeout;
    let mut last_error = "no response".to_string();
    while Instant::now() < deadline {
        match health_once(config) {
            Ok(health) if health.ok && health.name == "sessionsd" => return Ok(()),
            Ok(health) => {
                last_error = format!("unexpected health response from {:?}", health.name);
            }
            Err(error) => last_error = error,
        }
        thread::sleep(config.poll_interval);
    }
    Err(format!(
        "background service did not start listening at {} within {}s: {} (logs: {})",
        config.health_url(),
        timeout.as_secs(),
        last_error,
        config.log_path.display()
    ))
}

fn wait_until_ready(config: &RuntimeConfig, baseline: &BTreeSet<String>) -> LifecycleResult<()> {
    let timeout = readiness_timeout(config, baseline.len());
    let deadline = Instant::now() + timeout;
    let mut last_error = "no response".to_string();
    while Instant::now() < deadline {
        match health_once(config) {
            Ok(health) if health.ok && health.name == "sessionsd" => match fetch_sessions(config) {
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
            },
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
    http_client(health_probe_timeout(config))?
        .get(config.health_url())
        .send()
        .and_then(|response| response.error_for_status())
        .and_then(|response| response.json::<HealthResponse>())
        .map_err(|error| format!("health probe failed: {error}"))
}

fn fetch_sessions(config: &RuntimeConfig) -> LifecycleResult<BTreeSet<String>> {
    let response = http_client(session_probe_timeout(config))?
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

fn health_probe_timeout(config: &RuntimeConfig) -> Duration {
    config.poll_interval.max(Duration::from_secs(1))
}

// Enumerating retained sessions is intentionally slower than a health probe.
// A heavily dogfooded host can have hundreds of records, and an in-progress
// discovery sweep may hold the inventory behind its reconciliation lock for
// minutes. This read happens before launchd is touched, so waiting for the same
// bounded budget used by fleet-sized re-adoption is safer than abandoning an
// otherwise healthy update after the short listening timeout.
fn session_probe_timeout(config: &RuntimeConfig) -> Duration {
    config.health_timeout_cap.max(Duration::from_secs(5))
}

fn http_client(request_timeout: Duration) -> LifecycleResult<reqwest::blocking::Client> {
    reqwest::blocking::Client::builder()
        .no_proxy()
        .connect_timeout(Duration::from_secs(1))
        .timeout(request_timeout)
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

// Shared with the native shell's window-geometry store: any file this app
// rewrites in place needs temp-plus-rename, not a truncating fs::write.
pub(crate) fn write_atomic(path: &Path, bytes: &[u8], mode: u32) -> LifecycleResult<()> {
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

pub(crate) fn unique_suffix() -> u128 {
    let timestamp = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_nanos();
    // Some filesystems and clocks expose coarser resolution than nanoseconds,
    // so two consecutive calls can observe the same timestamp. Keep the time
    // component for cross-process uniqueness and append a process-local
    // sequence so staging paths are guaranteed to differ within this process.
    (timestamp << 64) | u128::from(UNIQUE_SUFFIX_SEQUENCE.fetch_add(1, Ordering::Relaxed))
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
        config.health_timeout_cap = Duration::from_secs(15 * 60);
        assert_eq!(readiness_timeout(&config, 0), Duration::from_secs(30));
        assert_eq!(readiness_timeout(&config, 7), Duration::from_secs(135));
        assert_eq!(readiness_timeout(&config, 9), Duration::from_secs(165));
        assert_eq!(readiness_timeout(&config, 19), Duration::from_secs(315));
        assert_eq!(readiness_timeout(&config, 58), Duration::from_secs(900));
        assert_eq!(readiness_timeout(&config, 10_000), Duration::from_secs(900));
    }

    #[test]
    fn retained_session_inventory_gets_a_real_request_budget() {
        let root = env::temp_dir().join("sessions-probe-timeout-test");
        let mut config = fixture_config(&root, "tech.somewhere.sessions.probe-timeout", 47_870);
        config.health_timeout = Duration::from_secs(30);
        config.poll_interval = Duration::from_millis(200);

        assert_eq!(health_probe_timeout(&config), Duration::from_secs(1));
        config.health_timeout_cap = Duration::from_secs(15 * 60);
        assert_eq!(session_probe_timeout(&config), Duration::from_secs(15 * 60));

        config.health_timeout = Duration::from_secs(1);
        config.health_timeout_cap = Duration::from_secs(3);
        assert_eq!(session_probe_timeout(&config), Duration::from_secs(5));
    }

    #[test]
    fn cold_start_accepts_a_healthy_daemon_while_runner_discovery_continues() {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let port = listener.local_addr().unwrap().port();
        let server = thread::spawn(move || {
            let (mut stream, _) = listener.accept().unwrap();
            let mut request = [0_u8; 4096];
            let _ = stream.read(&mut request);
            let body = r#"{"ok":true,"name":"sessionsd","discovering":true}"#;
            let response = format!(
                "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{}",
                body.len(), body
            );
            stream.write_all(response.as_bytes()).unwrap();
        });
        let root = env::temp_dir().join("sessions-cold-start-listening-test");
        let mut config = fixture_config(&root, "tech.somewhere.sessions.cold-start", port);
        config.health_timeout = Duration::from_secs(1);
        config.poll_interval = Duration::from_millis(25);

        wait_until_listening(&config).unwrap();
        server.join().unwrap();
    }

    #[test]
    fn update_accepts_complete_baseline_while_unrelated_discovery_continues() {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let port = listener.local_addr().unwrap().port();
        let server = thread::spawn(move || {
            for _ in 0..2 {
                let (mut stream, _) = listener.accept().unwrap();
                let mut request = [0_u8; 4096];
                let count = stream.read(&mut request).unwrap_or_default();
                let request = String::from_utf8_lossy(&request[..count]);
                let body = if request.starts_with("GET /api/sessions ") {
                    r#"{"sessions":[{"id":"alpha"},{"id":"beta"}]}"#
                } else {
                    r#"{"ok":true,"name":"sessionsd","discovering":true}"#
                };
                let response = format!(
                    "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{}",
                    body.len(), body
                );
                stream.write_all(response.as_bytes()).unwrap();
            }
        });
        let root = env::temp_dir().join("sessions-update-discovery-baseline-test");
        let mut config = fixture_config(&root, "tech.somewhere.sessions.update-discovery", port);
        config.health_timeout = Duration::from_secs(1);
        config.poll_interval = Duration::from_millis(25);
        let baseline = BTreeSet::from(["alpha".to_string(), "beta".to_string()]);

        wait_until_ready(&config, &baseline).unwrap();
        server.join().unwrap();
    }

    #[test]
    fn concurrent_service_mutations_are_refused_instead_of_interleaved() {
        let held = lock_service_mutation().unwrap();
        let refused = lock_service_mutation().unwrap_err();
        assert!(
            refused.contains("already reconciling"),
            "second caller was not told why it was refused: {refused}"
        );
        assert!(
            refused.contains("try again"),
            "refusal did not name a next action: {refused}"
        );
        drop(held);
        // The lock must not stay wedged after the first caller finishes, or a
        // single reconcile would disable Recover for the life of the app.
        assert!(lock_service_mutation().is_ok());
    }

    #[test]
    fn unreadable_connection_settings_fall_back_visibly_instead_of_wedging_reconcile() {
        let healthy = resolved_port(Ok(9123));
        assert_eq!(healthy.port(), 9123);
        assert_eq!(healthy.problem(), None);
        let unchanged = healthy.annotate(RuntimeStatus::informational("ready", "installed"));
        assert_eq!(unchanged.detail, "installed");

        let corrupt = resolved_port(Err(
            "parse native connection settings /tmp/connections.json: expected value".to_string(),
        ));
        // Not fatal: reconcile still has a port to work with.
        assert_eq!(corrupt.port(), DEFAULT_LOOPBACK_PORT);
        // Not silent: the reason reaches the status the user actually reads.
        let annotated = corrupt.annotate(RuntimeStatus::informational("ready", "installed"));
        assert_eq!(annotated.state, "ready");
        assert!(annotated.detail.starts_with("installed; "), "{annotated:?}");
        assert!(annotated.detail.contains("/tmp/connections.json"));
        assert!(annotated
            .detail
            .contains(&format!("localhost:{DEFAULT_LOOPBACK_PORT}")));
        assert!(annotated.detail.contains("Settings"));
    }

    #[test]
    fn a_corrupt_manifest_is_reported_rather_than_aborting_the_app() {
        let root = env::temp_dir().join(format!(
            "sessions-manifest-guard-test-{}-{}",
            std::process::id(),
            unique_suffix()
        ));
        let mut config = fixture_config(&root, "tech.somewhere.sessions.manifest-guard", 47_873);
        config.verify_signatures = false;
        let mut manifest = RuntimeManifest {
            schema_version: 1,
            runtime_version: "v1".to_string(),
            target: "darwin-arm64".to_string(),
            binaries: REQUIRED_BINARIES
                .iter()
                .map(|name| (name.to_string(), "0".repeat(64)))
                .collect(),
        };
        // The first binary the verifier looks up, so the missing entry is
        // reached before any digest work can fail for another reason.
        manifest.binaries.remove(REQUIRED_BINARIES[0]);

        let error = verify_runtime_directory(&config, &root, &manifest).unwrap_err();
        assert!(error.contains(REQUIRED_BINARIES[0]), "{error}");
        assert!(error.contains("reinstall Sessions"), "{error}");
        assert!(error.contains(&root.display().to_string()), "{error}");
    }

    #[cfg(target_os = "macos")]
    #[test]
    fn uninstall_removes_only_what_sessions_installed_outside_itself() {
        use std::os::unix::fs::symlink;

        let root = env::temp_dir().join(format!(
            "sessions-uninstall-test-{}-{}",
            std::process::id(),
            unique_suffix()
        ));
        let integration = macos_integration(&root);
        let managed = integration.managed_root.join("v1");
        fs::create_dir_all(&managed).unwrap();
        fs::write(managed.join("sessions"), b"cli").unwrap();
        fs::create_dir_all(integration.plist_path.parent().unwrap()).unwrap();
        fs::write(&integration.plist_path, b"<plist/>").unwrap();

        let managed_link = root.join("bin").join("sessions");
        let foreign_link = root.join("foreign").join("sessions");
        let real_file = root.join("real").join("sessions");
        let elsewhere = root.join("someone-elses-sessions");
        for path in [&managed_link, &foreign_link, &real_file, &elsewhere] {
            fs::create_dir_all(path.parent().unwrap()).unwrap();
        }
        fs::write(&elsewhere, b"another tool").unwrap();
        fs::write(&real_file, b"a real binary someone installed").unwrap();
        symlink(managed.join("sessions"), &managed_link).unwrap();
        symlink(&elsewhere, &foreign_link).unwrap();

        let integration = MacosIntegration {
            cli_link_paths: vec![
                managed_link.clone(),
                foreign_link.clone(),
                real_file.clone(),
                root.join("never").join("existed"),
            ],
            ..integration
        };
        let removal = remove_macos_integration(&integration);
        assert!(removal.is_complete(), "{}", removal.report());

        // Gone: the definition that would restart the daemon at every login,
        // and the link Sessions published on a shared command path.
        assert!(!integration.plist_path.exists());
        assert!(fs::symlink_metadata(&managed_link).is_err());
        // Untouched: a real file at a candidate path, a link pointing outside
        // Sessions' managed runtime, and everything the user owns.
        assert_eq!(
            fs::read(&real_file).unwrap(),
            b"a real binary someone installed"
        );
        assert_eq!(fs::read_link(&foreign_link).unwrap(), elsewhere);
        assert!(managed.join("sessions").is_file());
        let report = removal.report();
        assert!(report.contains("login service definition"), "{report}");
        assert!(
            report.contains(&managed_link.display().to_string()),
            "{report}"
        );
        assert!(report.contains("kept on purpose"), "{report}");
        assert!(report.contains("running daemon, runner"), "{report}");

        // Running it twice must be a clean no-op, not a second round of errors:
        // an uninstaller is retried far more often than it is designed for.
        let again = remove_macos_integration(&integration);
        assert!(again.is_complete(), "{}", again.report());
        assert!(again.report().contains("already absent"));

        fs::remove_dir_all(&root).unwrap();
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
