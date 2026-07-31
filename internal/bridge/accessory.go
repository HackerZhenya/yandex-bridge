package bridge

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/brutella/hap"
	"github.com/brutella/hap/accessory"
	"github.com/brutella/hap/characteristic"
	"github.com/brutella/hap/service"

	"yandex-bridge/internal/yandex"
)

// Manufacturer shown in the Home app's accessory details.
const manufacturer = "Yandex Smart Home"

// Write keys. A key names a group of characteristics that turn into one
// Yandex action, so that hue and saturation cannot be sent separately.
const (
	writeOnOff      = "on_off"
	writeBrightness = "brightness"
	writeColor      = "color"
	writeTarget     = "target_temp"
	writeTogglePfx  = "toggle:"
)

// HomeKit heating/cooling states used by the Thermostat service.
const (
	heatingStateOff  = 0
	heatingStateHeat = 1
)

// Controller applies HomeKit writes to Yandex and reports whether the bridge
// can currently reach it.
type Controller interface {
	// Apply sends actions to a device and blocks until they are accepted.
	Apply(ctx context.Context, deviceID string, actions []yandex.Action) error
	// Reachable reports whether the last interaction with Yandex succeeded.
	Reachable() bool
}

// expectation is a value written from HomeKit that Yandex has not confirmed
// yet. Until it does — or the deadline passes — polled state for that
// characteristic is ignored.
type expectation struct {
	value any
	until time.Time
}

// Accessory is one Yandex device exposed to HomeKit, together with the
// characteristics that mirror its state.
type Accessory struct {
	Spec Spec
	A    *accessory.A

	on         *characteristic.On
	brightness *characteristic.Brightness
	hue        *characteristic.Hue
	saturation *characteristic.Saturation
	colorTemp  *characteristic.ColorTemperature

	targetState *characteristic.TargetHeatingCoolingState
	currentHeat *characteristic.CurrentHeatingCoolingState
	targetTemp  *characteristic.TargetTemperature
	currentTemp *characteristic.CurrentTemperature

	temperature *characteristic.CurrentTemperature
	humidity    *characteristic.CurrentRelativeHumidity
	battery     *service.BatteryService
	toggles     map[string]*characteristic.On

	// offline mirrors the device's own state as reported by Yandex, which is
	// separate from whether the bridge can reach Yandex at all.
	offline atomic.Bool

	// writeMu guards the coalescing buffer.
	writeMu    sync.Mutex
	writeDirty map[string]struct{}
	writeSched bool

	// expectMu guards the echo-suppression map.
	expectMu sync.Mutex
	expect   map[*characteristic.C]expectation

	coalesceDelay time.Duration
	settleWindow  time.Duration

	ctrl   Controller
	logger *slog.Logger
}

// BuildOptions carries the tunables an accessory needs.
type BuildOptions struct {
	CoalesceDelay time.Duration
	SettleWindow  time.Duration
}

// BuildAccessory constructs the HomeKit accessory for a spec.
//
// Services are added in a fixed order and derived only from the spec — never
// from live values — because hap assigns instance ids by position. A layout
// that varied with the current state would renumber characteristics under a
// paired controller.
func BuildAccessory(spec Spec, aid uint64, ctrl Controller, opts BuildOptions, logger *slog.Logger) *Accessory {
	a := &Accessory{
		Spec:          spec,
		ctrl:          ctrl,
		logger:        logger,
		writeDirty:    make(map[string]struct{}),
		expect:        make(map[*characteristic.C]expectation),
		toggles:       make(map[string]*characteristic.On),
		coalesceDelay: opts.CoalesceDelay,
		settleWindow:  opts.SettleWindow,
	}
	if a.coalesceDelay <= 0 {
		a.coalesceDelay = 60 * time.Millisecond
	}

	a.A = accessory.New(accessory.Info{
		Name:         spec.Name,
		Manufacturer: manufacturer,
		Model:        spec.Model,
		SerialNumber: spec.DeviceID,
		Firmware:     Version,
	}, accessoryTypeFor(spec.Kind))
	a.A.Id = aid

	a.addPrimaryService()
	a.addSensorServices()
	a.addToggleServices()
	return a
}

