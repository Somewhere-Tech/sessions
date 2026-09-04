fn pairing_http_host_is_private(host: &str) -> bool {
    if host.eq_ignore_ascii_case("localhost") {
        return true;
    }
    host.parse::<IpAddr>().is_ok_and(|address| match address {
        IpAddr::V4(address) => address.is_loopback() || address.is_private(),
        IpAddr::V6(address) => address.is_loopback() || address.segments()[0] & 0xfe00 == 0xfc00,
    })
}

fn valid_remote_uuid(value: &str) -> bool {
    if value.len() != 36 {
        return false;
    }
    value.char_indices().all(|(index, character)| {
        if matches!(index, 8 | 13 | 18 | 23) {
            character == '-'
        } else {
            character.is_ascii_hexdigit() && !character.is_ascii_uppercase()
        }
    }) && value.as_bytes().get(14) == Some(&b'4')
        && value
            .as_bytes()
            .get(19)
            .is_some_and(|value| matches!(*value, b'8' | b'9' | b'a' | b'b'))
}

fn parse_tailnet_endpoint(value: &str) -> Result<String, String> {
    let parsed = url::Url::parse(value.trim())
        .map_err(|_| "Sessions received an invalid tailnet address".to_string())?;
    let host = parsed
        .host_str()
        .ok_or_else(|| "Sessions received an invalid tailnet address".to_string())?;
    if parsed.scheme() != "https"
        || !host.to_ascii_lowercase().ends_with(".ts.net")
        || parsed.port().is_some()
        || !parsed.username().is_empty()
        || parsed.password().is_some()
        || !matches!(parsed.path(), "" | "/")
        || parsed.query().is_some()
        || parsed.fragment().is_some()
    {
        return Err(
            "Sessions discovery accepts only a Tailscale HTTPS machine address".to_string(),
        );
    }
    Ok(format!("https://{}", host.to_ascii_lowercase()))
}

fn parse_nearby_endpoint(value: &str) -> Result<String, String> {
    let parsed = url::Url::parse(value.trim())
        .map_err(|_| "Sessions received an invalid nearby address".to_string())?;
    let host = parsed
        .host_str()
        .ok_or_else(|| "Sessions received an invalid nearby address".to_string())?;
    let private_ipv4 = host
        .parse::<std::net::Ipv4Addr>()
        .is_ok_and(|address| address.is_private() && !address.is_loopback());
    let port = parsed.port();
    if parsed.scheme() != "http"
        || !private_ipv4
        || !port.is_some_and(|port| port >= 1024)
        || !parsed.username().is_empty()
        || parsed.password().is_some()
        || !matches!(parsed.path(), "" | "/")
        || parsed.query().is_some()
        || parsed.fragment().is_some()
    {
        return Err(
            "Nearby discovery accepts only a private IPv4 HTTP address with an explicit port"
                .to_string(),
        );
    }
    Ok(format!("http://{host}:{}", port.unwrap_or_default()))
}

fn tailnet_http_client(timeout: Duration) -> Result<reqwest::blocking::Client, String> {
    reqwest::blocking::Client::builder()
        .connect_timeout(Duration::from_secs(3))
        .timeout(timeout)
        .redirect(reqwest::redirect::Policy::none())
        .build()
        .map_err(|error| format!("prepare Tailscale request: {error}"))
}

