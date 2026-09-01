fn support_page_url(kind: &str) -> Result<&'static str, String> {
    match kind {
        "choose" => Ok(SUPPORT_TICKET_URL),
        "feedback" => Ok(SUPPORT_FEEDBACK_URL),
        "bug" => Ok(SUPPORT_BUG_URL),
        "security" => Ok(SUPPORT_SECURITY_URL),
        _ => Err("unsupported support page".to_string()),
    }
}

fn validate_external_url(value: &str) -> Result<url::Url, String> {
    let value = value.trim();
    if value.is_empty() || value.len() > 8192 || value.chars().any(char::is_control) {
        return Err("invalid external URL".to_string());
    }
    let parsed = url::Url::parse(value).map_err(|_| "invalid external URL".to_string())?;
    match parsed.scheme() {
        "http" | "https" if parsed.host_str().is_some() => Ok(parsed),
        "mailto" if !parsed.path().is_empty() => Ok(parsed),
        "vscode" if !parsed.path().is_empty() => Ok(parsed),
        _ => Err("unsupported external URL scheme".to_string()),
    }
}

fn open_external_target(url: &str) -> Result<(), String> {
    let parsed = validate_external_url(url)?;
    let target = parsed.as_str();
    #[cfg(target_os = "macos")]
    let mut command = {
        let mut command = Command::new("/usr/bin/open");
        command.arg(target);
        command
    };
    #[cfg(target_os = "windows")]
    let mut command = {
        let mut command = Command::new("rundll32.exe");
        command.args(["url.dll,FileProtocolHandler", target]);
        command
    };
    #[cfg(all(not(target_os = "macos"), not(target_os = "windows")))]
    let mut command = {
        let mut command = Command::new("xdg-open");
        command.arg(target);
        command
    };
    // Never adopt the launched application's lifetime. `/usr/bin/open` returns
    // as soon as LaunchServices accepts the request, but a cold `vscode://`
    // handler can take seconds and several xdg-open backends stay in the
    // foreground for the entire life of the browser they started. Give the
    // launcher a bounded window to report a real failure (no handler for the
    // scheme, bad arguments) and hand it to a reaper if it outlives that
    // window. Output stays inherited so the launcher's own diagnostics still
    // reach the app log; only stdin is closed, because a detached process must
    // never end up waiting on this process's input.
    command.stdin(Stdio::null());
    let mut child = command
        .spawn()
        .map_err(|error| format!("open external URL: {error}"))?;
    match wait_bounded(&mut child, EXTERNAL_LAUNCH_TIMEOUT) {
        Ok(Some(status)) if status.success() => Ok(()),
        Ok(Some(status)) => Err(format!("open external URL failed with {status}")),
        // Still running means the handoff happened and the handler simply owns
        // the foreground now. That is a successful open, not an unknown state.
        Ok(None) => {
            reap_in_background(child);
            Ok(())
        }
        Err(error) => {
            reap_in_background(child);
            Err(format!("open external URL: {error}"))
        }
    }
}

// Wait for a child up to `timeout`. Ok(None) means it is still running; the
// caller decides whether that is success or a failure, because "still running"
// means opposite things for a URL handler and for a CLI we need output from.
fn wait_bounded(child: &mut Child, timeout: Duration) -> std::io::Result<Option<ExitStatus>> {
    let deadline = Instant::now() + timeout;
    loop {
        if let Some(status) = child.try_wait()? {
            return Ok(Some(status));
        }
        let remaining = deadline.saturating_duration_since(Instant::now());
        if remaining.is_zero() {
            return Ok(None);
        }
        thread::sleep(remaining.min(Duration::from_millis(25)));
    }
}

// Child::drop does not wait, so anything we stop waiting for would stay a
// zombie for the life of the app. One short-lived thread owns the final wait.
fn reap_in_background(mut child: Child) {
    thread::spawn(move || {
        let _ = child.wait();
    });
}

#[tauri::command]
async fn open_external_url(url: String) -> Result<(), String> {
    // Sync commands run on Tauri v2's main thread, where any wait freezes every
    // webview. Follow the same spawn_blocking pattern as the other commands.
    tauri::async_runtime::spawn_blocking(move || open_external_target(&url))
        .await
        .map_err(|error| format!("external link worker failed: {error}"))?
}

#[tauri::command]
async fn open_support_page(kind: String) -> Result<(), String> {
    tauri::async_runtime::spawn_blocking(move || {
        let url = support_page_url(kind.trim())?;
        open_external_target(url)
    })
    .await
    .map_err(|error| format!("support page worker failed: {error}"))?
}

#[tauri::command]
async fn somewhere_cli_status() -> Result<SomewhereCliStatus, String> {
    tauri::async_runtime::spawn_blocking(inspect_somewhere_cli)
        .await
        .map_err(|error| format!("Somewhere CLI status worker failed: {error}"))
}

