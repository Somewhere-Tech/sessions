package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/recovery"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// These are provider-owned saved observations, not a network connection check.
// Never read or expose credentials, or rewrite Claude's reconnection records.
type restartPreview struct {
	SourceSessionID string `json:"sourceSessionId"`
	Profile         string `json:"profile,omitempty"`
	RemoteURL       string `json:"remoteUrl,omitempty"`
	SavedLoginEmail string `json:"savedLoginEmail,omitempty"`
	AccountChanged  bool   `json:"accountChanged"`
	Warning         string `json:"warning,omitempty"`
}

func (s *Server) handleRestartPreview(w http.ResponseWriter, r *http.Request, origin string) {
	var body struct {
		SourceSessionID string `json:"sourceSessionId"`
	}
	if err := readJSON(r, &body); err != nil || body.SourceSessionID == "" {
		s.sendJSON(w, 400, map[string]any{"error": "sourceSessionId is required"}, origin)
		return
	}
	source, found := s.registry.Get(body.SourceSessionID)
	if !found {
		s.sendJSON(w, 404, map[string]any{"error": "This runtime is not available on this computer. Nothing was restarted."}, origin)
		return
	}
	info := source.Info()
	result := restartPreview{SourceSessionID: info.ID, Profile: info.Profile}
	if info.Tool == state.ToolClaude {
		result = claudeRestartPreview(info)
	}
	s.sendJSON(w, 200, result, origin)
}

func claudeRestartPreview(info state.SessionInfo) restartPreview {
	result := restartPreview{SourceSessionID: info.ID, Profile: info.Profile}
	uuid, _ := ledger.ExistingProviderResume(info.Cmd, info.Args)
	if uuid == "" {
		uuid = info.ClaudeSessionID
	}
	if uuid == "" {
		uuid = info.ConversationID
	}
	options := recovery.AdoptionOptions{}
	if info.ConfigDir != "" {
		options.ClaudeProjectsDir = filepath.Join(info.ConfigDir, "projects")
	}
	adoption, err := recovery.ResolveAdoption(uuid, options)
	if err != nil {
		result.Warning = "Could not read the recorded Remote Control account. Restart uses this computer's current login; the previous Claude link is not guaranteed."
		return result
	}
	owner, url := recordedClaudeBridge(adoption.Path)
	result.RemoteURL = url
	home, _ := os.UserHomeDir()
	loginPath := filepath.Join(home, ".claude.json")
	if info.ConfigDir != "" {
		loginPath = filepath.Join(info.ConfigDir, ".claude.json")
	}
	account, email := savedClaudeLogin(loginPath)
	result.SavedLoginEmail = email
	result.AccountChanged = owner != "" && account != "" && owner != account
	if result.AccountChanged {
		result.Warning = "Claude's saved login differs from the recorded Remote Control owner. To keep the previous Claude-app connection, sign into its original account on this computer before restarting. Otherwise Claude may create a different link under the current login. Local history stays intact."
	} else if owner == "" || account == "" {
		result.Warning = "The Remote Control account could not be compared. Restart uses this computer's current login; the previous Claude link is not guaranteed."
	}
	return result
}

func savedClaudeLogin(path string) (string, string) {
	file, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer file.Close()
	var data struct {
		Account struct {
			UUID  string `json:"accountUuid"`
			Email string `json:"emailAddress"`
		} `json:"oauthAccount"`
	}
	if json.NewDecoder(io.LimitReader(file, 2<<20)).Decode(&data) != nil {
		return "", ""
	}
	return data.Account.UUID, data.Account.Email
}

// Bound work even for a 500 MB transcript. Skip a partial first line; an absent
// record is unknown, never proof that Remote Control is off or disconnected.
func recordedClaudeBridge(path string) (string, string) {
	file, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return "", ""
	}
	start := stat.Size() - (1 << 20)
	if start < 0 {
		start = 0
	}
	if _, err = file.Seek(start, io.SeekStart); err != nil {
		return "", ""
	}
	data, err := io.ReadAll(io.LimitReader(file, 1<<20))
	if err != nil {
		return "", ""
	}
	lines := bytes.Split(data, []byte{'\n'})
	if start > 0 {
		lines = lines[1:]
	}
	owner, url := "", ""
	for _, line := range lines {
		if !bytes.Contains(line, []byte(`"bridge-session"`)) && !bytes.Contains(line, []byte(`"bridge_status"`)) {
			continue
		}
		var record struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
			Owner   string `json:"ownerAccountUuid"`
			Bridge  string `json:"bridgeSessionId"`
			URL     string `json:"url"`
		}
		if json.Unmarshal(line, &record) != nil {
			continue
		}
		if record.Type == "bridge-session" {
			owner = record.Owner
			url = ""
			if strings.HasPrefix(record.Bridge, "cse_") && !strings.ContainsAny(record.Bridge, "/?# \t\r\n") {
				url = "https://claude.ai/code/session_" + strings.TrimPrefix(record.Bridge, "cse_")
			}
		}
		if record.Type == "system" && record.Subtype == "bridge_status" && strings.HasPrefix(record.URL, "https://claude.ai/code/") {
			url = record.URL
		}
	}
	return owner, url
}
