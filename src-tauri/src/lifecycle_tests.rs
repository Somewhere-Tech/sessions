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
    fn update_accepts_ended_baseline_while_unrelated_discovery_continues() {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let port = listener.local_addr().unwrap().port();
        let server = thread::spawn(move || {
            for _ in 0..2 {
                let (mut stream, _) = listener.accept().unwrap();
                let mut request = [0_u8; 4096];
                let count = stream.read(&mut request).unwrap_or_default();
                let request = String::from_utf8_lossy(&request[..count]);
                let body = if request.starts_with("GET /api/sessions?include_exited=1 ") {
                    r#"{"sessions":[{"id":"alpha"},{"id":"beta","exited":true},{"id":"gamma","unreachable":true,"ended_by_kind":"user"}]}"#
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
        let baseline = BTreeSet::from([
            "alpha".to_string(),
            "beta".to_string(),
            "gamma".to_string(),
        ]);

        wait_until_ready(&config, &baseline).unwrap();
        server.join().unwrap();
    }

    #[test]
    fn readiness_only_rejects_unknown_or_unreachable_baseline_sessions() {
        let baseline = BTreeSet::from([
            "reachable".to_string(),
            "ended".to_string(),
            "unreachable".to_string(),
            "unknown".to_string(),
        ]);
        let current = BTreeMap::from([
            ("reachable".to_string(), SessionReadiness::Reachable),
            ("ended".to_string(), SessionReadiness::Ended),
            (
                "unreachable".to_string(),
                SessionReadiness::Unreachable,
            ),
        ]);

        assert_eq!(
            baseline_sessions_not_ready(&baseline, &current),
            vec!["unknown".to_string(), "unreachable".to_string()]
        );
    }

    #[test]
    fn update_baseline_excludes_retained_history_without_a_live_runner() {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let port = listener.local_addr().unwrap().port();
        let server = thread::spawn(move || {
            let (mut stream, _) = listener.accept().unwrap();
            let mut request = [0_u8; 4096];
            let _ = stream.read(&mut request);
            let body = r#"{"sessions":[{"id":"live","pid":42},{"id":"stale","pid":0},{"id":"ended","pid":43,"exited":true},{"id":"older-daemon"}]}"#;
            let response = format!(
                "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{}",
                body.len(), body
            );
            stream.write_all(response.as_bytes()).unwrap();
        });
        let root = env::temp_dir().join("sessions-update-live-baseline-test");
        let config = fixture_config(&root, "tech.somewhere.sessions.live-baseline", port);

        assert_eq!(
            live_session_ids(fetch_sessions(&config, false).unwrap()),
            BTreeSet::from(["live".to_string(), "older-daemon".to_string()])
        );
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
            let body = if request.starts_with("GET /api/sessions ")
                || request.starts_with("GET /api/sessions?include_exited=1 ")
            {
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
