package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/blake2b"
)

const (
	nativeUpdateManifestURL = "https://sessions.somewhere.tech/releases/latest.json"
	nativeUpdateAppPath     = "/Applications/Sessions.app"
	nativeUpdatePublicKey   = "RWRy8fmaeITA8hP3+8vLUlt97CulTCDjcd4VS5XA5tfm1Ov+epG6VZ+H"
	maxUpdateManifestBytes  = 1 << 20
	maxUpdateArtifactBytes  = 512 << 20
	maxUpdateArchiveEntries = 10_000
)

var semanticVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

type nativeUpdateResult struct {
	CurrentVersion string `json:"currentVersion"`
	LatestVersion  string `json:"latestVersion"`
	Updated        bool   `json:"updated"`
	Reopened       bool   `json:"reopened"`
	Detail         string `json:"detail"`
}

type nativeUpdateManifest struct {
	Version   string                          `json:"version"`
	Platforms map[string]nativeUpdatePlatform `json:"platforms"`
}

type nativeUpdatePlatform struct {
	Signature string `json:"signature"`
	URL       string `json:"url"`
}

type parsedMinisign struct {
	keyID          [8]byte
	signature      [64]byte
	trustedComment string
	global         [64]byte
}

type nativeUpdater struct {
	appPath string
	client  *http.Client
}

func (a *app) cmdUpdate(args []string) error {
	checkOnly := removeFirst(&args, "--check")
	if len(args) != 0 {
		return fail(1, "usage: sessions update [--check]")
	}
	if runtime.GOOS != "darwin" {
		return fail(2, "sessions update currently requires macOS; this command updates Sessions.app, not a remote daemon")
	}
	if os.Geteuid() == 0 {
		return fail(2, "do not run `sessions update` with sudo; update from the macOS user who owns Sessions.app")
	}
	result, err := a.runUpdate(context.Background(), checkOnly)
	if err != nil {
		return err
	}
	if a.wantJSON {
		return writeJSON(a.stdout, result, true)
	}
	_, err = fmt.Fprintln(a.stdout, result.Detail)
	return err
}

func runNativeAppUpdate(ctx context.Context, checkOnly bool) (nativeUpdateResult, error) {
	updater := nativeUpdater{
		appPath: nativeUpdateAppPath,
		client:  secureUpdateHTTPClient(),
	}
	return updater.run(ctx, checkOnly)
}

func secureUpdateHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 4
	transport.MaxIdleConnsPerHost = 2
	client := &http.Client{
		Transport: transport,
		Timeout:   10 * time.Minute,
	}
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many update redirects")
		}
		if request.URL.Scheme != "https" || !allowedUpdateHost(request.URL.Hostname()) {
			return fmt.Errorf("refusing update redirect to %s", request.URL.Redacted())
		}
		request.Header.Del("Authorization")
		request.Header.Del("Cookie")
		return nil
	}
	return client
}

func allowedUpdateHost(host string) bool {
	switch strings.ToLower(host) {
	case "sessions.somewhere.tech", "sessions.somewhere.site", "github.com", "release-assets.githubusercontent.com":
		return true
	default:
		return false
	}
}

