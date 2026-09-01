#[cfg(test)]
mod tests {
    use super::*;
    use std::net::{TcpListener, TcpStream};

    fn read_test_http_request(stream: &mut TcpStream) -> String {
        stream
            .set_read_timeout(Some(Duration::from_secs(2)))
            .unwrap();
        let mut request = Vec::new();
        loop {
            let mut chunk = [0_u8; 2048];
            let count = stream.read(&mut chunk).unwrap();
            if count == 0 {
                break;
            }
            request.extend_from_slice(&chunk[..count]);
            let Some(headers_end) = request.windows(4).position(|window| window == b"\r\n\r\n")
            else {
                continue;
            };
            let headers_end = headers_end + 4;
            let headers = String::from_utf8_lossy(&request[..headers_end]);
            let content_length = headers
                .lines()
                .find_map(|line| {
                    line.to_ascii_lowercase()
                        .strip_prefix("content-length:")
                        .and_then(|value| value.trim().parse::<usize>().ok())
                })
                .unwrap_or(0);
            if request.len() >= headers_end + content_length {
                break;
            }
        }
        String::from_utf8(request).unwrap()
    }

    fn write_test_http_json(stream: &mut TcpStream, status: &str, body: &str) {
        write!(
            stream,
            "HTTP/1.1 {status}\r\ncontent-type: application/json\r\ncontent-length: {}\r\nconnection: close\r\n\r\n{body}",
            body.len()
        )
        .unwrap();
    }

    fn unique_test_suffix() -> u128 {
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap_or_default()
            .as_nanos()
    }

    #[cfg(desktop)]
    #[test]
    fn window_geometry_is_debounced_and_replaced_atomically() {
        let root = env::temp_dir().join(format!(
            "sessions-window-geometry-test-{}-{}",
            std::process::id(),
            unique_test_suffix()
        ));
        let path = root.join("window-geometry.json");
        let store = WindowGeometryStore::load(path.clone());
        let at = |x: i32| WindowBounds {
            x,
            y: 10,
            width: 1200,
            height: 800,
            maximized: false,
        };

        // One drag. The old code re-serialized the whole map and truncated the
        // file once per event, with the mutex held for the entire write.
        for x in 0..300 {
            store.remember("main".to_string(), at(x));
        }
        assert!(
            !path.exists(),
            "window geometry was written synchronously during a drag"
        );

        thread::sleep(WINDOW_GEOMETRY_FLUSH_DELAY + Duration::from_millis(500));
        let saved: HashMap<String, WindowBounds> =
            serde_json::from_slice(&fs::read(&path).unwrap()).unwrap();
        assert_eq!(saved.get("main"), Some(&at(299)));
        assert_eq!(store.get("main"), Some(at(299)));

        // Temp-plus-rename must not leave a partial file to be loaded next time.
        let leftovers: Vec<String> = fs::read_dir(&root)
            .unwrap()
            .filter_map(|entry| entry.ok())
            .map(|entry| entry.file_name().to_string_lossy().into_owned())
            .filter(|name| name != "window-geometry.json")
            .collect();
        assert!(
            leftovers.is_empty(),
            "atomic write left files behind: {leftovers:?}"
        );

        // Quitting or closing right after a move must still keep that position.
        store.remember("main".to_string(), at(900));
        store.flush();
        let saved: HashMap<String, WindowBounds> =
            serde_json::from_slice(&fs::read(&path).unwrap()).unwrap();
        assert_eq!(saved.get("main"), Some(&at(900)));

        thread::sleep(WINDOW_GEOMETRY_FLUSH_DELAY + Duration::from_millis(500));
        fs::remove_dir_all(&root).unwrap();
    }

    #[cfg(unix)]
    #[test]
    fn bundled_cli_input_and_output_stream_together_instead_of_deadlocking() {
        // The child writes far past a pipe buffer before it reads a byte of
        // stdin. The old code wrote all of stdin first and only began draining
        // in wait_with_output afterwards, so a machine registry over ~64 KiB
        // hung this call and its blocking-pool thread forever.
        let mut command = Command::new("/bin/sh");
        command.args([
            "-c",
            "dd if=/dev/zero bs=1024 count=200 2>/dev/null | tr '\\0' 'd' >&2; cat >/dev/null; printf '{\"machines\":[]}'",
        ]);
        let registry = vec![b'x'; 512 * 1024];
        let answer = run_json_command(
            command,
            "agent machine sync",
            Some(&registry),
            Duration::from_secs(60),
        )
        .unwrap();
        assert_eq!(answer.data, serde_json::json!({ "machines": [] }));
        assert_eq!(answer.detail.len(), 200 * 1024);
    }

