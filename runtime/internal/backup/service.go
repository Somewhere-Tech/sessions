package backup

import (
	"context"
	"errors"
	"log"
	"os"
	"sync"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

type Service struct {
	pusher   *Pusher
	sessions func() []state.SessionInfo

	pushMu     sync.Mutex
	periodicMu sync.Mutex
	periodic   *periodicWorker
	closed     bool
}

type periodicWorker struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func NewService(options Options, sessions func() []state.SessionInfo) *Service {
	return &Service{pusher: NewPusher(options), sessions: sessions}
}

func (s *Service) Push(ctx context.Context) (Result, error) {
	s.pushMu.Lock()
	defer s.pushMu.Unlock()
	var sessions []state.SessionInfo
	if s.sessions != nil {
		sessions = s.sessions()
	}
	return s.pusher.Push(ctx, sessions)
}

func (s *Service) Status() (Status, error) {
	config, err := LoadConfig(s.pusher.options.ConfigPath)
	if errors.Is(err, os.ErrNotExist) {
		return Status{}, nil
	}
	if err != nil {
		return Status{}, err
	}
	status := config.Status()
	if status.Encrypt {
		status.KeyPath = s.pusher.options.KeyPath
	}
	return status, nil
}

// ReloadPeriodic applies the current enable flag and interval. It is called at
// daemon construction and after `sessions backup enable`; no goroutine runs for
// a disabled or absent configuration.
func (s *Service) ReloadPeriodic() error {
	config, err := LoadConfig(s.pusher.options.ConfigPath)
	if errors.Is(err, os.ErrNotExist) {
		s.replacePeriodic(0, false)
		return nil
	}
	if err != nil {
		return err
	}
	if !config.Enabled {
		s.replacePeriodic(0, false)
		return nil
	}
	interval, err := config.interval()
	if err != nil {
		return err
	}
	s.replacePeriodic(interval, true)
	return nil
}

func (s *Service) replacePeriodic(interval time.Duration, enabled bool) {
	s.periodicMu.Lock()
	defer s.periodicMu.Unlock()
	s.stopPeriodicLocked()
	if !enabled || s.closed {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	worker := &periodicWorker{cancel: cancel, done: make(chan struct{})}
	s.periodic = worker
	go func() {
		defer close(worker.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if ctx.Err() != nil {
					return
				}
				if _, err := s.Push(ctx); err != nil && !errors.Is(err, context.Canceled) {
					log.Printf("periodic session backup: %v", err)
				}
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (s *Service) Close() {
	s.periodicMu.Lock()
	defer s.periodicMu.Unlock()
	s.closed = true
	s.stopPeriodicLocked()
}

func (s *Service) stopPeriodicLocked() {
	if s.periodic != nil {
		s.periodic.cancel()
		<-s.periodic.done
		s.periodic = nil
	}
}
