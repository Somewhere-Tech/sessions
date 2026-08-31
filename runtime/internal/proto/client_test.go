package proto

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"
)

func TestSocketRunnerConfigureModelWaitsForAcknowledgement(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	runner := &SocketRunner{
		conn: client,
		info: RunnerInfo{ProtocolVersion: 2},
		subs: make(map[uint64]chan Event),
	}
	go func() {
		frame, err := Read(server)
		if err != nil {
			return
		}
		if frame.Type != ModelReq {
			return
		}
		control, err := DecodeModelControl(frame.Payload)
		result := ModelControlResult{}
		if err != nil {
			result.Error = err.Error()
		} else if control.Model != "gpt-next" || control.Effort != "high" {
			result.Error = "unexpected model control"
		}
		payload, _ := json.Marshal(result)
		_ = Write(server, ModelRes, payload)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := runner.ConfigureModel(ctx, ModelControl{Model: "gpt-next", Effort: "high"}); err != nil {
		t.Fatalf("ConfigureModel() error = %v", err)
	}
}

func TestSocketRunnerConfigureModelReturnsRunnerError(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	runner := &SocketRunner{
		conn: client,
		info: RunnerInfo{ProtocolVersion: 2},
		subs: make(map[uint64]chan Event),
	}
	go func() {
		if _, err := Read(server); err != nil {
			return
		}
		payload, _ := json.Marshal(ModelControlResult{Error: "metadata write failed"})
		_ = Write(server, ModelRes, payload)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := runner.ConfigureModel(ctx, ModelControl{Model: "opus"}); err == nil || err.Error() != "metadata write failed" {
		t.Fatalf("ConfigureModel() error = %v", err)
	}
}

func TestSocketRunnerConfigureModelRejectsOldProtocol(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	runner := &SocketRunner{
		conn: client,
		info: RunnerInfo{ProtocolVersion: 1},
		subs: make(map[uint64]chan Event),
	}
	if err := runner.ConfigureModel(context.Background(), ModelControl{Model: "opus"}); err == nil {
		t.Fatal("protocol-1 runner accepted model control")
	}
}

func TestSocketRunnerReplayBoundsStructuredEventsFromOlderRunner(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	runner := &SocketRunner{
		conn: client,
		info: RunnerInfo{ProtocolVersion: ProtocolVersion},
		subs: make(map[uint64]chan Event),
	}
	total := MaxStructuredReplayEvents + 37
	serverDone := make(chan error, 1)
	go func() {
		frame, err := Read(server)
		if err != nil {
			serverDone <- err
			return
		}
		if frame.Type != ReplayReq {
			serverDone <- errors.New("expected replay request")
			return
		}
		for index := 0; index < total; index++ {
			payload, err := json.Marshal(map[string]int{"index": index})
			if err != nil {
				serverDone <- err
				return
			}
			if err := Write(server, Structured, payload); err != nil {
				serverDone <- err
				return
			}
		}
		serverDone <- Write(server, ReplayDone, nil)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	replay := runner.Replay(ctx, 0)
	if err := <-serverDone; err != nil {
		t.Fatalf("serve replay: %v", err)
	}
	if len(replay.Structured) != MaxStructuredReplayEvents {
		t.Fatalf("structured replay length = %d, want %d", len(replay.Structured), MaxStructuredReplayEvents)
	}
	var first, last struct {
		Index int `json:"index"`
	}
	if err := json.Unmarshal(replay.Structured[0], &first); err != nil {
		t.Fatalf("decode first event: %v", err)
	}
	if err := json.Unmarshal(replay.Structured[len(replay.Structured)-1], &last); err != nil {
		t.Fatalf("decode last event: %v", err)
	}
	if first.Index != total-MaxStructuredReplayEvents || last.Index != total-1 {
		t.Fatalf("structured replay range = %d..%d, want %d..%d", first.Index, last.Index, total-MaxStructuredReplayEvents, total-1)
	}
}
