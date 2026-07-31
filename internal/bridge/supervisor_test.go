package bridge

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"

	"github.com/brutella/hap/characteristic"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"yandex-bridge/internal/config"
	"yandex-bridge/internal/yandex"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testOpts keeps the coalescing window short so tests do not sleep.
var testOpts = BuildOptions{CoalesceDelay: 5 * time.Millisecond, SettleWindow: 10 * time.Second}

// stubAPI is a scripted Yandex API.
type stubAPI struct {
	mu sync.Mutex
	// responses are returned in order; the last one repeats.
	responses []stubResponse
	calls     int
	devices   map[string]yandex.Device
	actions   []yandex.ActionRequest
	actErr    error
	// actDelay simulates how long Yandex takes to accept a write.
	actDelay time.Duration
	// deviceDelay simulates a slow single-device read.
	deviceDelay time.Duration

	userInfoCalls atomic.Int32
	deviceCalls   atomic.Int32
}

type stubResponse struct {
	devices []yandex.Device
	err     error
}

func (s *stubAPI) UserInfo(ctx context.Context) (*yandex.UserInfo, error) {
	s.userInfoCalls.Add(1)

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
	s.deviceCalls.Add(1)

	s.mu.Lock()
	delay := s.deviceDelay
	d, ok := s.devices[deviceID]
	s.mu.Unlock()

	if delay > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	if !ok {
		return nil, errors.New("not found")
	}
	return &d, nil
}

func (s *stubAPI) Act(ctx context.Context, req yandex.ActionRequest) (*yandex.ActionResponse, error) {
	s.mu.Lock()
	delay := s.actDelay
	s.actions = append(s.actions, req)
	err := s.actErr
	s.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}
	if err != nil {
		return nil, err
	}
	return &yandex.ActionResponse{}, nil
}

func (s *stubAPI) actionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.actions)
}

func (s *stubAPI) allActions() []yandex.ActionRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]yandex.ActionRequest(nil), s.actions...)
}

