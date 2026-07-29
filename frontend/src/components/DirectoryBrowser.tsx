import { useEffect, useState, useCallback } from 'react';
import { listFs, type FsListing } from '../api/sessionsd';

interface Props {
  // The path that's currently selected (controlled). Empty string ⇒
  // default to $HOME on first load.
  value: string;
  // Called whenever the user picks a directory by clicking it. The
  // browser also navigates INTO directories; this fires on each
  // navigation so the parent can keep its "selected path" in sync.
  // Pass true for `confirmed` when the user explicitly clicked the
  // Select button — parents that want to auto-close on confirm can
  // distinguish that from passive navigation.
  onChange: (path: string, confirmed?: boolean) => void;
}

// Real directory browser. Click a folder to descend, click ".." to go
// up, click "Select" to confirm. No "project-shaped" curation — every
// child of the requested path that sessionsd can stat shows up.
//
// Hidden entries (dotfiles) are folded by default with a "Show hidden"
// toggle; on a typical $HOME there are dozens of dotfiles that bury
// the actual project dirs, but they're a click away when needed.
export function DirectoryBrowser({ value, onChange }: Props): JSX.Element {
  const [listing, setListing] = useState<FsListing | null>(null);
  const [showHidden, setShowHidden] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  const load = useCallback(async (path?: string): Promise<void> => {
    setLoading(true);
    setError(null);
    try {
      const next = await listFs(path);
      setListing(next);
      // Keep parent in sync with the path we ACTUALLY landed on (after
      // realpathSync). Don't fire confirmed=true — this is navigation,
      // not a final selection.
      onChange(next.path, false);
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setLoading(false);
    }
  }, [onChange]);

  useEffect(() => {
    // Initial load — use the current value if non-empty, otherwise
    // let the server default to $HOME.
    void load(value || undefined);
    // Only on mount; subsequent navigation goes through `load(path)`
    // calls inside this component.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const onPathInput = (e: React.KeyboardEvent<HTMLInputElement>): void => {
    if (e.key !== 'Enter') return;
    e.preventDefault();
    const target = (e.target as HTMLInputElement).value.trim();
    if (target) void load(target);
  };

  const visibleEntries = listing
    ? listing.entries.filter((en) => showHidden || !en.hidden)
    : [];

  return (
    <div className="dir-browser">
      <div className="dir-browser-toolbar">
        <input
          className="dir-browser-path"
          type="text"
          value={listing?.path ?? value ?? ''}
          onChange={(e) => onChange(e.target.value, false)}
          onKeyDown={onPathInput}
          placeholder="Type a path + Enter, or click below"
          spellCheck={false}
          autoCorrect="off"
          autoCapitalize="off"
        />
        <label className="dir-browser-hidden-toggle" title="Show dotfiles">
          <input
            type="checkbox"
            checked={showHidden}
            onChange={(e) => setShowHidden(e.target.checked)}
          />
          <span>Hidden</span>
        </label>
      </div>

      {error ? (
        <div className="dir-browser-error">{error}</div>
      ) : null}

      <div className="dir-browser-list">
        {listing?.parent !== null && listing?.parent !== undefined ? (
          <button
            type="button"
            className="dir-browser-row is-parent"
            onClick={() => void load(listing.parent!)}
            disabled={loading}
          >
            <span className="dir-browser-icon" aria-hidden>↰</span>
            <span className="dir-browser-name">..</span>
            <span className="dir-browser-meta">parent</span>
          </button>
        ) : null}
        {loading && !listing ? (
          <div className="dir-browser-empty">Loading…</div>
        ) : null}
        {visibleEntries.length === 0 && listing && !loading ? (
          <div className="dir-browser-empty">
            {listing.entries.length === 0 ? '(empty)' : '(only hidden — toggle "Hidden" to see)'}
          </div>
        ) : null}
        {visibleEntries.map((en) => {
          const isDir = en.kind === 'dir';
          return (
            <button
              key={en.name}
              type="button"
              className={`dir-browser-row${en.hidden ? ' is-hidden' : ''}${isDir ? ' is-dir' : ' is-file'}`}
              onClick={() => {
                if (!listing) return;
                if (isDir) {
                  // Navigate into the directory.
                  const next = listing.path === '/' ? `/${en.name}` : `${listing.path}/${en.name}`;
                  void load(next);
                }
                // Files are non-selectable (sessions need a directory
                // cwd). Showing them gives the user context that the
                // dir is non-empty and what's in it.
              }}
              disabled={!isDir}
            >
              <span className="dir-browser-icon" aria-hidden>
                {isDir ? '📁' : en.kind === 'symlink' ? '↪' : '·'}
              </span>
              <span className="dir-browser-name">{en.name}</span>
              {!isDir ? (
                <span className="dir-browser-meta">{en.kind}</span>
              ) : null}
            </button>
          );
        })}
      </div>

      <div className="dir-browser-footer">
        <span className="dir-browser-current" title={listing?.path}>
          {listing ? `Selected: ${listing.path}` : 'No directory selected'}
        </span>
        <button
          type="button"
          className="btn btn-primary dir-browser-select"
          onClick={() => listing && onChange(listing.path, true)}
          disabled={!listing || loading}
        >
          Select
        </button>
      </div>
    </div>
  );
}
