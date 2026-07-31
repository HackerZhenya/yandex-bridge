package bridge

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"yandex-bridge/internal/config"
	"yandex-bridge/internal/yandex"
)

// API is the part of the Yandex client the sync loop needs.
type API interface {
	UserInfo(ctx context.Context) (*yandex.UserInfo, error)
	Device(ctx context.Context, deviceID string) (*yandex.Device, error)
	Act(ctx context.Context, req yandex.ActionRequest) (*yandex.ActionResponse, error)
}

// Observer is notified about the health of the link to Yandex.
type Observer interface {
	// PollSucceeded is called after every successful poll.
	PollSucceeded()
	// PollFailed is called after every failed poll, with the running count of
	// consecutive failures.
	PollFailed(consecutive int, err error)
}

// Syncer keeps a set of accessories in step with Yandex.
//
// It owns both directions: the poll loop that pushes Yandex state into HomeKit
// characteristics, and the Controller implementation that pushes HomeKit
// writes back out. Keeping them in one type is what lets a write schedule its
// own confirmation poll.
type Syncer struct {
	api    API
	cfg    config.Config
	logger *slog.Logger
	obs    Observer

	accessories map[string]*Accessory

	mu        sync.RWMutex
	reachable bool
	failures  int
	lastPoll  time.Time

	// confirm carries device ids whose state should be re-read soon after a
	// write, because Yandex reaches the device through the vendor's cloud and
	// the new state is not visible immediately.
	confirm chan string

	// onDevices, when set, receives the device list from every successful
	// poll. The supervisor uses it to watch for topology changes.
	onDevices func([]yandex.Device)
}

// NewSyncer returns a Syncer for the given accessories, keyed by device id.
func NewSyncer(api API, cfg config.Config, logger *slog.Logger) *Syncer {
	return &Syncer{
		api:         api,
		cfg:         cfg,
		logger:      logger,
		accessories: make(map[string]*Accessory),
		reachable:   true,
		confirm:     make(chan string, 32),
	}
}

// SetAccessories replaces the accessory set the syncer maintains.
func (s *Syncer) SetAccessories(list []*Accessory) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.accessories = make(map[string]*Accessory, len(list))
	for _, a := range list {
		s.accessories[a.Spec.DeviceID] = a
	}
}

// SetObserver installs the health observer.
func (s *Syncer) SetObserver(o Observer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.obs = o
}

// OnDevices installs a callback invoked with the device list of every
// successful poll.
func (s *Syncer) OnDevices(fn func([]yandex.Device)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onDevices = fn
}

// Reachable implements Controller. It reports whether the most recent
// interaction with Yandex succeeded.
func (s *Syncer) Reachable() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.reachable
}

// Failures returns the number of consecutive failed polls.
func (s *Syncer) Failures() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.failures
}

// LastPoll returns when the last successful poll completed.
func (s *Syncer) LastPoll() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastPoll
}

// Apply implements Controller: it sends a HomeKit-originated change to Yandex
// and schedules a confirmation read.
func (s *Syncer) Apply(ctx context.Context, deviceID string, actions []yandex.Action) error {
	_, err := s.api.Act(ctx, yandex.ActionRequest{
		Devices: []yandex.DeviceActions{{ID: deviceID, Actions: actions}},
	})

	// A single unreachable device says nothing about the link to Yandex, so
	// only a transport-level failure changes the global reachability flag.
	var deviceErr *yandex.DeviceError
	if err != nil && !errors.As(err, &deviceErr) {
		s.markUnreachable(err)
		return err
	}
	s.markReachable()
	if err != nil {
		return err
	}

	// Read the real state back shortly: the device answers through the
	// vendor's cloud, so what we just set is not necessarily what happened.
	select {
	case s.confirm <- deviceID:
	default:
		// The queue is full because writes are arriving faster than they can
		// be confirmed; the next full poll will catch up regardless.
	}
	return nil
}

