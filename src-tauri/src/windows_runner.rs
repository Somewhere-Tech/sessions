use sha2::{Digest, Sha256};
use std::{
    fs,
    io::Read,
    path::{Path, PathBuf},
    time::{Duration, SystemTime, UNIX_EPOCH},
};

// The Windows counterpart to lifecycle.rs's stable_runner_path /
// activate_stable_runner.
//
// The supervisor definition in HKCU\...\Run carries the runner path the daemon
// hands to every new session (SESSIONS_RUNNER). Pointing it at the versioned
// directory of the moment means that the moment an update stages a new runtime
// and the old versioned directory is cleaned up, a live runner is executing a
// file at a path that no longer exists, and the daemon is being told to launch
// replacements from a path that is about to disappear. macOS avoids this by
// pinning one path and swapping the bytes underneath it; Windows needs the same
// indirection for the same reason.
//
// What differs is how the swap is performed. Windows will not let anyone
// replace or delete an executable image that a process is currently running,
// so a plain "copy over the top" fails exactly when a session is live — the
// case that matters. It *does* allow renaming that file: a rename changes the
// directory entry, not the open image, so a live runner keeps executing the
// same bytes under its new name and never notices. So the swap is
// rename-aside-then-rename-in rather than replace-in-place, and the renamed
// aside copy is deleted later, once nothing is running it.
const STABLE_RUNNER_NAME: &str = "sessions-runner.exe";
const RETIRED_PREFIX: &str = ".sessions-runner-retired-";
const STAGED_PREFIX: &str = ".sessions-runner-staged-";
// A staged file belongs to whichever process is mid-activation right now, so
// only sweep one old enough that no live activation could still be holding it.
const STALE_STAGING: Duration = Duration::from_secs(60 * 60);

pub(crate) fn stable_runner_path(managed_root: &Path) -> PathBuf {
    managed_root.join(STABLE_RUNNER_NAME)
}

// True when the pinned path already holds a usable runner, whatever its
// version. Callers use this before the daemon definition is written so the
// supervisor never names a runner that is not there yet; the version swap
// itself is deferred until the new daemon is known healthy.
//
// Unlike macOS this cannot re-verify a Developer ID signature cheaply, so the
// check is existence and shape only. Digest verification against the manifest
// still happens in activate_stable_runner before anything is put in place.
pub(crate) fn stable_runner_is_present(managed_root: &Path) -> bool {
    stable_runner_path(managed_root)
        .metadata()
        .map(|metadata| metadata.is_file() && metadata.len() > 0)
        .unwrap_or(false)
}

// Put `versioned_runner`'s bytes at the pinned path, verifying the manifest
// digest before and after, without ever replacing a file a live runner may be
// executing. Returns the pinned path.
pub(crate) fn activate_stable_runner(
    managed_root: &Path,
    versioned_runner: &Path,
    expected_digest: &str,
) -> Result<PathBuf, String> {
    let destination = stable_runner_path(managed_root);
    if matches_digest(&destination, expected_digest) {
        return Ok(destination);
    }

    fs::create_dir_all(managed_root).map_err(|error| {
        format!(
            "create Sessions runtime root {}: {error}",
            managed_root.display()
        )
    })?;
    let staged = managed_root.join(format!("{STAGED_PREFIX}{}", unique_component()));
    let staged_result = (|| -> Result<(), String> {
        fs::copy(versioned_runner, &staged).map_err(|error| {
            format!(
                "stage the pinned Sessions runner {} from {}: {error}",
                staged.display(),
                versioned_runner.display()
            )
        })?;
        protect(&staged)?;
        verify_digest(&staged, expected_digest)
    })();
    if let Err(error) = staged_result {
        let _ = fs::remove_file(&staged);
        return Err(error);
    }

    // Move the live runner aside before anything is written to its path. If the
    // swap below fails, this is also what gets moved back, so the pinned path
    // is never left empty while the daemon is being told to use it.
    let retired = match destination.symlink_metadata() {
        Ok(_) => {
            let retired = managed_root.join(format!("{RETIRED_PREFIX}{}", unique_component()));
            fs::rename(&destination, &retired).map_err(|error| {
                let _ = fs::remove_file(&staged);
                format!(
                    "retire the previous pinned Sessions runner {}: {error}",
                    destination.display()
                )
            })?;
            Some(retired)
        }
        Err(_) => None,
    };

    if let Err(error) = fs::rename(&staged, &destination) {
        let _ = fs::remove_file(&staged);
        // Restoring the retired runner matters more than the error that got us
        // here: without it the next session has no runner to launch at all.
        let restored = match retired {
            Some(retired) => fs::rename(&retired, &destination)
                .map_err(|restore_error| restore_error.to_string())
                .err(),
            None => None,
        };
        return Err(match restored {
            None => format!(
                "activate the pinned Sessions runner {}: {error}",
                destination.display()
            ),
            Some(restore_error) => format!(
                "activate the pinned Sessions runner {}: {error}; restoring the previous runner also failed: {restore_error}. Reopen Sessions to repair it; live runners keep running from their own copy.",
                destination.display()
            ),
        });
    }
    verify_digest(&destination, expected_digest)?;
    // The retired copy is deliberately left where it is. On Windows deleting it
    // fails for exactly as long as a live runner is still executing those bytes
    // — the case this whole dance exists to protect — so collection belongs in
    // a sweep that expects refusal, not on a path where a refusal would look
    // like a failed activation.
    let _ = retired;
    Ok(destination)
}

