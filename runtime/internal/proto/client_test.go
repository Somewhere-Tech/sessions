package proto

import (
	"context"
	"encoding/json"
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
