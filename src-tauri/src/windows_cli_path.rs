use std::path::Path;

pub(crate) fn merge_user_path(current: &str, managed_root: &Path, runtime: &Path) -> String {
    let runtime_display = runtime.to_string_lossy().into_owned();
    let entries = unmanaged_entries(current, managed_root);
    if entries.is_empty() {
        runtime_display
    } else {
        format!("{runtime_display};{}", entries.join(";"))
    }
}

// Uninstall's counterpart to merge_user_path.
//
// It drops exactly the entries merge_user_path would have replaced and leaves
// every other component — and their order — byte-identical, because HKCU PATH
// is a value the user also edits by hand and shares with unrelated tools. An
// uninstaller that rewrote more than the one entry Sessions added would be
// removing someone else's integration, not its own.
pub(crate) fn remove_user_path(current: &str, managed_root: &Path) -> String {
    unmanaged_entries(current, managed_root).join(";")
}

// One definition of "this PATH entry belongs to Sessions", so install and
// uninstall can never disagree about which component is ours.
fn unmanaged_entries<'a>(current: &'a str, managed_root: &Path) -> Vec<&'a str> {
    let managed_key = windows_path_key(&managed_root.to_string_lossy());
    let managed_prefix = format!("{managed_key}\\");
    let mut entries = Vec::new();
    for entry in current.split(';') {
        let key = windows_path_key(entry);
        if key.is_empty() {
            if !current.is_empty() {
                entries.push(entry);
            }
            continue;
        }
        if key == managed_key || key.starts_with(&managed_prefix) {
            continue;
        }
        entries.push(entry);
    }
    entries
}

// One disposition for a failed PATH integration, wherever it happens.
//
// The install path already carried this into the RuntimeStatus the tray and
// settings screen show, while the port-change path printed the same failure to
// stderr — which a packaged Windows build has nowhere to display, so the user
// simply never learned that their `sessions` command had stopped tracking the
// installed runtime. It is never fatal: a CLI convenience must not turn a
// healthy daemon into a rollback. It just has to be visible.
pub(crate) fn describe_cli_path(detail: &mut String, result: Result<(), String>) {
    match result {
        Ok(()) => {
            detail.push_str(". Open a new terminal to use the versioned sessions CLI from PATH");
        }
        Err(error) => {
            detail.push_str(&format!(
                ". The host remains healthy, but CLI PATH setup needs attention: {error}. Restart Sessions to retry"
            ));
        }
    }
}

fn windows_path_key(value: &str) -> String {
    value
        .trim()
        .trim_matches('"')
        .replace('/', "\\")
        .trim_end_matches('\\')
        .to_ascii_lowercase()
}

#[cfg(test)]
mod tests {
    use super::{describe_cli_path, merge_user_path, remove_user_path};
    use std::path::Path;

    // Install and port change must report a PATH failure the same way, and both
    // must keep saying the host itself is fine.
    #[test]
    fn a_path_failure_reaches_the_status_the_user_reads_on_every_path() {
        let mut installed = "Windows host runtime is installed and healthy".to_string();
        describe_cli_path(&mut installed, Ok(()));
        let mut moved = "Windows background service moved safely".to_string();
        describe_cli_path(&mut moved, Ok(()));
        for detail in [&installed, &moved] {
            assert!(detail.contains("Open a new terminal"), "{detail}");
        }

        let mut installed = "Windows host runtime is installed and healthy".to_string();
        describe_cli_path(&mut installed, Err("PATH is too long".to_string()));
        let mut moved = "Windows background service moved safely".to_string();
        describe_cli_path(&mut moved, Err("PATH is too long".to_string()));
        for detail in [&installed, &moved] {
            assert!(detail.contains("PATH is too long"), "{detail}");
            assert!(detail.contains("remains healthy"), "{detail}");
            assert!(detail.contains("Restart Sessions to retry"), "{detail}");
        }
    }

    #[test]
    fn adds_current_runtime_first() {
        let root = Path::new(r"C:\Users\Ada\AppData\Local\Sessions\runtime");
        let runtime = root.join("v0.2.7-bin.abc123");

        assert_eq!(
            merge_user_path(r"C:\Windows\System32;C:\Tools", root, &runtime),
            format!(r"{};C:\Windows\System32;C:\Tools", runtime.display())
        );
    }

    #[test]
    fn replaces_only_sessions_managed_runtime_entries() {
        let root = Path::new(r"C:\Users\Ada\AppData\Local\Sessions\runtime");
        let runtime = root.join("v0.2.8-bin.new");
        let current = concat!(
            r"C:\Users\Ada\AppData\Local\Sessions\runtime\v0.2.7-bin.old;",
            r"C:\Tools;",
            r"c:/users/ada/appdata/local/sessions/runtime/v0.2.6-bin.older;",
            r"C:\Other\runtime"
        );

        assert_eq!(
            merge_user_path(current, root, &runtime),
            format!(r"{};C:\Tools;C:\Other\runtime", runtime.display())
        );
    }

    #[test]
    fn is_idempotent_and_preserves_unrelated_entries() {
        let root = Path::new(r"C:\Users\Ada\AppData\Local\Sessions\runtime");
        let runtime = root.join("v0.2.7-bin.abc123");
        let current = format!(
            r"{};C:\Program Files\Unrelated Sessions;C:\Windows",
            runtime.display()
        );

        assert_eq!(merge_user_path(&current, root, &runtime), current);
    }

    #[test]
    fn handles_an_empty_user_path_without_empty_components() {
        let root = Path::new(r"C:\Users\Ada\AppData\Local\Sessions\runtime");
        let runtime = root.join("v0.2.7-bin.abc123");

        assert_eq!(
            merge_user_path("", root, &runtime),
            runtime.display().to_string()
        );
    }

    #[test]
    fn removal_takes_back_every_managed_entry_and_nothing_else() {
        let root = Path::new(r"C:\Users\Ada\AppData\Local\Sessions\runtime");
        let current = concat!(
            r"C:\Users\Ada\AppData\Local\Sessions\runtime\v0.2.8-bin.new;",
            r"C:\Tools;",
            r"c:/users/ada/appdata/local/sessions/runtime/v0.2.7-bin.old\;",
            r"C:\Program Files\Unrelated Sessions;",
            r"C:\Other\runtime"
        );

        assert_eq!(
            remove_user_path(current, root),
            concat!(
                r"C:\Tools;",
                r"C:\Program Files\Unrelated Sessions;",
                r"C:\Other\runtime"
            )
        );
    }

    #[test]
    fn removal_is_idempotent_and_leaves_an_untouched_path_alone() {
        let root = Path::new(r"C:\Users\Ada\AppData\Local\Sessions\runtime");
        let unrelated = r"C:\Windows\System32;C:\Tools";

        assert_eq!(remove_user_path(unrelated, root), unrelated);
        assert_eq!(
            remove_user_path(&remove_user_path(unrelated, root), root),
            unrelated
        );
    }

    // A PATH that was only ever Sessions' entry must come back empty rather
    // than as a stray ";" — the caller deletes the value entirely in that case.
    #[test]
    fn removing_the_only_entry_yields_an_empty_path() {
        let root = Path::new(r"C:\Users\Ada\AppData\Local\Sessions\runtime");
        let current = root.join("v0.2.7-bin.abc123").display().to_string();

        assert!(remove_user_path(&current, root).is_empty());
        assert!(remove_user_path("", root).is_empty());
    }
}
