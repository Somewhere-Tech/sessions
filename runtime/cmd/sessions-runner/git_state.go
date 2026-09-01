package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ipc"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

type gitWorktreeState struct {
	root  string
	scope string
	head  string
	paths map[string]string
}

// captureGitWorktreeState records only paths already visible to Git. Comparing
// this state at lane exit means files_changed describes what changed during
// this run, rather than the unrelated dirty-file count the repository happened
// to have before the lane started.
func captureGitWorktreeState(cwd string) gitWorktreeState {
	check := exec.Command("git", "-C", cwd, "rev-parse", "--is-inside-work-tree")
	if output, err := check.Output(); err != nil || strings.TrimSpace(string(output)) != "true" {
		return gitWorktreeState{}
	}
	rootCommand := exec.Command("git", "-C", cwd, "rev-parse", "--show-toplevel")
	rootOutput, err := rootCommand.Output()
	if err != nil {
		return gitWorktreeState{}
	}
	root := strings.TrimSpace(string(rootOutput))
	if root == "" {
		return gitWorktreeState{}
	}
	scope, ok := gitWorkspaceScope(cwd, root)
	if !ok {
		return gitWorktreeState{}
	}
	head := ""
	if headOutput, headErr := exec.Command("git", "-C", root, "rev-parse", "--verify", "HEAD").Output(); headErr == nil {
		head = strings.TrimSpace(string(headOutput))
	}
	if head == "" {
		emptyTree := exec.Command("git", "-C", root, "hash-object", "-t", "tree", "--stdin")
		emptyTree.Stdin = strings.NewReader("")
		if emptyTreeOutput, emptyTreeErr := emptyTree.Output(); emptyTreeErr == nil {
			head = strings.TrimSpace(string(emptyTreeOutput))
		}
	}
	status := exec.Command(
		"git", "-C", root, "status", "--porcelain=v2", "-z", "--untracked-files=all", "--", scope,
	)
	output, err := status.Output()
	if err != nil {
		return gitWorktreeState{}
	}
	result := gitWorktreeState{root: root, scope: scope, head: head, paths: make(map[string]string)}
	skipRenameSource := false
	for _, encoded := range bytes.Split(output, []byte{0}) {
		if len(encoded) == 0 {
			continue
		}
		if skipRenameSource {
			skipRenameSource = false
			continue
		}
		record := string(encoded)
		path := ""
		switch {
		case strings.HasPrefix(record, "? ") || strings.HasPrefix(record, "! "):
			path = record[2:]
		case strings.HasPrefix(record, "1 "):
			path = porcelainPath(record, 9)
		case strings.HasPrefix(record, "2 "):
			path = porcelainPath(record, 10)
			skipRenameSource = true
		case strings.HasPrefix(record, "u "):
			path = porcelainPath(record, 11)
		}
		if path == "" {
			continue
		}
		result.paths[path] = record + "\x00" + worktreePathSignature(filepath.Join(root, filepath.FromSlash(path)))
	}
	return result
}

// gitWorkspaceScope keeps automatic files_changed accounting inside the
// directory the user selected. A Git repository rooted at the home directory
// must never turn a small lane into a recursive scan of Desktop, Documents,
// cloud drives, media libraries, or other protected siblings.
func gitWorkspaceScope(cwd, root string) (string, bool) {
	absCWD, err := filepath.Abs(cwd)
	if err != nil {
		return "", false
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", false
	}
	if resolved, resolveErr := filepath.EvalSymlinks(absCWD); resolveErr == nil {
		absCWD = resolved
	}
	if resolved, resolveErr := filepath.EvalSymlinks(absRoot); resolveErr == nil {
		absRoot = resolved
	}
	relative, err := filepath.Rel(absRoot, absCWD)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", false
	}
	if relative == "." {
		if home, homeErr := os.UserHomeDir(); homeErr == nil {
			absHome, absErr := filepath.Abs(home)
			if resolved, resolveErr := filepath.EvalSymlinks(absHome); resolveErr == nil {
				absHome = resolved
			}
			if absErr == nil && absHome == absRoot {
				return "", false
			}
		}
		if filepath.Dir(absRoot) == absRoot {
			return "", false
		}
		return ".", true
	}
	return filepath.ToSlash(relative), true
}

func porcelainPath(record string, fields int) string {
	parts := strings.SplitN(record, " ", fields)
	if len(parts) != fields {
		return ""
	}
	return parts[fields-1]
}

