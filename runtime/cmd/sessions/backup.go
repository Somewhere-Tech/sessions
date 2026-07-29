package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	backupstore "github.com/somewhere-tech/sessions/runtime/internal/backup"
)

const (
	backupEnableUsage    = "usage: sessions backup enable --project <somewhere-project> [--interval 15m] [--encrypt]"
	backupDecryptUsage   = "usage: sessions backup decrypt <file.enc> [--out PATH] [--key-phrase \"...\"]"
	backupKeyDisplayPath = "~/.config/sessions/backup.key"
)

func (a *app) cmdBackup(args []string) error {
	if len(args) == 0 {
		return fail(1, "usage: sessions backup <enable|now|status|decrypt>")
	}
	switch args[0] {
	case "enable":
		return a.cmdBackupEnable(append([]string(nil), args[1:]...))
	case "now":
		if len(args) != 1 {
			return fail(1, "usage: sessions backup now")
		}
		return a.cmdBackupNow()
	case "status":
		if len(args) != 1 {
			return fail(1, "usage: sessions backup status")
		}
		return a.cmdBackupStatus()
	case "decrypt":
		return a.cmdBackupDecrypt(append([]string(nil), args[1:]...))
	default:
		return fail(1, "unknown backup command: %s", args[0])
	}
}

func (a *app) cmdBackupEnable(args []string) error {
	project, found := pluck(&args, "--project")
	if !found || strings.TrimSpace(project) == "" {
		return fail(1, backupEnableUsage)
	}
	encrypt := removeFirst(&args, "--encrypt")
	interval := backupstore.DefaultInterval
	if raw, present := pluck(&args, "--interval"); present {
		parsed, err := parseDuration(raw, 0)
		if err != nil {
			return err
		}
		if parsed <= 0 {
			return fail(1, "--interval must be greater than zero")
		}
		interval = parsed
	}
	if len(args) != 0 {
		return fail(1, backupEnableUsage)
	}
	config, keySetup, err := backupstore.EnableWithEncryption(
		backupstore.ConfigPath(a.home), backupstore.SomewhereConfigPath(a.home), backupstore.KeyPath(a.home),
		project, interval, encrypt,
	)
	if err != nil {
		return fail(1, "%s", err)
	}
	// Wake a currently running daemon. Enable remains durable even when the
	// daemon is stopped; the daemon also reloads this config on its next start.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, _ = a.api.request(ctx, http.MethodPost, "/api/backup/reload", nil, 0)
	status := config.Status()
	if status.Encrypt {
		status.KeyPath = backupstore.KeyPath(a.home)
	}
	if a.wantJSON {
		if !encrypt {
			return writeJSON(a.stdout, status, true)
		}
		return writeJSON(a.stdout, struct {
			backupstore.Status
			RecoveryPhrase string `json:"recovery_phrase"`
			KeyReused      bool   `json:"key_reused"`
		}{Status: status, RecoveryPhrase: keySetup.RecoveryPhrase, KeyReused: keySetup.Reused}, true)
	}
	if _, err = fmt.Fprintf(a.stdout, "Backup enabled for somewhere project %s (every %s).\n", config.Project, config.Interval); err != nil {
		return err
	}
	if !encrypt {
		return nil
	}
	if keySetup.Reused {
		if _, err = fmt.Fprintf(a.stdout, "Encryption is on. Reusing the existing key at %s.\n", backupKeyDisplayPath); err != nil {
			return err
		}
	} else if _, err = fmt.Fprintf(a.stdout, "Encryption is on. Created a new key at %s (mode 0600).\n", backupKeyDisplayPath); err != nil {
		return err
	}
	_, err = fmt.Fprintf(a.stdout,
		"RECOVERY PHRASE: %s\nWRITE THIS DOWN; WITHOUT IT YOUR BACKUPS ARE UNRECOVERABLE; WE CANNOT RESET IT.\n",
		keySetup.RecoveryPhrase,
	)
	return err
}

