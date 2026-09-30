package relay

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

type tunnel struct {
	connection *websocket.Conn
	context    context.Context
	cancel     context.CancelFunc
	writeMu    sync.Mutex
	streamsMu  sync.Mutex
	streams    map[uint32]*stream
	nextID     atomic.Uint32
}

type stream struct {
	tunnel  *tunnel
	id      uint32
	reads   chan []byte
	current *bytes.Reader
	closed  chan struct{}
	once    sync.Once
	idleMu  sync.Mutex
	idle    time.Duration
	timer   *time.Timer
}

func newTunnel(parent context.Context, connection *websocket.Conn) *tunnel {
	ctx, cancel := context.WithCancel(parent)
	return &tunnel{connection: connection, context: ctx, cancel: cancel, streams: make(map[uint32]*stream)}
}

func (t *tunnel) open() (*stream, error) {
	id := t.nextID.Add(1)
	if id == 0 {
		id = t.nextID.Add(1)
	}
	stream := t.addStream(id)
	if err := t.send(frameOpen, id, nil); err != nil {
		stream.finish()
		return nil, err
	}
	return stream, nil
}

func (t *tunnel) addStream(id uint32) *stream {
	stream := &stream{tunnel: t, id: id, reads: make(chan []byte, 8), closed: make(chan struct{})}
	t.streamsMu.Lock()
	t.streams[id] = stream
	t.streamsMu.Unlock()
	return stream
}

func (t *tunnel) send(kind byte, id uint32, payload []byte) error {
	encoded, err := encodeFrame(kind, id, payload)
	if err != nil {
		return err
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	return t.connection.Write(t.context, websocket.MessageBinary, encoded)
}

func (t *tunnel) readLoop(onOpen func(*stream)) error {
	defer t.shutdown()
	for {
		messageType, encoded, err := t.connection.Read(t.context)
		if err != nil {
			return err
		}
		if messageType != websocket.MessageBinary {
			return errors.New("relay tunnel received a non-binary frame")
		}
		decoded, err := decodeFrame(encoded)
		if err != nil {
			return err
		}
		if err := t.deliver(decoded, onOpen); err != nil {
			return err
		}
	}
}

func (t *tunnel) deliver(message frame, onOpen func(*stream)) error {
	if message.kind == frameOpen {
		if onOpen == nil {
			return errors.New("peer cannot open relay streams")
		}
		t.streamsMu.Lock()
		_, exists := t.streams[message.streamID]
		t.streamsMu.Unlock()
		if exists || message.streamID == 0 {
			return errors.New("duplicate relay stream")
		}
		go onOpen(t.addStream(message.streamID))
		return nil
	}
	t.streamsMu.Lock()
	stream := t.streams[message.streamID]
	t.streamsMu.Unlock()
	if stream == nil {
		return nil
	}
	switch message.kind {
	case frameData:
		payload := append([]byte(nil), message.payload...)
		select {
		case stream.reads <- payload:
		case <-stream.closed:
		case <-t.context.Done():
		}
	case frameEOF:
		select {
		case stream.reads <- nil:
		case <-stream.closed:
		case <-t.context.Done():
		}
	case frameClose:
		stream.finish()
	}
	return nil
}

func (t *tunnel) shutdown() {
	t.cancel()
	t.streamsMu.Lock()
	streams := make([]*stream, 0, len(t.streams))
	for _, stream := range t.streams {
		streams = append(streams, stream)
	}
	t.streamsMu.Unlock()
	for _, stream := range streams {
		stream.finish()
	}
}

func (s *stream) Read(target []byte) (int, error) {
	for s.current == nil || s.current.Len() == 0 {
		select {
		case payload := <-s.reads:
			if payload == nil {
				return 0, io.EOF
			}
			s.current = bytes.NewReader(payload)
			continue
		default:
		}
		select {
		case payload := <-s.reads:
			if payload == nil {
				return 0, io.EOF
			}
			s.current = bytes.NewReader(payload)
		case <-s.closed:
			return 0, io.EOF
		case <-s.tunnel.context.Done():
			return 0, io.EOF
		}
	}
	count, err := s.current.Read(target)
	if count > 0 {
		s.touch()
	}
	return count, err
}

func (s *stream) Write(payload []byte) (int, error) {
	written := 0
	for len(payload) > 0 {
		size := min(len(payload), MaxPayload)
		if err := s.tunnel.send(frameData, s.id, payload[:size]); err != nil {
			return written, err
		}
		written += size
		s.touch()
		payload = payload[size:]
	}
	return written, nil
}

func (s *stream) CloseWrite() error { return s.tunnel.send(frameEOF, s.id, nil) }

func (s *stream) Close() error {
	err := s.tunnel.send(frameClose, s.id, nil)
	s.finish()
	return err
}

func (s *stream) finish() {
	s.once.Do(func() {
		s.idleMu.Lock()
		if s.timer != nil {
			s.timer.Stop()
		}
		s.idleMu.Unlock()
		close(s.closed)
		s.tunnel.streamsMu.Lock()
		delete(s.tunnel.streams, s.id)
		s.tunnel.streamsMu.Unlock()
	})
}

func (s *stream) setIdle(duration time.Duration) {
	s.idleMu.Lock()
	defer s.idleMu.Unlock()
	s.idle = duration
	if duration > 0 {
		s.timer = time.AfterFunc(duration, func() { _ = s.Close() })
	}
}

func (s *stream) touch() {
	s.idleMu.Lock()
	defer s.idleMu.Unlock()
	if s.timer != nil {
		s.timer.Reset(s.idle)
	}
}
