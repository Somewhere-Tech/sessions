use crate::windows_runner::stable_runner_path;
use std::{
    path::{Path, PathBuf},
    time::Duration,
};

const SUPERVISE_MARKER: &str = "\" --supervise --port ";
const RUNNER_MARKER: &str = " --runner \"";

// The same readiness budget lifecycle.rs applies on macOS, for the same
// measured reason: retained runners are re-adopted serially, a successful
// attach can consume a two-second HELLO wait plus the initial ten-second replay
// window, and failed probes retry. A flat deadline reports a large retained
// fleet as failed while it is in fact recovering — and the caller's answer to
// "failed" is to stop the service and roll back, destroying the recovery that
// was in progress.
const READINESS_BASE: Duration = Duration::from_secs(30);
const READINESS_PER_SESSION: Duration = Duration::from_secs(15);
// A manager machine can legitimately retain dozens of live runners. Keep a hard
// bound, but do not cut the measured per-session budget short of what a
// fleet-sized restart needs.
const READINESS_CAP: Duration = Duration::from_secs(15 * 60);

// A stopped Windows daemon does not release its socket immediately.
// runtime/cmd/sessionsd/supervisor_windows.go gives the daemon ten seconds to
// exit on its own before terminating it, so any budget below that declared a
// completely normal shutdown a failure and aborted the handoff. Start above the
// supervisor's own timeout, and allow a little more per live session because a
// daemon detaching from a large fleet takes longer to reach exit.
const PORT_RELEASE_BASE: Duration = Duration::from_secs(15);
const PORT_RELEASE_PER_SESSION: Duration = Duration::from_secs(1);
const PORT_RELEASE_CAP: Duration = Duration::from_secs(2 * 60);

pub(crate) fn readiness_timeout(live_sessions: usize) -> Duration {
    scaled_timeout(
        READINESS_BASE,
        READINESS_PER_SESSION,
        live_sessions,
        READINESS_CAP,
    )
}

pub(crate) fn port_release_timeout(live_sessions: usize) -> Duration {
    scaled_timeout(
        PORT_RELEASE_BASE,
        PORT_RELEASE_PER_SESSION,
        live_sessions,
        PORT_RELEASE_CAP,
    )
}

fn scaled_timeout(base: Duration, per_session: Duration, count: usize, cap: Duration) -> Duration {
    let count = u32::try_from(count).unwrap_or(u32::MAX);
    per_session
        .checked_mul(count)
        .and_then(|scaled| base.checked_add(scaled))
        .unwrap_or(cap)
        .min(cap)
}

// The daemon and the runner named by a logon definition must both be immutable
// Sessions-managed files, and they must belong to one installation rather than
// being assembled from two.
//
// Two shapes satisfy that. The current one pins the runner at the root of the
// managed runtime so an update can swap the bytes without moving the path a
// live runner opened. Definitions written before that indirection existed name
// a runner beside their own daemon; they are still coherent, and refusing them
// would make the app unable to read back its own predecessor's registration and
// therefore unable to update at all.
pub(crate) fn validate_supervisor_definition(
    definition: &SupervisorDefinition,
    managed_root: &Path,
) -> Result<(), String> {
    for (label, path) in [
        ("daemon", definition.daemon.as_path()),
        ("runner", definition.runner.as_path()),
    ] {
        if !path.starts_with(managed_root) || !path.is_file() {
            return Err(format!(
                "the Windows supervisor {label} is not an immutable Sessions-managed file: {}",
                path.display()
            ));
        }
    }
    let pinned = stable_runner_path(managed_root);
    if definition.runner != pinned && definition.daemon.parent() != definition.runner.parent() {
        return Err(
            "the Windows supervisor daemon and runner must use one immutable runtime".to_string(),
        );
    }
    Ok(())
}

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