func accessoryTypeFor(k Kind) byte {
	switch k {
	case KindLightbulb:
		return accessory.TypeLightbulb
	case KindOutlet:
		return accessory.TypeOutlet
	case KindSwitch:
		return accessory.TypeSwitch
	case KindFan:
		return accessory.TypeFan
	case KindThermostat:
		return accessory.TypeThermostat
	default:
		return accessory.TypeSensor
	}
}

// addPrimaryService adds the switchable service, if the device has one.
func (a *Accessory) addPrimaryService() {
	var primary *service.S

	switch a.Spec.Kind {
	case KindLightbulb:
		s := service.New(service.TypeLightbulb)
		a.on = characteristic.NewOn()
		s.AddC(a.on.C)

		if a.Spec.Brightness != nil {
			a.brightness = characteristic.NewBrightness()
			s.AddC(a.brightness.C)
		}
		switch a.Spec.Color {
		case ColorHSV:
			// Saturation before Hue, matching hap's own ColoredLightbulb.
			a.saturation = characteristic.NewSaturation()
			s.AddC(a.saturation.C)
			a.hue = characteristic.NewHue()
			s.AddC(a.hue.C)
		case ColorTemperature:
			a.colorTemp = characteristic.NewColorTemperature()
			if k := a.Spec.Kelvin; k != nil {
				// HomeKit works in mireds, which invert Kelvin: the coldest
				// colour is the smallest mired value.
				a.colorTemp.SetMinValue(kelvinToMired(k.Max))
				a.colorTemp.SetMaxValue(kelvinToMired(k.Min))
			}
			s.AddC(a.colorTemp.C)
		}
		primary = s

	case KindOutlet:
		s := service.NewOutlet()
		a.on = s.On
		// The bridge cannot tell whether something is plugged in, so mirror
		// the switch state rather than claim knowledge it does not have.
		s.OutletInUse.SetValue(true)
		primary = s.S

	case KindSwitch:
		s := service.NewSwitch()
		a.on = s.On
		primary = s.S

	case KindFan:
		// Service type Fan (not FanV2) gives a single on/off control, which
		// is all a dumb fan on a smart socket can honestly offer.
		s := service.NewFan()
		a.on = s.On
		primary = s.S

	case KindThermostat:
		primary = a.buildThermostat()

	case KindSensor:
		// Readings only; the sensor services are added below.
	}

	if primary != nil {
		// Marks which control the Home app shows on the collapsed tile, so a
		// toggle cannot end up standing in for the device itself.
		primary.Primary = true
		a.A.AddS(primary)
	}

	a.wirePrimaryHandlers()
}

// buildThermostat maps a device that heats to a temperature.
//
// Thermostat is used rather than HeaterCooler because its characteristics say
// what the device actually does: TargetHeatingCoolingState is a two-state
// control that maps onto on_off, and TargetTemperature is a target rather than
// a threshold of a deadband.
func (a *Accessory) buildThermostat() *service.S {
	s := service.NewThermostat()

	// The device only heats, so Cool and Auto would be dead options.
	s.TargetHeatingCoolingState.ValidVals = []int{heatingStateOff, heatingStateHeat}
	s.CurrentHeatingCoolingState.ValidVals = []int{heatingStateOff, heatingStateHeat}
	a.targetState = s.TargetHeatingCoolingState
	a.currentHeat = s.CurrentHeatingCoolingState

	// Celsius. Yandex reports temperatures in Celsius and this only affects
	// what the accessory says its own units are.
	_ = s.TemperatureDisplayUnits.SetValue(0)

	a.currentTemp = s.CurrentTemperature
	a.currentTemp.SetMinValue(-100)
	a.currentTemp.SetMaxValue(150)

	a.targetTemp = s.TargetTemperature
	if t := a.Spec.Target; t != nil {
		// HAP defines TargetTemperature as 10-38 °C, which no kettle fits in.
		// The Home app takes the bounds from the accessory's own metadata, so
		// widening them here is what makes 40-100 °C selectable.
		a.targetTemp.SetMinValue(t.Min)
		a.targetTemp.SetMaxValue(t.Max)
		a.targetTemp.SetStepValue(t.Precision)
		a.targetTemp.SetValue(t.Min)
	}
	return s.S
}