fn discover_tailnet_peers() -> Result<Vec<NativeTailnetPeer>, String> {
    let executable = tailscale_executable().ok_or_else(|| {
        "Tailscale is not installed. Install it, sign in, and try again.".to_string()
    })?;
    let output = Command::new(executable)
        .args(["status", "--json"])
        .output()
        .map_err(|error| format!("could not read Tailscale status: {error}"))?;
    if !output.status.success() {
        let detail = String::from_utf8_lossy(&output.stderr).trim().to_string();
        return Err(if detail.is_empty() {
            "Tailscale is not connected. Open Tailscale and sign in.".to_string()
        } else {
            format!("Tailscale is not ready: {detail}")
        });
    }
    let status: NativeTailscaleStatus = serde_json::from_slice(&output.stdout)
        .map_err(|_| "Tailscale returned an unreadable device list".to_string())?;
    if status.backend_state != "Running" {
        return Err("Tailscale is not connected. Open Tailscale and sign in.".to_string());
    }

    let candidates: Vec<NativeTailnetPeer> = status
        .peer
        .into_values()
        .filter(|peer| peer.online)
        .filter_map(|peer| {
            let dns_name = peer.dns_name.trim().trim_end_matches('.');
            let endpoint = parse_tailnet_endpoint(&format!("https://{dns_name}")).ok()?;
            let hostname = peer.host_name.trim();
            Some(NativeTailnetPeer {
                endpoint,
                hostname: if hostname.is_empty() {
                    dns_name
                } else {
                    hostname
                }
                .to_string(),
                os: peer.os.trim().to_string(),
            })
        })
        .take(32)
        .collect();
    let client = tailnet_http_client(Duration::from_secs(5))?;
    let handles: Vec<_> = candidates
        .into_iter()
        .map(|candidate| {
            let client = client.clone();
            thread::spawn(move || {
                let response = client
                    .get(format!("{}/api/health", candidate.endpoint))
                    .header("accept", "application/json")
                    .send()
                    .ok()?;
                if !response.status().is_success() {
                    return None;
                }
                let health = response.json::<SessionsHealthResponse>().ok()?;
                (health.ok && health.name == "sessionsd" && health.accepts_this_client())
                    .then_some(candidate)
            })
        })
        .collect();
    let mut peers: Vec<NativeTailnetPeer> = handles
        .into_iter()
        .filter_map(|handle| handle.join().ok().flatten())
        .collect();
    peers.sort_by(|left, right| {
        left.hostname
            .to_lowercase()
            .cmp(&right.hostname.to_lowercase())
    });
    Ok(peers)
}

fn response_error(body: &[u8], fallback: String) -> String {
    serde_json::from_slice::<Value>(body)
        .ok()
        .and_then(|value| value.get("error")?.as_str().map(str::to_string))
        .filter(|value| !value.trim().is_empty())
        .unwrap_or(fallback)
}

fn local_device_name() -> String {
    #[cfg(target_os = "windows")]
    if let Some(name) = env::var_os("COMPUTERNAME") {
        let value = name.to_string_lossy().trim().to_string();
        if !value.is_empty() && value.chars().count() <= 80 && !value.chars().any(char::is_control)
        {
            return value;
        }
    }
    let candidates = [
        ("/usr/sbin/scutil", &["--get", "ComputerName"][..]),
        ("/bin/hostname", &[][..]),
    ];
    for (executable, args) in candidates {
        if let Ok(output) = Command::new(executable).args(args).output() {
            let value = String::from_utf8_lossy(&output.stdout).trim().to_string();
            if output.status.success()
                && !value.is_empty()
                && value.chars().count() <= 80
                && !value.chars().any(char::is_control)
            {
                return value;
            }
        }
    }
    "Sessions device".to_string()
}

fn request_tailnet_access(
    endpoint: &str,
    client_id: &str,
    name: &str,
) -> Result<NativeTailnetRequest, String> {
    let endpoint = parse_tailnet_endpoint(endpoint)?;
    if !valid_remote_uuid(client_id.trim()) {
        return Err("Sessions could not create a stable identity for this device".to_string());
    }
    let requested_name = name.trim();
    let name = if requested_name.is_empty() {
        local_device_name()
    } else {
        requested_name.to_string()
    };
    if name.chars().count() > 80 || name.chars().any(char::is_control) {
        return Err("device name must be at most 80 characters".to_string());
    }
    let client = tailnet_http_client(Duration::from_secs(12))?;
    let response = client
        .post(format!("{endpoint}/api/tailnet/access/request"))
        .header("accept", "application/json")
        .json(&serde_json::json!({ "client_id": client_id, "name": name }))
        .send()
        .map_err(|error| format!("could not request access from the other Mac: {error}"))?;
    let status = response.status();
    let body = bounded_pairing_response(response)?;
    if status.as_u16() != 202 {
        return Err(response_error(
            &body,
            format!("the other Mac returned HTTP {}", status.as_u16()),
        ));
    }
    let requested: TailnetRequestResponse = serde_json::from_slice(&body)
        .map_err(|_| "the other Mac returned an invalid access request".to_string())?;
    if !valid_remote_uuid(&requested.request_id)
        || requested.request_secret.trim().is_empty()
        || requested.request_secret.len() > 512
        || requested.request_secret.chars().any(char::is_control)
        || requested.status != "pending"
        || requested.expires_at.trim().is_empty()
    {
        return Err("the other Mac returned an invalid access request".to_string());
    }
    Ok(NativeTailnetRequest {
        endpoint,
        request_id: requested.request_id,
        request_secret: requested.request_secret,
        expires_at: requested.expires_at,
        status: requested.status,
    })
}