fn inspect_somewhere_cli() -> SomewhereCliStatus {
    let install_command = format!("npm install -g {SOMEWHERE_PACKAGE}");
    let update_command = "somewhere update".to_string();
    let Some(executable) = find_executable("somewhere") else {
        return SomewhereCliStatus {
            installed: false,
            installed_version: None,
            latest_version: fetch_somewhere_latest(),
            update_available: false,
            install_command,
            update_command,
            detail: "Somewhere CLI is not installed".to_string(),
        };
    };
    let installed_version = Command::new(executable)
        .arg("--version")
        .output()
        .ok()
        .filter(|output| output.status.success())
        .and_then(|output| clean_version(&String::from_utf8_lossy(&output.stdout)));
    let latest_version = fetch_somewhere_latest();
    let update_available = installed_version
        .as_deref()
        .zip(latest_version.as_deref())
        .is_some_and(|(installed, latest)| version_is_newer(latest, installed));
    let detail = match (&installed_version, &latest_version, update_available) {
        (Some(installed), Some(latest), true) => {
            format!("Somewhere CLI {installed} is installed; {latest} is available")
        }
        (Some(installed), Some(_), false) => {
            format!("Somewhere CLI {installed} is installed and up to date")
        }
        (Some(installed), None, _) => {
            format!("Somewhere CLI {installed} is installed; update check unavailable")
        }
        (None, _, _) => "Somewhere CLI was found but did not report a valid version".to_string(),
    };
    SomewhereCliStatus {
        installed: true,
        installed_version,
        latest_version,
        update_available,
        install_command,
        update_command,
        detail,
    }
}

fn find_executable(name: &str) -> Option<PathBuf> {
    if let Some(path) = env::var_os("PATH") {
        for directory in env::split_paths(&path) {
            let candidate = directory.join(name);
            if candidate.is_file() {
                return Some(candidate);
            }
            #[cfg(target_os = "windows")]
            {
                let candidate = directory.join(format!("{name}.exe"));
                if candidate.is_file() {
                    return Some(candidate);
                }
            }
        }
    }
    let home = env::var_os("HOME").map(PathBuf::from);
    [
        Some(PathBuf::from("/opt/homebrew/bin").join(name)),
        Some(PathBuf::from("/usr/local/bin").join(name)),
        home.map(|directory| directory.join(".local/bin").join(name)),
    ]
    .into_iter()
    .flatten()
    .find(|candidate| candidate.is_file())
}

fn tailscale_executable() -> Option<PathBuf> {
    find_executable("tailscale").or_else(|| {
        #[cfg(target_os = "macos")]
        {
            let app_binary = PathBuf::from("/Applications/Tailscale.app/Contents/MacOS/Tailscale");
            app_binary.is_file().then_some(app_binary)
        }
        #[cfg(target_os = "windows")]
        {
            return ["ProgramFiles", "LOCALAPPDATA"]
                .into_iter()
                .filter_map(env::var_os)
                .map(PathBuf::from)
                .map(|directory| directory.join("Tailscale").join("tailscale.exe"))
                .find(|candidate| candidate.is_file());
        }
        #[cfg(all(not(target_os = "macos"), not(target_os = "windows")))]
        {
            None
        }
    })
}

fn fetch_somewhere_latest() -> Option<String> {
    let client = reqwest::blocking::Client::builder()
        .connect_timeout(Duration::from_secs(2))
        .timeout(Duration::from_secs(4))
        .build()
        .ok()?;
    let response = client
        .get(SOMEWHERE_LATEST_URL)
        .header("accept", "application/json")
        .send()
        .ok()?
        .error_for_status()
        .ok()?;
    let body = response.json::<Value>().ok()?;
    clean_version(body.get("version")?.as_str()?)
}

fn clean_version(value: &str) -> Option<String> {
    let version = value.lines().next()?.trim().trim_start_matches('v');
    if version.is_empty()
        || version.len() > 40
        || !version.chars().all(|character| {
            character.is_ascii_alphanumeric() || matches!(character, '.' | '-' | '+')
        })
    {
        return None;
    }
    Some(version.to_string())
}

fn version_is_newer(candidate: &str, current: &str) -> bool {
    fn parts(value: &str) -> Option<[u64; 3]> {
        let stable = value.split('-').next()?;
        let parsed = stable
            .split('.')
            .map(str::parse::<u64>)
            .collect::<Result<Vec<_>, _>>()
            .ok()?;
        (parsed.len() == 3).then(|| [parsed[0], parsed[1], parsed[2]])
    }
    parts(candidate)
        .zip(parts(current))
        .is_some_and(|(candidate, current)| candidate > current)
}