func (updater nativeUpdater) run(ctx context.Context, checkOnly bool) (nativeUpdateResult, error) {
	if updater.client == nil {
		return nativeUpdateResult{}, fail(2, "secure update client is unavailable")
	}
	currentVersion, err := installedAppVersion(updater.appPath)
	if err != nil {
		return nativeUpdateResult{}, fail(2, "%s", err)
	}
	if err := verifySignedApp(updater.appPath, currentVersion); err != nil {
		return nativeUpdateResult{}, fail(2, "refusing to update an untrusted installed Sessions.app: %s", err)
	}
	manifest, platform, err := updater.fetchManifest(ctx)
	if err != nil {
		return nativeUpdateResult{}, fail(2, "%s", err)
	}
	comparison, err := compareSemanticVersions(manifest.Version, currentVersion)
	if err != nil {
		return nativeUpdateResult{}, fail(2, "invalid signed-channel version metadata: %s", err)
	}
	result := nativeUpdateResult{
		CurrentVersion: currentVersion,
		LatestVersion:  manifest.Version,
	}
	if comparison < 0 {
		return nativeUpdateResult{}, fail(2, "release channel announced Sessions %s, older than installed %s; refusing to downgrade", manifest.Version, currentVersion)
	}
	if comparison == 0 {
		result.Detail = fmt.Sprintf("Sessions %s is up to date.", currentVersion)
		return result, nil
	}
	if checkOnly {
		result.Detail = fmt.Sprintf("Sessions %s is available (installed: %s). Run `sessions update` to install it.", manifest.Version, currentVersion)
		return result, nil
	}

	tempDirectory, err := os.MkdirTemp("", "sessions-update-download-*")
	if err != nil {
		return nativeUpdateResult{}, fail(2, "create private update staging directory: %s", err)
	}
	defer os.RemoveAll(tempDirectory)
	if err := os.Chmod(tempDirectory, 0o700); err != nil {
		return nativeUpdateResult{}, fail(2, "secure update staging directory: %s", err)
	}
	artifactPath := filepath.Join(tempDirectory, "Sessions.app.tar.gz")
	if err := updater.downloadVerifiedArtifact(ctx, platform, artifactPath); err != nil {
		return nativeUpdateResult{}, fail(2, "%s", err)
	}
	if err := installVerifiedAppArchive(artifactPath, updater.appPath, currentVersion, manifest.Version); err != nil {
		return nativeUpdateResult{}, fail(2, "%s", err)
	}

	result.Updated = true
	result.Reopened = reopenInstalledApp(updater.appPath) == nil
	if result.Reopened {
		result.Detail = fmt.Sprintf("Updated Sessions %s → %s and reopened the app. The background service will update in place; runners were not stopped.", currentVersion, manifest.Version)
	} else {
		result.Detail = fmt.Sprintf("Updated Sessions %s → %s. Reopen Sessions.app to finish the background-service update; runners were not stopped.", currentVersion, manifest.Version)
	}
	return result, nil
}

func (updater nativeUpdater) fetchManifest(ctx context.Context) (nativeUpdateManifest, nativeUpdatePlatform, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, nativeUpdateManifestURL, nil)
	if err != nil {
		return nativeUpdateManifest{}, nativeUpdatePlatform{}, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "sessions-updater/"+version)
	response, err := updater.client.Do(request)
	if err != nil {
		return nativeUpdateManifest{}, nativeUpdatePlatform{}, fmt.Errorf("fetch public update manifest: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nativeUpdateManifest{}, nativeUpdatePlatform{}, fmt.Errorf("update manifest returned HTTP %d", response.StatusCode)
	}
	body, err := readLimited(response.Body, maxUpdateManifestBytes)
	if err != nil {
		return nativeUpdateManifest{}, nativeUpdatePlatform{}, fmt.Errorf("read update manifest: %w", err)
	}
	var manifest nativeUpdateManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return nativeUpdateManifest{}, nativeUpdatePlatform{}, fmt.Errorf("parse update manifest: %w", err)
	}
	if !semanticVersionPattern.MatchString(manifest.Version) {
		return nativeUpdateManifest{}, nativeUpdatePlatform{}, fmt.Errorf("update manifest has invalid version %q", manifest.Version)
	}
	platform, ok := manifest.Platforms["darwin-aarch64"]
	if !ok || strings.TrimSpace(platform.Signature) == "" || strings.TrimSpace(platform.URL) == "" {
		return nativeUpdateManifest{}, nativeUpdatePlatform{}, errors.New("update manifest has no complete darwin-aarch64 release")
	}
	if err := validateUpdateArtifactURL(platform.URL, manifest.Version); err != nil {
		return nativeUpdateManifest{}, nativeUpdatePlatform{}, err
	}
	if _, err := decodeMinisign(platform.Signature); err != nil {
		return nativeUpdateManifest{}, nativeUpdatePlatform{}, fmt.Errorf("update manifest has an invalid artifact signature: %w", err)
	}
	return manifest, platform, nil
}

func validateUpdateArtifactURL(rawURL, releaseVersion string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parse update artifact URL: %w", err)
	}
	expectedPath := "/somewhere-tech/sessions/releases/download/v" + releaseVersion + "/Sessions.app.tar.gz"
	if parsed.Scheme != "https" || parsed.Hostname() != "github.com" || parsed.Port() != "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.EscapedPath() != expectedPath {
		return fmt.Errorf("refusing update artifact outside the immutable Sessions release path: %s", parsed.Redacted())
	}
	return nil
}

