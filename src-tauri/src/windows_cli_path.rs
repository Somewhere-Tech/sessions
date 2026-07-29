use std::path::Path;

pub(crate) fn merge_user_path(current: &str, managed_root: &Path, runtime: &Path) -> String {
    let managed_key = windows_path_key(&managed_root.to_string_lossy());
    let managed_prefix = format!("{managed_key}\\");
    let runtime_display = runtime.to_string_lossy().into_owned();
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

    if entries.is_empty() {
        runtime_display
    } else {
        format!("{runtime_display};{}", entries.join(";"))
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
    use super::merge_user_path;
    use std::path::Path;

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
}