func newTestSupervisor(t *testing.T, cfg config.Config) (*Supervisor, *Syncer, *stubAPI) {
	t.Helper()

	api := &stubAPI{}
	registry, err := LoadRegistry(filepath.Join(t.TempDir(), "aids.json"))
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	syncer := NewSyncer(api, cfg, testLogger())
	health := NewHealth("Health", nil, true, testLogger())
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

func colorLamp(t *testing.T, id string) yandex.Device {
	t.Helper()
	return device(t, `{"id":"`+id+`","name":"Лента","type":"devices.types.light","capabilities":[`+
		onOffCap+`,`+brightnessCap+`,`+hsvCap+`]}`)
}

// specsOf builds specs, dropping the report.
func specsOf(info *yandex.UserInfo, cfg config.Config) []Spec {
	specs, _ := BuildSpecs(info, cfg)
	return specs
}

func topologyOf(cfg config.Config, devices ...yandex.Device) string {
	return TopologyHash(specsOf(home(devices...), cfg))
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

// newAccessory builds an accessory wired to a syncer over a stub API.
func newAccessory(t *testing.T, dev yandex.Device, cfg config.Config) (*Accessory, *Syncer, *stubAPI) {
	t.Helper()

	api := &stubAPI{}
	syncer := NewSyncer(api, cfg, testLogger())
	spec, _, ok := BuildSpec(dev, cfg.Override(dev.ID), cfg.ExposeToggles)
	if !ok {
		t.Fatalf("device %q was not exported", dev.ID)
	}
	acc := BuildAccessory(spec, 3, syncer, testOpts, testLogger())
	syncer.SetAccessories([]*Accessory{acc})
	return acc, syncer, api
}

// homeKitWrite drives a characteristic exactly as hap does when a paired
// controller sends a PUT: the setter runs first and the stored value is
// committed afterwards.
func homeKitWrite(t *testing.T, c *characteristic.C, value any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/characteristics", nil)
	if _, code := c.SetValueRequest(value, req); code != 0 {
		t.Fatalf("SetValueRequest(%v) returned code %d", value, code)
	}
}

// settle waits for the coalescing window plus any simulated API latency.
func settle(d ...time.Duration) {
	wait := 80 * time.Millisecond
	for _, extra := range d {
		wait += extra
	}
	time.Sleep(wait)
}

// --- regression: colour changes must be one action ---

// TestColourChangeIsASingleAction is the regression for the bug where dragging
// the colour picker produced two Yandex calls per change, the first carrying a
// stale component. HomeKit sends hue and saturation in one PUT, and hap hands
// them over one characteristic at a time, updating the stored value only after
// each setter returns.
func TestColourChangeIsASingleAction(t *testing.T) {
	cfg := config.Defaults()
	acc, _, api := newAccessory(t, colorLamp(t, "strip"), cfg)

	// The two characteristics of one PUT, in the order hap processes them.
	homeKitWrite(t, acc.hue.C, 155.0)
	homeKitWrite(t, acc.saturation.C, 89.0)
	settle()

	actions := api.allActions()
	if len(actions) != 1 {
		t.Fatalf("Yandex calls = %d, want 1 for a single colour change", len(actions))
	}
	if got := len(actions[0].Devices[0].Actions); got != 1 {
		t.Fatalf("actions in the request = %d, want 1", got)
	}

	state := actions[0].Devices[0].Actions[0].State
	if state.Instance != string(yandex.ColorHSV) {
		t.Fatalf("instance = %q, want hsv", state.Instance)
	}
	hsv, ok := state.Value.(map[string]float64)
	if !ok {
		t.Fatalf("value is %T, want map[string]float64", state.Value)
	}
	// The whole point: both components carry the values HomeKit asked for.
	if hsv["h"] != 155 {
		t.Errorf("h = %v, want 155", hsv["h"])
	}
	if hsv["s"] != 89 {
		t.Errorf("s = %v, want 89 (a stale 100 is the bug this guards)", hsv["s"])
	}
}

func TestOnOffAndColourInOneRequest(t *testing.T) {
	cfg := config.Defaults()
	acc, _, api := newAccessory(t, colorLamp(t, "strip"), cfg)

	homeKitWrite(t, acc.on.C, true)
	homeKitWrite(t, acc.hue.C, 200.0)
	settle()

	actions := api.allActions()
	if len(actions) != 1 {
		t.Fatalf("Yandex calls = %d, want 1", len(actions))
	}
	// devices/actions accepts several actions per device, so switching on and
	// recolouring is one round trip rather than two.
	if got := len(actions[0].Devices[0].Actions); got != 2 {
		t.Errorf("actions in the request = %d, want on_off and colour together", got)
	}
}

// TestAtMostOneWriteInFlight covers a drag: HomeKit produces a stream of
// updates and Yandex takes the better part of a second per call, so without
// coalescing the bridge would queue up seconds of stale writes.
func TestAtMostOneWriteInFlight(t *testing.T) {
	cfg := config.Defaults()
	acc, _, api := newAccessory(t, colorLamp(t, "strip"), cfg)
	api.mu.Lock()
	api.actDelay = 30 * time.Millisecond
	api.mu.Unlock()

	for i := range 20 {
		homeKitWrite(t, acc.hue.C, float64(100+i))
		time.Sleep(2 * time.Millisecond)
	}
	settle(200 * time.Millisecond)

	calls := api.actionCount()
	if calls == 0 {
		t.Fatal("no writes reached Yandex")
	}
	if calls >= 20 {
		t.Errorf("Yandex calls = %d for 20 updates, want them coalesced", calls)
	}

	// Whatever was collapsed, the final position must arrive.
	actions := api.allActions()
	last := actions[len(actions)-1].Devices[0].Actions[0].State.Value.(map[string]float64)
	if last["h"] != 119 {
		t.Errorf("final hue = %v, want the last value the user selected (119)", last["h"])
	}
}

// --- regression: polled state must not fight the user ---

// TestStaleStateDoesNotOverwriteRecentWrite is the regression for the colour
// picker jumping back under the user's finger. Yandex reaches devices through
// the vendor's cloud, so right after a write it still reports the old value —
// and hap broadcasts anything the bridge writes to every connected controller,
// including the phone mid-drag.
func TestStaleStateDoesNotOverwriteRecentWrite(t *testing.T) {
	cfg := config.Defaults()
	acc, _, _ := newAccessory(t, colorLamp(t, "strip"), cfg)

	homeKitWrite(t, acc.hue.C, 155.0)
	homeKitWrite(t, acc.saturation.C, 89.0)
	settle()

	// A poll that has not caught up yet, still reporting the previous colour.
	acc.Apply(device(t, `{"id":"strip","name":"Лента","type":"devices.types.light","capabilities":[`+
		onOffCap+`,`+brightnessCap+`,
		{"type":"devices.capabilities.color_setting","retrievable":true,
		 "parameters":{"color_model":"hsv"},
		 "state":{"instance":"hsv","value":{"h":120,"s":50,"v":80}}}]}`))

	if got := acc.hue.Value(); got != 155 {
		t.Errorf("hue = %v after a stale poll, want the value the user chose (155)", got)
	}
	if got := acc.saturation.Value(); got != 89 {
		t.Errorf("saturation = %v after a stale poll, want 89", got)
	}
}

func TestConvergedStateClearsTheExpectation(t *testing.T) {
	cfg := config.Defaults()
	acc, _, _ := newAccessory(t, colorLamp(t, "strip"), cfg)

	homeKitWrite(t, acc.hue.C, 155.0)
	homeKitWrite(t, acc.saturation.C, 89.0)
	settle()

	// Yandex catches up and reports what was asked for.
	converged := device(t, `{"id":"strip","name":"Лента","type":"devices.types.light","capabilities":[`+
		onOffCap+`,`+brightnessCap+`,
		{"type":"devices.capabilities.color_setting","retrievable":true,
		 "parameters":{"color_model":"hsv"},
		 "state":{"instance":"hsv","value":{"h":155,"s":89,"v":50}}}]}`)
	acc.Apply(converged)

	// With the expectation cleared, a genuine change made in the Yandex app
	// must come through immediately rather than waiting out the window.
	acc.Apply(device(t, `{"id":"strip","name":"Лента","type":"devices.types.light","capabilities":[`+
		onOffCap+`,`+brightnessCap+`,
		{"type":"devices.capabilities.color_setting","retrievable":true,
		 "parameters":{"color_model":"hsv"},
		 "state":{"instance":"hsv","value":{"h":10,"s":20,"v":50}}}]}`))

	if got := acc.hue.Value(); got != 10 {
		t.Errorf("hue = %v, want 10 once the write was confirmed", got)
	}
}

func TestExpectationExpires(t *testing.T) {
	cfg := config.Defaults()
	acc, _, _ := newAccessory(t, lampDevice(t, "lamp"), cfg)
	// A device that never confirms must not be protected forever.
	acc.settleWindow = 20 * time.Millisecond

	homeKitWrite(t, acc.on.C, false)
	settle()
	time.Sleep(40 * time.Millisecond)

	acc.Apply(lampDevice(t, "lamp")) // reports on
	if !acc.on.Value() {
		t.Error("state stayed suppressed after the settle window expired")
	}
}

func TestFailedWriteClearsExpectations(t *testing.T) {
	cfg := config.Defaults()
	acc, _, api := newAccessory(t, lampDevice(t, "lamp"), cfg)
	api.mu.Lock()
	api.actErr = errors.New("yandex is down")
	api.mu.Unlock()

	homeKitWrite(t, acc.on.C, false)
	settle()

	// The write failed, so the optimistic value must not be defended: the next
	// poll should put reality straight back on screen.
	acc.Apply(lampDevice(t, "lamp")) // reports on
	if !acc.on.Value() {
		t.Error("a failed write kept protecting its value from the real state")
	}
}

func TestSensorReadingsAreNeverSuppressed(t *testing.T) {
	cfg := config.Defaults()
	dev := device(t, `{"id":"socket","name":"Розетка","type":"devices.types.socket",
		"capabilities":[`+onOffCap+`],"properties":[`+temperatureProp+`]}`)
	acc, _, _ := newAccessory(t, dev, cfg)

	homeKitWrite(t, acc.on.C, false)
	settle()

	acc.Apply(dev)
	// Nothing writes a sensor from HomeKit, so there is no echo to guard
	// against and readings must always come through.
	if got := acc.temperature.Value(); got != 21.5 {
		t.Errorf("temperature = %v, want 21.5", got)
	}
}

// --- regression: confirmations must not block the poll loop ---

// TestConfirmationsDoNotBlockPolling is the regression for the log storm: a
// drag queued dozens of confirmation windows, each ran on the poll goroutine
// for its full duration, and the regular poll stopped happening for a minute.
func TestConfirmationsDoNotBlockPolling(t *testing.T) {
	cfg := config.Defaults()
	cfg.PollInterval = config.Duration(20 * time.Millisecond)
	cfg.ConfirmWindow = config.Duration(500 * time.Millisecond)
	cfg.ConfirmInterval = config.Duration(10 * time.Millisecond)

	lamp := lampDevice(t, "lamp")
	api := &stubAPI{
		responses:   []stubResponse{{devices: []yandex.Device{lamp}}},
		devices:     map[string]yandex.Device{"lamp": lamp},
		deviceDelay: 200 * time.Millisecond,
	}
	syncer := NewSyncer(api, cfg, testLogger())

	spec, _, _ := BuildSpec(lamp, config.DeviceOverride{}, true)
	acc := BuildAccessory(spec, 3, syncer, testOpts, testLogger())
	syncer.SetAccessories([]*Accessory{acc})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	go func() {
		// Queue a burst of confirmations, as a drag would.
		for range 10 {
			_ = syncer.Apply(context.Background(), "lamp", []yandex.Action{{
				Type:  yandex.CapabilityOnOff,
				State: yandex.ActionState{Instance: "on", Value: true},
			}})
		}
	}()
	syncer.Run(ctx)

	// ~300ms at a 20ms interval is a dozen or so polls. Before the fix this
	// would have been 1: the loop was stuck inside a confirmation window.
	if got := api.userInfoCalls.Load(); got < 5 {
		t.Errorf("polls = %d in 300ms, want the loop to keep running alongside confirmations", got)
	}
}

func TestConfirmationsAreDeduplicated(t *testing.T) {
	cfg := config.Defaults()
	cfg.PollInterval = config.Duration(time.Hour) // only the initial poll
	cfg.ConfirmWindow = config.Duration(200 * time.Millisecond)
	cfg.ConfirmInterval = config.Duration(50 * time.Millisecond)

	lamp := lampDevice(t, "lamp")
	api := &stubAPI{
		responses: []stubResponse{{devices: []yandex.Device{lamp}}},
		devices:   map[string]yandex.Device{"lamp": lamp},
	}
	syncer := NewSyncer(api, cfg, testLogger())

	spec, _, _ := BuildSpec(lamp, config.DeviceOverride{}, true)
	acc := BuildAccessory(spec, 3, syncer, testOpts, testLogger())
	syncer.SetAccessories([]*Accessory{acc})

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	for range 10 {
		_ = syncer.Apply(context.Background(), "lamp", []yandex.Action{{
			Type:  yandex.CapabilityOnOff,
			State: yandex.ActionState{Instance: "on", Value: true},
		}})
	}
	syncer.Run(ctx)

	// One window polls every 50ms for 200ms: a handful of reads. Ten windows
	// running in parallel would be several times that.
	if got := api.deviceCalls.Load(); got > 8 {
		t.Errorf("device reads = %d, want a single confirmation window", got)
	}
}

// --- topology confirmation ---

func TestTopologyChangeNeedsConfirmations(t *testing.T) {
	cfg := config.Defaults()
	cfg.TopologyConfirmations = 3
	sup, _, _ := newTestSupervisor(t, cfg)

	lamp, socket := lampDevice(t, "lamp"), socketDevice(t, "socket")
	sup.current = topologyOf(cfg, lamp)
	changed := home(lamp, socket)

	sup.observePoll(changed)
	if rebuildRequested(sup) {
		t.Fatal("rebuild requested after 1 of 3 confirmations")
	}
	sup.observePoll(changed)
	if rebuildRequested(sup) {
		t.Fatal("rebuild requested after 2 of 3 confirmations")
	}
	sup.observePoll(changed)
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

	lamp, socket := lampDevice(t, "lamp"), socketDevice(t, "socket")
	full, partial := home(lamp, socket), home(lamp)
	sup.current = topologyOf(cfg, lamp, socket)

	// One poll comes back short, then things recover.
	sup.observePoll(partial)
	sup.observePoll(full)
	sup.observePoll(partial)
	sup.observePoll(full)

	if rebuildRequested(sup) {
		t.Fatal("a flapping device triggered a rebuild")
	}
}

func TestConfirmationCounterResetsOnRecovery(t *testing.T) {
	cfg := config.Defaults()
	cfg.TopologyConfirmations = 3
	sup, _, _ := newTestSupervisor(t, cfg)

	lamp, socket := lampDevice(t, "lamp"), socketDevice(t, "socket")
	full, partial := home(lamp, socket), home(lamp)
	sup.current = topologyOf(cfg, lamp, socket)

	// Two polls short of the threshold, then a recovery, then two more.
	// Without a reset these four would add up to a spurious rebuild.
	sup.observePoll(partial)
	sup.observePoll(partial)
	sup.observePoll(full)
	sup.observePoll(partial)
	sup.observePoll(partial)

	if rebuildRequested(sup) {
		t.Fatal("confirmation counter was not reset by the recovery")
	}
}

func TestSustainedRemovalIsEventuallyApplied(t *testing.T) {
	cfg := config.Defaults()
	cfg.TopologyConfirmations = 3
	sup, _, _ := newTestSupervisor(t, cfg)

	lamp, socket := lampDevice(t, "lamp"), socketDevice(t, "socket")
	sup.current = topologyOf(cfg, lamp, socket)

	for range 3 {
		sup.observePoll(home(lamp))
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

	lamp, socket := lampDevice(t, "lamp"), socketDevice(t, "socket")
	sup.observePoll(home(lamp))
	sup.observePoll(home(lamp, socket))

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

	lamp := lampDevice(t, "lamp")
	sup.current = topologyOf(cfg, lamp)

	for range 10 {
		sup.observePoll(home(lamp))
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

	var observed atomic.Int32
	syncer.OnPoll(func(*yandex.UserInfo) { observed.Add(1) })

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	syncer.Run(ctx)

	if got := observed.Load(); got != 0 {
		t.Errorf("the topology watcher saw %d failed polls, want 0", got)
	}
	if syncer.Reachable() {
		t.Error("Reachable = true after every poll failed")
	}
	if syncer.Failures() == 0 {
		t.Error("Failures = 0 after failed polls")
	}
}

func TestInventoryIsAvailableAfterAPoll(t *testing.T) {
	cfg := config.Defaults()
	sup, _, _ := newTestSupervisor(t, cfg)

	if len(sup.Inventory()) != 0 {
		t.Error("inventory is populated before any poll")
	}
	sup.observePoll(home(lampDevice(t, "lamp")))

	reports := sup.Inventory()
	if len(reports) != 1 || reports[0].DeviceID != "lamp" {
		t.Errorf("inventory = %+v, want the polled device", reports)
	}
}

// --- accessory state and control ---

func TestSyncerMarksAccessoriesUnreachableAfterThreshold(t *testing.T) {
	cfg := config.Defaults()
	cfg.PollInterval = config.Duration(10 * time.Millisecond)
	cfg.UnhealthyAfter = 2

	api := &stubAPI{responses: []stubResponse{{err: errors.New("boom")}}}
	syncer := NewSyncer(api, cfg, testLogger())

	spec, _, _ := BuildSpec(lampDevice(t, "lamp"), config.DeviceOverride{}, true)
	acc := BuildAccessory(spec, 3, syncer, testOpts, testLogger())
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

	spec, _, _ := BuildSpec(lamp, config.DeviceOverride{}, true)
	acc := BuildAccessory(spec, 3, syncer, testOpts, testLogger())
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

	spec, _, _ := BuildSpec(lamp, config.DeviceOverride{}, true)
	acc := BuildAccessory(spec, 3, syncer, testOpts, testLogger())
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
	acc, _, api := newAccessory(t, lampDevice(t, "lamp"), cfg)

	homeKitWrite(t, acc.on.C, true)
	settle()

	if api.actionCount() != 1 {
		t.Fatalf("actions sent = %d, want 1", api.actionCount())
	}
	req := api.allActions()[0]
	if req.Devices[0].ID != "lamp" {
		t.Errorf("device id = %q, want lamp", req.Devices[0].ID)
	}
	action := req.Devices[0].Actions[0]
	if action.Type != yandex.CapabilityOnOff || action.State.Value != true {
		t.Errorf("action = %+v, want on_off/true", action)
	}
}

func TestFailedWriteMarksYandexUnreachable(t *testing.T) {
	cfg := config.Defaults()
	acc, syncer, api := newAccessory(t, lampDevice(t, "lamp"), cfg)
	api.mu.Lock()
	api.actErr = errors.New("yandex is down")
	api.mu.Unlock()

	homeKitWrite(t, acc.on.C, true)
	settle()

	if syncer.Reachable() {
		t.Error("Reachable = true after a transport-level write failure")
	}
}

// TestUnreachableDeviceDoesNotMarkYandexDown separates the two failure modes:
// one dead lamp is not an outage.
func TestUnreachableDeviceDoesNotMarkYandexDown(t *testing.T) {
	cfg := config.Defaults()
	acc, syncer, api := newAccessory(t, lampDevice(t, "lamp"), cfg)
	api.mu.Lock()
	api.actErr = &yandex.DeviceError{DeviceID: "lamp", Code: yandex.ErrCodeDeviceUnreachable}
	api.mu.Unlock()

	homeKitWrite(t, acc.on.C, true)
	settle()

	if !syncer.Reachable() {
		t.Error("one unreachable device marked the whole Yandex link as down")
	}
}

// --- kettle ---

func TestKettleAccessoryHasOneCombinedService(t *testing.T) {
	cfg := config.Defaults()
	acc, _, _ := newAccessory(t, device(t, kettleJSON), cfg)

	if acc.targetTemp == nil || acc.currentTemp == nil || acc.targetState == nil {
		t.Fatal("thermostat characteristics are missing")
	}
	// A separate TemperatureSensor would put the kettle back to two tiles.
	if acc.temperature != nil {
		t.Error("a standalone TemperatureSensor was added alongside the thermostat")
	}
	// HAP defines TargetTemperature as 10-38 °C; the kettle's own range has to
	// win or the dial cannot reach boiling.
	if got := acc.targetTemp.MinValue(); got != 40 {
		t.Errorf("TargetTemperature min = %v, want 40", got)
	}
	if got := acc.targetTemp.MaxValue(); got != 100 {
		t.Errorf("TargetTemperature max = %v, want 100", got)
	}
	// Only Off and Heat: the kettle cannot cool.
	if got := acc.targetState.ValidVals; len(got) != 2 {
		t.Errorf("TargetHeatingCoolingState valid values = %v, want [Off Heat]", got)
	}
}

func TestKettleStateMapsBothWays(t *testing.T) {
	cfg := config.Defaults()
	acc, _, api := newAccessory(t, device(t, kettleJSON), cfg)

	acc.Apply(device(t, kettleJSON))
	if got := acc.targetState.Value(); got != heatingStateHeat {
		t.Errorf("TargetHeatingCoolingState = %d, want Heat for a running kettle", got)
	}
	if got := acc.currentTemp.Value(); got != 21.5 {
		t.Errorf("CurrentTemperature = %v, want 21.5", got)
	}
	if got := acc.targetTemp.Value(); got != 80 {
		t.Errorf("TargetTemperature = %v, want 80", got)
	}

	homeKitWrite(t, acc.targetTemp.C, 60.0)
	settle()

	actions := api.allActions()
	if len(actions) != 1 {
		t.Fatalf("Yandex calls = %d, want 1", len(actions))
	}
	state := actions[0].Devices[0].Actions[0].State
	if state.Instance != string(yandex.RangeTemperature) || state.Value != 60.0 {
		t.Errorf("action = %+v, want range:temperature = 60", state)
	}
}

func TestKettleOffMapsToOnOff(t *testing.T) {
	cfg := config.Defaults()
	acc, _, api := newAccessory(t, device(t, kettleJSON), cfg)

	// The kettle is running, so start from Heat; hap skips the setter entirely
	// when a write does not change the stored value.
	acc.Apply(device(t, kettleJSON))
	if acc.targetState.Value() != heatingStateHeat {
		t.Fatalf("setup: TargetHeatingCoolingState = %d, want Heat", acc.targetState.Value())
	}

	homeKitWrite(t, acc.targetState.C, heatingStateOff)
	settle()

	actions := api.allActions()
	if len(actions) != 1 {
		t.Fatalf("Yandex calls = %d, want 1", len(actions))
	}
	action := actions[0].Devices[0].Actions[0]
	if action.Type != yandex.CapabilityOnOff || action.State.Value != false {
		t.Errorf("action = %+v, want on_off = false", action)
	}
}

func TestToggleSwitchesControlTheDevice(t *testing.T) {
	cfg := config.Defaults()
	dev := device(t, `{"id":"kettle","name":"Чайник","type":"devices.types.cooking.kettle",
		"capabilities":[`+onOffCap+`,`+targetTempCap+`,`+keepWarmToggle+`]}`)
	acc, _, api := newAccessory(t, dev, cfg)

	warm, ok := acc.toggles["keep_warm"]
	if !ok {
		t.Fatal("keep_warm switch was not created")
	}

	// Yandex reports it on, so the switch should follow.
	acc.Apply(dev)
	if !warm.Value() {
		t.Error("keep_warm switch did not follow the polled state")
	}

	homeKitWrite(t, warm.C, false)
	settle()

	actions := api.allActions()
	if len(actions) != 1 {
		t.Fatalf("Yandex calls = %d, want 1", len(actions))
	}
	action := actions[0].Devices[0].Actions[0]
	if action.Type != yandex.CapabilityToggle || action.State.Instance != "keep_warm" {
		t.Errorf("action = %+v, want toggle:keep_warm", action)
	}
}

// --- accessory ids ---

func TestBuildAssignsStableAIDsAcrossRebuilds(t *testing.T) {
	cfg := config.Defaults()
	sup, _, _ := newTestSupervisor(t, cfg)

	first, err := sup.build(specsOf(home(lampDevice(t, "lamp"), socketDevice(t, "socket")), cfg))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	before := map[string]uint64{}
	for _, a := range first {
		before[a.Spec.DeviceID] = a.A.Id
	}

	// Rebuild with a new device in the middle of the set.
	second, err := sup.build(specsOf(home(
		lampDevice(t, "lamp"),
		lampDevice(t, "middle"),
		socketDevice(t, "socket"),
	), cfg))
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

	accessories, err := sup.build(specsOf(home(lampDevice(t, "lamp")), cfg))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, a := range accessories {
		if a.A.Id == BridgeAID || a.A.Id == HealthAID {
			t.Errorf("device %q took reserved aid %d", a.Spec.DeviceID, a.A.Id)
		}
	}
}