func (updater nativeUpdater) downloadVerifiedArtifact(ctx context.Context, platform nativeUpdatePlatform, destination string) error {
	signature, publicKey, err := validateMinisignEnvelope(platform.Signature)
	if err != nil {
		return fmt.Errorf("validate update signature: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, platform.URL, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/octet-stream")
	request.Header.Set("User-Agent", "sessions-updater/"+version)
	response, err := updater.client.Do(request)
	if err != nil {
		return fmt.Errorf("download update artifact: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("update artifact returned HTTP %d", response.StatusCode)
	}
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create update artifact: %w", err)
	}
	hasher, _ := blake2b.New512(nil)
	written, copyErr := io.Copy(io.MultiWriter(file, hasher), io.LimitReader(response.Body, maxUpdateArtifactBytes+1))
	closeErr := file.Close()
	if copyErr != nil {
		return fmt.Errorf("download update artifact: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close update artifact: %w", closeErr)
	}
	if written == 0 || written > maxUpdateArtifactBytes {
		return fmt.Errorf("update artifact size %d is outside the allowed range", written)
	}
	if !ed25519.Verify(publicKey, hasher.Sum(nil), signature.signature[:]) {
		return errors.New("update artifact failed pinned Minisign verification")
	}
	return nil
}

func validateMinisignEnvelope(encoded string) (parsedMinisign, ed25519.PublicKey, error) {
	signature, err := decodeMinisign(encoded)
	if err != nil {
		return parsedMinisign{}, nil, err
	}
	publicKey, keyID, err := decodePinnedPublicKey()
	if err != nil {
		return parsedMinisign{}, nil, err
	}
	if signature.keyID != keyID {
		return parsedMinisign{}, nil, errors.New("update signature was created by a different key")
	}
	globalPayload := append(append([]byte(nil), signature.signature[:]...), []byte(signature.trustedComment)...)
	if !ed25519.Verify(publicKey, globalPayload, signature.global[:]) {
		return parsedMinisign{}, nil, errors.New("update signature trusted-comment verification failed")
	}
	return signature, publicKey, nil
}

func decodePinnedPublicKey() (ed25519.PublicKey, [8]byte, error) {
	var keyID [8]byte
	decoded, err := base64.StdEncoding.DecodeString(nativeUpdatePublicKey)
	if err != nil || len(decoded) != 42 {
		return nil, keyID, errors.New("invalid pinned key encoding")
	}
	if string(decoded[:2]) != "Ed" && string(decoded[:2]) != "ED" {
		return nil, keyID, errors.New("unsupported pinned key algorithm")
	}
	copy(keyID[:], decoded[2:10])
	return ed25519.PublicKey(append([]byte(nil), decoded[10:]...)), keyID, nil
}

func decodeMinisign(encoded string) (parsedMinisign, error) {
	var parsed parsedMinisign
	text, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return parsed, errors.New("invalid outer base64")
	}
	lines := strings.Split(strings.TrimSuffix(string(text), "\n"), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "untrusted comment: ") ||
		!strings.HasPrefix(lines[2], "trusted comment: ") {
		return parsed, errors.New("invalid minisign envelope")
	}
	rawSignature, err := base64.StdEncoding.DecodeString(lines[1])
	if err != nil || len(rawSignature) != 74 {
		return parsed, errors.New("invalid minisign signature")
	}
	if string(rawSignature[:2]) != "ED" {
		return parsed, errors.New("only prehashed Minisign signatures are accepted")
	}
	rawGlobal, err := base64.StdEncoding.DecodeString(lines[3])
	if err != nil || len(rawGlobal) != 64 {
		return parsed, errors.New("invalid minisign global signature")
	}
	copy(parsed.keyID[:], rawSignature[2:10])
	copy(parsed.signature[:], rawSignature[10:])
	copy(parsed.global[:], rawGlobal)
	parsed.trustedComment = strings.TrimPrefix(lines[2], "trusted comment: ")
	return parsed, nil
}

func readLimited(reader io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	return body, nil
}

func installedAppVersion(appPath string) (string, error) {
	info, err := os.Lstat(appPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("Sessions.app is not installed at %s; install the signed app before using `sessions update`", appPath)
		}
		return "", fmt.Errorf("inspect %s: %w", appPath, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("refusing unexpected Sessions.app path type at %s", appPath)
	}
	output, err := exec.Command("/usr/bin/plutil", "-extract", "CFBundleShortVersionString", "raw", "-o", "-", filepath.Join(appPath, "Contents", "Info.plist")).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("read installed Sessions version: %s", outputOrError(output, err))
	}
	value := strings.TrimSpace(string(output))
	if !semanticVersionPattern.MatchString(value) {
		return "", fmt.Errorf("installed Sessions.app has invalid version %q", value)
	}
	return value, nil
}