    #[cfg(unix)]
    #[test]
    fn a_bundled_cli_run_that_outlives_its_budget_is_stopped_and_explained() {
        let mut command = Command::new("/bin/sh");
        command.args(["-c", "sleep 60"]);
        let started = Instant::now();
        let error = run_json_command(command, "support", None, Duration::from_secs(1)).unwrap_err();
        assert!(error.contains("did not finish within 1 seconds"), "{error}");
        // Rule 4: say what is still true and what to do next.
        assert!(error.contains("keep running"), "{error}");
        assert!(error.contains("sessions CLI"), "{error}");
        assert!(
            started.elapsed() < Duration::from_secs(20),
            "the timeout did not release the caller"
        );
    }

    #[cfg(unix)]
    #[test]
    fn external_launch_waits_only_long_enough_to_see_an_immediate_failure() {
        let detached = |program: &str| {
            Command::new("/bin/sh")
                .args(["-c", program])
                .stdin(Stdio::null())
                .stdout(Stdio::null())
                .stderr(Stdio::null())
                .spawn()
                .unwrap()
        };

        // A scheme with no registered handler still has to be reported.
        let mut refused = detached("exit 3");
        let status = wait_bounded(&mut refused, Duration::from_secs(10))
            .unwrap()
            .expect("an immediate failure was not observed");
        assert_eq!(status.code(), Some(3));

        // An xdg-open backend that stays in the foreground for the life of the
        // browser must not hold the caller — the old code called status().
        let mut lingering = detached("sleep 60");
        let started = Instant::now();
        assert!(wait_bounded(&mut lingering, Duration::from_millis(200))
            .unwrap()
            .is_none());
        assert!(
            started.elapsed() < Duration::from_secs(10),
            "a long-lived URL handler blocked the caller"
        );
        let _ = lingering.kill();
        reap_in_background(lingering);
    }

    #[cfg(desktop)]
    #[test]
    fn tray_status_polls_the_loopback_address_the_daemon_actually_binds() {
        // Not "localhost": a resolver that answers ::1 first leaves the tray
        // permanently stuck on "daemon unreachable".
        assert_eq!(
            tray_sessions_url(8787),
            "http://127.0.0.1:8787/api/sessions"
        );

        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let port = listener.local_addr().unwrap().port();
        let server = std::thread::spawn(move || {
            let (mut stream, _) = listener.accept().unwrap();
            let request = read_test_http_request(&mut stream);
            assert!(request.starts_with("GET /api/sessions HTTP/1.1"));
            write_test_http_json(
                &mut stream,
                "200 OK",
                r#"{"sessions":[{"working":true,"exited":false,"exitCode":null,"idleReason":null,"lastUserMessageAt":null}]}"#,
            );
        });

        let client = tray_http_client().expect("tray client must build");
        let snapshot = fetch_tray_snapshot(&client, port);
        assert_eq!(snapshot.working, 1);
        assert!(snapshot.reachable);
        server.join().unwrap();

        // A daemon that is not there reads as unreachable, not as idle.
        let closed = TcpListener::bind("127.0.0.1:0").unwrap();
        let closed_port = closed.local_addr().unwrap().port();
        drop(closed);
        assert!(!fetch_tray_snapshot(&client, closed_port).reachable);
    }

    #[test]
    fn scoped_queries_are_validated_and_stable() {
        let server = parse_scoped_window("?server=studio%20mac", "Studio".to_string()).unwrap();
        assert_eq!(server.label, "win-server-studio-mac");
        assert_eq!(server.query, "server=studio+mac");

        let tool = parse_scoped_window("tool=claude", "Claude".to_string()).unwrap();
        assert_eq!(tool.label, "win-tool-claude");

        let session =
            parse_scoped_window("session=abc-123&mode=single", "Session".to_string()).unwrap();
        assert_eq!(session.label, "win-session-abc-123");
        assert!(parse_scoped_window("tool=unknown", String::new()).is_err());
        assert!(parse_scoped_window("server=x&tool=codex", String::new()).is_err());
    }

    #[test]
    fn tray_counts_are_mutually_exclusive() {
        let snapshot = tray_snapshot(SessionsResponse {
            sessions: vec![
                TraySession {
                    working: true,
                    exited: false,
                    exit_code: None,
                    idle_reason: None,
                    last_user_message_at: Some(1),
                },
                TraySession {
                    working: false,
                    exited: false,
                    exit_code: None,
                    idle_reason: Some("completed".to_string()),
                    last_user_message_at: None,
                },
                TraySession {
                    working: false,
                    exited: false,
                    exit_code: None,
                    idle_reason: Some("needs-input".to_string()),
                    last_user_message_at: Some(1),
                },
                TraySession {
                    working: false,
                    exited: true,
                    exit_code: Some(1),
                    idle_reason: Some("failed".to_string()),
                    last_user_message_at: None,
                },
            ],
        });
        assert_eq!(snapshot.working, 1);
        assert_eq!(snapshot.idle, 1);
        assert_eq!(snapshot.attention, 2);
        assert!(snapshot.reachable);
    }

