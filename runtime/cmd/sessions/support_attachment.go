package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var (
	supportProjectPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	supportTicketPattern  = regexp.MustCompile(`^tsk_[A-Za-z0-9]{8,64}$`)
	supportHashPattern    = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

type supportAttachmentRequest struct {
	Project string
	Ticket  string
	Bundle  []byte
	SHA256  string
}

type supportAttachmentReceipt struct {
	SchemaVersion   int    `json:"schema_version"`
	Project         string `json:"project"`
	Ticket          string `json:"ticket"`
	Path            string `json:"path"`
	SHA256          string `json:"sha256"`
	SizeBytes       int    `json:"size_bytes"`
	Uploaded        bool   `json:"uploaded"`
	AlreadyAttached bool   `json:"already_attached"`
}

type supportCommandRunner func(
	context.Context,
	string,
	...string,
) ([]byte, []byte, error)

func (a *app) attachSupportWithSomewhere(
	ctx context.Context,
	request supportAttachmentRequest,
) (supportAttachmentReceipt, error) {
	return runSupportAttachment(ctx, request, runSupportCommand)
}

func runSupportAttachment(
	ctx context.Context,
	request supportAttachmentRequest,
	runCommand supportCommandRunner,
) (supportAttachmentReceipt, error) {
	request.Project = strings.TrimSpace(request.Project)
	request.Ticket = strings.TrimSpace(request.Ticket)
	if !supportProjectPattern.MatchString(request.Project) {
		return supportAttachmentReceipt{}, fmt.Errorf(
			"invalid Somewhere project %q; use its slug, ID, or `default`",
			request.Project,
		)
	}
	if !supportTicketPattern.MatchString(request.Ticket) {
		return supportAttachmentReceipt{}, fmt.Errorf(
			"invalid Somewhere ticket %q; expected a tsk_ ID",
			request.Ticket,
		)
	}
	if len(request.Bundle) == 0 || len(request.Bundle) > 64*1024 {
		return supportAttachmentReceipt{}, fmt.Errorf(
			"redacted diagnostic bundle must be between 1 byte and 64 KiB",
		)
	}
	if !supportHashPattern.MatchString(request.SHA256) {
		return supportAttachmentReceipt{}, fmt.Errorf("invalid support bundle SHA-256")
	}
	sum := sha256.Sum256(request.Bundle)
	if fmt.Sprintf("%x", sum) != request.SHA256 {
		return supportAttachmentReceipt{}, fmt.Errorf(
			"support bundle SHA-256 does not match its redacted content",
		)
	}

	remotePath := fmt.Sprintf(
		"/support/%s/sessions-support-%s.json",
		request.Ticket,
		request.SHA256[:16],
	)
	script := supportAttachmentScript(request, remotePath)
	scratchRoot, err := os.MkdirTemp("", "sessions-support-attach-")
	if err != nil {
		return supportAttachmentReceipt{}, fmt.Errorf("create private attachment staging directory: %w", err)
	}
	defer os.RemoveAll(scratchRoot)
	if err := os.Chmod(scratchRoot, 0o700); err != nil {
		return supportAttachmentReceipt{}, fmt.Errorf("protect attachment staging directory: %w", err)
	}
	scriptPath := filepath.Join(scratchRoot, "attach.mjs")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		return supportAttachmentReceipt{}, fmt.Errorf("stage attachment request: %w", err)
	}

	stdout, stderr, err := runCommand(
		ctx,
		"somewhere",
		"run",
		"--project",
		request.Project,
		"--timeout",
		"10000",
		"--json",
		scriptPath,
	)
	if err != nil {
		detail := strings.TrimSpace(string(stderr))
		if detail == "" {
			detail = err.Error()
		}
		if len(detail) > 800 {
			detail = detail[:800] + "…"
		}
		return supportAttachmentReceipt{}, fmt.Errorf(
			"Somewhere CLI did not attach the bundle: %s",
			detail,
		)
	}
	var envelope struct {
		Result supportAttachmentReceipt `json:"result"`
		Error  json.RawMessage          `json:"error"`
	}
	if err := json.Unmarshal(stdout, &envelope); err != nil {
		return supportAttachmentReceipt{}, fmt.Errorf(
			"Somewhere CLI returned an unreadable attachment receipt",
		)
	}
	receipt := envelope.Result
	if len(envelope.Error) != 0 && string(envelope.Error) != "null" {
		return supportAttachmentReceipt{}, fmt.Errorf(
			"Somewhere CLI reported an attachment error",
		)
	}
	if receipt.Project != request.Project ||
		receipt.Ticket != request.Ticket ||
		receipt.Path != remotePath ||
		receipt.SHA256 != request.SHA256 ||
		receipt.SizeBytes != len(request.Bundle) ||
		!receipt.Uploaded {
		return supportAttachmentReceipt{}, fmt.Errorf(
			"Somewhere CLI returned a mismatched attachment receipt",
		)
	}
	return receipt, nil
}

func supportAttachmentScript(request supportAttachmentRequest, remotePath string) string {
	bundle := strconv.Quote(string(request.Bundle))
	ticket := strconv.Quote(request.Ticket)
	project := strconv.Quote(request.Project)
	path := strconv.Quote(remotePath)
	hash := strconv.Quote(request.SHA256)
	return fmt.Sprintf(`export default async function (sw) {
  const project = %s;
  const ticket = %s;
  const path = %s;
  const sha256 = %s;
  const bytes = new TextEncoder().encode(%s);
  const current = await sw.tasks.get(ticket);
  const existing = Array.isArray(current.attachments) ? current.attachments : [];
  if (existing.includes(path)) {
    return {
      schema_version: 1,
      project,
      ticket,
      path,
      sha256,
      size_bytes: bytes.byteLength,
      uploaded: true,
      already_attached: true
    };
  }
  await sw.fs.write(path, bytes, { contentType: "application/json" });
  try {
    const latest = await sw.tasks.get(ticket);
    const attachments = Array.isArray(latest.attachments) ? latest.attachments : [];
    await sw.tasks.update(ticket, {
      attachments: attachments.includes(path) ? attachments : [...attachments, path]
    });
  } catch (error) {
    await sw.fs.delete(path).catch(() => {});
    throw error;
  }
  return {
    schema_version: 1,
    project,
    ticket,
    path,
    sha256,
    size_bytes: bytes.byteLength,
    uploaded: true,
    already_attached: false
  };
}
`, project, ticket, path, hash, bundle)
}

func runSupportCommand(
	ctx context.Context,
	name string,
	args ...string,
) ([]byte, []byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}
