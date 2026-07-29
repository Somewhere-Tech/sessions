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

type Result struct {
	PushedAt     string `json:"pushed_at"`
	Uploaded     int    `json:"uploaded"`
	Skipped      int    `json:"skipped"`
	SessionCount int    `json:"session_count"`
	Unresolved   int    `json:"unresolved"`
	ManifestPath string `json:"manifest_path"`
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
		Now:               p.options.Now,
	}
	for _, session := range CollectSessions(live, p.options.RunnerStateDir) {
		if session.OptOut {
			continue
		}
		localPath, tool := resolver.Resolve(session)
		if localPath == "" || tool == "" {
			if normalizedTool(session.Tool, session.Command) != "" {
				result.Unresolved++
			}
			continue
		}
		info, err := os.Stat(localPath)
		if err != nil || !info.Mode().IsRegular() {
			result.Unresolved++
			continue
		}
		lastActivity := max(session.LastActivityAt, info.ModTime().UnixMilli())
		remotePath := strings.Join([]string{
			"sessions", machine, tool, sanitizeSegment(session.ID) + ".jsonl",
		}, "/")
		if config.Encrypt {
			remotePath += ".enc"
		}
		manifest.Sessions[session.ID] = ManifestEntry{
			Name: session.Name, CWD: session.CWD, Tool: tool,
			LastActivityAt: lastActivity, Path: remotePath,
		}
		result.SessionCount++
		fingerprint := Fingerprint{Size: info.Size(), ModTimeNano: info.ModTime().UnixNano()}
		cacheKey := config.Project + "/" + remotePath
		if config.Cache[cacheKey] == fingerprint {
			result.Skipped++
			continue
		}
		contents, stable, err := readStableFile(localPath)
		if err != nil {
			return result, p.saveProgress(config, err)
		}
		if config.Encrypt {
			contents, err = Encrypt(encryptionKey, contents)
			if err != nil {
				return result, p.saveProgress(config, err)
			}
		}
		if err := p.put(ctx, token, config.Project, remotePath, "application/octet-stream", contents); err != nil {
			return result, p.saveProgress(config, err)
		}
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
		manifestBytes, err = Encrypt(encryptionKey, manifestBytes)
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
	config.LastSessionCount = result.SessionCount
	if err := SaveConfig(p.options.ConfigPath, config); err != nil {
		return result, err
	}
	return result, nil
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
	return nil, Fingerprint{}, fmt.Errorf("transcript changed while reading: %s", path)
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
