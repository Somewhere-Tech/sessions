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