    #[test]
    fn somewhere_versions_compare_numerically() {
        assert!(version_is_newer("0.27.3", "0.21.0"));
        assert!(version_is_newer("1.10.0", "1.9.9"));
        assert!(!version_is_newer("1.9.0", "1.10.0"));
        assert!(!version_is_newer("invalid", "1.0.0"));
        assert_eq!(clean_version("v0.27.3\n"), Some("0.27.3".to_string()));
    }

    #[test]
    fn support_pages_are_fixed_https_destinations() {
        assert_eq!(support_page_url("choose").unwrap(), SUPPORT_TICKET_URL);
        assert_eq!(support_page_url("feedback").unwrap(), SUPPORT_FEEDBACK_URL);
        assert_eq!(support_page_url("bug").unwrap(), SUPPORT_BUG_URL);
        assert_eq!(support_page_url("security").unwrap(), SUPPORT_SECURITY_URL);
        assert!(support_page_url("https://attacker.example").is_err());
        for url in [
            SUPPORT_TICKET_URL,
            SUPPORT_FEEDBACK_URL,
            SUPPORT_BUG_URL,
            SUPPORT_SECURITY_URL,
        ] {
            let parsed = url::Url::parse(url).unwrap();
            assert_eq!(parsed.scheme(), "https");
            assert_eq!(parsed.host_str(), Some("github.com"));
        }
    }

    #[test]
    fn external_links_allow_only_explicit_non_executable_schemes() {
        for valid in [
            "https://somewhere.tech/sessions",
            "http://127.0.0.1:8787/docs",
            "mailto:support@somewhere.tech",
            "vscode://file/Users/test/project/main.go:12",
        ] {
            assert!(
                validate_external_url(valid).is_ok(),
                "external URL was rejected: {valid}"
            );
        }
        for invalid in [
            "javascript:alert(1)",
            "data:text/html,hello",
            "file:///Users/test/.ssh/id_ed25519",
            "https://example.com/\nnext",
            "relative/path",
        ] {
            assert!(
                validate_external_url(invalid).is_err(),
                "unsafe URL was accepted: {invalid}"
            );
        }
    }

    #[test]
    fn native_pairing_links_keep_tickets_out_of_the_request_url() {
        let parsed = parse_native_pairing_link(
            "https://mac-mini.example.ts.net/#pair=ticket-id.ticket-secret",
        )
        .unwrap();
        assert_eq!(
            parsed,
            ParsedPairingLink {
                endpoint: "https://mac-mini.example.ts.net".to_string(),
                claim_url: "https://mac-mini.example.ts.net/api/pair/claim".to_string(),
                ticket: "ticket-id.ticket-secret".to_string(),
            }
        );

        let lan = parse_native_pairing_link("http://192.168.1.25:8787/#pair=one%2Etwo").unwrap();
        assert_eq!(lan.endpoint, "http://192.168.1.25:8787");
        assert_eq!(lan.ticket, "one.two");
    }

    #[test]
    fn native_pairing_rejects_unsafe_or_ambiguous_links() {
        for invalid in [
            "ticket-only",
            "http://example.com/#pair=secret",
            "ftp://192.168.1.25/#pair=secret",
            "https://user:password@example.com/#pair=secret",
            "https://example.com/other#pair=secret",
            "https://example.com/?query=1#pair=secret",
            "https://example.com/#pair=one&pair=two",
            "https://example.com/#other=secret",
        ] {
            assert!(
                parse_native_pairing_link(invalid).is_err(),
                "unsafe link was accepted: {invalid}"
            );
        }
    }

    #[test]
    fn native_pairing_http_accepts_only_private_or_loopback_hosts() {
        for valid in [
            "localhost",
            "127.0.0.1",
            "192.168.1.25",
            "::1",
            "fc00::1",
            "fd12:3456:789a::1",
        ] {
            assert!(
                pairing_http_host_is_private(valid),
                "private host was rejected: {valid}"
            );
        }
        for invalid in [
            "example.com",
            "8.8.8.8",
            "169.254.1.1",
            "fe80::1",
            "2001:db8::1",
        ] {
            assert!(
                !pairing_http_host_is_private(invalid),
                "public or link-local host was accepted: {invalid}"
            );
        }
    }

    #[test]
    fn remote_machine_ids_are_strict_v4_uuids() {
        assert!(valid_remote_uuid("11111111-1111-4111-8111-111111111111"));
        assert!(!valid_remote_uuid("11111111-1111-5111-8111-111111111111"));
        assert!(!valid_remote_uuid("11111111-1111-4111-C111-111111111111"));
        assert!(!valid_remote_uuid("../machine"));
    }

