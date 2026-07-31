package bridge

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"yandex-bridge/internal/config"
	"yandex-bridge/internal/yandex"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// stubAPI is a scripted Yandex API.
type stubAPI struct {
	mu sync.Mutex
	// responses are returned in order; the last one repeats.
	responses []stubResponse
	calls     int
	devices   map[string]yandex.Device
	actions   []yandex.ActionRequest
	actErr    error
}

type stubResponse struct {
	devices []yandex.Device
	err     error
}

func (s *stubAPI) UserInfo(ctx context.Context) (*yandex.UserInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.responses) == 0 {
		return &yandex.UserInfo{}, nil
	}
	i := min(s.calls, len(s.responses)-1)
	s.calls++
	r := s.responses[i]
	if r.err != nil {
		return nil, r.err
	}
	return &yandex.UserInfo{Devices: r.devices}, nil
}

func (s *stubAPI) Device(ctx context.Context, deviceID string) (*yandex.Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[deviceID]
	if !ok {
		return nil, errors.New("not found")
	}
	return &d, nil
}

func (s *stubAPI) Act(ctx context.Context, req yandex.ActionRequest) (*yandex.ActionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.actions = append(s.actions, req)
	if s.actErr != nil {
		return nil, s.actErr
	}
	return &yandex.ActionResponse{}, nil
}

func (s *stubAPI) actionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.actions)
}

func newTestSupervisor(t *testing.T, cfg config.Config) (*Supervisor, *Syncer, *stubAPI) {
	t.Helper()

	api := &stubAPI{}
	registry, err := LoadRegistry(filepath.Join(t.TempDir(), "aids.json"))
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	syncer := NewSyncer(api, cfg, testLogger())
	health := NewHealth("Health", nil, testLogger())
	sup := NewSupervisor(cfg, api, registry, syncer, health, nil, testLogger())
	return sup, syncer, api
}

func lampDevice(t *testing.T, id string) yandex.Device {
	t.Helper()
	return device(t, `{"id":"`+id+`","name":"Лампа","type":"devices.types.light","capabilities":[`+onOffCap+`]}`)
}

func socketDevice(t *testing.T, id string) yandex.Device {
	t.Helper()
	return device(t, `{"id":"`+id+`","name":"Розетка","type":"devices.types.socket","capabilities":[`+onOffCap+`]}`)
}

// rebuildRequested reports whether a rebuild is queued, without consuming it.
func rebuildRequested(s *Supervisor) bool {
	select {
	case specs := <-s.rebuild:
		s.rebuild <- specs
		return true
	default:
		return false
	}
}

func TestTopologyChangeNeedsConfirmations(t *testing.T) {
	cfg := config.Defaults()
	cfg.TopologyConfirmations = 3
	sup, _, _ := newTestSupervisor(t, cfg)

	initial := []yandex.Device{lampDevice(t, "lamp")}
	sup.current = TopologyHash(BuildSpecs(initial, cfg))

	changed := []yandex.Device{lampDevice(t, "lamp"), socketDevice(t, "socket")}

	sup.observeDevices(changed)
	if rebuildRequested(sup) {
		t.Fatal("rebuild requested after 1 of 3 confirmations")
	}
	sup.observeDevices(changed)
	if rebuildRequested(sup) {
		t.Fatal("rebuild requested after 2 of 3 confirmations")
	}
	sup.observeDevices(changed)
	if !rebuildRequested(sup) {
		t.Fatal("no rebuild after 3 of 3 confirmations")
	}
}