fn request_nearby_access(
    endpoint: &str,
    client_id: &str,
    name: &str,
) -> Result<NativeTailnetRequest, String> {
    let endpoint = parse_nearby_endpoint(endpoint)?;
    request_machine_access(
        endpoint,
        client_id,
        name,
        "/api/lan/access/request",
        "nearby machine",
    )
}

fn request_machine_access(
    endpoint: String,
    client_id: &str,
    name: &str,
    path: &str,
    machine_label: &str,
) -> Result<NativeTailnetRequest, String> {
    if !valid_remote_uuid(client_id.trim()) {
        return Err("Sessions could not create a stable identity for this device".to_string());
    }
    let requested_name = name.trim();
    let name = if requested_name.is_empty() {
        local_device_name()
    } else {
        requested_name.to_string()
    };
    if name.chars().count() > 80 || name.chars().any(char::is_control) {
        return Err("device name must be at most 80 characters".to_string());
    }
    let client = tailnet_http_client(Duration::from_secs(12))?;
    let response = client
        .post(format!("{endpoint}{path}"))
        .header("accept", "application/json")
        .json(&serde_json::json!({ "client_id": client_id, "name": name }))
        .send()
        .map_err(|error| format!("could not request access from the {machine_label}: {error}"))?;
    let status = response.status();
    let body = bounded_pairing_response(response)?;
    if status.as_u16() != 202 {
        return Err(response_error(
            &body,
            format!("{machine_label} returned HTTP {}", status.as_u16()),
        ));
    }
    let requested: TailnetRequestResponse = serde_json::from_slice(&body)
        .map_err(|_| format!("{machine_label} returned an invalid access request"))?;
    if !valid_remote_uuid(&requested.request_id)
        || requested.request_secret.trim().is_empty()
        || requested.request_secret.len() > 512
        || requested.request_secret.chars().any(char::is_control)
        || requested.status != "pending"
        || requested.expires_at.trim().is_empty()
    {
        return Err(format!(
            "{machine_label} returned an invalid access request"
        ));
    }
    Ok(NativeTailnetRequest {
        endpoint,
        request_id: requested.request_id,
        request_secret: requested.request_secret,
        expires_at: requested.expires_at,
        status: requested.status,
    })
}

fn validate_native_claim(
    endpoint: String,
    claimed: PairingClaimResponse,
) -> Result<NativePairingClaim, String> {
    let machine_id = claimed
        .machine_id
        .as_deref()
        .map(str::trim)
        .filter(|value| valid_remote_uuid(value))
        .ok_or_else(|| "the other Mac is running an older Sessions daemon".to_string())?
        .to_string();
    let machine_name = claimed
        .machine_name
        .as_deref()
        .map(str::trim)
        .filter(|value| !value.is_empty())
        .map(str::to_string)
        .unwrap_or_else(|| {
            url::Url::parse(&endpoint)
                .ok()
                .and_then(|url| url.host_str().map(str::to_string))
                .unwrap_or_else(|| "Sessions machine".to_string())
        });
    if !valid_remote_uuid(claimed.device_id.trim())
        || claimed.token.trim().is_empty()
        || claimed.token.len() > 512
        || claimed.token.chars().any(char::is_control)
        || claimed.name.trim().is_empty()
        || claimed.name.chars().count() > 80
        || claimed.name.chars().any(char::is_control)
        || machine_name.chars().count() > 80
        || machine_name.chars().any(char::is_control)
    {
        return Err("the other machine returned an invalid device credential".to_string());
    }
    Ok(NativePairingClaim {
        endpoint,
        machine_id,
        machine_name,
        device_id: claimed.device_id,
        token: claimed.token,
        name: claimed.name,
		lan_endpoint: claimed.lan_endpoint,
		tailnet_endpoint: claimed.tailnet_endpoint,
		tailnet_ip_endpoint: claimed.tailnet_ip_endpoint,
    })
}

