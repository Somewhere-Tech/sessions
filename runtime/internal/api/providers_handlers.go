package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

type providerStatus struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Installed       bool   `json:"installed"`
	Version         string `json:"version,omitempty"`
	LatestVersion   string `json:"latestVersion,omitempty"`
	LastCheckedAt   string `json:"lastCheckedAt,omitempty"`
	UpdateAvailable bool   `json:"updateAvailable"`
}

var (
	providerVersionPattern = regexp.MustCompile(`\d+(?:\.\d+){1,3}(?:[-+][0-9A-Za-z.-]+)?`)
	providerUpdateMu       sync.Mutex
)

func (s *Server) handleProvidersRoute(response http.ResponseWriter, request *http.Request, corsOrigin string) bool {
	if request.URL.Path == "/api/providers" {
		// A wrong method on a route this handler owns is 405 here, as it is on
		// every sibling route family; returning false handed the request to the
		// router's catch-all 404 and told the caller the endpoint did not exist.
		if request.Method != http.MethodGet {
			s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
			return true
		}
		statuses := []providerStatus{
			localProviderStatus(request.Context(), "claude"),
			localProviderStatus(request.Context(), "codex"),
		}
		s.sendJSON(response, http.StatusOK, map[string]any{"providers": statuses}, corsOrigin)
		return true
	}
	const prefix = "/api/providers/"
	const suffix = "/update"
	if !strings.HasPrefix(request.URL.Path, prefix) || !strings.HasSuffix(request.URL.Path, suffix) {
		return false
	}
	if request.Method != http.MethodPost {
		s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
		return true
	}
	principal, ok := request.Context().Value(authPrincipalContextKey{}).(authPrincipal)
	if !ok || !principal.Local {
		s.sendJSON(response, http.StatusForbidden, map[string]any{
			"error": "provider updates require a local Sessions client",
		}, corsOrigin)
		return true
	}
	id := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, prefix), suffix)
	if id != "claude" && id != "codex" {
		s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "unknown provider"}, corsOrigin)
		return true
	}
	path, err := providerExecutable(id)
	if err != nil {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": id + " is not installed"}, corsOrigin)
		return true
	}
	providerUpdateMu.Lock()
	defer providerUpdateMu.Unlock()
	// Provider installers replace their own executable. Once explicitly
	// started, an app close or client disconnect must not kill that installer
	// midway through its atomic swap and leave Claude/Codex unusable.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(request.Context()), 5*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, path, "update")
	command.Env = append(os.Environ(), "NO_COLOR=1")
	output, runErr := command.CombinedOutput()
	if ctx.Err() != nil {
		runErr = errors.New("provider update timed out after five minutes")
	}
	if len(output) > 32<<10 {
		output = output[len(output)-(32<<10):]
	}
	if runErr != nil {
		s.sendJSON(response, http.StatusBadGateway, map[string]any{
			"error":  runErr.Error(),
			"output": strings.TrimSpace(string(output)),
		}, corsOrigin)
		return true
	}
	s.sendJSON(response, http.StatusOK, map[string]any{
		"provider": localProviderStatus(request.Context(), id),
		"output":   strings.TrimSpace(string(output)),
	}, corsOrigin)
	return true
}

func localProviderStatus(parent context.Context, id string) providerStatus {
	name := "Claude Code"
	if id == "codex" {
		name = "Codex"
	}
	status := providerStatus{ID: id, Name: name}
	path, err := providerExecutable(id)
	if err != nil {
		return status
	}
	status.Installed = true
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err == nil {
		status.Version = providerVersionPattern.FindString(string(output))
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return status
	}
	if id == "codex" {
		var cached struct {
			LatestVersion string `json:"latest_version"`
			LastCheckedAt string `json:"last_checked_at"`
		}
		if readSmallJSON(filepath.Join(home, ".codex", "version.json"), &cached) == nil {
			status.LatestVersion = cached.LatestVersion
			status.LastCheckedAt = cached.LastCheckedAt
			status.UpdateAvailable = versionLess(status.Version, status.LatestVersion)
		}
		return status
	}
	var lastUpdate struct {
		Timestamp string `json:"timestamp"`
		VersionTo string `json:"version_to"`
		Status    string `json:"status"`
	}
	if readSmallJSON(filepath.Join(home, ".claude", ".last-update-result.json"), &lastUpdate) == nil {
		status.LastCheckedAt = lastUpdate.Timestamp
		if status.Version == "" {
			status.Version = lastUpdate.VersionTo
		}
		if lastUpdate.Status == "success" {
			status.LatestVersion = lastUpdate.VersionTo
			status.UpdateAvailable = versionLess(status.Version, status.LatestVersion)
		}
	}
	return status
}

// providerExecutable mirrors the interactive user's common installation
// locations. sessionsd is normally launched by a per-user service whose PATH
// is intentionally smaller than the user's shell PATH.
func providerExecutable(id string) (string, error) {
	if path, err := exec.LookPath(id); err == nil {
		return path, nil
	}
	names := []string{id}
	if runtime.GOOS == "windows" {
		names = append(names, id+".exe", id+".cmd")
	}
	directories := []string{"/opt/homebrew/bin", "/usr/local/bin"}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		directories = append([]string{filepath.Join(home, ".local", "bin"), filepath.Join(home, "bin")}, directories...)
	}
	for _, directory := range directories {
		for _, name := range names {
			candidate := filepath.Join(directory, name)
			info, err := os.Stat(candidate)
			if err != nil || info.IsDir() {
				continue
			}
			if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
				continue
			}
			return candidate, nil
		}
	}
	return "", exec.ErrNotFound
}

func readSmallJSON(path string, target any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return json.NewDecoder(&boundedReader{reader: file, remaining: 64 << 10}).Decode(target)
}

type boundedReader struct {
	reader    *os.File
	remaining int64
}

func (reader *boundedReader) Read(buffer []byte) (int, error) {
	if reader.remaining <= 0 {
		return 0, errors.New("provider status file is too large")
	}
	if int64(len(buffer)) > reader.remaining {
		buffer = buffer[:reader.remaining]
	}
	count, err := reader.reader.Read(buffer)
	reader.remaining -= int64(count)
	return count, err
}

func versionLess(current, latest string) bool {
	currentParts := versionNumbers(current)
	latestParts := versionNumbers(latest)
	if len(currentParts) == 0 || len(latestParts) == 0 {
		return false
	}
	maximum := len(currentParts)
	if len(latestParts) > maximum {
		maximum = len(latestParts)
	}
	for index := 0; index < maximum; index++ {
		var left, right int
		if index < len(currentParts) {
			left = currentParts[index]
		}
		if index < len(latestParts) {
			right = latestParts[index]
		}
		if left != right {
			return left < right
		}
	}
	return false
}

func versionNumbers(version string) []int {
	match := providerVersionPattern.FindString(version)
	if match == "" {
		return nil
	}
	match = strings.FieldsFunc(match, func(value rune) bool { return value == '-' || value == '+' })[0]
	parts := strings.Split(match, ".")
	result := make([]int, 0, len(parts))
	for _, part := range parts {
		value := 0
		for _, digit := range part {
			if digit < '0' || digit > '9' {
				return nil
			}
			value = value*10 + int(digit-'0')
		}
		result = append(result, value)
	}
	return result
}
