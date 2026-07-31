package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/brutella/hap"
	"github.com/brutella/hap/accessory"

	"yandex-bridge/internal/config"
	"yandex-bridge/internal/yandex"
)

// Supervisor owns the HAP server and decides when the accessory set changed
// enough to justify rebuilding it.
//
// hap fixes the accessory list at NewServer time, so adding or removing a
// device means standing a new server up. That is disruptive — paired
// controllers reconnect and re-read everything — and it is also dangerous if
// triggered carelessly: rebuilding from a partial API response would drop
// accessories that are merely missing from one answer. Hence the confirmation
// rule below.
type Supervisor struct {
	cfg      config.Config
	api      API
	registry *Registry
	syncer   *Syncer
	health   *Health
	store    hap.Store
	logger   *slog.Logger

	mu sync.Mutex
	// current is the topology hash the running server was built from.
	current string
	// candidate is a differing topology awaiting confirmation.
	candidate string
	// candidateSeen counts consecutive polls agreeing on candidate.
	candidateSeen int

	// rebuild carries a confirmed new accessory set to the run loop. It holds
	// one entry: a newer set always supersedes an older one.
	rebuild chan []Spec

	// inventory is the latest mapping report, served over HTTP so device ids
	// can be looked up without grepping the logs.
	inventoryMu sync.RWMutex
	inventory   []MappingReport
}

// setInventory stores the latest mapping report.
func (s *Supervisor) setInventory(reports []MappingReport) {
	s.inventoryMu.Lock()
	defer s.inventoryMu.Unlock()
	s.inventory = reports
}

// Inventory returns the latest mapping report, for the /devices endpoint.
func (s *Supervisor) Inventory() []MappingReport {
	s.inventoryMu.RLock()
	defer s.inventoryMu.RUnlock()
	return slices.Clone(s.inventory)
}

// NewSupervisor wires the pieces together.
func NewSupervisor(cfg config.Config, api API, registry *Registry, syncer *Syncer, health *Health, store hap.Store, logger *slog.Logger) *Supervisor {
	s := &Supervisor{
		cfg:      cfg,
		api:      api,
		registry: registry,
		syncer:   syncer,
		health:   health,
		store:    store,
		logger:   logger,
		rebuild:  make(chan []Spec, 1),
	}
	syncer.OnPoll(s.observePoll)
	return s
}

// observePoll is called with the result of every successful poll.
//
// A failed poll never reaches here, which is the point: the accessory set can
// only ever change on the strength of evidence that Yandex actually answered.
func (s *Supervisor) observePoll(info *yandex.UserInfo) {
	specs, reports := BuildSpecs(info, s.cfg)
	hash := TopologyHash(specs)
	s.setInventory(reports)

	s.mu.Lock()
	defer s.mu.Unlock()

	if hash == s.current {
		// Back to what is already running; forget any half-confirmed change.
		s.candidate, s.candidateSeen = "", 0
		return
	}

	if hash != s.candidate {
		s.candidate = hash
		s.candidateSeen = 0
		s.logger.Info("device topology changed, waiting for confirmation",
			slog.String("from", s.current),
			slog.String("to", hash),
			slog.Int("devices", len(specs)),
			slog.Int("needed", s.cfg.TopologyConfirmations))
	}

	// Counted after the reset, so the first sighting is itself one
	// confirmation and a threshold of 1 applies the change immediately.
	s.candidateSeen++
	if s.candidateSeen < s.cfg.TopologyConfirmations {
		s.logger.Debug("topology change pending",
			slog.Int("seen", s.candidateSeen),
			slog.Int("needed", s.cfg.TopologyConfirmations))
		return
	}

	s.logger.Info("device topology change confirmed, rebuilding accessories",
		slog.String("topology", hash),
		slog.Int("devices", len(specs)))

	// Re-print the inventory: the device set just changed, so this is exactly
	// when the operator needs to see ids and what got mapped.
	LogInventory(s.logger, reports)

	// Replace any queued rebuild: the newest confirmed set is the one to use.
	select {
	case <-s.rebuild:
	default:
	}
	s.rebuild <- specs

	s.current = hash
	s.candidate, s.candidateSeen = "", 0
}

// Run brings up the HAP server and keeps it running, rebuilding it whenever a
// topology change is confirmed. It returns when ctx is done.
func (s *Supervisor) Run(ctx context.Context) error {
	specs, err := s.initialSpecs(ctx)
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.current = TopologyHash(specs)
	s.mu.Unlock()

	for {
		serveErr, stop, err := s.serve(ctx, specs)
		if err != nil {
			return err
		}

		select {
		case <-ctx.Done():
			stop()
			<-serveErr
			return nil

		case next := <-s.rebuild:
			s.logger.Info("restarting HAP server for the new accessory set")
			stop()
			if err := <-serveErr; err != nil && !isServerClosed(err) {
				s.logger.Warn("HAP server stopped with an error", slog.String("error", err.Error()))
			}
			specs = next

		case err := <-serveErr:
			if ctx.Err() != nil {
				return nil
			}
			// The server stopped on its own: a port conflict, or mDNS
			// failing to bind. Neither recovers by itself.
			return fmt.Errorf("HAP server stopped: %w", err)
		}
	}
}

