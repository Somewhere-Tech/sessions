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
        .filter(|session| !session.exited && session.pid != Some(0))
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
