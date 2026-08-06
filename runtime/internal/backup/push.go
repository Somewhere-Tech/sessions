package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

const defaultAPIBase = "https://api.somewhere.tech"

type Options struct {
	ConfigPath        string
	KeyPath           string
	RunnerStateDir    string
	ClaudeProjectsDir string
	CodexSessionsDir  string
	Machine           string
	APIBase           string
	HTTPClient        *http.Client
	Now               func() time.Time
}

type Pusher struct {
	options Options
}

// Result describes one push run. It is meaningful whether or not Push also
// returned an error: see Push for the split.
type Result struct {
	PushedAt           string              `json:"pushed_at"`
	Uploaded           int                 `json:"uploaded"`
	Skipped            int                 `json:"skipped"`
	SessionCount       int                 `json:"session_count"`
	Unresolved         int                 `json:"unresolved"`
	UnresolvedSessions []UnresolvedSession `json:"unresolved_sessions,omitempty"`
	ManifestPath       string              `json:"manifest_path"`
}

// UnresolvedSession names one session this push could not back up and says
// why. A live transcript that grew mid-read, or a single upload that failed,
// is a routine per-session outcome: the rest of the run continues and the next
// push retries this one. The set is reported rather than summed away so a
// partial push is never presented as a complete one.
type UnresolvedSession struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

type Manifest struct {
	Machine     string                   `json:"machine"`
	GeneratedAt string                   `json:"generated_at"`
	Sessions    map[string]ManifestEntry `json:"sessions"`
}

type ManifestEntry struct {
	Name           string `json:"name,omitempty"`
	CWD            string `json:"cwd"`
	Tool           string `json:"tool"`
	LastActivityAt int64  `json:"last_activity_at"`
	Path           string `json:"path"`
}