// serve builds the accessories and starts a HAP server for them. It returns a
// channel carrying the server's exit and a function that stops it.
func (s *Supervisor) serve(ctx context.Context, specs []Spec) (<-chan error, func(), error) {
	accessories, err := s.build(specs)
	if err != nil {
		return nil, nil, err
	}

	bridgeAcc := accessory.NewBridge(accessory.Info{
		Name:         s.cfg.HomeKit.Name,
		Manufacturer: manufacturer,
		Model:        "yandex-bridge",
		SerialNumber: "yandex-bridge-1",
		Firmware:     Version,
	})
	bridgeAcc.Id = BridgeAID

	others := make([]*accessory.A, 0, len(accessories)+1)
	if s.cfg.Health.Enabled {
		// A fresh health accessory each time: hap attaches callbacks to every
		// characteristic it is handed, so reusing one would multiply
		// notifications.
		s.health.Rebuild()
		others = append(others, s.health.A)
	}
	for _, a := range accessories {
		others = append(others, a.A)
	}

	srv, err := hap.NewServer(s.store, bridgeAcc.A, others...)
	if err != nil {
		return nil, nil, fmt.Errorf("create HAP server: %w", err)
	}
	srv.Pin = s.cfg.HomeKit.Pin
	srv.Addr = fmt.Sprintf(":%d", s.cfg.HomeKit.Port)
	srv.Ifaces = s.cfg.HomeKit.Interfaces

	s.syncer.SetAccessories(accessories)

	s.logger.Info("starting HAP server",
		slog.String("addr", srv.Addr),
		slog.Any("interfaces", s.cfg.HomeKit.Interfaces),
		slog.Int("accessories", len(others)+1),
		slog.Bool("paired", srv.IsPaired()))
	if !srv.IsPaired() {
		s.logger.Info("not paired yet: add the bridge in the Home app",
			slog.String("bridge", s.cfg.HomeKit.Name),
			slog.String("setup_code", formatPin(s.cfg.HomeKit.Pin)))
	}

	serveCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe(serveCtx) }()

	return done, cancel, nil
}

// build turns specs into accessories with stable ids.
func (s *Supervisor) build(specs []Spec) ([]*Accessory, error) {
	ids := make([]string, 0, len(specs))
	for _, spec := range specs {
		ids = append(ids, spec.DeviceID)
	}

	aids, err := s.registry.Assign(ids)
	if err != nil {
		return nil, fmt.Errorf("assign accessory ids: %w", err)
	}

	opts := BuildOptions{
		CoalesceDelay: s.cfg.CoalesceDelay.Std(),
		SettleWindow:  s.cfg.SettleWindow.Std(),
	}

	out := make([]*Accessory, 0, len(specs))
	for _, spec := range specs {
		aid, ok := aids[spec.DeviceID]
		if !ok {
			s.logger.Error("device has no accessory id, skipping",
				slog.String("device_id", spec.DeviceID))
			continue
		}
		out = append(out, BuildAccessory(spec, aid, s.syncer, opts, s.logger))
	}

	// Order by aid so the list handed to hap is deterministic. The ids are
	// set explicitly, so hap's positional fallback never applies — but a
	// stable order still keeps logs and diffs readable.
	slices.SortFunc(out, func(a, b *Accessory) int { return int(a.A.Id) - int(b.A.Id) })
	return out, nil
}

// initialSpecs polls until Yandex answers, so a bridge that boots before the
// network is up waits rather than starting with an empty accessory set — which
// would unpair every accessory in the user's home.
func (s *Supervisor) initialSpecs(ctx context.Context) ([]Spec, error) {
	backoff := 5 * time.Second
	const maxBackoff = 2 * time.Minute

	for attempt := 1; ; attempt++ {
		info, err := s.api.UserInfo(ctx)
		if err == nil {
			specs, reports := BuildSpecs(info, s.cfg)
			// The full inventory goes out at startup so the device ids and the
			// mapping decisions needed to write config.yaml are on hand
			// without turning on debug logging.
			LogInventory(s.logger, reports)
			s.setInventory(reports)
			return specs, nil
		}

		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		s.logger.Warn("cannot reach Yandex yet, retrying before starting HomeKit",
			slog.Int("attempt", attempt),
			slog.Duration("retry_in", backoff),
			slog.String("error", err.Error()))
		s.health.Report(problemPoll, fmt.Sprintf("cannot reach Yandex at startup: %v", err))

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// isServerClosed reports whether an error is the ordinary result of stopping.
func isServerClosed(err error) bool {
	return errors.Is(err, http.ErrServerClosed) || errors.Is(err, context.Canceled)
}

// formatPin renders a setup code the way the Home app displays it.
func formatPin(pin string) string {
	if len(pin) != 8 {
		return pin
	}
	return pin[:3] + "-" + pin[3:5] + "-" + pin[5:]
}