fn claim_tailnet_access(
    endpoint: &str,
    request_id: &str,
    request_secret: &str,
) -> Result<NativeTailnetClaim, String> {
    let endpoint = parse_tailnet_endpoint(endpoint)?;
    if !valid_remote_uuid(request_id.trim())
        || request_secret.trim().is_empty()
        || request_secret.len() > 512
        || request_secret.chars().any(char::is_control)
    {
        return Err("the access request is invalid or expired".to_string());
    }
    let client = tailnet_http_client(Duration::from_secs(12))?;
    let response = client
        .post(format!("{endpoint}/api/tailnet/access/claim"))
        .header("accept", "application/json")
        .json(&serde_json::json!({
            "request_id": request_id,
            "request_secret": request_secret
        }))
        .send()
        .map_err(|error| format!("could not check the access request: {error}"))?;
    let status = response.status();
    let body = bounded_pairing_response(response)?;
    match status.as_u16() {
        202 => Ok(NativeTailnetClaim {
            status: "pending".to_string(),
            claim: None,
        }),
        403 => Ok(NativeTailnetClaim {
            status: "denied".to_string(),
            claim: None,
        }),
        410 => Ok(NativeTailnetClaim {
            status: "expired".to_string(),
            claim: None,
        }),
        201 => {
            let claimed: PairingClaimResponse = serde_json::from_slice(&body)
                .map_err(|_| "the other Mac returned an invalid device credential".to_string())?;
            Ok(NativeTailnetClaim {
                status: "accepted".to_string(),
                claim: Some(validate_native_claim(endpoint, claimed)?),
            })
        }
        _ => Err(response_error(
            &body,
            format!("the other Mac returned HTTP {}", status.as_u16()),
        )),
    }
}

fn claim_nearby_access(
    endpoint: &str,
    request_id: &str,
    request_secret: &str,
) -> Result<NativeTailnetClaim, String> {
    let endpoint = parse_nearby_endpoint(endpoint)?;
    claim_machine_access(
        endpoint,
        request_id,
        request_secret,
        "/api/lan/access/claim",
        "nearby machine",
    )
}

fn claim_machine_access(
    endpoint: String,
    request_id: &str,
    request_secret: &str,
    path: &str,
    machine_label: &str,
) -> Result<NativeTailnetClaim, String> {
    if !valid_remote_uuid(request_id.trim())
        || request_secret.trim().is_empty()
        || request_secret.len() > 512
        || request_secret.chars().any(char::is_control)
    {
        return Err("the access request is invalid or expired".to_string());
    }
    let client = tailnet_http_client(Duration::from_secs(12))?;
    let response = client
        .post(format!("{endpoint}{path}"))
        .header("accept", "application/json")
        .json(&serde_json::json!({
            "request_id": request_id,
            "request_secret": request_secret
        }))
        .send()
        .map_err(|error| format!("could not check the access request: {error}"))?;
    let status = response.status();
    let body = bounded_pairing_response(response)?;
    match status.as_u16() {
        202 => Ok(NativeTailnetClaim {
            status: "pending".to_string(),
            claim: None,
        }),
        403 => Ok(NativeTailnetClaim {
            status: "denied".to_string(),
            claim: None,
        }),
        410 => Ok(NativeTailnetClaim {
            status: "expired".to_string(),
            claim: None,
        }),
        201 => {
            let claimed: PairingClaimResponse = serde_json::from_slice(&body)
                .map_err(|_| format!("{machine_label} returned an invalid device credential"))?;
            Ok(NativeTailnetClaim {
                status: "accepted".to_string(),
                claim: Some(validate_native_claim(endpoint, claimed)?),
            })
        }
        _ => Err(response_error(
            &body,
            format!("{machine_label} returned HTTP {}", status.as_u16()),
        )),
    }
}

