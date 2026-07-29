package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const updaterSignatureFixture = "dW50cnVzdGVkIGNvbW1lbnQ6IHNpZ25hdHVyZSBmcm9tIHRhdXJpIHNlY3JldCBrZXkKUlVSeThmbWFlSVRBOG9zYXdCQ3FQdGRxd2Z1bDdoSmN2UkdMVjFkZDlqNUQweTVHc2duR2dKMHBXWkZ6S0tINXBBV1VOV0w2L0lkK3RKem1sSDhFYjJSSlA3L3U0cXRJeGcwPQp0cnVzdGVkIGNvbW1lbnQ6IHRpbWVzdGFtcDoxNzg0ODQyOTI2CWZpbGU6U2Vzc2lvbnMuYXBwLnRhci5negpVcUJ2UkoyYy9BVS9yem5GU242TFhaNk9oTHN3OGcrem5BSnFMbXVZQlVVbWpFeUFuS0RFeGtHT0FkRG9Dc3NDUUIzemJoWDNyNnAvRlpvdEtGSmNDdz09Cg=="

func TestPinnedUpdaterKeyMatchesReleaseSourceAndValidatesEnvelope(t *testing.T) {
	encoded, err := os.ReadFile(filepath.Join("..", "..", "..", "release", "updater.pub"))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(decoded)), "\n")
	if len(lines) != 2 || lines[1] != nativeUpdatePublicKey {
		t.Fatalf("compiled updater key does not match release/updater.pub")
	}
	if _, _, err := validateMinisignEnvelope(updaterSignatureFixture); err != nil {
		t.Fatalf("official updater signature envelope did not verify: %v", err)
	}

	tampered, err := base64.StdEncoding.DecodeString(updaterSignatureFixture)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(strings.TrimSuffix(string(tampered), "\n"), "\n")
	global, err := base64.StdEncoding.DecodeString(parts[3])
	if err != nil {
		t.Fatal(err)
	}
	global[0] ^= 0x01
	parts[3] = base64.StdEncoding.EncodeToString(global)
	if _, _, err := validateMinisignEnvelope(base64.StdEncoding.EncodeToString([]byte(strings.Join(parts, "\n") + "\n"))); err == nil {
		t.Fatal("tampered updater signature envelope unexpectedly verified")
	}
}

func TestUpdateArtifactURLIsFailClosed(t *testing.T) {
	const valid = "https://github.com/somewhere-tech/sessions/releases/download/v0.2.2/Sessions.app.tar.gz"
	if err := validateUpdateArtifactURL(valid, "0.2.2"); err != nil {
		t.Fatalf("valid immutable artifact rejected: %v", err)
	}
	for _, invalid := range []string{
		"http://github.com/somewhere-tech/sessions/releases/download/v0.2.2/Sessions.app.tar.gz",
		"https://example.com/somewhere-tech/sessions/releases/download/v0.2.2/Sessions.app.tar.gz",
		"https://github.com/somewhere-tech/sessions/releases/download/v0.2.1/Sessions.app.tar.gz",
		valid + "?download=1",
		"https://github.com/somewhere-tech/other/releases/download/v0.2.2/Sessions.app.tar.gz",
	} {
		if err := validateUpdateArtifactURL(invalid, "0.2.2"); err == nil {
			t.Errorf("unsafe artifact URL accepted: %s", invalid)
		}
	}
}

func TestSemanticVersionComparison(t *testing.T) {
	tests := []struct {
		left, right string
		want        int
	}{
		{"0.2.2", "0.2.1", 1},
		{"0.2.2", "0.2.2", 0},
		{"0.2.1", "0.2.2", -1},
		{"1.0.0", "1.0.0-rc.1", 1},
		{"1.0.0-rc.2", "1.0.0-rc.10", -1},
		{"1.0.0-alpha.1", "1.0.0-alpha.beta", -1},
		{"10.0.0", "2.99.99", 1},
	}
	for _, test := range tests {
		actual, err := compareSemanticVersions(test.left, test.right)
		if err != nil {
			t.Errorf("%s vs %s: %v", test.left, test.right, err)
		} else if actual != test.want {
			t.Errorf("%s vs %s = %d, want %d", test.left, test.right, actual, test.want)
		}
	}
	for _, invalid := range []string{"v1.0.0", "1.0", "01.0.0", "1.0.0-01"} {
		if _, err := compareSemanticVersions(invalid, "1.0.0"); err == nil {
			t.Errorf("invalid semantic version accepted: %s", invalid)
		}
	}
}

func TestAppArchiveExtractionRejectsTraversalAndLinks(t *testing.T) {
	tests := []struct {
		name   string
		header tar.Header
	}{
		{name: "traversal", header: tar.Header{Name: "Sessions.app/../../escape", Typeflag: tar.TypeReg, Size: 1, Mode: 0o644}},
		{name: "other root", header: tar.Header{Name: "Other.app/file", Typeflag: tar.TypeReg, Size: 1, Mode: 0o644}},
		{name: "symlink", header: tar.Header{Name: "Sessions.app/link", Typeflag: tar.TypeSymlink, Linkname: "/tmp/escape", Mode: 0o777}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			archive := filepath.Join(t.TempDir(), "update.tar.gz")
			writeTestUpdateArchive(t, archive, []tar.Header{test.header})
			if err := extractAppArchive(archive, t.TempDir()); err == nil {
				t.Fatal("unsafe archive unexpectedly extracted")
			}
		})
	}

	archive := filepath.Join(t.TempDir(), "valid.tar.gz")
	writeTestUpdateArchive(t, archive, []tar.Header{
		{Name: "Sessions.app/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "Sessions.app/Contents/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "Sessions.app/Contents/test", Typeflag: tar.TypeReg, Size: 1, Mode: 0o755},
	})
	destination := t.TempDir()
	if err := extractAppArchive(archive, destination); err != nil {
		t.Fatalf("valid archive rejected: %v", err)
	}
	if contents, err := os.ReadFile(filepath.Join(destination, "Sessions.app", "Contents", "test")); err != nil || string(contents) != "x" {
		t.Fatalf("valid archive contents = %q, %v", contents, err)
	}
}