// wirePrimaryHandlers connects the writable characteristics to the coalescing
// buffer and the readable ones to the reachability guard.
func (a *Accessory) wirePrimaryHandlers() {
	if a.on != nil {
		a.on.OnSetRemoteValue(func(bool) error { a.queueWrite(writeOnOff); return nil })
		a.on.ValueRequestFunc = a.readGuard(func() any { return a.on.Value() })
	}
	if a.brightness != nil {
		a.brightness.OnSetRemoteValue(func(int) error { a.queueWrite(writeBrightness); return nil })
		a.brightness.ValueRequestFunc = a.readGuard(func() any { return a.brightness.Value() })
	}
	if a.hue != nil {
		a.hue.OnSetRemoteValue(func(float64) error { a.queueWrite(writeColor); return nil })
		a.hue.ValueRequestFunc = a.readGuard(func() any { return a.hue.Value() })
	}
	if a.saturation != nil {
		a.saturation.OnSetRemoteValue(func(float64) error { a.queueWrite(writeColor); return nil })
		a.saturation.ValueRequestFunc = a.readGuard(func() any { return a.saturation.Value() })
	}
	if a.colorTemp != nil {
		a.colorTemp.OnSetRemoteValue(func(int) error { a.queueWrite(writeColor); return nil })
		a.colorTemp.ValueRequestFunc = a.readGuard(func() any { return a.colorTemp.Value() })
	}
	if a.targetState != nil {
		a.targetState.OnSetRemoteValue(func(int) error { a.queueWrite(writeOnOff); return nil })
		a.targetState.ValueRequestFunc = a.readGuard(func() any { return a.targetState.Value() })
	}
	if a.targetTemp != nil {
		a.targetTemp.OnSetRemoteValue(func(float64) error { a.queueWrite(writeTarget); return nil })
		a.targetTemp.ValueRequestFunc = a.readGuard(func() any { return a.targetTemp.Value() })
	}
	if a.currentTemp != nil {
		a.currentTemp.ValueRequestFunc = a.readGuard(func() any { return a.currentTemp.Value() })
	}
}

// addSensorServices attaches read-only readings to the same accessory, so a
// climate sensor stays one tile in the Home app rather than three.
func (a *Accessory) addSensorServices() {
	if a.Spec.HasSensor(SensorTemperature) {
		s := service.NewTemperatureSensor()
		// The default range starts at 0 °C, which would clamp any reading
		// from a sensor outside a Russian window in winter.
		s.CurrentTemperature.SetMinValue(-100)
		s.CurrentTemperature.SetMaxValue(150)
		a.temperature = s.CurrentTemperature
		a.temperature.ValueRequestFunc = a.readGuard(func() any { return a.temperature.Value() })
		a.A.AddS(s.S)
	}
	if a.Spec.HasSensor(SensorHumidity) {
		s := service.NewHumiditySensor()
		a.humidity = s.CurrentRelativeHumidity
		a.humidity.ValueRequestFunc = a.readGuard(func() any { return a.humidity.Value() })
		a.A.AddS(s.S)
	}
	if a.Spec.HasSensor(SensorBattery) {
		s := service.NewBatteryService()
		// Battery-powered Yandex sensors are not chargeable in HomeKit's
		// sense: 2 means "not chargeable".
		_ = s.ChargingState.SetValue(2)
		a.battery = s
		a.A.AddS(s.S)
	}
}

// addToggleServices exposes each toggle capability as its own switch inside
// the same accessory. The Home app groups an accessory's services into one
// tile by default, so these land in the device's own card rather than
// scattering across the Home screen.
func (a *Accessory) addToggleServices() {
	for _, t := range a.Spec.Toggles {
		s := service.NewSwitch()

		// Without a Name characteristic every switch would show the
		// accessory's own name and be indistinguishable from its siblings.
		name := characteristic.NewName()
		name.SetValue(t.Name)
		s.AddC(name.C)

		instance := t.Instance
		s.On.OnSetRemoteValue(func(bool) error { a.queueWrite(writeTogglePfx + instance); return nil })
		s.On.ValueRequestFunc = a.readGuard(func() any { return s.On.Value() })

		a.toggles[instance] = s.On
		a.A.AddS(s.S)
	}
}

