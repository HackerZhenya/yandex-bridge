package bridge

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"sync/atomic"

	"github.com/brutella/hap"
	"github.com/brutella/hap/accessory"
	"github.com/brutella/hap/characteristic"
	"github.com/brutella/hap/service"

	"yandex-bridge/internal/yandex"
)

// Manufacturer shown in the Home app's accessory details.
const manufacturer = "Yandex Smart Home"

// Controller applies HomeKit writes to Yandex and reports whether the bridge
// can currently reach it.
type Controller interface {
	// Apply sends actions to a device and blocks until they are accepted.
	Apply(ctx context.Context, deviceID string, actions []yandex.Action) error
	// Reachable reports whether the last interaction with Yandex succeeded.
	Reachable() bool
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

	temperature *characteristic.CurrentTemperature
	humidity    *characteristic.CurrentRelativeHumidity
	battery     *service.BatteryService

	// offline mirrors the device's own state as reported by Yandex, which is
	// separate from whether the bridge can reach Yandex at all.
	offline atomic.Bool

	ctrl   Controller
	logger *slog.Logger
}

// BuildAccessory constructs the HomeKit accessory for a spec.
//
// Services are added in a fixed order and derived only from the spec — never
// from live values — because hap assigns instance ids by position. A layout
// that varied with the current state would renumber characteristics under a
// paired controller.
func BuildAccessory(spec Spec, aid uint64, ctrl Controller, logger *slog.Logger) *Accessory {
	a := &Accessory{Spec: spec, ctrl: ctrl, logger: logger}

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
	default:
		return accessory.TypeSensor
	}
}

// addPrimaryService adds the switchable service, if the device has one.
func (a *Accessory) addPrimaryService() {
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
		a.A.AddS(s)

	case KindOutlet:
		s := service.NewOutlet()
		a.on = s.On
		// The bridge cannot tell whether something is plugged in, so mirror
		// the switch state rather than claim knowledge it does not have.
		s.OutletInUse.SetValue(true)
		a.A.AddS(s.S)

	case KindSwitch:
		s := service.NewSwitch()
		a.on = s.On
		a.A.AddS(s.S)

	case KindFan:
		// Service type Fan (not FanV2) gives a single on/off control, which
		// is all a dumb fan on a smart socket can honestly offer.
		s := service.NewFan()
		a.on = s.On
		a.A.AddS(s.S)

	case KindSensor:
		// Readings only; the sensor services are added below.
	}

	if a.on != nil {
		a.on.OnSetRemoteValue(a.setOn)
		a.on.ValueRequestFunc = a.readGuard(func() any { return a.on.Value() })
	}
	if a.brightness != nil {
		a.brightness.OnSetRemoteValue(a.setBrightness)
		a.brightness.ValueRequestFunc = a.readGuard(func() any { return a.brightness.Value() })
	}
	if a.hue != nil {
		a.hue.OnSetRemoteValue(func(v float64) error { return a.setHueSaturation(v, a.saturation.Value()) })
		a.hue.ValueRequestFunc = a.readGuard(func() any { return a.hue.Value() })
	}
	if a.saturation != nil {
		a.saturation.OnSetRemoteValue(func(v float64) error { return a.setHueSaturation(a.hue.Value(), v) })
		a.saturation.ValueRequestFunc = a.readGuard(func() any { return a.saturation.Value() })
	}
	if a.colorTemp != nil {
		a.colorTemp.OnSetRemoteValue(a.setColorTemperature)
		a.colorTemp.ValueRequestFunc = a.readGuard(func() any { return a.colorTemp.Value() })
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

	if a.on != nil {
		if cap, ok := dev.Capability(yandex.CapabilityOnOff); ok {
			if on, err := cap.OnOffState(); err == nil && on != a.on.Value() {
				a.on.SetValue(on)
			}
		}
	}

	if a.brightness != nil {
		a.applyBrightness(dev)
	}
	if a.hue != nil || a.colorTemp != nil {
		a.applyColor(dev)
	}
	a.applySensors(dev)
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
	if pct := a.brightnessToPercent(value); pct != a.brightness.Value() {
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
		if state.HSV.H != a.hue.Value() {
			a.hue.SetValue(state.HSV.H)
		}
		if state.HSV.S != a.saturation.Value() {
			a.saturation.SetValue(state.HSV.S)
		}

	case yandex.ColorTemperatureK:
		if a.colorTemp == nil {
			// The device is in white mode but HomeKit is showing hue and
			// saturation; report white as a fully desaturated colour so the
			// two views agree.
			if a.saturation != nil && a.saturation.Value() != 0 {
				a.saturation.SetValue(0)
			}
			return
		}
		if mired := kelvinToMired(state.TemperatureK); mired != a.colorTemp.Value() {
			_ = a.colorTemp.SetValue(mired)
		}
	}
}

func (a *Accessory) applySensors(dev yandex.Device) {
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

// --- write path ---

func (a *Accessory) setOn(on bool) error {
	return a.apply(yandex.Action{
		Type:  yandex.CapabilityOnOff,
		State: yandex.ActionState{Instance: "on", Value: on},
	})
}

func (a *Accessory) setBrightness(pct int) error {
	// A device with a brightness range takes brightness through it; one with
	// only colour support carries brightness in the HSV value component.
	if a.Spec.Brightness != nil {
		return a.apply(yandex.Action{
			Type:  yandex.CapabilityRange,
			State: yandex.ActionState{Instance: string(yandex.RangeBrightness), Value: a.percentToBrightness(pct)},
		})
	}
	if a.hue != nil {
		return a.setHSV(a.hue.Value(), a.saturation.Value(), float64(pct))
	}
	return nil
}

func (a *Accessory) setHueSaturation(hue, saturation float64) error {
	value := float64(100)
	if a.brightness != nil {
		value = float64(a.brightness.Value())
	}
	return a.setHSV(hue, saturation, value)
}

func (a *Accessory) setHSV(hue, saturation, value float64) error {
	return a.apply(yandex.Action{
		Type: yandex.CapabilityColorSetting,
		State: yandex.ActionState{
			Instance: string(yandex.ColorHSV),
			Value: map[string]float64{
				"h": math.Round(hue),
				"s": math.Round(saturation),
				"v": math.Round(value),
			},
		},
	})
}

func (a *Accessory) setColorTemperature(mired int) error {
	kelvin := miredToKelvin(mired)
	if k := a.Spec.Kelvin; k != nil {
		kelvin = clampFloat(kelvin, k.Min, k.Max)
	}
	return a.apply(yandex.Action{
		Type: yandex.CapabilityColorSetting,
		State: yandex.ActionState{
			Instance: string(yandex.ColorTemperatureK),
			Value:    int(math.Round(kelvin)),
		},
	})
}

// apply sends one action and reports failure to HomeKit. Returning an error
// makes hap answer with -70402, which the Home app shows as the accessory
// failing rather than silently ignoring the tap.
func (a *Accessory) apply(action yandex.Action) error {
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()

	if err := a.ctrl.Apply(ctx, a.Spec.DeviceID, []yandex.Action{action}); err != nil {
		a.logger.Warn("failed to apply HomeKit change",
			slog.String("device_id", a.Spec.DeviceID),
			slog.String("device", a.Spec.Name),
			slog.String("capability", string(action.Type)),
			slog.String("instance", action.State.Instance),
			slog.String("error", err.Error()))
		return err
	}
	a.logger.Debug("applied HomeKit change",
		slog.String("device", a.Spec.Name),
		slog.String("capability", string(action.Type)),
		slog.String("instance", action.State.Instance),
		slog.Any("value", action.State.Value))
	return nil
}

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