    #[test]
    fn tailnet_discovery_accepts_only_https_machine_names() {
        assert_eq!(
            parse_tailnet_endpoint("https://Mac-Mini.tail1234.ts.net/").unwrap(),
            "https://mac-mini.tail1234.ts.net"
        );
        for invalid in [
            "http://mac-mini.tail1234.ts.net",
            "https://example.com",
            "https://mac-mini.tail1234.ts.net:8443",
            "https://mac-mini.tail1234.ts.net/api/health",
            "https://user@mac-mini.tail1234.ts.net",
        ] {
            assert!(
                parse_tailnet_endpoint(invalid).is_err(),
                "unsafe tailnet endpoint was accepted: {invalid}"
            );
        }
    }

    #[test]
    fn nearby_discovery_accepts_only_private_ipv4_http_origins() {
        assert_eq!(
            parse_nearby_endpoint("http://192.168.1.25:8787/").unwrap(),
            "http://192.168.1.25:8787"
        );
        for invalid in [
            "https://192.168.1.25:8787",
            "http://127.0.0.1:8787",
            "http://example.com:8787",
            "http://192.168.1.25",
            "http://192.168.1.25:80",
            "http://192.168.1.25:8787/api/health",
            "http://user@192.168.1.25:8787",
        ] {
            assert!(
                parse_nearby_endpoint(invalid).is_err(),
                "unsafe nearby endpoint was accepted: {invalid}"
            );
        }
    }

    #[test]
    fn health_compatibility_accepts_legacy_and_current_but_rejects_explicit_skew() {
        let legacy: SessionsHealthResponse =
            serde_json::from_str(r#"{"ok":true,"name":"sessionsd"}"#).unwrap();
        assert!(legacy.accepts_this_client());

        let current: SessionsHealthResponse = serde_json::from_str(
            r#"{"ok":true,"name":"sessionsd","compatibility":{"api":{"minimumClient":1,"maximumClient":1}}}"#,
        )
        .unwrap();
        assert!(current.accepts_this_client());

        for incompatible in [
            r#"{"ok":true,"name":"sessionsd","compatibility":{"api":{"minimumClient":2,"maximumClient":3}}}"#,
            r#"{"ok":true,"name":"sessionsd","compatibility":{"api":{"minimumClient":0,"maximumClient":0}}}"#,
        ] {
            let health: SessionsHealthResponse = serde_json::from_str(incompatible).unwrap();
            assert!(!health.accepts_this_client());
        }
    }

    #[test]
    fn tailscale_status_parses_online_peer_metadata() {
        let status: NativeTailscaleStatus = serde_json::from_str(
            r#"{
              "BackendState":"Running",
              "Peer":{
                "nodekey:example":{
                  "HostName":"Studio Mac",
                  "DNSName":"studio-mac.tail1234.ts.net.",
                  "OS":"macOS",
                  "Online":true
                }
              }
            }"#,
        )
        .unwrap();
        assert_eq!(status.backend_state, "Running");
        let peer = status.peer.get("nodekey:example").unwrap();
        assert_eq!(peer.host_name, "Studio Mac");
        assert!(peer.online);
    }

    #[test]
    fn native_pairing_probes_before_it_consumes_the_ticket() {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let address = listener.local_addr().unwrap();
        let server = std::thread::spawn(move || {
            let (mut health, _) = listener.accept().unwrap();
            let health_request = read_test_http_request(&mut health);
            assert!(health_request.starts_with("GET /api/health HTTP/1.1"));
            write_test_http_json(&mut health, "200 OK", r#"{"ok":true,"name":"sessionsd"}"#);

            let (mut claim, _) = listener.accept().unwrap();
            let claim_request = read_test_http_request(&mut claim);
            assert!(claim_request.starts_with("POST /api/pair/claim HTTP/1.1"));
            assert!(claim_request.contains(r#""ticket":"ticket-id.ticket-secret""#));
            write_test_http_json(
                &mut claim,
                "201 Created",
                r#"{"machine_id":"11111111-1111-4111-8111-111111111111","machine_name":"Studio Mac","device_id":"22222222-2222-4222-8222-222222222222","token":"device-token","name":"MacBook"}"#,
            );
        });

        let paired =
            claim_native_pairing_link(&format!("http://{address}/#pair=ticket-id.ticket-secret"))
                .unwrap();
        assert_eq!(paired.machine_name, "Studio Mac");
        assert_eq!(paired.name, "MacBook");
        assert_eq!(paired.endpoint, format!("http://{address}"));
        server.join().unwrap();
    }
}