// readGuard wraps a characteristic read so that an unreachable device or an
// unreachable Yandex surfaces in HomeKit as "Not Responding" instead of a
// stale value presented as current.
func (a *Accessory) readGuard(read func() any) func(*http.Request) (any, int) {
	return func(*http.Request) (any, int) {
		if !a.ctrl.Reachable() || a.offline.Load() {
			return nil, hap.JsonStatusServiceCommunicationFailure
		}
		return read(), 0
	}
}

// --- write path ---

// queueWrite marks a group of characteristics as changed and schedules a flush.
//
// Writes are buffered rather than sent immediately for two reasons. HomeKit
// sends hue and saturation in a single request and hap hands them over one
// characteristic at a time, so sending on each one would mean two Yandex calls
// per colour change — and the first would carry a stale component, because
// hap updates the stored value only after the setter returns. Buffering also
// collapses the flood of updates a colour picker produces while being dragged.
func (a *Accessory) queueWrite(key string) {
	a.writeMu.Lock()
	a.writeDirty[key] = struct{}{}
	if a.writeSched {
		a.writeMu.Unlock()
		return
	}
	a.writeSched = true
	a.writeMu.Unlock()

	time.AfterFunc(a.coalesceDelay, a.flush)
}

// flush sends every buffered change as a single Yandex request.
//
// At most one request per accessory is in flight: anything that arrives while
// one is running is collected and sent immediately afterwards. That keeps the
// bridge from queueing up seconds of stale writes during a drag without
// needing a separate rate limiter.
func (a *Accessory) flush() {
	a.writeMu.Lock()
	dirty := a.writeDirty
	a.writeDirty = make(map[string]struct{})
	a.writeMu.Unlock()

	if len(dirty) > 0 {
		a.send(dirty)
	}

	a.writeMu.Lock()
	more := len(a.writeDirty) > 0
	a.writeSched = more
	a.writeMu.Unlock()

	if more {
		time.AfterFunc(a.coalesceDelay, a.flush)
	}
}

// send builds one action list from the dirty keys and applies it.
func (a *Accessory) send(dirty map[string]struct{}) {
	actions, written := a.buildActions(dirty)
	if len(actions) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()

	if err := a.ctrl.Apply(ctx, a.Spec.DeviceID, actions); err != nil {
		// Drop any expectations so the next poll immediately puts the real
		// state back into HomeKit rather than leaving the failed value on
		// screen for the whole settle window.
		a.clearExpectations()
		a.logger.Warn("failed to apply HomeKit change",
			slog.String("device_id", a.Spec.DeviceID),
			slog.String("device", a.Spec.Name),
			slog.Int("actions", len(actions)),
			slog.String("error", err.Error()))
		return
	}

	a.recordExpectations(written)
	a.logger.Debug("applied HomeKit change",
		slog.String("device", a.Spec.Name),
		slog.Any("actions", actions))
}