func (a *app) cmdBackupNow() error {
	var result backupstore.Result
	if err := a.postJSON("/api/backup/now", nil, &result, 2); err != nil {
		return err
	}
	if a.wantJSON {
		return writeJSON(a.stdout, result, true)
	}
	_, err := fmt.Fprintf(a.stdout, "Backup pushed: %d uploaded, %d unchanged, %d sessions.\n", result.Uploaded, result.Skipped, result.SessionCount)
	return err
}

func (a *app) cmdBackupStatus() error {
	config, err := backupstore.LoadConfig(backupstore.ConfigPath(a.home))
	if errors.Is(err, os.ErrNotExist) {
		if a.wantJSON {
			return writeJSON(a.stdout, backupstore.Status{}, true)
		}
		if _, err = fmt.Fprintln(a.stdout, "Backup is disabled."); err != nil {
			return err
		}
		return a.writeBackupEncryptionStatus(false)
	}
	if err != nil {
		return fail(1, "%s", err)
	}
	status := config.Status()
	if status.Encrypt {
		status.KeyPath = backupstore.KeyPath(a.home)
	}
	if a.wantJSON {
		return writeJSON(a.stdout, status, true)
	}
	if !status.Enabled {
		if _, err = fmt.Fprintln(a.stdout, "Backup is disabled."); err != nil {
			return err
		}
		return a.writeBackupEncryptionStatus(status.Encrypt)
	}
	lastPush := "never"
	if status.LastPushAt != "" {
		lastPush = status.LastPushAt
	}
	if _, err = fmt.Fprintf(a.stdout,
		"Backup enabled: project %s, every %s. Last push: %s (%d uploaded, %d unchanged, %d sessions).\n",
		status.Project, status.Interval, lastPush, status.LastPushCount, status.LastPushSkipped, status.LastSessionCount,
	); err != nil {
		return err
	}
	return a.writeBackupEncryptionStatus(status.Encrypt)
}

func (a *app) writeBackupEncryptionStatus(encrypt bool) error {
	if encrypt {
		_, err := fmt.Fprintf(a.stdout, "encryption: on (key: %s)\n", backupKeyDisplayPath)
		return err
	}
	_, err := fmt.Fprintln(a.stdout, "encryption: off")
	return err
}

func (a *app) cmdBackupDecrypt(args []string) error {
	outputPath, hasOutput := pluck(&args, "--out")
	phrase, hasPhrase := pluck(&args, "--key-phrase")
	if len(args) != 1 || (hasOutput && strings.TrimSpace(outputPath) == "") || (hasPhrase && strings.TrimSpace(phrase) == "") {
		return fail(1, backupDecryptUsage)
	}
	inputPath := args[0]
	if !strings.HasSuffix(inputPath, ".enc") {
		return fail(1, "encrypted backup path must end in .enc")
	}
	if !hasOutput {
		outputPath = strings.TrimSuffix(inputPath, ".enc")
	}
	if outputPath == "" || outputPath == inputPath {
		return fail(1, "decrypted output path must differ from the encrypted input")
	}
	var key []byte
	var err error
	if hasPhrase {
		key, err = backupstore.KeyFromRecoveryPhrase(phrase)
	} else {
		key, err = backupstore.ReadKey(backupstore.KeyPath(a.home))
	}
	if err != nil {
		return fail(1, "%s", err)
	}
	if err := backupstore.DecryptFile(inputPath, outputPath, key); err != nil {
		return fail(1, "%s", err)
	}
	if a.wantJSON {
		return writeJSON(a.stdout, struct {
			Input string `json:"input"`
			Out   string `json:"out"`
		}{Input: inputPath, Out: outputPath}, true)
	}
	_, err = fmt.Fprintf(a.stdout, "Decrypted %s to %s.\n", inputPath, outputPath)
	return err
}