// TestTransientTopologyChangeIsIgnored is the regression test for the failure
// this design exists to prevent: a device briefly missing from one otherwise
// valid response must not tear down and renumber the accessory set.
func TestTransientTopologyChangeIsIgnored(t *testing.T) {
	cfg := config.Defaults()
	cfg.TopologyConfirmations = 3
	sup, _, _ := newTestSupervisor(t, cfg)

	full := []yandex.Device{lampDevice(t, "lamp"), socketDevice(t, "socket")}
	partial := []yandex.Device{lampDevice(t, "lamp")}
	sup.current = TopologyHash(BuildSpecs(full, cfg))

	// One poll comes back short, then things recover.
	sup.observeDevices(partial)
	sup.observeDevices(full)
	sup.observeDevices(partial)
	sup.observeDevices(full)

	if rebuildRequested(sup) {
		t.Fatal("a flapping device triggered a rebuild")
	}
}

func TestConfirmationCounterResetsOnRecovery(t *testing.T) {
	cfg := config.Defaults()
	cfg.TopologyConfirmations = 3
	sup, _, _ := newTestSupervisor(t, cfg)

	full := []yandex.Device{lampDevice(t, "lamp"), socketDevice(t, "socket")}
	partial := []yandex.Device{lampDevice(t, "lamp")}
	sup.current = TopologyHash(BuildSpecs(full, cfg))

	// Two polls short of the threshold, then a recovery, then two more.
	// Without a reset these four would add up to a spurious rebuild.
	sup.observeDevices(partial)
	sup.observeDevices(partial)
	sup.observeDevices(full)
	sup.observeDevices(partial)
	sup.observeDevices(partial)

	if rebuildRequested(sup) {
		t.Fatal("confirmation counter was not reset by the recovery")
	}
}

func TestSustainedRemovalIsEventuallyApplied(t *testing.T) {
	cfg := config.Defaults()
	cfg.TopologyConfirmations = 3
	sup, _, _ := newTestSupervisor(t, cfg)

	full := []yandex.Device{lampDevice(t, "lamp"), socketDevice(t, "socket")}
	partial := []yandex.Device{lampDevice(t, "lamp")}
	sup.current = TopologyHash(BuildSpecs(full, cfg))

	for range 3 {
		sup.observeDevices(partial)
	}

	select {
	case specs := <-sup.rebuild:
		if len(specs) != 1 || specs[0].DeviceID != "lamp" {
			t.Errorf("rebuild specs = %+v, want just the lamp", specs)
		}
	default:
		t.Fatal("a genuinely removed device was never applied")
	}
}

func TestRebuildQueueKeepsOnlyTheNewestSet(t *testing.T) {
	cfg := config.Defaults()
	cfg.TopologyConfirmations = 1
	sup, _, _ := newTestSupervisor(t, cfg)
	sup.current = "initial"

	one := []yandex.Device{lampDevice(t, "lamp")}
	two := []yandex.Device{lampDevice(t, "lamp"), socketDevice(t, "socket")}

	sup.observeDevices(one)
	sup.observeDevices(two)

	specs := <-sup.rebuild
	if len(specs) != 2 {
		t.Errorf("queued specs = %d devices, want the newest set of 2", len(specs))
	}
	if rebuildRequested(sup) {
		t.Error("more than one rebuild was queued")
	}
}

func TestUnchangedTopologyNeverRebuilds(t *testing.T) {
	cfg := config.Defaults()
	cfg.TopologyConfirmations = 1
	sup, _, _ := newTestSupervisor(t, cfg)

	devices := []yandex.Device{lampDevice(t, "lamp")}
	sup.current = TopologyHash(BuildSpecs(devices, cfg))

	for range 10 {
		sup.observeDevices(devices)
	}
	if rebuildRequested(sup) {
		t.Fatal("a steady topology triggered a rebuild")
	}
}

