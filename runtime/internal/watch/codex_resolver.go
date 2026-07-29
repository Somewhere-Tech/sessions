package watch

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	// CodexFirstLineBytes leaves room for session_meta launch context that
	// legitimately exceeds the former 16 KiB read.
	CodexFirstLineBytes = 65_536
	codexFirstLineBytes = CodexFirstLineBytes
)

var codexResumeIDPattern = regexp.MustCompile(`(?i)^[0-9a-f-]{8,}$`)

// CodexResolveReason is the exact TypeScript resolver reason string.
type CodexResolveReason string

const (
	CodexResumeMatch   CodexResolveReason = "resume-match"
	CodexResumeMissing CodexResolveReason = "resume-missing"
	CodexFreshMatch    CodexResolveReason = "fresh-match"
	CodexFreshFullScan CodexResolveReason = "fresh-match-fullscan"
	CodexNoDir         CodexResolveReason = "no-dir"
	CodexEmptyDir      CodexResolveReason = "empty-dir"
	CodexNoCWDMatch    CodexResolveReason = "no-cwd-match"
	CodexNoAfterSpawn  CodexResolveReason = "no-after-spawn"
)

// CodexResolution identifies the rollout to follow.
type CodexResolution struct {
	Path           string             `json:"path"`
	Reason         CodexResolveReason `json:"reason"`
	AmbiguousCount int                `json:"ambiguousCount,omitempty"`
}

// CodexResolveOptions supplies the runner launch metadata and an optional
// isolated sessions root. SessionsDir defaults to ~/.codex/sessions, and Now
// defaults to the current time.
type CodexResolveOptions struct {
	CWD         string
	Args        []string
	CreatedAt   time.Time
	SessionsDir string
	Now         time.Time
}

type rolloutCandidate struct {
	path    string
	modTime time.Time
	meta    *codexSessionMeta
}

type codexSessionMeta struct {
	cwd       string
	timestamp time.Time
}

func defaultCodexSessionsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "sessions"), nil
}

func resolveCodexRoot(root string) string {
	if root != "" {
		return root
	}
	resolved, err := defaultCodexSessionsDir()
	if err != nil {
		return filepath.Join(string(filepath.Separator), ".codex-home-unavailable", "sessions")
	}
	return resolved
}

func codexSessionDates(now, createdAt time.Time) []time.Time {
	if now.IsZero() {
		now = time.Now()
	}
	location := now.Location()
	yesterday := now.AddDate(0, 0, -1)
	dates := []time.Time{now, yesterday}
	if !createdAt.IsZero() {
		dates = append(dates, createdAt.In(location))
	}
	return dates
}

func codexDateDir(root string, date time.Time) string {
	return filepath.Join(root, date.Format("2006"), date.Format("01"), date.Format("02"))
}

func uniquePaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}
	return out
}

// CodexFreshSessionDirs returns the bounded today/yesterday/createdAt search
// window in deterministic order.
func CodexFreshSessionDirs(root string, now, createdAt time.Time) []string {
	root = resolveCodexRoot(root)
	paths := make([]string, 0, 3)
	for _, date := range codexSessionDates(now, createdAt) {
		paths = append(paths, codexDateDir(root, date))
	}
	return uniquePaths(paths)
}

// CodexWatchDirs includes every existing-or-future ancestor that can wake the
// resolver when Codex creates a dated rollout directory.
func CodexWatchDirs(root string, now, createdAt time.Time) []string {
	root = resolveCodexRoot(root)
	paths := make([]string, 0, 12)
	for _, date := range codexSessionDates(now, createdAt) {
		year := date.Format("2006")
		month := date.Format("01")
		day := date.Format("02")
		paths = append(paths,
			root,
			filepath.Join(root, year),
			filepath.Join(root, year, month),
			filepath.Join(root, year, month, day),
		)
	}
	return uniquePaths(paths)
}

// ExtractCodexResumeID recognizes the same resume forms as the TypeScript
// implementation: resume ID, --resume ID, and --resume=ID.
func ExtractCodexResumeID(args []string) string {
	for index, flag := range args {
		if flag == "resume" || flag == "--resume" {
			if index+1 < len(args) && codexResumeIDPattern.MatchString(args[index+1]) {
				return args[index+1]
			}
		}
		if strings.HasPrefix(flag, "--resume=") {
			value := strings.TrimPrefix(flag, "--resume=")
			if codexResumeIDPattern.MatchString(value) {
				return value
			}
		}
	}
	return ""
}

func listRolloutsInDir(dir string) []rolloutCandidate {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]rolloutCandidate, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err == nil && info.Mode().IsRegular() {
			out = append(out, rolloutCandidate{path: path, modTime: info.ModTime()})
		}
	}
	return out
}

func listRolloutsRecursive(root string) []rolloutCandidate {
	out := make([]rolloutCandidate, 0)
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
			return nil
		}
		info, statErr := entry.Info()
		if statErr == nil && info.Mode().IsRegular() {
			out = append(out, rolloutCandidate{path: path, modTime: info.ModTime()})
		}
		return nil
	})
	return out
}