// buildActions turns dirty keys into Yandex actions, reading the current
// characteristic values. By the time this runs hap has committed every value
// from the originating request, which is what makes a colour change carry the
// correct hue and saturation together.
func (a *Accessory) buildActions(dirty map[string]struct{}) ([]yandex.Action, []*characteristic.C) {
	var (
		actions []yandex.Action
		written []*characteristic.C
	)

	if _, ok := dirty[writeOnOff]; ok {
		switch {
		case a.on != nil:
			actions = append(actions, yandex.Action{
				Type:  yandex.CapabilityOnOff,
				State: yandex.ActionState{Instance: "on", Value: a.on.Value()},
			})
			written = append(written, a.on.C)
		case a.targetState != nil:
			on := a.targetState.Value() != heatingStateOff
			actions = append(actions, yandex.Action{
				Type:  yandex.CapabilityOnOff,
				State: yandex.ActionState{Instance: "on", Value: on},
			})
			written = append(written, a.targetState.C)
		}
	}

	// Brightness reaches the device through its own range capability when it
	// has one, and otherwise as the value component of the colour.
	_, wantBrightness := dirty[writeBrightness]
	if wantBrightness && a.brightness != nil && a.Spec.Brightness != nil {
		actions = append(actions, yandex.Action{
			Type: yandex.CapabilityRange,
			State: yandex.ActionState{
				Instance: string(yandex.RangeBrightness),
				Value:    a.percentToBrightness(a.brightness.Value()),
			},
		})
		written = append(written, a.brightness.C)
		wantBrightness = false
	}

	_, wantColor := dirty[writeColor]
	if wantColor || wantBrightness {
		switch {
		case a.hue != nil:
			value := float64(100)
			if a.brightness != nil {
				value = float64(a.brightness.Value())
			}
			actions = append(actions, yandex.Action{
				Type: yandex.CapabilityColorSetting,
				State: yandex.ActionState{
					Instance: string(yandex.ColorHSV),
					Value: map[string]float64{
						"h": math.Round(a.hue.Value()),
						"s": math.Round(a.saturation.Value()),
						"v": math.Round(value),
					},
				},
			})
			written = append(written, a.hue.C, a.saturation.C)
			if a.brightness != nil {
				written = append(written, a.brightness.C)
			}

		case a.colorTemp != nil && wantColor:
			kelvin := miredToKelvin(a.colorTemp.Value())
			if k := a.Spec.Kelvin; k != nil {
				kelvin = clampFloat(kelvin, k.Min, k.Max)
			}
			actions = append(actions, yandex.Action{
				Type: yandex.CapabilityColorSetting,
				State: yandex.ActionState{
					Instance: string(yandex.ColorTemperatureK),
					Value:    int(math.Round(kelvin)),
				},
			})
			written = append(written, a.colorTemp.C)
		}
	}

	if _, ok := dirty[writeTarget]; ok && a.targetTemp != nil {
		value := a.targetTemp.Value()
		if t := a.Spec.Target; t != nil {
			value = clampFloat(value, t.Min, t.Max)
		}
		actions = append(actions, yandex.Action{
			Type: yandex.CapabilityRange,
			State: yandex.ActionState{
				Instance: string(yandex.RangeTemperature),
				Value:    value,
			},
		})
		written = append(written, a.targetTemp.C)
	}

	for key := range dirty {
		instance, ok := strings.CutPrefix(key, writeTogglePfx)
		if !ok {
			continue
		}
		c, known := a.toggles[instance]
		if !known {
			continue
		}
		actions = append(actions, yandex.Action{
			Type:  yandex.CapabilityToggle,
			State: yandex.ActionState{Instance: instance, Value: c.Value()},
		})
		written = append(written, c.C)
	}

	return actions, written
}

// --- echo suppression ---

// recordExpectations remembers what was just written, so that state polled
// before Yandex catches up does not overwrite it.
func (a *Accessory) recordExpectations(cs []*characteristic.C) {
	if a.settleWindow <= 0 {
		return
	}
	until := time.Now().Add(a.settleWindow)

	a.expectMu.Lock()
	defer a.expectMu.Unlock()
	for _, c := range cs {
		a.expect[c] = expectation{value: c.Value(), until: until}
	}
}

// clearExpectations forgets every pending write, letting polled state win.
func (a *Accessory) clearExpectations() {
	a.expectMu.Lock()
	defer a.expectMu.Unlock()
	clear(a.expect)
}

// suppressed reports whether an incoming value should be ignored because it
// contradicts a write that Yandex has not confirmed yet.
//
// This is what stops the colour picker from jumping back under the user's
// finger: Yandex reaches the device through the vendor's cloud, so for a
// second or three it keeps reporting the old colour, and hap broadcasts any
// value the bridge writes to every connected controller — including the phone
// that is mid-drag.
func (a *Accessory) suppressed(c *characteristic.C, incoming any) bool {
	a.expectMu.Lock()
	defer a.expectMu.Unlock()

	e, ok := a.expect[c]
	if !ok {
		return false
	}
	if time.Now().After(e.until) {
		// Yandex never converged; stop arguing and accept reality.
		delete(a.expect, c)
		return false
	}
	if valuesEqual(e.value, incoming) {
		delete(a.expect, c)
		return false
	}
	return true
}

// valuesEqual compares characteristic values, tolerating the int/float mix
// that comes from HomeKit integers and Yandex floats.
func valuesEqual(a, b any) bool {
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if aok && bok {
		return math.Abs(af-bf) < 0.001
	}
	return a == b
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case float64:
		return n, true
	default:
		return 0, false
	}
}

// --- read path ---