func worktreePathSignature(path string) string {
	info, err := os.Lstat(path)
	if err != nil {
		return "missing:" + err.Error()
	}
	signature := fmt.Sprintf("%s:%d:%d", info.Mode(), info.Size(), info.ModTime().UnixNano())
	if info.Mode()&os.ModeSymlink != 0 {
		target, readErr := os.Readlink(path)
		return signature + ":symlink:" + target + ":" + fmt.Sprint(readErr)
	}
	const maximumHashedFile = 64 * 1024 * 1024
	if !info.Mode().IsRegular() || info.Size() > maximumHashedFile {
		return signature
	}
	file, err := os.Open(path)
	if err != nil {
		return signature + ":open:" + err.Error()
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return signature + ":read:" + err.Error()
	}
	return signature + ":sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func gitFilesChangedSince(cwd string, before gitWorktreeState) *int {
	if before.root == "" {
		return nil
	}
	after := captureGitWorktreeState(cwd)
	if after.root == "" || after.root != before.root || after.scope != before.scope {
		return nil
	}
	paths := make(map[string]struct{}, len(before.paths)+len(after.paths))
	for path := range before.paths {
		paths[path] = struct{}{}
	}
	for path := range after.paths {
		paths[path] = struct{}{}
	}
	if before.head != "" && after.head != "" && before.head != after.head {
		command := exec.Command(
			"git", "-C", before.root, "diff", "--name-only", "-z", before.head, after.head, "--", before.scope,
		)
		output, err := command.Output()
		if err != nil {
			return nil
		}
		for _, path := range bytes.Split(output, []byte{0}) {
			if len(path) > 0 {
				paths[string(path)] = struct{}{}
			}
		}
	}
	changed := 0
	for path := range paths {
		if before.paths[path] != after.paths[path] {
			changed++
			continue
		}
		if before.head != after.head {
			command := exec.Command(
				"git", "-C", before.root, "diff", "--quiet", before.head, after.head, "--", path,
			)
			if err := command.Run(); err != nil {
				var exitError *exec.ExitError
				if errors.As(err, &exitError) && exitError.ExitCode() == 1 {
					changed++
					continue
				}
				return nil
			}
		}
	}
	return &changed
}

func (r *runner) snapshotLocked() []byte {
	replay := r.log.Since(0)
	var out bytes.Buffer
	for _, ev := range replay.Events {
		_, _ = out.Write(ev.Data)
	}
	return out.Bytes()
}

func (r *runner) broadcastBytes(frame []byte) {
	r.mu.Lock()
	clients := make([]*client, 0, len(r.clients))
	for c := range r.clients {
		clients = append(clients, c)
	}
	r.mu.Unlock()
	for _, c := range clients {
		c.enqueueFrame(frame)
	}
}

func (r *runner) removeClient(c *client) {
	r.mu.Lock()
	delete(r.clients, c)
	shouldSchedule := r.exited && len(r.clients) == 0
	r.mu.Unlock()
	if shouldSchedule {
		r.scheduleIdleShutdown()
	}
}

func (r *runner) scheduleIdleShutdown() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.exited || len(r.clients) > 0 || r.idle != nil {
		return
	}
	r.idle = time.AfterFunc(idleShutdown, func() {
		r.mu.Lock()
		r.idle = nil
		reconnected := !r.exited || len(r.clients) > 0
		permanent := !r.jsonlMissing
		r.mu.Unlock()
		if reconnected {
			// A client connected while this callback was already scheduled.
			// Stop cannot recall a fired timer, so the decision is re-checked
			// here; the next disconnect schedules a fresh grace period.
			return
		}
		r.shutdown(permanent, 0)
	})
}

// cancelIdleShutdownLocked disarms the post-exit grace period. The caller
// holds r.mu; Stop never waits for a running callback, so this cannot deadlock
// against a callback that is itself trying to take r.mu.
func (r *runner) cancelIdleShutdownLocked() {
	if r.idle == nil {
		return
	}
	r.idle.Stop()
	r.idle = nil
}

func (r *runner) shutdown(permanent bool, code int) {
	r.shutdownWithRestartPolicy(permanent, code, false)
}

// shutdownForHostExit is the launchd/reboot path. Preserve the boot permit so
// the next launch can distinguish same-boot recovery from a new boot and apply
// the bounded restore policy. Explicit Kill and normal provider exit use
// shutdown, which removes the permit.
func (r *runner) shutdownForHostExit(permanent bool, code int) {
	r.shutdownWithRestartPolicy(permanent, code, true)
}

func (r *runner) shutdownWithRestartPolicy(permanent bool, code int, preserveRestartPermit bool) {
	r.shutdownOnce.Do(func() {
		r.streamMu.Lock()
		if r.listener != nil {
			_ = r.listener.Close()
		}
		_ = ipc.Remove(r.paths.Socket)
		_ = os.Remove(r.paths.Meta)
		if !preserveRestartPermit {
			removeRestartState(r.paths)
		}
		if permanent {
			_ = r.persistent.Unlink()
		} else {
			_ = r.persistent.Close()
		}
		if r.process != nil {
			_ = r.process.RequestStop()
			_ = r.process.CloseOutput()
		}
		r.streamMu.Unlock()
		os.Exit(code)
	})
}

func removeRestartState(paths state.Paths) {
	_ = os.Remove(paths.KeepAlive)
	_ = os.Remove(paths.RestorePending)
}

func writeBytes(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}

func jsISOString(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}