func installVerifiedAppArchive(archivePath, appPath, previousVersion, expectedVersion string) error {
	parent := filepath.Dir(appPath)
	staging, err := os.MkdirTemp(parent, ".sessions-update-*")
	if err != nil {
		return fmt.Errorf("create atomic update staging beside Sessions.app: %w", err)
	}
	defer os.RemoveAll(staging)
	if err := os.Chmod(staging, 0o700); err != nil {
		return fmt.Errorf("secure atomic update staging: %w", err)
	}
	if err := extractAppArchive(archivePath, staging); err != nil {
		return err
	}
	nextApp := filepath.Join(staging, "Sessions.app")
	if err := verifySignedApp(nextApp, expectedVersion); err != nil {
		return fmt.Errorf("reject downloaded Sessions.app: %w", err)
	}
	if err := syncUpdateTree(nextApp); err != nil {
		return fmt.Errorf("durably stage downloaded Sessions.app: %w", err)
	}
	if err := stopInstalledApp(appPath, 5*time.Second); err != nil {
		return err
	}
	if err := verifySignedApp(appPath, previousVersion); err != nil {
		return fmt.Errorf("installed app changed during update staging: %w; no update was installed", err)
	}

	// renamex_np(RENAME_SWAP) exchanges both same-volume directory entries in
	// one kernel operation. The canonical app path therefore always names
	// either the fully verified old bundle or the fully verified new bundle.
	if err := atomicSwapApps(appPath, nextApp); err != nil {
		return fmt.Errorf("atomically exchange Sessions.app with the verified update: %w", err)
	}
	rollback := func(reason error) error {
		swapErr := atomicSwapApps(appPath, nextApp)
		if swapErr != nil {
			return fmt.Errorf("%w; atomic rollback failed: %v", reason, swapErr)
		}
		syncErr := syncUpdateDirectories(filepath.Dir(appPath), staging)
		if verifyErr := verifySignedApp(appPath, previousVersion); verifyErr != nil {
			return fmt.Errorf("%w; previous app was exchanged back but verification failed: %v", reason, verifyErr)
		}
		if syncErr != nil {
			return fmt.Errorf("%w; previous app was exchanged back and verified, but rollback durability was not confirmed: %v", reason, syncErr)
		}
		return fmt.Errorf("%w; previous app restored", reason)
	}
	if err := syncUpdateDirectories(filepath.Dir(appPath), staging); err != nil {
		return rollback(fmt.Errorf("persist atomic app exchange: %w", err))
	}
	if err := verifySignedApp(appPath, expectedVersion); err != nil {
		return rollback(fmt.Errorf("post-install verification failed: %w", err))
	}
	if err := os.RemoveAll(nextApp); err != nil {
		return fmt.Errorf("Sessions.app %s is installed and verified, but removing its temporary rollback copy failed: %w", expectedVersion, err)
	}
	if _, err := os.Lstat(nextApp); !os.IsNotExist(err) {
		return fmt.Errorf("Sessions.app %s is installed and verified, but its temporary rollback copy still exists at %s", expectedVersion, nextApp)
	}
	if err := syncUpdateDirectories(staging); err != nil {
		return fmt.Errorf("Sessions.app %s is installed and verified, but persisting rollback cleanup failed: %w", expectedVersion, err)
	}
	now := time.Now()
	_ = os.Chtimes(appPath, now, now)
	return nil
}

func extractAppArchive(archivePath, destination string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open verified update archive: %w", err)
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("open verified update gzip: %w", err)
	}
	defer gzipReader.Close()
	reader := tar.NewReader(gzipReader)
	entries := 0
	var expanded int64
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read verified update archive: %w", err)
		}
		entries++
		if entries > maxUpdateArchiveEntries {
			return errors.New("verified update archive contains too many entries")
		}
		name := strings.TrimSuffix(header.Name, "/")
		clean := path.Clean(name)
		if clean != name || (clean != "Sessions.app" && !strings.HasPrefix(clean, "Sessions.app/")) {
			return fmt.Errorf("verified update archive has unsafe path %q", header.Name)
		}
		target := filepath.Join(destination, filepath.FromSlash(clean))
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(header.Mode)&0o777); err != nil {
				return fmt.Errorf("extract update directory %s: %w", clean, err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if header.Size < 0 || expanded > maxUpdateArtifactBytes-header.Size {
				return errors.New("verified update archive expands beyond the allowed size")
			}
			expanded += header.Size
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("create update directory for %s: %w", clean, err)
			}
			output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(header.Mode)&0o777)
			if err != nil {
				return fmt.Errorf("extract update file %s: %w", clean, err)
			}
			written, copyErr := io.Copy(output, io.LimitReader(reader, header.Size))
			closeErr := output.Close()
			if copyErr != nil || written != header.Size {
				return fmt.Errorf("extract update file %s: copied %d of %d bytes: %v", clean, written, header.Size, copyErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close update file %s: %w", clean, closeErr)
			}
		default:
			return fmt.Errorf("verified update archive has unsupported entry type %d at %q", header.Typeflag, header.Name)
		}
	}
	if entries == 0 {
		return errors.New("verified update archive is empty")
	}
	return nil
}

