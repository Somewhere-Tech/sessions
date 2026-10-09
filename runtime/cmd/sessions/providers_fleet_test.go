package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFleetProviderUpdatesReportEveryHostAndNeverTouchSessions(t *testing.T) {
	var mu sync.Mutex
	requests := []string{}
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/api/providers/codex/update") {
			t.Errorf("unexpected operation %s %s", r.Method, r.URL.Path)
			w.WriteHeader(400)
			return
		}
		if strings.Contains(r.URL.Path, "bbbbbbbb") {
			w.WriteHeader(403)
			_, _ = w.Write([]byte(`{"error":"pair this computer again"}`))
			return
		}
		_, _ = w.Write([]byte(`{"provider":{"id":"codex","name":"Codex","installed":true,"version":"fixture-version"},"output":"done"}`))
	}))
	defer fixture.Close()
	home := t.TempDir()
	for _, machine := range []savedMachine{
		{MachineID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Alias: "studio", Name: "Studio", Endpoint: fixture.URL, ConnectedAt: time.Now()},
		{MachineID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Alias: "laptop", Name: "Laptop", Endpoint: fixture.URL, ConnectedAt: time.Now()},
	} {
		if _, err := saveMachine(home, machine, "fixture-token"); err != nil {
			t.Fatal(err)
		}
	}
	client, err := newAPIClient(fixture.URL, "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	var output bytes.Buffer
	application := &app{home: home, api: client, wantJSON: true, stdout: &output}
	if code := exitCode(application.cmdProviders([]string{"update", "codex", "--all"})); code != 2 {
		t.Fatalf("partial exit=%d", code)
	}
	var receipt struct {
		Complete bool                    `json:"complete"`
		Results  []machineProviderUpdate `json:"results"`
	}
	if err := json.Unmarshal(output.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Complete || len(receipt.Results) != 3 || len(requests) != 3 {
		t.Fatalf("receipt=%+v requests=%v", receipt, requests)
	}
	for _, result := range receipt.Results {
		if result.Machine == "laptop" {
			if result.Status != "unknown" || !strings.Contains(result.Detail, "pair this computer again") {
				t.Fatalf("partial failure lost its recovery instruction: %+v", result)
			}
		} else if result.Status != "updated" {
			t.Fatalf("result=%+v", result)
		}
	}
}

func TestFleetProviderUpdateRefusesAmbiguousSelectedHost(t *testing.T) {
	application := &app{explicitTarget: true}
	if err := application.cmdProviders([]string{"update", "claude", "--all"}); err == nil || !strings.Contains(err.Error(), "omit --machine") {
		t.Fatalf("err=%v", err)
	}
}