// Run drives the poll loop until ctx is done.
func (s *Syncer) Run(ctx context.Context) {
	interval := s.cfg.PollInterval.Std()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Poll once immediately so HomeKit shows real state as soon as it pairs,
	// rather than the zero values it was built with.
	s.pollOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.pollOnce(ctx)
		case deviceID := <-s.confirm:
			s.confirmDevice(ctx, deviceID)
		}
	}
}

// pollOnce fetches the whole home and applies it to the accessories.
func (s *Syncer) pollOnce(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.PollInterval.Std()+30*time.Second)
	defer cancel()

	info, err := s.api.UserInfo(ctx)
	if err != nil {
		s.markUnreachable(err)
		return
	}
	s.markReachable()

	s.mu.RLock()
	accessories := s.accessories
	onDevices := s.onDevices
	s.mu.RUnlock()

	for _, dev := range info.Devices {
		if a, ok := accessories[dev.ID]; ok {
			a.Apply(dev)
		}
	}

	// A device that vanished from an otherwise successful response is genuinely
	// gone from the account, so its accessory should stop claiming to work.
	// The accessory itself is left in place: removing it is the supervisor's
	// decision, and only after several polls agree.
	present := make(map[string]struct{}, len(info.Devices))
	for _, dev := range info.Devices {
		present[dev.ID] = struct{}{}
	}
	for id, a := range accessories {
		if _, ok := present[id]; !ok {
			a.SetUnreachable(true)
		}
	}

	if onDevices != nil {
		onDevices(info.Devices)
	}
}

// confirmDevice re-reads one device repeatedly for a short window after a
// write, so HomeKit converges on the device's real state quickly instead of
// waiting for the next full poll.
func (s *Syncer) confirmDevice(ctx context.Context, deviceID string) {
	s.mu.RLock()
	a, ok := s.accessories[deviceID]
	s.mu.RUnlock()
	if !ok {
		return
	}

	window := s.cfg.ConfirmWindow.Std()
	step := s.cfg.ConfirmInterval.Std()
	if window <= 0 || step <= 0 {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, window)
	defer cancel()

	ticker := time.NewTicker(step)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		dev, err := s.api.Device(ctx, deviceID)
		if err != nil {
			// Do not touch reachability here: the full poll is the authority
			// on whether Yandex is up, and a confirmation read failing during
			// its short window is not evidence of an outage.
			s.logger.Debug("confirmation read failed",
				slog.String("device_id", deviceID),
				slog.String("error", err.Error()))
			return
		}
		a.Apply(*dev)
	}
}

func (s *Syncer) markReachable() {
	s.mu.Lock()
	wasDown := !s.reachable
	failures := s.failures
	s.reachable = true
	s.failures = 0
	s.lastPoll = time.Now()
	obs := s.obs
	accessories := s.accessories
	s.mu.Unlock()

	if wasDown {
		s.logger.Info("Yandex is reachable again", slog.Int("failed_polls", failures))
		// Clear the blanket "not responding" flag; the next poll refines it
		// per device from the state Yandex reports.
		for _, a := range accessories {
			a.SetUnreachable(false)
		}
	}
	if obs != nil {
		obs.PollSucceeded()
	}
}

func (s *Syncer) markUnreachable(err error) {
	s.mu.Lock()
	s.reachable = false
	s.failures++
	failures := s.failures
	obs := s.obs
	accessories := s.accessories
	threshold := s.cfg.UnhealthyAfter
	s.mu.Unlock()

	s.logger.Warn("poll failed",
		slog.Int("consecutive_failures", failures),
		slog.String("error", err.Error()))

	// Hold off on marking everything unreachable until the failures look like
	// an outage rather than a blip: a single lost request should not grey out
	// the whole house in the Home app.
	if failures >= threshold {
		for _, a := range accessories {
			a.SetUnreachable(true)
		}
	}
	if obs != nil {
		obs.PollFailed(failures, err)
	}
}