// Collect the copies activate_stable_runner could not delete because something
// was still running them, plus staging files left behind by a process that died
// mid-copy. Never touches the pinned runner or a versioned runtime directory,
// and never reports a failure: a file that is still in use is a live session,
// not an error.
pub(crate) fn sweep_retired_runners(managed_root: &Path) -> usize {
    let Ok(entries) = fs::read_dir(managed_root) else {
        return 0;
    };
    let mut removed = 0;
    for entry in entries.flatten() {
        let path = entry.path();
        let Some(name) = path.file_name().and_then(|name| name.to_str()) else {
            continue;
        };
        if !entry
            .file_type()
            .map(|file_type| file_type.is_file())
            .unwrap_or(false)
        {
            continue;
        }
        let collectable = if name.starts_with(RETIRED_PREFIX) {
            true
        } else if name.starts_with(STAGED_PREFIX) {
            entry
                .metadata()
                .and_then(|metadata| metadata.modified())
                .map(|modified| {
                    SystemTime::now()
                        .duration_since(modified)
                        .map(|age| age >= STALE_STAGING)
                        .unwrap_or(false)
                })
                .unwrap_or(false)
        } else {
            false
        };
        if collectable && fs::remove_file(&path).is_ok() {
            removed += 1;
        }
    }
    removed
}

pub(crate) fn file_sha256(path: &Path) -> Result<String, String> {
    let mut file =
        fs::File::open(path).map_err(|error| format!("open {}: {error}", path.display()))?;
    let mut hasher = Sha256::new();
    let mut buffer = [0_u8; 64 * 1024];
    loop {
        let count = file
            .read(&mut buffer)
            .map_err(|error| format!("read {}: {error}", path.display()))?;
        if count == 0 {
            break;
        }
        hasher.update(&buffer[..count]);
    }
    Ok(format!("{:x}", hasher.finalize()))
}

fn matches_digest(path: &Path, expected_digest: &str) -> bool {
    file_sha256(path)
        .map(|digest| digest.eq_ignore_ascii_case(expected_digest))
        .unwrap_or(false)
}

fn verify_digest(path: &Path, expected_digest: &str) -> Result<(), String> {
    let digest = file_sha256(path)?;
    if digest.eq_ignore_ascii_case(expected_digest) {
        return Ok(());
    }
    Err(format!(
        "pinned Sessions runner digest mismatch for {}: expected {expected_digest}, got {digest}",
        path.display()
    ))
}

// The pinned runner sits at the root of the managed runtime rather than inside
// an already owner-scoped versioned directory, so its own ACL is asserted here
// instead of being inherited from a directory created in the same breath.
#[cfg(target_os = "windows")]
fn protect(path: &Path) -> Result<(), String> {
    crate::windows_credentials::apply_owner_acl(path, false)
}

#[cfg(not(target_os = "windows"))]
fn protect(_path: &Path) -> Result<(), String> {
    Ok(())
}

