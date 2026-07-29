package state

import "sync"

const DefaultEventCap = 4 * 1024 * 1024

type Event struct {
	Seq  uint32
	Data []byte
}

type Replay struct {
	Events  []Event
	Gap     bool
	Oldest  uint32
	Current uint32
}

// EventLog is the runner's bounded, in-memory replay ring.
type EventLog struct {
	mu      sync.RWMutex
	events  []Event
	bytes   int
	nextSeq uint32
	cap     int
}

func NewEventLog(capBytes int) *EventLog {
	if capBytes <= 0 {
		capBytes = DefaultEventCap
	}
	return &EventLog{nextSeq: 1, cap: capBytes}
}

func (l *EventLog) Push(data []byte) Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.pushAt(l.nextSeq, data)
}

func (l *EventLog) PushAt(seq uint32, data []byte) Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.pushAt(seq, data)
}

func (l *EventLog) pushAt(seq uint32, data []byte) Event {
	copyData := append([]byte(nil), data...)
	ev := Event{Seq: seq, Data: copyData}
	l.events = append(l.events, ev)
	l.bytes += len(copyData)
	if seq >= l.nextSeq {
		l.nextSeq = seq + 1
	}
	for l.bytes > l.cap && len(l.events) > 1 {
		l.bytes -= len(l.events[0].Data)
		l.events[0] = Event{}
		l.events = l.events[1:]
	}
	return Event{Seq: ev.Seq, Data: append([]byte(nil), ev.Data...)}
}

func (l *EventLog) Since(after uint32) Replay {
	l.mu.RLock()
	defer l.mu.RUnlock()
	oldest := l.nextSeq
	if len(l.events) > 0 {
		oldest = l.events[0].Seq
	}
	current := l.nextSeq - 1
	result := Replay{Oldest: oldest, Current: current}
	if len(l.events) == 0 {
		return result
	}
	result.Gap = after+1 < oldest
	for _, ev := range l.events {
		if ev.Seq > after {
			result.Events = append(result.Events, Event{Seq: ev.Seq, Data: append([]byte(nil), ev.Data...)})
		}
	}
	return result
}

func (l *EventLog) CurrentSeq() uint32 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.nextSeq - 1
}
