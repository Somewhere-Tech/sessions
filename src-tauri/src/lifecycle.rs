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

// Keep the lifecycle implementation in one Rust module while bounding each
// maintenance concern: runtime installation, service migration, utilities,
// and tests. This preserves every private invariant and platform cfg.
include!("lifecycle_install.rs");
include!("lifecycle_service.rs");
include!("lifecycle_util.rs");
include!("lifecycle_tests.rs");