// TestFailedPollNeverReachesTheSupervisor is the other half of the guarantee:
// the topology watcher is fed from successful polls only, so an outage cannot
// be read as "every device was removed".
func TestFailedPollNeverReachesTheSupervisor(t *testing.T) {
	cfg := config.Defaults()
	cfg.PollInterval = config.Duration(10 * time.Millisecond)
	cfg.TopologyConfirmations = 1

	api := &stubAPI{responses: []stubResponse{{err: errors.New("network is unreachable")}}}
	syncer := NewSyncer(api, cfg, testLogger())

	var observed int
	var mu sync.Mutex
	syncer.OnDevices(func([]yandex.Device) {
		mu.Lock()
		observed++
		mu.Unlock()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	syncer.Run(ctx)

	mu.Lock()
	defer mu.Unlock()
	if observed != 0 {
		t.Errorf("the topology watcher saw %d failed polls, want 0", observed)
	}
	if syncer.Reachable() {
		t.Error("Reachable = true after every poll failed")
	}
	if syncer.Failures() == 0 {
		t.Error("Failures = 0 after failed polls")
	}
}

func TestSyncerMarksAccessoriesUnreachableAfterThreshold(t *testing.T) {
	cfg := config.Defaults()
	cfg.PollInterval = config.Duration(10 * time.Millisecond)
	cfg.UnhealthyAfter = 2

	api := &stubAPI{responses: []stubResponse{{err: errors.New("boom")}}}
	syncer := NewSyncer(api, cfg, testLogger())

	spec, _ := BuildSpec(lampDevice(t, "lamp"), config.DeviceOverride{})
	acc := BuildAccessory(spec, 3, syncer, testLogger())
	syncer.SetAccessories([]*Accessory{acc})

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	syncer.Run(ctx)

	if !acc.Offline() {
		t.Error("accessory was not marked unreachable after sustained failures")
	}
}

func TestSyncerRecoversAfterOutage(t *testing.T) {
	cfg := config.Defaults()
	cfg.PollInterval = config.Duration(10 * time.Millisecond)
	cfg.UnhealthyAfter = 1

	lamp := lampDevice(t, "lamp")
	api := &stubAPI{responses: []stubResponse{
		{err: errors.New("boom")},
		{err: errors.New("boom")},
		{devices: []yandex.Device{lamp}},
	}}
	syncer := NewSyncer(api, cfg, testLogger())

	spec, _ := BuildSpec(lamp, config.DeviceOverride{})
	acc := BuildAccessory(spec, 3, syncer, testLogger())
	syncer.SetAccessories([]*Accessory{acc})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	syncer.Run(ctx)

	if acc.Offline() {
		t.Error("accessory stayed unreachable after Yandex recovered")
	}
	if !syncer.Reachable() {
		t.Error("Reachable = false after recovery")
	}
	if syncer.Failures() != 0 {
		t.Errorf("Failures = %d after recovery, want 0", syncer.Failures())
	}
}

func TestSyncerAppliesStateToAccessories(t *testing.T) {
	cfg := config.Defaults()
	cfg.PollInterval = config.Duration(10 * time.Millisecond)

	lamp := device(t, `{"id":"lamp","name":"Лампа","type":"devices.types.light","capabilities":[
		{"type":"devices.capabilities.on_off","retrievable":true,"state":{"instance":"on","value":true}},
		`+brightnessCap+`]}`)

	api := &stubAPI{responses: []stubResponse{{devices: []yandex.Device{lamp}}}}
	syncer := NewSyncer(api, cfg, testLogger())

	spec, _ := BuildSpec(lamp, config.DeviceOverride{})
	acc := BuildAccessory(spec, 3, syncer, testLogger())
	syncer.SetAccessories([]*Accessory{acc})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	syncer.Run(ctx)

	if !acc.on.Value() {
		t.Error("On = false, want true from the polled state")
	}
	if got := acc.brightness.Value(); got != 50 {
		t.Errorf("Brightness = %d, want 50", got)
	}
}

func TestHomeKitWriteReachesYandex(t *testing.T) {
	cfg := config.Defaults()
	api := &stubAPI{}
	syncer := NewSyncer(api, cfg, testLogger())

	spec, _ := BuildSpec(lampDevice(t, "lamp"), config.DeviceOverride{})
	acc := BuildAccessory(spec, 3, syncer, testLogger())
	syncer.SetAccessories([]*Accessory{acc})

	if err := acc.setOn(true); err != nil {
		t.Fatalf("setOn: %v", err)
	}
	if api.actionCount() != 1 {
		t.Fatalf("actions sent = %d, want 1", api.actionCount())
	}

	req := api.actions[0]
	if req.Devices[0].ID != "lamp" {
		t.Errorf("device id = %q, want lamp", req.Devices[0].ID)
	}
	action := req.Devices[0].Actions[0]
	if action.Type != yandex.CapabilityOnOff || action.State.Value != true {
		t.Errorf("action = %+v, want on_off/true", action)
	}
}

func TestFailedWriteIsReportedToHomeKit(t *testing.T) {
	cfg := config.Defaults()
	api := &stubAPI{actErr: errors.New("yandex is down")}
	syncer := NewSyncer(api, cfg, testLogger())

	spec, _ := BuildSpec(lampDevice(t, "lamp"), config.DeviceOverride{})
	acc := BuildAccessory(spec, 3, syncer, testLogger())
	syncer.SetAccessories([]*Accessory{acc})

	// hap turns a non-nil error into -70402, which the Home app shows as
	// "Not Responding" rather than silently accepting the tap.
	if err := acc.setOn(true); err == nil {
		t.Fatal("setOn succeeded despite the API failing")
	}
	if syncer.Reachable() {
		t.Error("Reachable = true after a transport-level write failure")
	}
}

// TestUnreachableDeviceDoesNotMarkYandexDown separates the two failure modes:
// one dead lamp is not an outage.
func TestUnreachableDeviceDoesNotMarkYandexDown(t *testing.T) {
	cfg := config.Defaults()
	api := &stubAPI{actErr: &yandex.DeviceError{
		DeviceID: "lamp",
		Code:     yandex.ErrCodeDeviceUnreachable,
	}}
	syncer := NewSyncer(api, cfg, testLogger())

	spec, _ := BuildSpec(lampDevice(t, "lamp"), config.DeviceOverride{})
	acc := BuildAccessory(spec, 3, syncer, testLogger())
	syncer.SetAccessories([]*Accessory{acc})

	if err := acc.setOn(true); err == nil {
		t.Fatal("setOn succeeded despite DEVICE_UNREACHABLE")
	}
	if !syncer.Reachable() {
		t.Error("one unreachable device marked the whole Yandex link as down")
	}
}

func TestBuildAssignsStableAIDsAcrossRebuilds(t *testing.T) {
	cfg := config.Defaults()
	sup, _, _ := newTestSupervisor(t, cfg)

	first, err := sup.build(BuildSpecs([]yandex.Device{lampDevice(t, "lamp"), socketDevice(t, "socket")}, cfg))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	before := map[string]uint64{}
	for _, a := range first {
		before[a.Spec.DeviceID] = a.A.Id
	}

	// Rebuild with a new device in the middle of the set.
	second, err := sup.build(BuildSpecs([]yandex.Device{
		lampDevice(t, "lamp"),
		lampDevice(t, "middle"),
		socketDevice(t, "socket"),
	}, cfg))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, a := range second {
		if want, ok := before[a.Spec.DeviceID]; ok && a.A.Id != want {
			t.Errorf("device %q: aid changed from %d to %d across a rebuild",
				a.Spec.DeviceID, want, a.A.Id)
		}
	}
}

func TestBuildNeverUsesReservedAIDs(t *testing.T) {
	cfg := config.Defaults()
	sup, _, _ := newTestSupervisor(t, cfg)

	accessories, err := sup.build(BuildSpecs([]yandex.Device{lampDevice(t, "lamp")}, cfg))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, a := range accessories {
		if a.A.Id == BridgeAID || a.A.Id == HealthAID {
			t.Errorf("device %q took reserved aid %d", a.Spec.DeviceID, a.A.Id)
		}
	}
}
