#[cfg(desktop)]
#[derive(Clone, Debug, Deserialize, PartialEq, Eq, Serialize)]
struct WindowBounds {
    x: i32,
    y: i32,
    width: u32,
    height: u32,
    maximized: bool,
}

// Dragging a window emits Moved/Resized tens to hundreds of times per second.
// Coalesce that burst into a single write instead of re-serializing the whole
// map and rewriting the file on every event.
#[cfg(desktop)]
const WINDOW_GEOMETRY_FLUSH_DELAY: Duration = Duration::from_millis(500);

#[cfg(desktop)]
struct WindowGeometryFile {
    path: PathBuf,
    bounds: Mutex<HashMap<String, WindowBounds>>,
    // True while a debounced writer is already armed for the current burst.
    flush_pending: AtomicBool,
}

#[cfg(desktop)]
impl WindowGeometryFile {
    fn write(&self) {
        // Serialize under the lock, then release it before touching the disk.
        // The lock is taken from the window-event handler, so a slow or full
        // volume must never be waited on while holding it.
        let encoded = {
            let Ok(bounds) = self.bounds.lock() else {
                return;
            };
            serde_json::to_vec_pretty(&*bounds)
        };
        let Ok(encoded) = encoded else {
            return;
        };
        if let Some(parent) = self.path.parent() {
            if let Err(error) = fs::create_dir_all(parent) {
                log::warn!(
                    "create window geometry directory {}: {error}",
                    parent.display()
                );
                return;
            }
        }
        // Temp-plus-rename: a crash or a power loss partway through a plain
        // fs::write truncates this file and loses every remembered window.
        if let Err(error) = lifecycle::write_atomic(&self.path, &encoded, 0o600) {
            log::warn!("save window geometry: {error}");
        }
    }
}

#[cfg(desktop)]
struct WindowGeometryStore {
    file: Arc<WindowGeometryFile>,
}

#[cfg(desktop)]
impl WindowGeometryStore {
    fn load(path: PathBuf) -> Self {
        let bounds = fs::read(&path)
            .ok()
            .and_then(|bytes| serde_json::from_slice(&bytes).ok())
            .unwrap_or_default();
        Self {
            file: Arc::new(WindowGeometryFile {
                path,
                bounds: Mutex::new(bounds),
                flush_pending: AtomicBool::new(false),
            }),
        }
    }

    fn get(&self, label: &str) -> Option<WindowBounds> {
        self.file.bounds.lock().ok()?.get(label).cloned()
    }

    fn remember(&self, label: String, bounds: WindowBounds) {
        {
            let Ok(mut all_bounds) = self.file.bounds.lock() else {
                return;
            };
            if all_bounds.get(&label) == Some(&bounds) {
                return;
            }
            all_bounds.insert(label, bounds);
        }
        // The first change of a burst arms one writer; every later change
        // during the window is free and is picked up when that writer wakes.
        if self.file.flush_pending.swap(true, Ordering::SeqCst) {
            return;
        }
        let file = Arc::clone(&self.file);
        thread::spawn(move || {
            thread::sleep(WINDOW_GEOMETRY_FLUSH_DELAY);
            // Clear before writing, so an event that lands mid-write arms a
            // fresh flush instead of being dropped on the floor.
            file.flush_pending.store(false, Ordering::SeqCst);
            file.write();
        });
    }

    // Closing a window or quitting right after a drag must still persist the
    // final position: the debounced writer may well still be sleeping.
    fn flush(&self) {
        self.file.flush_pending.store(false, Ordering::SeqCst);
        self.file.write();
    }
}

#[cfg(desktop)]
fn stable_label_part(value: &str) -> String {
    let cleaned: String = value
        .chars()
        .map(|ch| {
            if ch.is_ascii_alphanumeric() || matches!(ch, '-' | '_' | '.') {
                ch
            } else {
                '-'
            }
        })
        .collect();
    let trimmed = cleaned.trim_matches('-');
    if trimmed.is_empty() {
        "scope".to_string()
    } else {
        trimmed.chars().take(80).collect()
    }
}

