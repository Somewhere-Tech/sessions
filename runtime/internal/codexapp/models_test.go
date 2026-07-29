package codexapp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"
	"time"
)

func TestResolveModelChoice(t *testing.T) {
	catalog := []Model{
		{
			ID: "alpha", Model: "wire-alpha", IsDefault: true,
			SupportedReasoningEfforts: []ReasoningEffortOption{{ReasoningEffort: "low"}, {ReasoningEffort: "high"}},
			ServiceTiers:              []ModelServiceTier{{ID: "priority"}},
		},
		{
			ID: "beta", Model: "wire-beta",
			SupportedReasoningEfforts: []ReasoningEffortOption{{ReasoningEffort: "medium"}},
		},
	}
	tests := []struct {
		name      string
		choice    ModelChoice
		want      ModelChoice
		wantError string
	}{
		{
			name: "valid exact declaration", choice: ModelChoice{Model: "alpha", Effort: "high", ServiceTier: "priority"},
			want: ModelChoice{Model: "wire-alpha", Effort: "high", ServiceTier: "priority"},
		},
		{
			name: "implicit live default", choice: ModelChoice{Effort: "low"},
			want: ModelChoice{Model: "wire-alpha", Effort: "low"},
		},
		{
			name: "invalid model", choice: ModelChoice{Model: "missing", Effort: "high"},
			wantError: `model "missing" not available; valid: [alpha, beta]`,
		},
		{
			name: "invalid effort cannot downgrade", choice: ModelChoice{Model: "beta", Effort: "high"},
			wantError: `effort "high" not supported by model "beta"; valid: [medium]`,
		},
		{
			name: "invalid service tier cannot disappear", choice: ModelChoice{Model: "beta", ServiceTier: "priority"},
			wantError: `service tier "priority" not supported by model "beta"; valid: []`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ResolveModelChoice(catalog, test.choice)
			if test.wantError != "" {
				if err == nil || err.Error() != test.wantError {
					t.Fatalf("ResolveModelChoice() error = %v, want %q", err, test.wantError)
				}
				if got != (ModelChoice{}) {
					t.Fatalf("invalid choice silently resolved to %#v", got)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("ResolveModelChoice() = %#v, %v; want %#v", got, err, test.want)
			}
		})
	}
}

func TestListModelsPaginatesLiveProtocol(t *testing.T) {
	serverInput, clientInput := io.Pipe()
	clientOutput, serverOutput := io.Pipe()
	serverDone := make(chan error, 1)
	releaseServer := make(chan struct{})
	go func() {
		defer serverOutput.Close()
		decoder := json.NewDecoder(serverInput)
		encoder := json.NewEncoder(serverOutput)
		request, err := readTestRequest(decoder, "initialize")
		if err != nil {
			serverDone <- err
			return
		}
		if err := encoder.Encode(map[string]any{"id": request.ID, "result": map[string]any{}}); err != nil {
			serverDone <- err
			return
		}
		if _, err := readTestRequest(decoder, "initialized"); err != nil {
			serverDone <- err
			return
		}
		request, err = readTestRequest(decoder, "model/list")
		if err != nil {
			serverDone <- err
			return
		}
		var first modelListParams
		if err := json.Unmarshal(request.Params, &first); err != nil || first.Cursor != nil || !first.IncludeHidden {
			serverDone <- fmt.Errorf("first model/list params = %s, %v", request.Params, err)
			return
		}
		if err := encoder.Encode(map[string]any{"id": request.ID, "result": map[string]any{
			"data":       []any{map[string]any{"id": "alpha", "model": "alpha", "displayName": "Alpha"}},
			"nextCursor": "page-2",
		}}); err != nil {
			serverDone <- err
			return
		}
		request, err = readTestRequest(decoder, "model/list")
		if err != nil {
			serverDone <- err
			return
		}
		var second modelListParams
		if err := json.Unmarshal(request.Params, &second); err != nil || second.Cursor == nil || *second.Cursor != "page-2" || !second.IncludeHidden {
			serverDone <- fmt.Errorf("second model/list params = %s, %v", request.Params, err)
			return
		}
		if err := encoder.Encode(map[string]any{"id": request.ID, "result": map[string]any{
			"data":       []any{map[string]any{"id": "beta", "model": "beta", "displayName": "Beta"}},
			"nextCursor": nil,
		}}); err != nil {
			serverDone <- err
			return
		}
		serverDone <- nil
		<-releaseServer
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, err := newClient(ctx, clientInput, clientOutput, func() {
		_ = clientInput.Close()
		_ = clientOutput.Close()
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	catalog, err := client.ListModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog) != 2 || catalog[0].ID != "alpha" || catalog[1].ID != "beta" {
		t.Fatalf("catalog = %#v", catalog)
	}
	close(releaseServer)
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}