func writeTestUpdateArchive(t *testing.T, destination string, headers []tar.Header) {
	t.Helper()
	file, err := os.Create(destination)
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, header := range headers {
		header := header
		if err := tarWriter.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if header.Size > 0 {
			if _, err := tarWriter.Write(bytes.Repeat([]byte("x"), int(header.Size))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateCommandUsesSecureRunnerAndSupportsCheck(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	application, err := newApp([]string{"update", "--check"}, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	defer application.close()
	called := false
	application.runUpdate = func(_ context.Context, checkOnly bool) (nativeUpdateResult, error) {
		called = true
		if !checkOnly {
			t.Fatal("update runner did not receive --check")
		}
		return nativeUpdateResult{CurrentVersion: "0.2.1", LatestVersion: "0.2.2", Detail: "safe check"}, nil
	}
	if err := application.dispatch(); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("secure update runner was not called")
	}
	if output := application.stdout.(*bytes.Buffer).String(); output != "safe check\n" {
		t.Fatalf("stdout = %q", output)
	}
}

func TestLiveUpdaterArtifactVerifiesAndExtractsWhenRequested(t *testing.T) {
	if os.Getenv("SESSIONS_TEST_LIVE_UPDATE") != "1" {
		t.Skip("set SESSIONS_TEST_LIVE_UPDATE=1 for the public signed-artifact gate")
	}
	updater := nativeUpdater{appPath: nativeUpdateAppPath, client: secureUpdateHTTPClient()}
	manifest, platform, err := updater.fetchManifest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(t.TempDir(), "Sessions.app.tar.gz")
	if err := updater.downloadVerifiedArtifact(context.Background(), platform, artifact); err != nil {
		t.Fatal(err)
	}
	extracted := t.TempDir()
	if err := extractAppArchive(artifact, extracted); err != nil {
		t.Fatal(err)
	}
	if err := verifySignedApp(filepath.Join(extracted, "Sessions.app"), manifest.Version); err != nil {
		t.Fatal(err)
	}
	t.Logf("verified live Sessions %s artifact with the pinned key, Developer ID, and Gatekeeper", manifest.Version)
}

func TestLiveUpdaterInstallTransactionWhenRequested(t *testing.T) {
	if os.Getenv("SESSIONS_TEST_LIVE_INSTALL") != "1" {
		t.Skip("set SESSIONS_TEST_LIVE_INSTALL=1 only on an authorized runner-free Mac")
	}
	if runtime.GOOS != "darwin" || os.Geteuid() == 0 {
		t.Fatal("live install rehearsal requires a non-root macOS user")
	}
	current, err := installedAppVersion(nativeUpdateAppPath)
	if err != nil {
		t.Fatal(err)
	}
	updater := nativeUpdater{appPath: nativeUpdateAppPath, client: secureUpdateHTTPClient()}
	manifest, platform, err := updater.fetchManifest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Version != current {
		t.Fatalf("rehearsal is same-version only: installed=%s live=%s", current, manifest.Version)
	}
	artifact := filepath.Join(t.TempDir(), "Sessions.app.tar.gz")
	if err := updater.downloadVerifiedArtifact(context.Background(), platform, artifact); err != nil {
		t.Fatal(err)
	}
	if err := installVerifiedAppArchive(artifact, nativeUpdateAppPath, current, manifest.Version); err != nil {
		t.Fatal(err)
	}
	if err := reopenInstalledApp(nativeUpdateAppPath); err != nil {
		t.Fatal(err)
	}
	t.Logf("atomically reinstalled and reopened live signed Sessions %s", manifest.Version)
}

func TestStopInstalledAppTargetsOnlyExactBundleExecutable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("exact installed-app process targeting is a macOS update contract")
	}
	appPath := filepath.Join(t.TempDir(), "Sessions.app")
	binary := filepath.Join(appPath, "Contents", "MacOS", "sessions-app")
	if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile("/bin/sleep")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, source, 0o755); err != nil {
		t.Fatal(err)
	}
	process := exec.Command(binary, "30")
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = process.Process.Kill()
		_, _ = process.Process.Wait()
	})
	time.Sleep(100 * time.Millisecond)
	if err := stopInstalledApp(appPath, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := process.Wait(); err == nil {
		t.Fatal("test UI process exited cleanly instead of receiving SIGTERM")
	}
}

func TestAtomicAppExchangeKeepsBothCompleteBundlesAddressable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("renamex_np(RENAME_SWAP) is the macOS updater primitive")
	}
	root := t.TempDir()
	current := filepath.Join(root, "Sessions.app")
	next := filepath.Join(root, "staging", "Sessions.app")
	if err := os.MkdirAll(current, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(next, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(current, "marker"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(next, "marker"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := syncUpdateTree(next); err != nil {
		t.Fatal(err)
	}
	if err := atomicSwapApps(current, next); err != nil {
		t.Fatal(err)
	}
	assertMarker := func(location, want string) {
		t.Helper()
		contents, err := os.ReadFile(filepath.Join(location, "marker"))
		if err != nil || string(contents) != want {
			t.Fatalf("%s marker = %q, %v; want %q", location, contents, err, want)
		}
	}
	assertMarker(current, "new")
	assertMarker(next, "old")
	if err := atomicSwapApps(current, next); err != nil {
		t.Fatal(err)
	}
	assertMarker(current, "old")
	assertMarker(next, "new")
}
