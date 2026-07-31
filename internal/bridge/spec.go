package bridge

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"yandex-bridge/internal/config"
	"yandex-bridge/internal/yandex"
)

// Kind is the HomeKit accessory shape a device is exported as.
type Kind string

const (
	KindLightbulb Kind = "lightbulb"
	KindOutlet    Kind = "outlet"
	KindSwitch    Kind = "switch"
	KindFan       Kind = "fan"
	// KindSensor is a device with readings but nothing to switch.
	KindSensor Kind = "sensor"
)

// ColorSupport says which colour controls a light exposes.
//
// Only one of them is ever chosen. HomeKit's Lightbulb service technically
// permits ColorTemperature alongside Hue and Saturation, but the Home app
// behaves erratically when both are present — the reference project ran into
// exactly this — so the bridge commits to one.
type ColorSupport string

const (
	ColorNone        ColorSupport = "none"
	ColorHSV         ColorSupport = "hsv"
	ColorTemperature ColorSupport = "temperature"
)

// SensorKind is a read-only reading exported as its own HomeKit service.
type SensorKind string

const (
	SensorTemperature SensorKind = "temperature"
	SensorHumidity    SensorKind = "humidity"
	SensorBattery     SensorKind = "battery"
)

// BrightnessRange is a device's accepted brightness span.
type BrightnessRange struct {
	Min float64
	Max float64
}

// KelvinRange is a device's accepted colour temperature span.
type KelvinRange struct {
	Min float64
	Max float64
}

// Spec is the HomeKit shape of one Yandex device: everything needed to build
// the accessory, and nothing that changes minute to minute.
type Spec struct {
	DeviceID string
	Name     string
	Room     string
	Kind     Kind
	Model    string

	HasOnOff   bool
	Brightness *BrightnessRange
	Color      ColorSupport
	Kelvin     *KelvinRange
	Sensors    []SensorKind
}

// ShapeHash fingerprints everything that affects the accessory's service and
// characteristic layout.
//
// The supervisor rebuilds the HAP server when a shape changes, because hap
// fixes the accessory set at NewServer time. The name is deliberately excluded:
// users rename accessories in the Home app and that name wins anyway, so
// treating a Yandex rename as a structural change would cause pointless
// rebuilds.
func (s Spec) ShapeHash() string {
	var b strings.Builder
	fmt.Fprintf(&b, "kind=%s;onoff=%t;color=%s;", s.Kind, s.HasOnOff, s.Color)
	if s.Brightness != nil {
		fmt.Fprintf(&b, "brightness=%g..%g;", s.Brightness.Min, s.Brightness.Max)
	}
	if s.Kelvin != nil {
		fmt.Fprintf(&b, "kelvin=%g..%g;", s.Kelvin.Min, s.Kelvin.Max)
	}
	sensors := slices.Clone(s.Sensors)
	slices.Sort(sensors)
	for _, sensor := range sensors {
		fmt.Fprintf(&b, "sensor=%s;", sensor)
	}

	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:8])
}

// HasSensor reports whether the spec includes a given reading.
func (s Spec) HasSensor(k SensorKind) bool { return slices.Contains(s.Sensors, k) }

// BuildSpec maps a Yandex device onto a HomeKit shape, applying any override.
// It returns false when the device should not be exported at all.
func BuildSpec(dev yandex.Device, o config.DeviceOverride) (Spec, bool) {
	if o.Exclude {
		return Spec{}, false
	}

	spec := Spec{
		DeviceID: dev.ID,
		Name:     dev.Name,
		Model:    string(dev.Type),
		HasOnOff: dev.HasCapability(yandex.CapabilityOnOff),
		Color:    ColorNone,
		Sensors:  sensorsFor(dev),
	}
	if o.Name != "" {
		spec.Name = o.Name
	}
	if spec.Name == "" {
		// hap rejects an accessory with an empty name, and a device with no
		// name is more useful under its id than not at all.
		spec.Name = dev.ID
	}

	kind, ok := kindFor(dev, o, len(spec.Sensors) > 0)
	if !ok {
		return Spec{}, false
	}
	spec.Kind = kind

	if kind != KindSensor {
		spec.Brightness = brightnessFor(dev)
		spec.Color, spec.Kelvin = colorFor(dev, o, kind)
	}
	return spec, true
}