func readCodexFirstLine(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	reader := bufio.NewReaderSize(io.LimitReader(file, codexFirstLineBytes), codexFirstLineBytes)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(strings.TrimSuffix(line, "\n")), nil
}

func readCodexSessionMeta(path string) *codexSessionMeta {
	line, err := readCodexFirstLine(path)
	if err != nil || line == "" {
		return nil
	}
	var record map[string]any
	if json.Unmarshal([]byte(line), &record) != nil || record["type"] != "session_meta" {
		return nil
	}
	payload, ok := record["payload"].(map[string]any)
	if !ok {
		return nil
	}
	cwd, ok := payload["cwd"].(string)
	if !ok {
		return nil
	}
	timestamp, _ := payload["timestamp"].(string)
	if timestamp == "" {
		timestamp, _ = record["timestamp"].(string)
	}
	parsed, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return nil
	}
	return &codexSessionMeta{cwd: cwd, timestamp: parsed}
}

func resolveResumedCodex(root string, args []string) (CodexResolution, bool) {
	resumeID := ExtractCodexResumeID(args)
	if resumeID == "" {
		return CodexResolution{}, false
	}
	matches := listRolloutsRecursive(root)
	filtered := matches[:0]
	for _, match := range matches {
		if strings.Contains(filepath.Base(match.path), resumeID) {
			filtered = append(filtered, match)
		}
	}
	if len(filtered) == 0 {
		return CodexResolution{Reason: CodexResumeMissing}, true
	}
	sort.Slice(filtered, func(i, j int) bool {
		iMillis := filtered[i].modTime.UnixMilli()
		jMillis := filtered[j].modTime.UnixMilli()
		if iMillis != jMillis {
			return iMillis > jMillis
		}
		return filtered[i].path > filtered[j].path
	})
	return CodexResolution{Path: filtered[0].path, Reason: CodexResumeMatch}, true
}

// ResolveCodexRolloutPath applies resume-first global matching, then the
// bounded date window, then the nearest-timestamp full-scan fallback.
func ResolveCodexRolloutPath(options CodexResolveOptions) CodexResolution {
	root := resolveCodexRoot(options.SessionsDir)
	if resumed, ok := resolveResumedCodex(root, options.Args); ok {
		return resumed
	}
	targetCWD := normalizeCWD(options.CWD)

	now := options.Now
	if now.IsZero() {
		now = time.Now()
	}
	dirs := CodexFreshSessionDirs(root, now, options.CreatedAt)
	sawDir := false
	sawFile := false
	sawCWDMatch := false
	matches := make([]rolloutCandidate, 0)

	for _, dir := range dirs {
		if _, err := os.Stat(dir); err == nil {
			sawDir = true
		}
		files := listRolloutsInDir(dir)
		if len(files) > 0 {
			sawFile = true
		}
		for _, file := range files {
			file.meta = readCodexSessionMeta(file.path)
			if file.meta == nil || normalizeCWD(file.meta.cwd) != targetCWD {
				continue
			}
			sawCWDMatch = true
			if file.meta.timestamp.Before(options.CreatedAt) {
				continue
			}
			matches = append(matches, file)
		}
	}

	if len(matches) > 0 {
		sort.Slice(matches, func(i, j int) bool {
			if !matches[i].meta.timestamp.Equal(matches[j].meta.timestamp) {
				return matches[i].meta.timestamp.Before(matches[j].meta.timestamp)
			}
			return matches[i].path < matches[j].path
		})
		return CodexResolution{
			Path:           matches[0].path,
			Reason:         CodexFreshMatch,
			AmbiguousCount: ambiguousCount(len(matches)),
		}
	}
	if !sawDir {
		return CodexResolution{Reason: CodexNoDir}
	}
	if !sawFile {
		return CodexResolution{Reason: CodexEmptyDir}
	}
	if sawCWDMatch {
		return CodexResolution{Reason: CodexNoAfterSpawn}
	}

	fullScan := make([]rolloutCandidate, 0)
	for _, file := range listRolloutsRecursive(root) {
		file.meta = readCodexSessionMeta(file.path)
		if file.meta != nil && normalizeCWD(file.meta.cwd) == targetCWD {
			fullScan = append(fullScan, file)
		}
	}
	if len(fullScan) == 0 {
		return CodexResolution{Reason: CodexNoCWDMatch}
	}
	sort.Slice(fullScan, func(i, j int) bool {
		iDistance := math.Abs(float64(fullScan[i].meta.timestamp.Sub(options.CreatedAt)))
		jDistance := math.Abs(float64(fullScan[j].meta.timestamp.Sub(options.CreatedAt)))
		if iDistance != jDistance {
			return iDistance < jDistance
		}
		return fullScan[i].path < fullScan[j].path
	})
	return CodexResolution{
		Path:           fullScan[0].path,
		Reason:         CodexFreshFullScan,
		AmbiguousCount: ambiguousCount(len(fullScan)),
	}
}

func ambiguousCount(count int) int {
	if count > 1 {
		return count
	}
	return 0
}