fn parse_native_pairing_link(value: &str) -> Result<ParsedPairingLink, String> {
    let value = value.trim();
    if value.is_empty() {
        return Err("paste the full pairing link from the other Sessions app".to_string());
    }
    if value.len() > 4096 {
        return Err("pairing link is too long".to_string());
    }

    let mut parsed = url::Url::parse(value)
        .map_err(|_| "paste the full pairing link, including https://".to_string())?;
    if parsed.host_str().is_none() {
        return Err("pairing link is missing a machine address".to_string());
    }
    if !parsed.username().is_empty() || parsed.password().is_some() {
        return Err("pairing links cannot contain a username or password".to_string());
    }
    match parsed.scheme() {
        "https" => {}
        "http" if pairing_http_host_is_private(parsed.host_str().unwrap_or_default()) => {}
        "http" => {
            return Err("unencrypted pairing is allowed only to a private LAN address".to_string())
        }
        _ => return Err("pairing links must use HTTPS or private-LAN HTTP".to_string()),
    }
    if !matches!(parsed.path(), "" | "/") || parsed.query().is_some() {
        return Err("pairing link has an unexpected path or query".to_string());
    }

    let fragment = parsed
        .fragment()
        .ok_or_else(|| "pairing link is missing its one-time ticket".to_string())?;
    let mut ticket: Option<String> = None;
    for (key, value) in url::form_urlencoded::parse(fragment.as_bytes()) {
        if key != "pair" || ticket.is_some() {
            return Err("pairing link has an invalid one-time ticket".to_string());
        }
        let value = value.trim();
        if value.is_empty() || value.len() > 512 || value.chars().any(char::is_control) {
            return Err("pairing link has an invalid one-time ticket".to_string());
        }
        ticket = Some(value.to_string());
    }
    let ticket = ticket.ok_or_else(|| "pairing link is missing its one-time ticket".to_string())?;

    parsed.set_fragment(None);
    parsed.set_path("");
    let endpoint = parsed.as_str().trim_end_matches('/').to_string();
    let claim_url = format!("{endpoint}/api/pair/claim");
    Ok(ParsedPairingLink {
        endpoint,
        claim_url,
        ticket,
    })
}

fn claim_native_pairing_link(value: &str) -> Result<NativePairingClaim, String> {
    let link = parse_native_pairing_link(value)?;
    let client = reqwest::blocking::Client::builder()
        .connect_timeout(Duration::from_secs(5))
        .timeout(Duration::from_secs(12))
        .redirect(reqwest::redirect::Policy::none())
        .build()
        .map_err(|error| format!("prepare secure pairing request: {error}"))?;

    // Match the stronger remote-environment onboarding pattern: identify the
    // endpoint before consuming its one-time credential. A typo or unrelated
    // HTTPS service therefore cannot burn a valid pairing link.
    let health_url = format!("{}/api/health", link.endpoint);
    let health_response = client
        .get(&health_url)
        .header("accept", "application/json")
        .send()
        .map_err(|error| format!("could not reach the other Sessions machine: {error}"))?;
    let health_status = health_response.status();
    let health_body = bounded_pairing_response(health_response)?;
    if !health_status.is_success() {
        return Err(format!(
            "the pairing link does not reach Sessions (HTTP {})",
            health_status.as_u16()
        ));
    }
    let health: SessionsHealthResponse = serde_json::from_slice(&health_body)
        .map_err(|_| "the pairing link does not reach a Sessions daemon".to_string())?;
    if !health.ok || health.name != "sessionsd" {
        return Err("the pairing link does not reach a Sessions daemon".to_string());
    }

    let response = client
        .post(&link.claim_url)
        .header("accept", "application/json")
        .json(&serde_json::json!({ "ticket": link.ticket }))
        .send()
        .map_err(|error| format!("could not reach the other Sessions machine: {error}"))?;
    let status = response.status();
    let body = bounded_pairing_response(response)?;
    if !status.is_success() {
        let detail = serde_json::from_slice::<Value>(&body)
            .ok()
            .and_then(|value| value.get("error")?.as_str().map(str::to_string))
            .filter(|value| !value.trim().is_empty())
            .unwrap_or_else(|| format!("the other machine returned HTTP {}", status.as_u16()));
        return Err(format!(
            "{detail}. Create a new pairing link and try again."
        ));
    }
    let claimed: PairingClaimResponse = serde_json::from_slice(&body)
        .map_err(|_| "the other machine returned an invalid device credential".to_string())?;
    validate_native_claim(link.endpoint, claimed).map_err(|error| {
        if error == "the other Mac is running an older Sessions daemon" {
            format!("{error}; update Sessions there and create a new pairing link")
        } else {
            error
        }
    })
}

