package state

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestPersistentFramingAndRestore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.events")
	p, err := OpenPersistent(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Append(7, []byte("abc")); err != nil {
		t.Fatal(err)
	}
	if err := p.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 11 || binary.BigEndian.Uint32(b[:4]) != 7 || binary.BigEndian.Uint32(b[4:8]) != 7 || string(b[8:]) != "abc" {
		t.Fatalf("unexpected bytes: %v", b)
	}
	events, err := Restore(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Seq != 7 || string(events[0].Data) != "abc" {
		t.Fatalf("unexpected restore: %#v", events)
	}
}

func TestRestoreIgnoresTruncatedTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.events")
	p, err := OpenPersistent(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Append(1, []byte("ok")); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte{0, 0, 0, 9, 0})
	_ = f.Close()
	events, err := Restore(path)
	if err != nil || len(events) != 1 || string(events[0].Data) != "ok" {
		t.Fatalf("events=%#v err=%v", events, err)
	}
}

func TestEventLogSequenceAndCap(t *testing.T) {
	l := NewEventLog(4)
	l.PushAt(9, []byte("abc"))
	l.Push([]byte("de"))
	r := l.Since(0)
	if !r.Gap || r.Current != 10 || len(r.Events) != 1 || r.Events[0].Seq != 10 {
		t.Fatalf("unexpected replay: %#v", r)
	}
}
