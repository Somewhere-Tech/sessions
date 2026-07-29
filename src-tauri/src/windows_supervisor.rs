use std::path::{Path, PathBuf};

const SUPERVISE_MARKER: &str = "\" --supervise --port ";
const RUNNER_MARKER: &str = " --runner \"";

#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct SupervisorDefinition {
    pub daemon: PathBuf,
    pub port: u16,
    pub runner: PathBuf,
}

impl SupervisorDefinition {
    pub(crate) fn new(daemon: &Path, port: u16, runner: &Path) -> Self {
        Self {
            daemon: daemon.to_path_buf(),
            port,
            runner: runner.to_path_buf(),
        }
    }

    pub(crate) fn command_line(&self) -> String {
        format!(
            "\"{}\" --supervise --port {} --runner \"{}\"",
            self.daemon.display(),
            self.port,
            self.runner.display()
        )
    }

    pub(crate) fn parse(value: &str) -> Result<Self, String> {
        let rest = value.strip_prefix('"').ok_or_else(|| {
            "Windows supervisor definition must begin with a quoted daemon".to_string()
        })?;
        let (daemon, rest) = rest.split_once(SUPERVISE_MARKER).ok_or_else(|| {
            "Windows supervisor definition has an unsupported daemon command".to_string()
        })?;
        let (port, runner) = rest.split_once(RUNNER_MARKER).ok_or_else(|| {
            "Windows supervisor definition has an unsupported runner command".to_string()
        })?;
        let runner = runner.strip_suffix('"').ok_or_else(|| {
            "Windows supervisor definition must end with a quoted runner".to_string()
        })?;
        if daemon.is_empty() || runner.is_empty() || daemon.contains('"') || runner.contains('"') {
            return Err(
                "Windows supervisor definition contains an invalid executable path".to_string(),
            );
        }
        let port = port
            .parse::<u16>()
            .map_err(|_| "Windows supervisor definition contains an invalid port".to_string())?;
        if port < 1024 {
            return Err("Windows supervisor definition contains a privileged port".to_string());
        }
        Ok(Self {
            daemon: PathBuf::from(daemon),
            port,
            runner: PathBuf::from(runner),
        })
    }
}

pub(crate) fn require_idle(
    discovering: bool,
    live_session_count: usize,
    operation: &str,
) -> Result<(), String> {
    if discovering {
        return Err(format!(
            "Sessions is still checking for live runners. Wait for discovery to finish before {operation}."
        ));
    }
    if live_session_count != 0 {
        return Err(format!(
            "Sessions found {live_session_count} live {}. {operation} is idle-only on Windows; the daemon, supervisor, runners, PATH, and saved port were left unchanged.",
            if live_session_count == 1 {
                "session"
            } else {
                "sessions"
            }
        ));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::{require_idle, SupervisorDefinition};
    use std::path::Path;

    #[test]
    fn supervisor_definition_round_trips_paths_with_spaces() {
        let definition = SupervisorDefinition::new(
            Path::new(r"C:\Users\Ada\AppData\Local\Sessions\runtime\v2\sessionsd.exe"),
            8787,
            Path::new(r"C:\Users\Ada\AppData\Local\Sessions\runtime\v2\sessions-runner.exe"),
        );

        assert_eq!(
            SupervisorDefinition::parse(&definition.command_line()).unwrap(),
            definition
        );
    }

    #[test]
    fn supervisor_definition_rejects_extra_arguments() {
        let value = concat!(
            r#""C:\Sessions\sessionsd.exe" --supervise --port 8787 "#,
            r#"--runner "C:\Sessions\sessions-runner.exe" --unexpected"#
        );

        assert!(SupervisorDefinition::parse(value).is_err());
    }

    #[test]
    fn supervisor_definition_rejects_privileged_port() {
        let value = concat!(
            r#""C:\Sessions\sessionsd.exe" --supervise --port 443 "#,
            r#"--runner "C:\Sessions\sessions-runner.exe""#
        );

        assert!(SupervisorDefinition::parse(value).is_err());
    }

    #[test]
    fn idle_gate_refuses_discovery_and_live_sessions() {
        assert!(require_idle(true, 0, "changing the runtime").is_err());
        let error = require_idle(false, 2, "changing the runtime").unwrap_err();
        assert!(error.contains("2 live sessions"));
        assert!(error.contains("left unchanged"));
    }

    #[test]
    fn idle_gate_accepts_stable_empty_runtime() {
        require_idle(false, 0, "changing the runtime").unwrap();
    }
}
