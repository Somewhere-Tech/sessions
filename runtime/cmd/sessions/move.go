package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/localnetwork"
	"github.com/somewhere-tech/sessions/runtime/internal/migrate"
	"github.com/somewhere-tech/sessions/runtime/internal/tokenstore"
)

func (a *app) cmdMove(args []string) error {
	const usage = "usage: sessions move <ended-session> (--machine NAME | --to ENDPOINT [--token T]) [--terminal] [--dry-run] [--allow-dirty]"
	if len(args) == 0 || strings.HasPrefix(args[0], "--") {
		return fail(1, usage)
	}
	sessionArg := args[0]
	args = args[1:]
	target, hasTarget := pluck(&args, "--to")
	machineRef, hasMachine := pluck(&args, "--machine")
	if hasTarget == hasMachine || (hasTarget && strings.TrimSpace(target) == "") || (hasMachine && strings.TrimSpace(machineRef) == "") {
		return fail(1, usage)
	}
	token, hasToken := pluck(&args, "--token")
	if hasToken && token == "" {
		return fail(1, "--token needs a value")
	}
	if hasMachine && hasToken {
		return fail(1, "--token cannot be combined with --machine; Sessions reads the saved device credential privately")
	}
	dryRun := removeFirst(&args, "--dry-run")
	allowDirty := removeFirst(&args, "--allow-dirty")
	terminal := removeFirst(&args, "--terminal")
	if len(args) != 0 {
		return fail(1, "unknown move option %s", args[0])
	}
	client, target, err := a.moveTargetClient(target, token, machineRef, hasMachine)
	if err != nil {
		return fail(1, "%s", err)
	}
	id, err := a.resolveSessionID(sessionArg)
	if err != nil {
		return err
	}
	if a.explicitTarget && (!a.api.localToken || a.api.pathPrefix != "") {
		return a.moveRemoteSource(context.Background(), id, client, dryRun, allowDirty, terminal)
	}
	sessions, err := a.listSessions(true)
	if err != nil {
		return err
	}
	var source *session
	for index := range sessions {
		if sessions[index].ID == id {
			source = &sessions[index]
			break
		}
	}
	if source == nil {
		return fail(1, "%s", unknownSessionMessage(id))
	}
	if !source.Exited {
		return fail(1, "session %s is still live; end it before continuing on another machine", source.ID)
	}
	if source.Tool != "claude-code" && source.Tool != "codex" {
		return fail(1, "cross-machine continuation is available only for ended Claude and Codex conversations")
	}
	if source.Profile != "" || source.ConfigDir != "" {
		return fail(1, "cross-machine continuation for isolated provider profiles is not available yet; no files were copied")
	}
	ctx := context.Background()
	store, err := ledger.Open(ctx, ledger.Options{})
	if err != nil {
		return fail(2, "open local ledger: %s", err)
	}
	defer store.Close()
	request, err := migrate.ResolveSource(ctx, store, migrate.SourceSession{
		ID: source.ID, Name: source.Name, Tool: source.Tool, Cmd: source.Cmd,
		Args: source.Args, Cwd: source.Cwd, CreatedAt: source.CreatedAt,
	})
	if err != nil {
		return fail(1, "%s", err)
	}
	workspace, err := migrate.PrepareWorkspace(ctx, source.Cwd, source.ID, migrate.WorkspaceOptions{
		AllowDirty: allowDirty, DryRun: dryRun,
	})
	if err != nil {
		return fail(1, "%s", err)
	}
	request.Workspace = workspace
	request.SourceEndpoint = localEndpoint(a)
	if terminal {
		request.RuntimeMode = migrate.RuntimeTerminal
	}
	result := migrate.MoveResult{
		SourceID: source.ID, TargetEndpoint: client.Endpoint(), Tool: request.Tool, Cwd: request.Cwd,
		ResumeRecipe: append([]string(nil), request.ResumeRecipe...), RuntimeMode: request.RuntimeMode, Workspace: workspace,
		ConversationSize: len(request.ConversationBytes), DryRun: dryRun,
	}
	if dryRun {
		if a.wantJSON {
			return writeJSON(a.stdout, result, true)
		}
		return writeMovePlan(a, result)
	}
	received, err := client.Receive(ctx, request)
	if err != nil {
		return fail(2, "%s", localnetwork.Explain(target, err))
	}
	created, err := client.Create(ctx, request)
	if err != nil {
		return fail(2, "conversation received but target resume failed: %s", localnetwork.Explain(target, err))
	}
	result.TargetID = created.Session.ID
	result.Receive = received
	result.TargetLineageRecorded = created.LineageRecorded
	result.Warning = created.Warning
	if err := store.Migrations().RecordMovedTo(ctx, ledger.MovedTo{
		Meta: ledger.Meta{LaneID: source.ID}, TargetEndpoint: client.Endpoint(),
		NewLaneID: created.Session.ID, CheckpointRef: workspace.CheckpointRef,
	}); err != nil {
		return fail(2, "target continued as %s but local moved_to ledger write failed: %s", created.Session.ID, err)
	}
	if a.wantJSON {
		return writeJSON(a.stdout, result, true)
	}
	if _, err := fmt.Fprintf(a.stdout, "continued %s on %s as %s (%s)\n", source.ID, client.Endpoint(), created.Session.ID, request.RuntimeMode); err != nil {
		return err
	}
	if workspace.Git {
		ref := workspace.Branch
		if workspace.CheckpointRef != "" {
			ref = workspace.CheckpointRef
		}
		if _, err := fmt.Fprintf(a.stdout, "workspace: %s %s at %s\n", migrate.DisplayRemote(workspace.RemoteURL), ref, workspace.Revision); err != nil {
			return err
		}
	} else if _, err := fmt.Fprintln(a.stdout, "workspace: non-Git cwd was already present on the target"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(a.stdout, "conversation: %d bytes transferred\n", len(request.ConversationBytes)); err != nil {
		return err
	}
	if created.Warning != "" {
		if _, err := fmt.Fprintf(a.stdout, "warning: %s\n", created.Warning); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintln(a.stdout, "source history was preserved on this machine")
	return err
}

func (a *app) moveTargetClient(target, token, machineRef string, saved bool) (*migrate.Client, string, error) {
	if !saved {
		client, err := migrate.NewClient(target, token)
		return client, target, err
	}
	machine, err := loadSavedMachine(a.home, machineRef)
	if err != nil {
		return nil, "", err
	}
	if !a.direct {
		client, err := migrate.NewRelayClient(daemonOrigin(a.api), machine.Endpoint, machine.MachineID)
		return client, machine.Endpoint, err
	}
	token, err = tokenstore.ReadSecret(savedMachineTokenPath(a.home, machine.MachineID))
	if err != nil {
		return nil, "", fail(2, "read saved credential for %q: %s", machine.Alias, err)
	}
	if token == "" {
		return nil, "", fail(2, "saved credential for %q is empty; forget and reconnect this machine", machine.Alias)
	}
	client, err := migrate.NewClient(machine.Endpoint, token)
	return client, machine.Endpoint, err
}

func daemonOrigin(client *apiClient) string {
	target, err := client.target("")
	if err != nil {
		return ""
	}
	target.Path, target.RawPath, target.RawQuery = "", "", ""
	return strings.TrimSuffix(target.String(), "/")
}

func (a *app) moveRemoteSource(
	ctx context.Context,
	id string,
	client *migrate.Client,
	dryRun bool,
	allowDirty bool,
	terminal bool,
) error {
	runtimeMode := migrate.RuntimeRich
	if terminal {
		runtimeMode = migrate.RuntimeTerminal
	}
	var exported migrate.ExportResult
	if err := a.postJSON("/api/migrate/export", migrate.ExportRequest{
		SessionID: id, SourceEndpoint: localEndpoint(a), RuntimeMode: runtimeMode,
		DryRun: dryRun, AllowDirty: allowDirty,
	}, &exported, 2); err != nil {
		return err
	}
	result := exported.Plan
	result.TargetEndpoint = client.Endpoint()
	if dryRun {
		if a.wantJSON {
			return writeJSON(a.stdout, result, true)
		}
		return writeMovePlan(a, result)
	}
	received, err := client.Receive(ctx, exported.Request)
	if err != nil {
		return fail(2, "%s", localnetwork.Explain(client.Endpoint(), err))
	}
	created, err := client.Create(ctx, exported.Request)
	if err != nil {
		return fail(2, "conversation received but target resume failed: %s", localnetwork.Explain(client.Endpoint(), err))
	}
	result.TargetID = created.Session.ID
	result.Receive = received
	result.TargetLineageRecorded = created.LineageRecorded
	result.Warning = created.Warning
	if err := a.postJSON("/api/migrate/complete", migrate.CompleteRequest{
		SourceID: id, TargetEndpoint: client.Endpoint(), TargetID: created.Session.ID,
		CheckpointRef: exported.Request.Workspace.CheckpointRef,
	}, &map[string]any{}, 2); err != nil {
		return fail(2, "target continued as %s but source lineage write failed: %s", created.Session.ID, err)
	}
	if a.wantJSON {
		return writeJSON(a.stdout, result, true)
	}
	if _, err := fmt.Fprintf(a.stdout, "continued %s on %s as %s (%s)\n", id, client.Endpoint(), created.Session.ID, exported.Request.RuntimeMode); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(a.stdout, "conversation: %d bytes transferred\n", len(exported.Request.ConversationBytes)); err != nil {
		return err
	}
	if created.Warning != "" {
		if _, err := fmt.Fprintf(a.stdout, "warning: %s\n", created.Warning); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintln(a.stdout, "source history was preserved on its original machine")
	return err
}

func writeMovePlan(a *app, result migrate.MoveResult) error {
	if _, err := fmt.Fprintf(a.stdout, "dry run: would move %s to %s\n", result.SourceID, result.TargetEndpoint); err != nil {
		return err
	}
	if result.Workspace.Git {
		ref := result.Workspace.Branch
		if result.Workspace.CheckpointRef != "" {
			ref = result.Workspace.CheckpointRef
		}
		if _, err := fmt.Fprintf(a.stdout, "workspace: %s %s at %s\n", migrate.DisplayRemote(result.Workspace.RemoteURL), ref, result.Workspace.Revision); err != nil {
			return err
		}
	} else if _, err := fmt.Fprintln(a.stdout, "workspace: target must already have the non-Git cwd"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(a.stdout, "conversation: %d bytes would be transferred\n", result.ConversationSize); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(a.stdout, "runtime: %s\n", result.RuntimeMode); err != nil {
		return err
	}
	_, err := fmt.Fprintln(a.stdout, "dry run: no files, sessions, or ledger events changed")
	return err
}

func localEndpoint(a *app) string {
	if a.api.relayEndpoint != "" {
		return a.api.relayEndpoint
	}
	target, err := a.api.target("")
	if err != nil {
		return ""
	}
	target.Path = ""
	target.RawQuery = ""
	return strings.TrimSuffix(target.String(), "/")
}