// Apply pushes a Yandex device state into the HomeKit characteristics.
//
// Values are written only when they actually change: hap notifies every paired
// controller on each SetValue, and re-announcing an unchanged value every poll
// would be a steady stream of pointless traffic to every iPhone in the house.
func (a *Accessory) Apply(dev yandex.Device) {
	// An empty state means the endpoint did not report one, which is not the
	// same as the device being offline.
	if dev.State != "" {
		a.offline.Store(dev.State.Offline())
	}

	a.applyOnOff(dev)
	if a.brightness != nil {
		a.applyBrightness(dev)
	}
	if a.hue != nil || a.colorTemp != nil {
		a.applyColor(dev)
	}
	if a.targetTemp != nil {
		a.applyTargetTemperature(dev)
	}
	a.applyToggles(dev)
	a.applySensors(dev)
}

func (a *Accessory) applyOnOff(dev yandex.Device) {
	cap, ok := dev.Capability(yandex.CapabilityOnOff)
	if !ok {
		return
	}
	on, err := cap.OnOffState()
	if err != nil {
		return
	}

	if a.on != nil && !a.suppressed(a.on.C, on) && on != a.on.Value() {
		a.on.SetValue(on)
	}

	if a.targetState != nil {
		state := heatingStateOff
		if on {
			state = heatingStateHeat
		}
		if !a.suppressed(a.targetState.C, state) && state != a.targetState.Value() {
			_ = a.targetState.SetValue(state)
		}
		// CurrentHeatingCoolingState is read-only, so it simply follows.
		if state != a.currentHeat.Value() {
			_ = a.currentHeat.SetValue(state)
		}
	}
}

func (a *Accessory) applyBrightness(dev yandex.Device) {
	cap, ok := dev.RangeCapability(yandex.RangeBrightness)
	if !ok {
		return
	}
	_, value, err := cap.RangeState()
	if err != nil {
		return
	}
	pct := a.brightnessToPercent(value)
	if !a.suppressed(a.brightness.C, pct) && pct != a.brightness.Value() {
		_ = a.brightness.SetValue(pct)
	}
}

func (a *Accessory) applyColor(dev yandex.Device) {
	cap, ok := dev.Capability(yandex.CapabilityColorSetting)
	if !ok {
		return
	}
	state, err := cap.ColorState()
	if err != nil {
		return
	}

	switch state.Instance {
	case yandex.ColorHSV:
		if a.hue == nil {
			return
		}
		if !a.suppressed(a.hue.C, state.HSV.H) && state.HSV.H != a.hue.Value() {
			a.hue.SetValue(state.HSV.H)
		}
		if !a.suppressed(a.saturation.C, state.HSV.S) && state.HSV.S != a.saturation.Value() {
			a.saturation.SetValue(state.HSV.S)
		}

	case yandex.ColorTemperatureK:
		if a.colorTemp == nil {
			// The device is in white mode but HomeKit is showing hue and
			// saturation; report white as a fully desaturated colour so the
			// two views agree.
			if a.saturation != nil && !a.suppressed(a.saturation.C, float64(0)) && a.saturation.Value() != 0 {
				a.saturation.SetValue(0)
			}
			return
		}
		mired := kelvinToMired(state.TemperatureK)
		if !a.suppressed(a.colorTemp.C, mired) && mired != a.colorTemp.Value() {
			_ = a.colorTemp.SetValue(mired)
		}
	}
}

func (a *Accessory) applyTargetTemperature(dev yandex.Device) {
	cap, ok := dev.RangeCapability(yandex.RangeTemperature)
	if !ok {
		return
	}
	_, value, err := cap.RangeState()
	if err != nil {
		return
	}
	if !a.suppressed(a.targetTemp.C, value) && value != a.targetTemp.Value() {
		a.targetTemp.SetValue(value)
	}
}

func (a *Accessory) applyToggles(dev yandex.Device) {
	for instance, c := range a.toggles {
		cap, ok := dev.ToggleCapability(instance)
		if !ok {
			continue
		}
		on, err := cap.ToggleState()
		if err != nil {
			continue
		}
		if !a.suppressed(c.C, on) && on != c.Value() {
			c.SetValue(on)
		}
	}
}

