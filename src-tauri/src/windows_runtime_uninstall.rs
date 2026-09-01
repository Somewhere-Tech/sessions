// Uninstall runs from the NSIS uninstaller, without a Tauri AppHandle to
// resolve paths with. %LOCALAPPDATA%\<identifier> is exactly what
// app_local_data_dir() returns on Windows, so derive it rather than guess at a
// PATH entry's shape — an uninstaller that matched entries loosely would be
// editing components it does not own.
fn managed_runtime_root() -> Result<PathBuf, String> {
    let local = std::env::var_os("LOCALAPPDATA")
        .filter(|value| !value.is_empty())
        .ok_or_else(|| {
            "LOCALAPPDATA is unset, so Sessions could not identify its own PATH entry to remove. Remove the Sessions runtime directory from the user PATH by hand.".to_string()
        })?;
    Ok(PathBuf::from(local).join(APP_IDENTIFIER).join("runtime"))
}

fn remove_cli_path(managed_root: &Path) -> Result<bool, String> {
    let environment = match RegKey::predef(HKEY_CURRENT_USER)
        .open_subkey_with_flags("Environment", KEY_QUERY_VALUE | KEY_SET_VALUE)
    {
        Ok(key) => key,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(false),
        Err(error) => return Err(format!("open the signed-in user's environment: {error}")),
    };
    let (current, value_type) = match environment.get_raw_value("Path") {
        Ok(value) if matches!(value.vtype, REG_SZ | REG_EXPAND_SZ) => (
            String::from_reg_value(&value)
                .map_err(|error| format!("read the signed-in user's PATH: {error}"))?,
            value.vtype,
        ),
        Ok(value) => {
            return Err(format!(
                "the signed-in user's PATH has unsupported registry type {:?}; Sessions left it untouched",
                value.vtype
            ));
        }
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(false),
        Err(error) => return Err(format!("read the signed-in user's PATH: {error}")),
    };
    let updated = remove_user_path(&current, managed_root);
    if updated == current {
        return Ok(false);
    }
    // Writing an empty string back would leave a per-user PATH that exists only
    // to say nothing. If Sessions' entry was the whole value, take the value
    // with it; the machine PATH is untouched either way.
    if updated.is_empty() {
        match environment.delete_value("Path") {
            Ok(()) => {}
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
            Err(error) => return Err(format!("clear the signed-in user's PATH: {error}")),
        }
    } else {
        let mut value = updated.to_reg_value();
        value.vtype = value_type;
        environment
            .set_raw_value("Path", &value)
            .map_err(|error| format!("write the signed-in user's PATH: {error}"))?;
    }
    broadcast_environment_change()?;
    Ok(true)
}

#[cfg(test)]
mod tests {
    use super::{
        describe_supervisor_start_error, detached_process_creation_flags,
        CREATE_BREAKAWAY_FROM_JOB, CREATE_NEW_PROCESS_GROUP, CREATE_NO_WINDOW,
    };
    use std::io;

    #[test]
    fn supervisor_start_requests_job_breakaway() {
        let flags = detached_process_creation_flags();
        assert_ne!(flags & CREATE_BREAKAWAY_FROM_JOB, 0);
        assert_ne!(flags & CREATE_NEW_PROCESS_GROUP, 0);
        assert_ne!(flags & CREATE_NO_WINDOW, 0);
    }

    #[test]
    fn denied_job_breakaway_has_an_instructional_error() {
        let message = describe_supervisor_start_error(io::Error::from_raw_os_error(5));
        assert!(message.contains("parent Job Object"));
        assert!(message.contains("Start menu"));
        assert!(message.contains("sign out and back in"));
    }
}