func verifySignedApp(appPath, expectedVersion string) error {
	verify := exec.Command("/usr/bin/codesign", "--verify", "--deep", "--strict", "--verbose=2", appPath)
	if output, err := verify.CombinedOutput(); err != nil {
		return fmt.Errorf("Developer ID verification failed: %s", outputOrError(output, err))
	}
	details, err := exec.Command("/usr/bin/codesign", "-dv", "--verbose=4", appPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("read Developer ID details: %s", outputOrError(details, err))
	}
	text := string(details)
	if !hasExactOutputLine(text, "Identifier=tech.somewhere.sessions") ||
		!hasExactOutputLine(text, "TeamIdentifier=7GW9T5ZWW8") ||
		!strings.Contains(text, "Authority=Developer ID Application: Uzair Haq (7GW9T5ZWW8)") {
		return errors.New("app identity does not match tech.somewhere.sessions signed by team 7GW9T5ZWW8")
	}
	assessment := exec.Command("/usr/sbin/spctl", "--assess", "--type", "execute", "--verbose=4", appPath)
	if output, err := assessment.CombinedOutput(); err != nil {
		return fmt.Errorf("Gatekeeper/notarization assessment failed: %s", outputOrError(output, err))
	}
	actualVersion, err := installedAppVersion(appPath)
	if err != nil {
		return err
	}
	if actualVersion != expectedVersion {
		return fmt.Errorf("app version is %s, manifest announced %s", actualVersion, expectedVersion)
	}
	return nil
}

func hasExactOutputLine(output, expected string) bool {
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == expected {
			return true
		}
	}
	return false
}

func syncUpdateTree(root string) error {
	var directories []string
	err := filepath.WalkDir(root, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			directories = append(directories, current)
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("refusing to sync non-regular update entry %s", current)
		}
		return syncUpdatePath(current)
	})
	if err != nil {
		return err
	}
	for index := len(directories) - 1; index >= 0; index-- {
		if err := syncUpdatePath(directories[index]); err != nil {
			return err
		}
	}
	return nil
}

func syncUpdateDirectories(directories ...string) error {
	for _, directory := range directories {
		if err := syncUpdatePath(directory); err != nil {
			return err
		}
	}
	return nil
}