// kindFor picks the HomeKit shape, honouring an explicit override first.
func kindFor(dev yandex.Device, o config.DeviceOverride, hasSensors bool) (Kind, bool) {
	if o.Type != config.TypeAuto {
		switch o.Type {
		case config.TypeLightbulb:
			return KindLightbulb, true
		case config.TypeOutlet:
			return KindOutlet, true
		case config.TypeSwitch:
			return KindSwitch, true
		case config.TypeFan:
			// The motivating case: a dumb fan plugged into a smart socket.
			// HomeKit gets a Fan with a single on/off control.
			return KindFan, true
		}
	}

	hasOnOff := dev.HasCapability(yandex.CapabilityOnOff)

	switch dev.Type.Base() {
	case yandex.DeviceTypeLight:
		return KindLightbulb, true
	case yandex.DeviceTypeSocket:
		return KindOutlet, true
	case yandex.DeviceTypeSwitch:
		return KindSwitch, true
	case yandex.DeviceTypeSensor:
		if hasSensors {
			return KindSensor, true
		}
		// A sensor whose readings the bridge cannot map is not worth an
		// empty accessory in the Home app.
		return "", false
	}

	// An unrecognised type that can still be switched is exported as a plain
	// switch; guessing anything richer would misrepresent the device.
	if hasOnOff {
		return KindSwitch, true
	}
	if hasSensors {
		return KindSensor, true
	}
	return "", false
}

// brightnessFor extracts the brightness range, if the device has one.
func brightnessFor(dev yandex.Device) *BrightnessRange {
	cap, ok := dev.RangeCapability(yandex.RangeBrightness)
	if !ok {
		return nil
	}
	params, err := cap.RangeParameters()
	if err != nil || params.Range == nil {
		// Yandex documents brightness as a percentage; assume the full span
		// rather than dropping the control.
		return &BrightnessRange{Min: 0, Max: 100}
	}
	if params.Range.Max <= params.Range.Min {
		return &BrightnessRange{Min: 0, Max: 100}
	}
	return &BrightnessRange{Min: params.Range.Min, Max: params.Range.Max}
}

// colorFor decides which colour control to expose, if any.
func colorFor(dev yandex.Device, o config.DeviceOverride, kind Kind) (ColorSupport, *KelvinRange) {
	if kind != KindLightbulb {
		return ColorNone, nil
	}
	cap, ok := dev.Capability(yandex.CapabilityColorSetting)
	if !ok {
		return ColorNone, nil
	}
	params, err := cap.ColorParameters()
	if err != nil {
		return ColorNone, nil
	}

	var kelvin *KelvinRange
	if params.SupportsTemperature() && params.TemperatureK.Max > params.TemperatureK.Min {
		kelvin = &KelvinRange{Min: params.TemperatureK.Min, Max: params.TemperatureK.Max}
	}

	switch o.ColorMode {
	case config.ColorTemperature:
		if kelvin != nil {
			return ColorTemperature, kelvin
		}
	case config.ColorHSV:
		if params.SupportsColor() {
			return ColorHSV, nil
		}
	}

	// Automatic: full colour is the more capable control, so it wins when the
	// device offers both.
	if params.SupportsColor() {
		return ColorHSV, nil
	}
	if kelvin != nil {
		return ColorTemperature, kelvin
	}
	return ColorNone, nil
}

// sensorsFor lists the readings the bridge knows how to export. A device of
// any kind may carry them — a socket with a power meter, a lamp with a
// thermometer — so they are attached as extra services rather than becoming
// separate accessories.
func sensorsFor(dev yandex.Device) []SensorKind {
	var out []SensorKind
	if _, ok := dev.FloatProperty(yandex.FloatTemperature); ok {
		out = append(out, SensorTemperature)
	}
	if _, ok := dev.FloatProperty(yandex.FloatHumidity); ok {
		out = append(out, SensorHumidity)
	}
	if _, ok := dev.FloatProperty(yandex.FloatBatteryLevel); ok {
		out = append(out, SensorBattery)
	}
	return out
}

// BuildSpecs maps a whole device list, skipping anything unsupported. The
// result is sorted by device id so that a caller comparing two polls sees a
// difference only when something genuinely changed.
func BuildSpecs(devices []yandex.Device, cfg config.Config) []Spec {
	specs := make([]Spec, 0, len(devices))
	for _, dev := range devices {
		if spec, ok := BuildSpec(dev, cfg.Override(dev.ID)); ok {
			specs = append(specs, spec)
		}
	}
	slices.SortFunc(specs, func(a, b Spec) int { return strings.Compare(a.DeviceID, b.DeviceID) })
	return specs
}

// TopologyHash fingerprints a whole device set: which devices exist and what
// shape each one has. The supervisor rebuilds the HAP server only when this
// changes, and only after several successive polls agree on the new value.
func TopologyHash(specs []Spec) string {
	sorted := slices.Clone(specs)
	slices.SortFunc(sorted, func(a, b Spec) int { return strings.Compare(a.DeviceID, b.DeviceID) })

	h := sha256.New()
	for _, s := range sorted {
		fmt.Fprintf(h, "%s=%s\n", s.DeviceID, s.ShapeHash())
	}
	return hex.EncodeToString(h.Sum(nil)[:12])
}
