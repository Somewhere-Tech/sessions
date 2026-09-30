//! Opt-in acceptance of supplied, signed app runtimes through the real installer.
//! Never starts the app, a provider, or the product's launchd service label.

use super::*;
use serde_json::{json, Value};
use std::io::{Read, Seek, SeekFrom};

struct SignedSource {
    runtime: PathBuf,
    version: String,
    manifest: RuntimeManifest,
}

impl SignedSource {
    fn from_env(key: &str) -> LifecycleResult<Self> {
        let app = PathBuf::from(env::var_os(key).ok_or_else(|| format!("set {key}"))?);
        let app = fs::canonicalize(&app).map_err(|error| format!("resolve {key}: {error}"))?;
        run_checked_path(
            Path::new("/usr/bin/codesign"),
            &["--verify", "--deep", "--strict"],
            &app,
        )?;
        let details = Command::new("/usr/bin/codesign")
            .args(["--display", "--verbose=4"])
            .arg(&app)
            .output()
            .map_err(|error| format!("inspect app signature: {error}"))?;
        if !details.status.success() {
            return Err(format!(
                "inspect app signature: {}",
                output_detail(&details)
            ));
        }
        let details = String::from_utf8_lossy(&details.stderr);
        if !details.contains("Authority=Developer ID Application:") {
            return Err(format!(
                "{key} must select a Developer ID app, not an ad-hoc fixture"
            ));
        }
        let plist = app.join("Contents/Info.plist");
        let version = command_text_path(
            Path::new("/usr/libexec/PlistBuddy"),
            &["-c", "Print :CFBundleShortVersionString"],
            &plist,
        )?;
        let runtime = app.join("Contents/Resources/runtime");
        let manifest = serde_json::from_slice(
            &fs::read(runtime.join("runtime-manifest.json"))
                .map_err(|error| format!("read signed runtime manifest: {error}"))?,
        )
        .map_err(|error| format!("parse signed runtime manifest: {error}"))?;
        Ok(Self {
            runtime,
            version,
            manifest,
        })
    }
}

struct SignedFixture {
    config: RuntimeConfig,
    root: PathBuf,
    session_id: Option<String>,
    runner_pid: Option<u32>,
    child_pid: Option<u32>,
    retain: bool,
}