func NewPusher(options Options) *Pusher {
	if options.KeyPath == "" {
		options.KeyPath = keyPathForConfig(options.ConfigPath)
	}
	if options.APIBase == "" {
		options.APIBase = defaultAPIBase
	}
	if options.HTTPClient == nil {
		options.HTTPClient = &http.Client{Timeout: 60 * time.Second}
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Pusher{options: options}
}

// Push uploads every backed-up session and then the manifest. Its two return
// values answer two different questions, and a caller that reads only one of
// them will misreport the run:
//
//	Result — what this run actually did. Uploaded, Skipped and SessionCount
//	         count sessions that succeeded or were already current; Unresolved
//	         and UnresolvedSessions name, individually, the sessions this run
//	         could not back up and why. A partial push is the normal outcome, not
//	         an error: a live transcript that grew mid-read or a single failed
//	         upload is one session deferred to the next run, and abandoning the
//	         remaining sessions over it would strand every session sorted after
//	         it for as long as any session stays busy.
//
//	error  — the run as a whole stopped early: the configuration, token or key
//	         could not be read, the context was cancelled, or the manifest
//	         upload failed. The Result returned alongside it is still accurate
//	         for the sessions that completed before the stop, and the upload
//	         cache has been saved, so the next run resumes rather than repeats.
//
// So: err == nil never means every session was backed up — check Unresolved.
// err != nil never means nothing was backed up — read the Result. A caller that
// discards the Result on error (internal/api/backup_handlers.go does) loses the
// count of what did upload before a cancellation; nothing durable is lost,
// because saveProgress has already persisted the cache.
func (p *Pusher) Push(ctx context.Context, live []state.SessionInfo) (Result, error) {
	config, err := LoadConfig(p.options.ConfigPath)
	if err != nil {
		return Result{}, err
	}
	if !config.Enabled {
		return Result{}, errors.New("backup is not enabled")
	}
	token, err := ReadSomewhereToken(config.TokenPath)
	if err != nil {
		return Result{}, err
	}
	var encryptionKey []byte
	if config.Encrypt {
		encryptionKey, err = ReadKey(p.options.KeyPath)
		if err != nil {
			return Result{}, err
		}
	}
	machine := sanitizeSegment(p.options.Machine)
	if p.options.Machine == "" {
		hostname, hostnameErr := os.Hostname()
		if hostnameErr != nil {
			return Result{}, fmt.Errorf("resolve machine name: %w", hostnameErr)
		}
		machine = sanitizeSegment(hostname)
	}
	now := p.options.Now().UTC()
	result := Result{PushedAt: now.Format(time.RFC3339Nano)}
	manifest := Manifest{
		Machine: machine, GeneratedAt: result.PushedAt,
		Sessions: make(map[string]ManifestEntry),
	}
	resolver := Resolver{
		ClaudeProjectsDir: p.options.ClaudeProjectsDir,
		CodexSessionsDir:  p.options.CodexSessionsDir,
		RunnerStateDir:    p.options.RunnerStateDir,
		Now:               p.options.Now,
	}
	for _, session := range CollectSessions(live, p.options.RunnerStateDir) {
		if session.OptOut {
			continue
		}
		localPath, tool := resolver.Resolve(session)
		if localPath == "" || tool == "" {
			if normalizedTool(session.Tool, session.Command) != "" {
				result.unresolve(session.ID, "no local transcript was found for this session")
			}
			continue
		}
		info, err := os.Stat(localPath)
		if err != nil || !info.Mode().IsRegular() {
			result.unresolve(session.ID, fmt.Sprintf("%s is not a readable transcript file", localPath))
			continue
		}
		lastActivity := max(session.LastActivityAt, info.ModTime().UnixMilli())
		remotePath := strings.Join([]string{
			"sessions", machine, tool, sanitizeSegment(session.ID) + ".jsonl",
		}, "/")
		if config.Encrypt {
			remotePath += ".enc"
		}
		entry := ManifestEntry{
			Name: session.Name, CWD: session.CWD, Tool: tool,
			LastActivityAt: lastActivity, Path: remotePath,
		}
		fingerprint := Fingerprint{Size: info.Size(), ModTimeNano: info.ModTime().UnixNano()}
		cacheKey := config.Project + "/" + remotePath
		uploaded, ok := config.Cache[cacheKey]
		if ok && uploaded == fingerprint {
			manifest.Sessions[session.ID] = entry
			result.SessionCount++
			result.Skipped++
			continue
		}
		// One busy or unreachable session must not abandon the run: the push is
		// scheduled, so abandoning it here would strand every session sorted
		// after this one for as long as any session stays live.
		contents, stable, err := readStableFile(localPath)
		if err == nil && config.Encrypt {
			// Bound to the exact object being written, so this transcript cannot
			// later be moved over another session's object and decrypt as that
			// session. See the payload-identity note in encrypt.go.
			contents, err = EncryptFor(encryptionKey, BackupIdentity(config.Project, remotePath), contents)
		}
		if err == nil {
			err = p.put(ctx, token, config.Project, remotePath, "application/octet-stream", contents)
		}
		if err != nil {
			if ctx.Err() != nil {
				return result, p.saveProgress(config, err)
			}
			result.unresolve(session.ID, err.Error())
			// A previous push already placed this transcript remotely, so the
			// manifest keeps pointing at it. The cache keeps the older
			// fingerprint, which is what makes the next push retry.
			if ok {
				manifest.Sessions[session.ID] = entry
				result.SessionCount++
			}
			continue
		}
		manifest.Sessions[session.ID] = entry
		result.SessionCount++
		config.Cache[cacheKey] = stable
		result.Uploaded++
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return result, p.saveProgress(config, err)
	}
	result.ManifestPath = strings.Join([]string{"sessions", machine, "manifest.json"}, "/")
	manifestContentType := "application/json"
	if config.Encrypt {
		result.ManifestPath += ".enc"
		manifestBytes, err = EncryptFor(encryptionKey, BackupIdentity(config.Project, result.ManifestPath), manifestBytes)
		if err != nil {
			return result, p.saveProgress(config, err)
		}
		manifestContentType = "application/octet-stream"
	}
	if err := p.put(ctx, token, config.Project, result.ManifestPath, manifestContentType, manifestBytes); err != nil {
		return result, p.saveProgress(config, err)
	}
	config.LastPushAt = result.PushedAt
	config.LastPushCount = result.Uploaded
	config.LastPushSkipped = result.Skipped
	config.LastPushPending = result.Unresolved
	config.LastSessionCount = result.SessionCount
	if err := SaveConfig(p.options.ConfigPath, config); err != nil {
		return result, err
	}
	return result, nil
}

func (r *Result) unresolve(sessionID, reason string) {
	r.Unresolved++
	r.UnresolvedSessions = append(r.UnresolvedSessions, UnresolvedSession{ID: sessionID, Reason: reason})
}

func (p *Pusher) saveProgress(config Config, pushErr error) error {
	if err := SaveConfig(p.options.ConfigPath, config); err != nil {
		return errors.Join(pushErr, err)
	}
	return pushErr
}

func (p *Pusher) put(ctx context.Context, token, project, remotePath, contentType string, contents []byte) error {
	target, err := uploadURL(p.options.APIBase, project, remotePath)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, target, bytes.NewReader(contents))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", contentType)
	response, err := p.options.HTTPClient.Do(request)
	if err != nil {
		return fmt.Errorf("upload %s: %w", remotePath, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		return fmt.Errorf("upload %s: somewhere returned %d: %s", remotePath, response.StatusCode, strings.TrimSpace(string(body)))
	}
	_, _ = io.Copy(io.Discard, response.Body)
	return nil
}

func uploadURL(base, project, remotePath string) (string, error) {
	if err := validateProject(project); err != nil {
		return "", err
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid somewhere API base %q", base)
	}
	segments := []string{"v1", "fs", project}
	for _, segment := range strings.Split(remotePath, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", fmt.Errorf("invalid backup path %q", remotePath)
		}
		segments = append(segments, segment)
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/") + "/" + strings.Join(segments, "/")
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

// readStableFile reads a transcript that may be being appended to right now,
// and returns it only if the file did not change underneath the read. A file
// that keeps moving is reported as an error, which the caller records as one
// unresolved session and retries on the next push rather than failing the run.
//
// It reads the whole file into memory, and an encrypted push then allocates a
// second copy for the ciphertext. That is bounded only by the transcript: the
// largest one on this development machine is 1.15 GB, so a push of it peaks
// above 2 GB. Fixing that means streaming the upload and framing the ciphertext
// in chunks, which changes the payload format again; it is a known cost, not an
// oversight.
func readStableFile(path string) ([]byte, Fingerprint, error) {
	for attempt := 0; attempt < 2; attempt++ {
		file, err := os.Open(path)
		if err != nil {
			return nil, Fingerprint{}, err
		}
		before, err := file.Stat()
		if err != nil {
			_ = file.Close()
			return nil, Fingerprint{}, err
		}
		contents, readErr := io.ReadAll(file)
		after, statErr := file.Stat()
		closeErr := file.Close()
		if err := errors.Join(readErr, statErr, closeErr); err != nil {
			return nil, Fingerprint{}, err
		}
		if before.Size() == after.Size() && before.ModTime() == after.ModTime() && int64(len(contents)) == after.Size() {
			return contents, Fingerprint{Size: after.Size(), ModTimeNano: after.ModTime().UnixNano()}, nil
		}
	}
	return nil, Fingerprint{}, fmt.Errorf("transcript changed while reading %s; the session is still writing, and the next push picks it up", path)
}

func sanitizeSegment(value string) string {
	value = strings.TrimSpace(value)
	var result strings.Builder
	lastDash := false
	for _, character := range value {
		allowed := unicode.IsLetter(character) || unicode.IsDigit(character) || character == '-' || character == '_' || character == '.'
		if allowed {
			result.WriteRune(character)
			lastDash = false
			continue
		}
		if !lastDash {
			result.WriteByte('-')
			lastDash = true
		}
	}
	cleaned := strings.Trim(result.String(), "-.")
	if cleaned == "" {
		return "unknown"
	}
	return filepath.Base(cleaned)
}
