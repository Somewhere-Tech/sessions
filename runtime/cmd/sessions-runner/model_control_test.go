package main

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func TestModelControlArgumentUpdates(t *testing.T) {
	args := []string{"--model", "old", "-c", "service_tier=priority", "-c", "model_reasoning_effort=medium"}
	args = withArgumentValue(args, "new", "--model", "-m")
	args = withConfigValue(args, "model_reasoning_effort", "high")
	want := []string{"-c", "service_tier=priority", "--model", "new", "-c", "model_reasoning_effort=high"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("updated args = %#v, want %#v", args, want)
	}

	if cleared := withArgumentValue([]string{"-m", "opus", "--verbose"}, "", "--model", "-m"); !reflect.DeepEqual(cleared, []string{"--verbose"}) {
		t.Fatalf("cleared args = %#v", cleared)
	}
}

func TestClaudeModelControlRollsBackWhenMetadataCannotPersist(t *testing.T) {
	original := []string{"--model", "sonnet", "--effort", "medium"}
	runner := claudeStructuredRunner{
		cfg:   config{args: append([]string(nil), original...)},
		paths: state.Paths{Meta: filepath.Join(t.TempDir(), "missing", "meta.json")},
	}
	if err := runner.configureModel(proto.ModelControl{Model: "opus", Effort: "high"}); err == nil {
		t.Fatal("configureModel() succeeded with an unwritable metadata path")
	}
	if !reflect.DeepEqual(runner.cfg.args, original) {
		t.Fatalf("runner args after failed persist = %#v, want %#v", runner.cfg.args, original)
	}
}

func TestClaudeModelControlRejectsActiveTurn(t *testing.T) {
	original := []string{"--model", "sonnet"}
	runner := claudeStructuredRunner{
		cfg:    config{args: append([]string(nil), original...)},
		paths:  state.Paths{Meta: filepath.Join(t.TempDir(), "meta.json")},
		active: true,
	}
	if err := runner.configureModel(proto.ModelControl{Model: "opus"}); err == nil {
		t.Fatal("configureModel() accepted an active turn")
	}
	if !reflect.DeepEqual(runner.cfg.args, original) {
		t.Fatalf("runner args after active-turn rejection = %#v, want %#v", runner.cfg.args, original)
	}
}