func syncUpdatePath(target string) error {
	file, err := os.Open(target)
	if err != nil {
		return fmt.Errorf("open %s for fsync: %w", target, err)
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil {
		return fmt.Errorf("fsync %s: %w", target, syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close %s after fsync: %w", target, closeErr)
	}
	return nil
}

func stopInstalledApp(appPath string, timeout time.Duration) error {
	binary := filepath.Join(appPath, "Contents", "MacOS", "sessions-app")
	pids, err := liveInstalledAppPIDs(binary)
	if err != nil {
		return err
	}
	for _, pid := range pids {
		process, findErr := os.FindProcess(pid)
		if findErr != nil {
			return fmt.Errorf("locate Sessions app process %d: %w", pid, findErr)
		}
		if signalErr := process.Signal(syscall.SIGTERM); signalErr != nil && !errors.Is(signalErr, os.ErrProcessDone) {
			return fmt.Errorf("stop Sessions UI process %d before update: %w", pid, signalErr)
		}
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pids, err := liveInstalledAppPIDs(binary)
		if err != nil {
			return fmt.Errorf("confirm Sessions UI stopped: %w", err)
		}
		if len(pids) == 0 {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("Sessions UI did not exit within 5s; no app files were changed")
}

func liveInstalledAppPIDs(binary string) ([]int, error) {
	pattern := "^" + regexp.QuoteMeta(binary) + "( |$)"
	output, err := exec.Command("/usr/bin/pgrep", "-u", strconv.Itoa(os.Getuid()), "-f", pattern).CombinedOutput()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return nil, nil
		}
		return nil, fmt.Errorf("inspect running Sessions app: %s", outputOrError(output, err))
	}
	var live []int
	for _, field := range strings.Fields(string(output)) {
		pid, parseErr := strconv.Atoi(field)
		if parseErr != nil || pid <= 1 {
			return nil, fmt.Errorf("inspect running Sessions app: invalid process id %q", field)
		}
		actualUID, executable, inspectErr := processIdentity(pid)
		if errors.Is(inspectErr, os.ErrProcessDone) {
			continue
		}
		if inspectErr != nil {
			return nil, fmt.Errorf("revalidate Sessions app process %d: %w", pid, inspectErr)
		}
		if actualUID != os.Getuid() || executable != binary {
			return nil, fmt.Errorf("refusing to signal process %d after its identity changed (uid=%d executable=%s)", pid, actualUID, executable)
		}
		live = append(live, pid)
	}
	return live, nil
}

func processIdentity(pid int) (int, string, error) {
	output, err := exec.Command("/bin/ps", "-ww", "-p", strconv.Itoa(pid), "-o", "uid=", "-o", "state=", "-o", "comm=").CombinedOutput()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return 0, "", os.ErrProcessDone
		}
		return 0, "", fmt.Errorf("%s", outputOrError(output, err))
	}
	fields := strings.Fields(string(output))
	if len(fields) < 3 {
		return 0, "", errors.New("ps returned no uid/state/executable identity")
	}
	uid, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, "", fmt.Errorf("invalid process uid %q", fields[0])
	}
	state := fields[1]
	executable := strings.Join(fields[2:], " ")
	if strings.HasPrefix(state, "Z") || executable == "<defunct>" {
		return 0, "", os.ErrProcessDone
	}
	return uid, executable, nil
}

func reopenInstalledApp(appPath string) error {
	return exec.Command("/usr/bin/open", appPath).Run()
}

type parsedSemanticVersion struct {
	core       [3]string
	prerelease []string
}

func compareSemanticVersions(left, right string) (int, error) {
	a, err := parseSemanticVersion(left)
	if err != nil {
		return 0, fmt.Errorf("%q: %w", left, err)
	}
	b, err := parseSemanticVersion(right)
	if err != nil {
		return 0, fmt.Errorf("%q: %w", right, err)
	}
	for index := range a.core {
		if comparison := compareNumericIdentifier(a.core[index], b.core[index]); comparison != 0 {
			return comparison, nil
		}
	}
	if len(a.prerelease) == 0 && len(b.prerelease) == 0 {
		return 0, nil
	}
	if len(a.prerelease) == 0 {
		return 1, nil
	}
	if len(b.prerelease) == 0 {
		return -1, nil
	}
	for index := 0; index < len(a.prerelease) && index < len(b.prerelease); index++ {
		leftPart, rightPart := a.prerelease[index], b.prerelease[index]
		leftNumeric, rightNumeric := isNumeric(leftPart), isNumeric(rightPart)
		switch {
		case leftNumeric && rightNumeric:
			if comparison := compareNumericIdentifier(leftPart, rightPart); comparison != 0 {
				return comparison, nil
			}
		case leftNumeric:
			return -1, nil
		case rightNumeric:
			return 1, nil
		default:
			if leftPart < rightPart {
				return -1, nil
			}
			if leftPart > rightPart {
				return 1, nil
			}
		}
	}
	switch {
	case len(a.prerelease) < len(b.prerelease):
		return -1, nil
	case len(a.prerelease) > len(b.prerelease):
		return 1, nil
	default:
		return 0, nil
	}
}

func parseSemanticVersion(value string) (parsedSemanticVersion, error) {
	match := semanticVersionPattern.FindStringSubmatch(value)
	if match == nil {
		return parsedSemanticVersion{}, errors.New("invalid semantic version")
	}
	parsed := parsedSemanticVersion{core: [3]string{match[1], match[2], match[3]}}
	if match[4] != "" {
		parsed.prerelease = strings.Split(match[4], ".")
		for _, identifier := range parsed.prerelease {
			if isNumeric(identifier) && len(identifier) > 1 && identifier[0] == '0' {
				return parsedSemanticVersion{}, errors.New("numeric prerelease identifiers cannot contain leading zeroes")
			}
		}
	}
	return parsed, nil
}

func compareNumericIdentifier(left, right string) int {
	if len(left) < len(right) {
		return -1
	}
	if len(left) > len(right) {
		return 1
	}
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}

func isNumeric(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}