// applySensors pushes read-only readings. They are never suppressed: nothing
// writes them from HomeKit, so there is no echo to guard against.
func (a *Accessory) applySensors(dev yandex.Device) {
	if target := a.currentTemp; target != nil {
		if p, ok := dev.FloatProperty(yandex.FloatTemperature); ok {
			if v, err := p.FloatState(); err == nil && v != target.Value() {
				target.SetValue(v)
			}
		}
	}
	if a.temperature != nil {
		if p, ok := dev.FloatProperty(yandex.FloatTemperature); ok {
			if v, err := p.FloatState(); err == nil && v != a.temperature.Value() {
				a.temperature.SetValue(v)
			}
		}
	}
	if a.humidity != nil {
		if p, ok := dev.FloatProperty(yandex.FloatHumidity); ok {
			if v, err := p.FloatState(); err == nil && v != a.humidity.Value() {
				a.humidity.SetValue(v)
			}
		}
	}
	if a.battery != nil {
		if p, ok := dev.FloatProperty(yandex.FloatBatteryLevel); ok {
			if v, err := p.FloatState(); err == nil {
				level := clampInt(int(math.Round(v)), 0, 100)
				if level != a.battery.BatteryLevel.Value() {
					_ = a.battery.BatteryLevel.SetValue(level)
				}
				low := 0
				if level <= 20 {
					low = 1
				}
				if low != a.battery.StatusLowBattery.Value() {
					_ = a.battery.StatusLowBattery.SetValue(low)
				}
			}
		}
	}
}

// SetUnreachable marks the accessory as not responding, used when Yandex as a
// whole is unreachable rather than this one device.
func (a *Accessory) SetUnreachable(unreachable bool) { a.offline.Store(unreachable) }

// Offline reports the accessory's current reachability.
func (a *Accessory) Offline() bool { return a.offline.Load() }

// --- unit conversions ---

// isPercentRange reports whether a device's brightness range is already a
// percentage. Yandex documents brightness in unit.percent, and the common
// range is 1..100 — where the 1 means "this control cannot reach zero", not
// "the scale starts at one".
//
// Rescaling such a range onto 0..100 would turn 50 % into 49 % and back into
// 49 %, so the Home app would disagree with the Yandex app and repeated
// adjustments would drift.
func isPercentRange(r *BrightnessRange) bool {
	return r != nil && r.Min >= 0 && r.Max <= 100
}

// brightnessToPercent maps a Yandex brightness onto HomeKit's 0-100 scale.
func (a *Accessory) brightnessToPercent(v float64) int {
	r := a.Spec.Brightness
	if r == nil || r.Max <= r.Min || isPercentRange(r) {
		return clampInt(int(math.Round(v)), 0, 100)
	}
	pct := (v - r.Min) / (r.Max - r.Min) * 100
	return clampInt(int(math.Round(pct)), 0, 100)
}

// percentToBrightness is the inverse of brightnessToPercent.
func (a *Accessory) percentToBrightness(pct int) float64 {
	r := a.Spec.Brightness
	if r == nil || r.Max <= r.Min {
		return float64(clampInt(pct, 0, 100))
	}
	if isPercentRange(r) {
		// Clamp into what the device accepts: HomeKit's slider reaches 0 even
		// when the device's minimum is 1.
		return clampFloat(float64(clampInt(pct, 0, 100)), r.Min, r.Max)
	}
	v := r.Min + float64(clampInt(pct, 0, 100))/100*(r.Max-r.Min)
	return math.Round(clampFloat(v, r.Min, r.Max))
}

// kelvinToMired converts a colour temperature to HomeKit's reciprocal megakelvin
// scale, clamped to the range HomeKit accepts.
func kelvinToMired(kelvin float64) int {
	if kelvin <= 0 {
		return 140
	}
	return clampInt(int(math.Round(1_000_000/kelvin)), 140, 500)
}

// miredToKelvin is the inverse of kelvinToMired.
func miredToKelvin(mired int) float64 {
	if mired <= 0 {
		return 2700
	}
	return 1_000_000 / float64(mired)
}

func clampInt(v, lo, hi int) int {
	return min(max(v, lo), hi)
}

func clampFloat(v, lo, hi float64) float64 {
	return math.Min(math.Max(v, lo), hi)
}