// A live session is no longer a reason to refuse a handoff — the daemon stop is
// not what would end it, and the caller preserves and verifies the exact live
// baseline across the change instead. An unsettled runtime still is: a baseline
// captured while the daemon is mid-discovery names only the runners it has
// found so far, and proceeding on a partial list would license losing the rest.
pub(crate) fn require_settled_runtime(discovering: bool, operation: &str) -> Result<(), String> {
    if discovering {
        return Err(format!(
            "Sessions is still checking for live runners, so it cannot yet prove which sessions must survive {operation}. Wait for discovery to finish and try again; the daemon, supervisor, runners, PATH, and saved port were left unchanged."
        ));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::{fs, path::PathBuf, time::SystemTime};

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
    fn a_mid_discovery_runtime_cannot_be_handed_off() {
        let error = require_settled_runtime(true, "changing the runtime").unwrap_err();
        assert!(error.contains("still checking for live runners"), "{error}");
        assert!(error.contains("left unchanged"), "{error}");
    }

    // Live sessions are preserved across the handoff and verified afterwards,
    // so a settled runtime with work in it is exactly what proceeds.
    #[test]
    fn a_settled_runtime_proceeds_whether_or_not_work_is_live() {
        require_settled_runtime(false, "changing the runtime").unwrap();
    }

    // Identical to the macOS budget in lifecycle.rs, so a fleet that survives
    // a restart on one platform is not reported as lost on the other.
    #[test]
    fn readiness_budget_scales_with_serial_runner_adoption_and_stays_bounded() {
        assert_eq!(readiness_timeout(0), Duration::from_secs(30));
        assert_eq!(readiness_timeout(7), Duration::from_secs(135));
        assert_eq!(readiness_timeout(19), Duration::from_secs(315));
        assert_eq!(readiness_timeout(58), Duration::from_secs(900));
        assert_eq!(readiness_timeout(10_000), Duration::from_secs(900));
        assert_eq!(readiness_timeout(usize::MAX), Duration::from_secs(900));
    }

    // The supervisor gives the daemon ten seconds to exit before terminating
    // it, so anything at or below that budget fails a normal shutdown.
    #[test]
    fn port_release_budget_outlasts_the_supervisor_stop_timeout() {
        assert!(port_release_timeout(0) > Duration::from_secs(10));
        assert_eq!(port_release_timeout(0), Duration::from_secs(15));
        assert_eq!(port_release_timeout(30), Duration::from_secs(45));
        assert_eq!(port_release_timeout(10_000), Duration::from_secs(120));
        assert_eq!(port_release_timeout(usize::MAX), Duration::from_secs(120));
    }

    fn scratch_runtime() -> PathBuf {
        let root = std::env::temp_dir().join(format!(
            "sessions-supervisor-definition-{}-{}",
            std::process::id(),
            SystemTime::now()
                .duration_since(SystemTime::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        fs::create_dir_all(root.join("v2")).unwrap();
        fs::write(root.join("v2").join("sessionsd.exe"), b"daemon").unwrap();
        fs::write(root.join("v2").join("sessions-runner.exe"), b"runner").unwrap();
        fs::write(root.join("sessions-runner.exe"), b"pinned").unwrap();
        root
    }

    #[test]
    fn definition_accepts_the_pinned_runner_and_a_predecessor_beside_its_daemon() {
        let root = scratch_runtime();
        let daemon = root.join("v2").join("sessionsd.exe");

        let pinned = SupervisorDefinition::new(&daemon, 8787, &stable_runner_path(&root));
        validate_supervisor_definition(&pinned, &root).unwrap();

        // A registration written before the pinned path existed must stay
        // readable, or the app cannot update from that version at all.
        let legacy =
            SupervisorDefinition::new(&daemon, 8787, &root.join("v2").join("sessions-runner.exe"));
        validate_supervisor_definition(&legacy, &root).unwrap();

        fs::remove_dir_all(&root).unwrap();
    }

    #[test]
    fn definition_refuses_unmanaged_and_mismatched_runtimes() {
        let root = scratch_runtime();
        let daemon = root.join("v2").join("sessionsd.exe");
        fs::create_dir_all(root.join("v1")).unwrap();
        fs::write(root.join("v1").join("sessions-runner.exe"), b"older").unwrap();

        // Two different versioned runtimes spliced together.
        let mixed =
            SupervisorDefinition::new(&daemon, 8787, &root.join("v1").join("sessions-runner.exe"));
        assert!(validate_supervisor_definition(&mixed, &root)
            .unwrap_err()
            .contains("one immutable runtime"));

        // Anything outside the managed root, however plausible it looks.
        let outside = SupervisorDefinition::new(
            Path::new(r"C:\Users\Ada\Downloads\sessionsd.exe"),
            8787,
            &stable_runner_path(&root),
        );
        assert!(validate_supervisor_definition(&outside, &root)
            .unwrap_err()
            .contains("Sessions-managed"));

        // A path that is managed but simply is not there.
        let missing = SupervisorDefinition::new(
            &root.join("v9").join("sessionsd.exe"),
            8787,
            &stable_runner_path(&root),
        );
        assert!(validate_supervisor_definition(&missing, &root).is_err());

        fs::remove_dir_all(&root).unwrap();
    }
}
