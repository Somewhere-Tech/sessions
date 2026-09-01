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