fn bounded_pairing_response(response: reqwest::blocking::Response) -> Result<Vec<u8>, String> {
    let mut body = Vec::new();
    response
        .take(64 * 1024 + 1)
        .read_to_end(&mut body)
        .map_err(|error| format!("read pairing response: {error}"))?;
    if body.len() > 64 * 1024 {
        return Err("the other machine returned an oversized pairing response".to_string());
    }
    Ok(body)
}

fn run_connection_action(
    app: &AppHandle,
    kind: &str,
    action: &str,
    name: Option<&str>,
) -> Result<NativeConnectionCommand, String> {
    let mut command_args = match (kind, action) {
        ("lan", "status" | "enable" | "disable") => vec!["lan".to_string(), action.to_string()],
        ("remote", "status" | "enable" | "disable") => {
            vec!["remote".to_string(), action.to_string()]
        }
        ("pair", "create") => vec!["pair".to_string()],
        _ => return Err("unsupported native connection action".to_string()),
    };
    if kind == "pair" {
        if let Some(name) = name.map(str::trim).filter(|value| !value.is_empty()) {
            if name.len() > 80 || name.chars().any(char::is_control) {
                return Err(
                    "device name must be at most 80 characters without control characters"
                        .to_string(),
                );
            }
            command_args.extend(["--name".to_string(), name.to_string()]);
        }
    }
    run_bundled_sessions_json(app, &command_args, "connection")
}

fn run_backup_action(
    app: &AppHandle,
    action: &str,
    project: Option<&str>,
) -> Result<NativeConnectionCommand, String> {
    // A first or catch-up backup uploads real data, so it gets the transfer
    // budget; status and enable only talk to the local daemon.
    let (command_args, timeout) = match action {
        "status" => (
            vec!["backup".to_string(), "status".to_string()],
            BUNDLED_CLI_TIMEOUT,
        ),
        "now" => (
            vec!["backup".to_string(), "now".to_string()],
            BUNDLED_CLI_TRANSFER_TIMEOUT,
        ),
        "enable" => {
            let project = project
                .map(str::trim)
                .filter(|value| !value.is_empty())
                .ok_or_else(|| "choose a Somewhere project for Sessions backups".to_string())?;
            if project.len() > 120
                || matches!(project, "." | "..")
                || !project.chars().all(|character| {
                    character.is_ascii_alphanumeric() || matches!(character, '-' | '_' | '.')
                })
            {
                return Err(
                    "Somewhere project must use only letters, numbers, dots, dashes, or underscores"
                        .to_string(),
                );
            }
            (
                vec![
                    "backup".to_string(),
                    "enable".to_string(),
                    "--project".to_string(),
                    project.to_string(),
                    "--interval".to_string(),
                    "15m".to_string(),
                    "--encrypt".to_string(),
                ],
                BUNDLED_CLI_TIMEOUT,
            )
        }
        _ => return Err("unsupported native backup action".to_string()),
    };
    run_bundled_sessions_json_with_timeout(app, &command_args, "backup", timeout)
}

fn run_bundled_sessions_json(
    app: &AppHandle,
    command_args: &[String],
    response_kind: &str,
) -> Result<NativeConnectionCommand, String> {
    run_bundled_sessions_json_with_input(
        app,
        command_args,
        response_kind,
        None,
        BUNDLED_CLI_TIMEOUT,
    )
}

fn run_bundled_sessions_json_with_timeout(
    app: &AppHandle,
    command_args: &[String],
    response_kind: &str,
    timeout: Duration,
) -> Result<NativeConnectionCommand, String> {
    run_bundled_sessions_json_with_input(app, command_args, response_kind, None, timeout)
}

fn run_bundled_sessions_json_with_input(
    app: &AppHandle,
    command_args: &[String],
    response_kind: &str,
    input: Option<&[u8]>,
    timeout: Duration,
) -> Result<NativeConnectionCommand, String> {
    let port = *app
        .state::<RuntimeState>()
        .port
        .lock()
        .map_err(|error| error.to_string())?;
    let resources = app
        .path()
        .resource_dir()
        .map_err(|error| format!("resolve Sessions resources: {error}"))?;
    let executable = resources.join("runtime").join("sessions");
    if !executable.is_file() {
        return Err(format!(
            "bundled Sessions CLI is missing: {}",
            executable.display()
        ));
    }
    let mut command = Command::new(executable);
    let port_string = port.to_string();
    command.args([
        "--json",
        "--host",
        "127.0.0.1",
        "--port",
        port_string.as_str(),
    ]);
    command.args(command_args);
    let inherited_path = env::var("PATH").unwrap_or_default();
    command.env(
        "PATH",
        format!("/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:{inherited_path}"),
    );
    run_json_command(command, response_kind, input, timeout)
}