impl SignedFixture {
    fn new() -> LifecycleResult<Self> {
        let uid = command_text(Path::new("/usr/bin/id"), &["-u"])?;
        let domain = format!("gui/{uid}");
        command_text(Path::new("/bin/launchctl"), &["print", &domain])?;
        let root = PathBuf::from(command_text(
            Path::new("/usr/bin/mktemp"),
            &["-d", "/tmp/ssr.XXXXXX"],
        )?);
        let home = root.join("h");
        let port_probe =
            TcpListener::bind((LOOPBACK_HOST, 0)).map_err(|error| error.to_string())?;
        let port = port_probe
            .local_addr()
            .map_err(|error| error.to_string())?
            .port();
        let label = format!(
            "tech.somewhere.sessions.signed-fixture.{}.{}",
            std::process::id(),
            unique_suffix()
        );
        let config = RuntimeConfig {
            source_dir: PathBuf::new(),
            managed_root: root.join("m"),
            cli_link_paths: vec![root.join("bin/sessions")],
            plist_path: root.join(format!("{label}.plist")),
            log_path: root.join("daemon.log"),
            label,
            domain,
            host: LOOPBACK_HOST.to_string(),
            port,
            launchctl: PathBuf::from("/bin/launchctl"),
            codesign: PathBuf::from("/usr/bin/codesign"),
            shasum: PathBuf::from("/usr/bin/shasum"),
            verify_signatures: true,
            daemon_arguments: vec!["--remote-auto-preview".to_string()],
            environment: vec![
                ("HOME".to_string(), home.display().to_string()),
                (
                    "SESSIONS_STATE_DIR".to_string(),
                    root.join("runners").display().to_string(),
                ),
                (
                    "SESSIONS_LEDGER_PATH".to_string(),
                    root.join("lanes.sqlite3").display().to_string(),
                ),
                ("SESSIONS_PORT".to_string(), port.to_string()),
                ("SESSIONS_SMOKE".to_string(), "0".to_string()),
                (
                    "CODEX_HOME".to_string(),
                    home.join("codex").display().to_string(),
                ),
                (
                    "CLAUDE_CONFIG_DIR".to_string(),
                    home.join("claude").display().to_string(),
                ),
                ("SHELL".to_string(), "/bin/bash".to_string()),
            ],
            health_timeout: Duration::from_secs(15),
            health_timeout_per_session: Duration::ZERO,
            health_timeout_cap: Duration::from_secs(15),
            poll_interval: Duration::from_millis(100),
        };
        let fixture = Self {
            config,
            root,
            session_id: None,
            runner_pid: None,
            child_pid: None,
            retain: true,
        };
        let settings = home.join(".local/state/sessions/settings.json");
        fs::create_dir_all(settings.parent().unwrap()).map_err(|error| error.to_string())?;
        write_atomic(&settings, br#"{"remote":{"auto":false}}"#, 0o600)?;
        Ok(fixture)
    }

    fn request(
        &self,
        method: reqwest::Method,
        suffix: &str,
        body: Option<Value>,
    ) -> LifecycleResult<Value> {
        let client = http_client(Duration::from_secs(15))?;
        let mut request = client.request(
            method,
            format!("http://{}:{}{suffix}", self.config.host, self.config.port),
        );
        if let Some(body) = body {
            request = request.json(&body);
        }
        request
            .send()
            .and_then(|response| response.error_for_status())
            .and_then(|response| response.json())
            .map_err(|error| format!("scratch API {suffix}: {error}"))
    }

    fn version_is(&self, expected: &str) -> LifecycleResult<()> {
        let health = self.request(reqwest::Method::GET, "/api/health", None)?;
        if health["version"]
            .as_str()
            .map(|value| value.trim_start_matches('v'))
            != Some(expected)
        {
            return Err(format!(
                "expected daemon {expected}, got {}",
                health["version"]
            ));
        }
        Ok(())
    }

    fn create_shell(&mut self) -> LifecycleResult<Value> {
        let created = self.request(
            reqwest::Method::POST,
            "/api/sessions",
            Some(json!({
                "cmd": "/bin/bash", "args": ["--noprofile", "--norc", "-i"],
                "cwd": self.root.join("h"), "name": "Signed runtime acceptance fixture",
                "env": {"HISTFILE": "/dev/null"}
            })),
        )?;
        let id = created["id"]
            .as_str()
            .ok_or("create returned no session id")?;
        owned_runner_label(id)?;
        self.session_id = Some(id.to_string());
        self.child_pid = Some(
            created["pid"]
                .as_u64()
                .and_then(|pid| u32::try_from(pid).ok())
                .filter(|pid| *pid > 0)
                .ok_or("create returned no live child pid")?,
        );
        self.runner_pid = Some(self.runner_pid()?);
        Ok(created)
    }

    fn runner_pid(&self) -> LifecycleResult<u32> {
        let id = self.session_id.as_deref().ok_or("no owned session")?;
        let target = format!("{}/{}", self.config.domain, owned_runner_label(id)?);
        let output = command_text(&self.config.launchctl, &["print", &target])?;
        output
            .lines()
            .find_map(|line| line.trim().strip_prefix("pid = ")?.parse::<u32>().ok())
            .filter(|pid| *pid > 0)
            .ok_or_else(|| "owned runner has no launchd pid".to_string())
    }

    fn communicate(&self, marker: &str) -> LifecycleResult<()> {
        let id = self.session_id.as_deref().ok_or("no owned session")?;
        let input = self.request(
            reqwest::Method::POST,
            &format!("/api/sessions/{id}/input"),
            Some(json!({"data": format!("printf '{marker}\\n'\r")})),
        )?;
        if input["ok"] != true {
            return Err("owned shell did not accept input".to_string());
        }
        let deadline = Instant::now() + Duration::from_secs(10);
        let url = format!(
            "http://{}:{}/api/sessions/{id}/snapshot",
            self.config.host, self.config.port
        );
        while Instant::now() < deadline {
            let text = http_client(Duration::from_secs(2))?
                .get(&url)
                .send()
                .and_then(|response| response.error_for_status())
                .and_then(|response| response.text())
                .map_err(|error| format!("read owned shell output: {error}"))?;
            if text.lines().any(|line| line.trim() == marker) {
                return Ok(());
            }
            thread::sleep(Duration::from_millis(100));
        }
        Err(format!(
            "literal shell output {marker} did not appear within 10s"
        ))
    }

    fn identities(&self) -> LifecycleResult<Value> {
        let id = self.session_id.as_deref().ok_or("no owned session")?;
        let sessions = self.request(reqwest::Method::GET, "/api/sessions", None)?;
        let session = sessions["sessions"]
            .as_array()
            .ok_or("sessions response is not an array")?
            .iter()
            .find(|session| session["id"] == id)
            .ok_or("owned session missing after install")?;
        if session["exited"] == true
            || session["unreachable"] == true
            || session["pid"].as_u64() != self.child_pid.map(u64::from)
        {
            return Err("owned session is not the same reachable live child".to_string());
        }
        let runner = self.runner_pid()?;
        if Some(runner) != self.runner_pid {
            return Err("owned runner pid changed".to_string());
        }
        Ok(
            json!({"runner_pid": runner, "runner_identity": process_identity(runner)?,
            "child_pid": self.child_pid, "child_identity": process_identity(self.child_pid.ok_or("no child pid")?)?}),
        )
    }

    fn exercise(
        &mut self,
        old: &SignedSource,
        new: &SignedSource,
        report: &mut Value,
    ) -> LifecycleResult<()> {
        self.config.source_dir = old.runtime.clone();
        verify_runtime_directory(&self.config, &old.runtime, &old.manifest)?;
        verify_runtime_directory(&self.config, &new.runtime, &new.manifest)?;
        let first = install_runtime(&self.config)?;
        if first.0 != InstallOutcome::Installed {
            return Err("first install was not fresh".to_string());
        }
        self.version_is(&old.version)?;
        report["checks"]["first_install"] =
            json!({"runtime_version": first.1, "daemon_version": old.version});
        self.create_shell()?;
        self.communicate("SIGNED_RUNTIME_BEFORE_UPDATE")?;
        let identity = self.identities()?;
        report["checks"]["owned_session"] = json!({"id": self.session_id, "identity": identity});
        let old_plist = fs::read(&self.config.plist_path).map_err(|error| error.to_string())?;
        self.config.source_dir = new.runtime.clone();
        self.config.daemon_arguments = vec!["--version".to_string()];
        let same = old.manifest.runtime_version == new.manifest.runtime_version
            && old.manifest.binaries == new.manifest.binaries;
        if same {
            self.set_smoke("1");
        } // Older published daemons ignore Unix arguments.
        let error = install_runtime(&self.config)
            .err()
            .ok_or("injected startup failure unexpectedly succeeded")?;
        if !error.contains("rolled back safely") {
            return Err(error);
        }
        self.version_is(&old.version)?;
        if fs::read(&self.config.plist_path).map_err(|error| error.to_string())? != old_plist {
            return Err("rollback did not restore exact previous plist".to_string());
        }
        if self.identities()? != identity {
            return Err("rollback changed runner/child identity".to_string());
        }
        verify_binary(
            &self.config,
            &stable_runner_path(&self.config),
            &old.manifest.binaries["sessions-runner"],
        )?;
        self.communicate("SIGNED_RUNTIME_AFTER_ROLLBACK")?;
        report["checks"]["rollback"] = json!({"error": error, "exact_previous_plist": true,
            "identity_preserved": true, "literal_output_observed": true,
            "injection": if same { "fixture arguments --version plus SESSIONS_SMOKE=1" } else { "fixture arguments --version" }});
        self.set_smoke("0");
        self.config.daemon_arguments = vec!["--remote-auto-preview".to_string()];
        if same {
            self.config.daemon_arguments.push("--serve".to_string());
        }
        let updated = install_runtime(&self.config)?;
        if updated.0 != (InstallOutcome::Updated { preserved: 1 }) {
            return Err(format!(
                "expected one preserved session, got {:?}",
                updated.0
            ));
        }
        self.version_is(&new.version)?;
        if self.identities()? != identity {
            return Err("successful update changed runner/child identity".to_string());
        }
        verify_runtime_directory(
            &self.config,
            &self.config.managed_root.join(&new.manifest.runtime_version),
            &new.manifest,
        )?;
        verify_binary(
            &self.config,
            &stable_runner_path(&self.config),
            &new.manifest.binaries["sessions-runner"],
        )?;
        self.communicate("SIGNED_RUNTIME_AFTER_UPDATE")?;
        report["checks"]["successful_update"] = json!({"runtime_version": updated.1, "daemon_version": new.version,
            "identity_preserved": true, "literal_output_observed": true, "installed_hashes_verified": true});
        Ok(())
    }

    fn set_smoke(&mut self, value: &str) {
        for (key, current) in &mut self.config.environment {
            if key == "SESSIONS_SMOKE" {
                *current = value.to_string();
            }
        }
    }

    fn recover_created_id(&mut self) -> LifecycleResult<()> {
        if self.session_id.is_some() {
            return Ok(());
        }
        let agents = self.root.join("h/Library/LaunchAgents");
        let entries = match fs::read_dir(&agents) {
            Ok(entries) => entries,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(()),
            Err(error) => return Err(error.to_string()),
        };
        for entry in entries {
            let path = entry.map_err(|error| error.to_string())?.path();
            let Some(file) = path.file_name().and_then(|file| file.to_str()) else {
                continue;
            };
            let Some(id) = file
                .strip_prefix("tech.somewhere.sessions.runner.")
                .and_then(|file| file.strip_suffix(".plist"))
            else {
                continue;
            };
            owned_runner_label(id)?;
            let plist = fs::read_to_string(&path).map_err(|error| error.to_string())?;
            if !plist.contains(&xml_escape(&self.root.display().to_string())) {
                return Err("scratch runner definition points outside fixture root".to_string());
            }
            if self.session_id.is_some() {
                return Err(
                    "multiple runner definitions in single-create fixture; preserve for inspection"
                        .to_string(),
                );
            }
            self.session_id = Some(id.to_string());
        }
        Ok(())
    }

    fn cleanup_runner(&self) -> LifecycleResult<()> {
        if let Some(id) = &self.session_id {
            let _ = self.request(
                reqwest::Method::DELETE,
                &format!("/api/sessions/{id}?force=1"),
                None,
            );
            let label = owned_runner_label(id)?;
            let path = self
                .root
                .join("h/Library/LaunchAgents")
                .join(format!("{label}.plist"));
            if path.exists() {
                let plist = fs::read_to_string(&path).map_err(|error| error.to_string())?;
                if !plist.contains(&xml_escape(&self.root.display().to_string())) {
                    return Err(
                        "owned runner definition no longer points into scratch root; retain it"
                            .to_string(),
                    );
                }
            }
            // The UUID came from our create response or verified private plist.
            // Reaping may already have removed that plist; still check its exact job.
            let mut runner = self.config.clone();
            runner.label = label;
            bootout_if_loaded(&runner)?;
        }
        Ok(())
    }

    fn cleanup(&mut self) -> LifecycleResult<()> {
        let runner_result = self
            .recover_created_id()
            .and_then(|_| self.cleanup_runner());
        // Stop our unique daemon even if runner evidence needs manual inspection.
        let daemon_result = bootout_if_loaded(&self.config);
        runner_result?;
        daemon_result?;
        let deadline = Instant::now() + Duration::from_secs(5);
        while Instant::now() < deadline {
            let live = [self.runner_pid, self.child_pid]
                .into_iter()
                .flatten()
                .any(|pid| process_identity(pid).is_ok());
            if !live {
                return Ok(());
            }
            thread::sleep(Duration::from_millis(100));
        }
        Err("owned runner/child still exists after cleanup; scratch state retained".to_string())
    }
}

impl Drop for SignedFixture {
    fn drop(&mut self) {
        match self.cleanup() {
            Ok(()) if !self.retain => {
                let _ = fs::remove_dir_all(&self.root);
            }
            result => eprintln!(
                "Signed fixture retained at {} (cleanup: {result:?})",
                self.root.display()
            ),
        }
    }
}

fn owned_runner_label(id: &str) -> LifecycleResult<String> {
    if id.len() != 36
        || !id.bytes().enumerate().all(|(index, byte)| {
            if [8, 13, 18, 23].contains(&index) {
                byte == b'-'
            } else {
                byte.is_ascii_hexdigit()
            }
        })
    {
        return Err("invalid owned session UUID".to_string());
    }
    Ok(format!("tech.somewhere.sessions.runner.{id}"))
}

fn process_identity(pid: u32) -> LifecycleResult<String> {
    command_text(
        Path::new("/bin/ps"),
        &["-p", &pid.to_string(), "-o", "lstart=", "-o", "command="],
    )
}

fn bounded_log_tail(path: &Path) -> Vec<u8> {
    let Ok(mut file) = fs::File::open(path) else {
        return Vec::new();
    };
    let Ok(metadata) = file.metadata() else {
        return Vec::new();
    };
    if file
        .seek(SeekFrom::Start(metadata.len().saturating_sub(16 * 1024)))
        .is_err()
    {
        return Vec::new();
    }
    let mut bytes = Vec::new();
    let _ = file.take(16 * 1024).read_to_end(&mut bytes);
    bytes
}

#[test]
fn signed_fixture_rejects_non_session_runner_targets() {
    assert!(owned_runner_label("tech.somewhere.sessions.daemon").is_err());
    assert!(owned_runner_label("../../another-session").is_err());
    assert!(owned_runner_label("12345678-1234-1234-1234-123456789abc").is_ok());
}

#[test]
fn signed_fixture_diagnostics_are_bounded_to_the_log_tail() {
    let root = env::temp_dir().join(format!("signed-log-tail-{}", unique_suffix()));
    fs::create_dir(&root).unwrap();
    let path = root.join("fixture.log");
    let mut bytes = vec![b'a'; 32 * 1024];
    bytes.extend_from_slice(b"last fixture line\n");
    fs::write(&path, &bytes).unwrap();
    let tail = bounded_log_tail(&path);
    assert_eq!(tail.len(), 16 * 1024);
    assert!(tail.ends_with(b"last fixture line\n"));
    fs::remove_dir_all(root).unwrap();
}

#[test]
#[ignore = "requires explicitly selected signed app bundles and an external receipt path"]
fn exact_signed_runtime_install_rollback_and_update() {
    let old = SignedSource::from_env("SESSIONS_ACCEPTANCE_OLD_APP").expect("verified old app");
    let new = SignedSource::from_env("SESSIONS_ACCEPTANCE_NEW_APP").expect("verified new app");
    let receipt = PathBuf::from(
        env::var_os("SESSIONS_ACCEPTANCE_RECEIPT").expect("set external receipt path"),
    );
    assert!(
        receipt.is_absolute() && !receipt.exists(),
        "receipt must be new and absolute"
    );
    let mut fixture = SignedFixture::new().expect("isolated signed fixture");
    assert!(
        !receipt.starts_with(&fixture.root),
        "receipt must survive scratch removal"
    );
    let same = old.manifest.runtime_version == new.manifest.runtime_version
        && old.manifest.binaries == new.manifest.binaries;
    let mut report = json!({"status": "failed", "mode": if same { "same_version_dry_run" } else { "signed_runtime_upgrade" },
        "old_runtime": old.manifest, "new_runtime": new.manifest, "scratch_root": fixture.root,
        "service_label": fixture.config.label, "port": fixture.config.port, "checks": {},
        "excluded": ["fresh-user GUI onboarding", "GUI updater", "provider login", "OS reboot", "phone hardware"]});
    let result = fixture.exercise(&old, &new, &mut report);
    let cleanup = fixture.cleanup();
    report["cleanup"] = json!({"ok": cleanup.is_ok(), "error": cleanup.as_ref().err()});
    report["error"] = json!(result.as_ref().err());
    if result.is_ok() && cleanup.is_ok() {
        report["status"] = json!("passed");
    }
    report["daemon_log_tail"] = json!(String::from_utf8_lossy(&bounded_log_tail(
        &fixture.config.log_path
    )));
    let bytes = serde_json::to_vec_pretty(&report).unwrap();
    let mut file = fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(&receipt)
        .expect("new receipt file");
    file.write_all(&bytes).expect("write acceptance receipt");
    file.sync_all().expect("sync acceptance receipt");
    set_file_mode(&receipt, 0o600).expect("private receipt permissions");
    fixture.retain = result.is_err() || cleanup.is_err();
    assert!(
        result.is_ok() && cleanup.is_ok(),
        "acceptance: {result:?}; cleanup: {cleanup:?}; receipt: {}",
        receipt.display()
    );
}