fn unique_component() -> String {
    format!(
        "{}-{}.exe",
        std::process::id(),
        SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default()
            .as_nanos()
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    fn scratch(name: &str) -> PathBuf {
        let root = std::env::temp_dir().join(format!(
            "sessions-windows-runner-{name}-{}-{}",
            std::process::id(),
            SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        fs::create_dir_all(&root).unwrap();
        root
    }

    fn write_versioned_runner(root: &Path, version: &str, bytes: &[u8]) -> (PathBuf, String) {
        let directory = root.join(version);
        fs::create_dir_all(&directory).unwrap();
        let runner = directory.join("sessions-runner.exe");
        fs::write(&runner, bytes).unwrap();
        let digest = file_sha256(&runner).unwrap();
        (runner, digest)
    }

    fn retired_files(root: &Path) -> Vec<PathBuf> {
        let mut found = fs::read_dir(root)
            .unwrap()
            .flatten()
            .map(|entry| entry.path())
            .filter(|path| {
                path.file_name()
                    .and_then(|name| name.to_str())
                    .map(|name| name.starts_with(RETIRED_PREFIX))
                    .unwrap_or(false)
            })
            .collect::<Vec<_>>();
        found.sort();
        found
    }

    // The whole point of the indirection: the supervisor definition names one
    // path for the life of the installation and an update changes what is
    // behind it, not where it is.
    #[test]
    fn update_changes_the_bytes_behind_one_unchanging_runner_path() {
        let root = scratch("pinned-path");
        let (v1, v1_digest) = write_versioned_runner(&root, "v1", b"runner-one");
        let (v2, v2_digest) = write_versioned_runner(&root, "v2", b"runner-two-longer");

        assert!(!stable_runner_is_present(&root));
        let pinned = activate_stable_runner(&root, &v1, &v1_digest).unwrap();
        assert_eq!(pinned, root.join("sessions-runner.exe"));
        assert!(stable_runner_is_present(&root));
        assert_eq!(fs::read(&pinned).unwrap(), b"runner-one");

        assert_eq!(
            activate_stable_runner(&root, &v2, &v2_digest).unwrap(),
            pinned
        );
        assert_eq!(fs::read(&pinned).unwrap(), b"runner-two-longer");
        fs::remove_dir_all(&root).unwrap();
    }

    // A second activation of the same bytes must not churn the file a live
    // runner has open, so an unchanged runtime retires nothing at all.
    #[test]
    fn reactivating_the_same_runtime_is_a_no_op() {
        let root = scratch("idempotent");
        let (v1, digest) = write_versioned_runner(&root, "v1", b"runner-one");

        activate_stable_runner(&root, &v1, &digest).unwrap();
        let first = fs::metadata(stable_runner_path(&root)).unwrap();
        activate_stable_runner(&root, &v1, &digest).unwrap();
        let second = fs::metadata(stable_runner_path(&root)).unwrap();

        assert_eq!(first.len(), second.len());
        assert!(retired_files(&root).is_empty());
        fs::remove_dir_all(&root).unwrap();
    }

    // Windows cannot replace or delete an executable a live runner is running,
    // so the previous copy has to survive the swap under another name. Prove
    // the old bytes are still reachable afterwards — that is what keeps a live
    // session alive across an update — and that the sweep, not the swap, is
    // what eventually reclaims them.
    #[test]
    fn a_live_runner_keeps_its_own_copy_after_the_swap() {
        let root = scratch("retired-copy");
        let (v1, v1_digest) = write_versioned_runner(&root, "v1", b"runner-one");
        let (v2, v2_digest) = write_versioned_runner(&root, "v2", b"runner-two");
        activate_stable_runner(&root, &v1, &v1_digest).unwrap();
        activate_stable_runner(&root, &v2, &v2_digest).unwrap();

        assert_eq!(fs::read(stable_runner_path(&root)).unwrap(), b"runner-two");
        let retired = retired_files(&root);
        assert_eq!(retired.len(), 1, "{retired:?}");
        assert_eq!(fs::read(&retired[0]).unwrap(), b"runner-one");

        // The retired copy is collected once nothing holds it, and the pinned
        // runner and versioned runtimes are left alone.
        assert_eq!(sweep_retired_runners(&root), 1);
        assert!(retired_files(&root).is_empty());
        assert!(stable_runner_is_present(&root));
        assert!(root.join("v1").join("sessions-runner.exe").is_file());
        fs::remove_dir_all(&root).unwrap();
    }

    // A runtime whose bytes do not match the manifest must never reach the
    // path the supervisor points at, and must not cost the working runner that
    // is already there.
    #[test]
    fn a_mismatched_runtime_never_replaces_the_working_runner() {
        let root = scratch("digest-guard");
        let (v1, v1_digest) = write_versioned_runner(&root, "v1", b"runner-one");
        let (v2, _) = write_versioned_runner(&root, "v2", b"runner-two");
        activate_stable_runner(&root, &v1, &v1_digest).unwrap();

        let error = activate_stable_runner(&root, &v2, &"a".repeat(64)).unwrap_err();
        assert!(error.contains("digest mismatch"), "{error}");
        assert_eq!(fs::read(stable_runner_path(&root)).unwrap(), b"runner-one");
        assert!(retired_files(&root).is_empty());
        // The rejected staging copy is not left behind either.
        assert_eq!(sweep_retired_runners(&root), 0);
        fs::remove_dir_all(&root).unwrap();
    }

    #[test]
    fn sweeping_ignores_unrelated_files_and_fresh_staging() {
        let root = scratch("sweep-scope");
        let (v1, digest) = write_versioned_runner(&root, "v1", b"runner-one");
        activate_stable_runner(&root, &v1, &digest).unwrap();
        fs::write(root.join("runtime-manifest.json"), b"{}").unwrap();
        fs::write(root.join(format!("{STAGED_PREFIX}999-1.exe")), b"in flight").unwrap();

        assert_eq!(sweep_retired_runners(&root), 0);
        assert!(root.join("runtime-manifest.json").is_file());
        assert!(stable_runner_is_present(&root));
        fs::remove_dir_all(&root).unwrap();
    }
}