#[cfg(desktop)]
fn parse_scoped_window(query: &str, title: String) -> Result<WindowSpec, String> {
    let pairs: Vec<(String, String)> =
        url::form_urlencoded::parse(query.trim().trim_start_matches('?').as_bytes())
            .map(|(key, value)| (key.into_owned(), value.into_owned()))
            .collect();

    let title = if title.trim().is_empty() {
        "Sessions".to_string()
    } else {
        title
    };

    if pairs.len() == 1 && pairs[0].0 == "server" && !pairs[0].1.trim().is_empty() {
        let id = pairs[0].1.trim();
        let query = url::form_urlencoded::Serializer::new(String::new())
            .append_pair("server", id)
            .finish();
        return Ok(WindowSpec {
            label: format!("win-server-{}", stable_label_part(id)),
            query,
            title,
            width: 1100.0,
            height: 760.0,
        });
    }

    if pairs.len() == 1 && pairs[0].0 == "tool" {
        let tool = pairs[0].1.as_str();
        if matches!(tool, "codex" | "claude" | "shell") {
            let query = url::form_urlencoded::Serializer::new(String::new())
                .append_pair("tool", tool)
                .finish();
            return Ok(WindowSpec {
                label: format!("win-tool-{tool}"),
                query,
                title,
                width: 1100.0,
                height: 760.0,
            });
        }
    }

    if pairs.len() == 2 {
        let session_id = pairs
            .iter()
            .find_map(|(key, value)| (key == "session").then_some(value.as_str()));
        let single = pairs
            .iter()
            .any(|(key, value)| key == "mode" && value == "single");
        if let Some(session_id) = session_id.filter(|id| !id.trim().is_empty()) {
            if single {
                let query = url::form_urlencoded::Serializer::new(String::new())
                    .append_pair("session", session_id.trim())
                    .append_pair("mode", "single")
                    .finish();
                return Ok(WindowSpec {
                    label: format!("win-session-{}", stable_label_part(session_id)),
                    query,
                    title,
                    width: 900.0,
                    height: 700.0,
                });
            }
        }
    }

    Err(
        "scope must be server=<id>, tool=codex|claude|shell, or session=<id>&mode=single"
            .to_string(),
    )
}

#[cfg(desktop)]
fn main_window_spec() -> WindowSpec {
    WindowSpec {
        label: "main".to_string(),
        query: String::new(),
        title: "Sessions".to_string(),
        width: 1200.0,
        height: 800.0,
    }
}

#[cfg(desktop)]
fn focus_window(window: &WebviewWindow) -> Result<(), String> {
    window.show().map_err(|error| error.to_string())?;
    window.unminimize().map_err(|error| error.to_string())?;
    window.set_focus().map_err(|error| error.to_string())
}

#[cfg(desktop)]
fn restore_window(window: &WebviewWindow) {
    let Some(saved) = window
        .app_handle()
        .state::<WindowGeometryStore>()
        .get(window.label())
    else {
        return;
    };
    if saved.width >= 400 && saved.height >= 300 {
        let _ = window.set_size(PhysicalSize::new(saved.width, saved.height));
    }
    let _ = window.set_position(PhysicalPosition::new(saved.x, saved.y));
    if saved.maximized {
        let _ = window.maximize();
    }
}

#[cfg(desktop)]
fn remember_window(window: &WebviewWindow) {
    let (Ok(position), Ok(size), Ok(maximized)) = (
        window.outer_position(),
        window.outer_size(),
        window.is_maximized(),
    ) else {
        return;
    };
    if size.width < 400 || size.height < 300 {
        return;
    }
    window.app_handle().state::<WindowGeometryStore>().remember(
        window.label().to_string(),
        WindowBounds {
            x: position.x,
            y: position.y,
            width: size.width,
            height: size.height,
            maximized,
        },
    );
}

#[cfg(desktop)]
fn track_window(window: &WebviewWindow) {
    let tracked = window.clone();
    window.on_window_event(move |event| match event {
        WindowEvent::Moved(_) | WindowEvent::Resized(_) => remember_window(&tracked),
        // Do not let the debounce outlive the window it is remembering.
        // try_state, not state: Destroyed can arrive during teardown, and a
        // panic in a window-event handler is not worth a saved position.
        WindowEvent::CloseRequested { .. } | WindowEvent::Destroyed => {
            if let Some(store) = tracked.app_handle().try_state::<WindowGeometryStore>() {
                store.flush();
            }
        }
        _ => {}
    });
}

#[cfg(desktop)]
fn open_window(app: &AppHandle, spec: WindowSpec) -> Result<(), String> {
    if let Some(existing) = app.get_webview_window(&spec.label) {
        return focus_window(&existing);
    }

    let path = if spec.query.is_empty() {
        "index.html".to_string()
    } else {
        format!("index.html?{}", spec.query)
    };
    let window = WebviewWindowBuilder::new(app, &spec.label, WebviewUrl::App(path.into()))
        .title(&spec.title)
        .inner_size(spec.width, spec.height)
        .resizable(true)
        .build()
        .map_err(|error| error.to_string())?;
    restore_window(&window);
    track_window(&window);
    focus_window(&window)
}

#[tauri::command]
#[cfg(desktop)]
fn open_scoped_window(app: AppHandle, query: String, title: String) -> Result<(), String> {
    open_window(&app, parse_scoped_window(&query, title)?)
}

#[tauri::command]
#[cfg(mobile)]
fn open_scoped_window(_app: AppHandle, _query: String, _title: String) -> Result<(), String> {
    Err("separate session windows are not available on mobile".to_string())
}