// Run one bundled-CLI invocation to completion and decode its JSON answer.
//
// Split out from the argument building so the process handling — which is the
// part with the failure modes — can be exercised directly in tests.
fn run_json_command(
    mut command: Command,
    response_kind: &str,
    input: Option<&[u8]>,
    timeout: Duration,
) -> Result<NativeConnectionCommand, String> {
    command
        .stdin(if input.is_some() {
            Stdio::piped()
        } else {
            Stdio::null()
        })
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    let mut child = command
        .spawn()
        .map_err(|error| format!("run bundled Sessions CLI: {error}"))?;

    // Drain both output pipes on their own threads *before* writing stdin. The
    // CLI writes progress and diagnostics while it reads its input, so an input
    // larger than the ~64 KiB pipe buffer used to deadlock permanently: this
    // side blocked in write_all because the child was not reading, while the
    // child blocked writing output nobody was reading — and the old
    // wait_with_output only began draining after write_all returned. The
    // machine registry that native_agent_machines_sync writes crosses that
    // buffer on any well-used machine.
    let stdout = child
        .stdout
        .take()
        .ok_or_else(|| "read bundled Sessions CLI output".to_string())?;
    let stderr = child
        .stderr
        .take()
        .ok_or_else(|| "read bundled Sessions CLI diagnostics".to_string())?;
    let stdout_reader = thread::spawn(move || drain_bounded(stdout));
    let stderr_reader = thread::spawn(move || drain_bounded(stderr));

    let write_result = match (input, child.stdin.take()) {
        // Dropping the handle at the end of this arm closes the pipe, which is
        // the EOF the CLI is waiting for.
        (Some(input), Some(mut stdin)) => stdin
            .write_all(input)
            .map_err(|error| format!("write bundled Sessions CLI input: {error}")),
        (Some(_), None) => Err("open bundled Sessions CLI input".to_string()),
        (None, _) => Ok(()),
    };

    let finished = if write_result.is_ok() {
        matches!(wait_bounded(&mut child, timeout), Ok(Some(_)))
    } else {
        false
    };
    let status = if finished {
        child.try_wait().ok().flatten()
    } else {
        // Stop the run so the reader threads see EOF and are released. The
        // daemon and every runner are separate processes; ending this CLI
        // invocation cannot touch a live session.
        let _ = child.kill();
        child.wait().ok()
    };
    // Join before reporting anything: these threads hold the pipe ends.
    let stdout = stdout_reader.join().unwrap_or_default();
    let stderr = stderr_reader.join().unwrap_or_default();
    write_result?;

    let stdout = String::from_utf8_lossy(&stdout).trim().to_string();
    let stderr = String::from_utf8_lossy(&stderr).trim().to_string();
    let Some(status) = status.filter(|_| finished) else {
        return Err(format!(
            "the sessions {response_kind} command did not finish within {} seconds and was stopped. Your daemon and every runner keep running; retry, or run the same command from the sessions CLI to watch its progress.",
            timeout.as_secs()
        ));
    };
    if !status.success() {
        let detail = if stderr.is_empty() { stdout } else { stderr };
        return Err(if detail.is_empty() {
            format!("sessions {response_kind} command failed with {status}")
        } else {
            detail
        });
    }
    let data = serde_json::from_str(&stdout)
        .map_err(|error| format!("Sessions returned invalid {response_kind} data: {error}"))?;
    Ok(NativeConnectionCommand {
        data,
        detail: stderr,
    })
}

// Read up to the cap, then keep draining to the void. The child must never
// block on a full pipe just because we stopped being interested in its output.
fn drain_bounded(mut source: impl Read) -> Vec<u8> {
    let mut buffer = Vec::new();
    let _ = source
        .by_ref()
        .take(BUNDLED_CLI_MAX_OUTPUT)
        .read_to_end(&mut buffer);
    let _ = std::io::copy(&mut source, &mut std::io::sink());
    buffer
}
