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
	// KindThermostat is a device that heats to a temperature: on/off, a
	// target temperature and the current one, all in a single service.
	KindThermostat Kind = "thermostat"
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

	// Event-driven readings. Yandex reports these as devices.properties.event
	// whose value persists until the next event, so polling sees every state —
	// just up to one poll interval late.
	SensorMotion  SensorKind = "motion"
	SensorContact SensorKind = "contact"
	SensorLeak    SensorKind = "leak"
	SensorSmoke   SensorKind = "smoke"
	SensorGas     SensorKind = "gas"
)

// eventSensors maps a Yandex event instance onto the HomeKit sensor service
// that means the same thing.
//
// Instances left out have no honest HomeKit equivalent: vibration is a pulse
// rather than a state, water_level has no matching characteristic, and
// voice_activity and noise are undocumented extras on Yandex speakers.
var eventSensors = map[string]SensorKind{
	yandex.EventMotion:    SensorMotion,
	yandex.EventOpen:      SensorContact,
	yandex.EventWaterLeak: SensorLeak,
	yandex.EventSmoke:     SensorSmoke,
	yandex.EventGas:       SensorGas,
}

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

// TemperatureRange is a device's accepted target temperature span.
type TemperatureRange struct {
	Min       float64
	Max       float64
	Precision float64
}

// ToggleSpec is one devices.capabilities.toggle exported as a switch.
type ToggleSpec struct {
	Instance string
	Name     string
}

// toggleNames are the seven toggle instances Yandex documents, with the label
// shown in the Home app. An unknown instance keeps its raw name rather than
// being dropped — a control the user can see beats one silently missing.
var toggleNames = map[string]string{
	"backlight":       "Подсветка",
	"controls_locked": "Блокировка управления",
	"ionization":      "Ионизация",
	"keep_warm":       "Поддержание тепла",
	"mute":            "Без звука",
	"oscillation":     "Вращение",
	"pause":           "Пауза",
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
	Target     *TemperatureRange
	Sensors    []SensorKind
	Toggles    []ToggleSpec
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
	if s.Target != nil {
		// The span drives the characteristic's advertised min/max, so a change
		// to it changes what HomeKit has cached.
		fmt.Fprintf(&b, "target=%g..%g/%g;", s.Target.Min, s.Target.Max, s.Target.Precision)
	}
	sensors := slices.Clone(s.Sensors)
	slices.Sort(sensors)
	for _, sensor := range sensors {
		fmt.Fprintf(&b, "sensor=%s;", sensor)
	}
	for _, t := range s.Toggles {
		fmt.Fprintf(&b, "toggle=%s;", t.Instance)
	}

	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:8])
}

// HasSensor reports whether the spec includes a given reading.
func (s Spec) HasSensor(k SensorKind) bool { return slices.Contains(s.Sensors, k) }

// MappingReport records what the bridge did with one device and, just as
// importantly, what it could not use. It is what the operator reads to work
// out what to put in config.yaml.
type MappingReport struct {
	DeviceID string            `json:"device_id"`
	Name     string            `json:"name"`
	Type     yandex.DeviceType `json:"type"`
	Room     string            `json:"room,omitempty"`
	Kind     Kind              `json:"homekit_kind,omitempty"`
	Skipped  bool              `json:"skipped"`
	Reason   string            `json:"reason,omitempty"`
	Mapped   []string          `json:"mapped,omitempty"`
	Unmapped []string          `json:"unmapped,omitempty"`
}

// BuildSpec maps a Yandex device onto a HomeKit shape, applying any override.
// It returns false when the device should not be exported at all, and always
// returns a report describing the outcome.
func BuildSpec(dev yandex.Device, o config.DeviceOverride, exposeToggles bool) (Spec, MappingReport, bool) {
	report := MappingReport{
		DeviceID: dev.ID,
		Name:     dev.Name,
		Type:     dev.Type,
	}

	if o.Exclude {
		report.Skipped = true
		report.Reason = "excluded in config"
		// Still describe it: knowing what an excluded device offers is how you
		// decide whether to un-exclude it.
		_, report.Unmapped = describeMapping(dev, Spec{})
		return Spec{}, report, false
	}

	spec := Spec{
		DeviceID: dev.ID,
		Name:     dev.Name,
		Model:    string(dev.Type),
		HasOnOff: dev.HasCapability(yandex.CapabilityOnOff),
		Color:    ColorNone,
	}
	if o.Name != "" {
		spec.Name = o.Name
	}
	if spec.Name == "" {
		// hap rejects an accessory with an empty name, and a device with no
		// name is more useful under its id than not at all.
		spec.Name = dev.ID
	}

	kind, ok := kindFor(dev, o)
	if !ok {
		report.Skipped = true
		report.Reason = skipReason(dev)
		// Everything the device offers is, by definition, unmapped. Listing it
		// is the only way to tell whether the device is worth supporting.
		_, report.Unmapped = describeMapping(dev, Spec{})
		return Spec{}, report, false
	}
	spec.Kind = kind

	if kind != KindSensor {
		spec.Brightness = brightnessFor(dev)
		spec.Color, spec.Kelvin = colorFor(dev, o, kind)
	}
	if kind == KindThermostat {
		spec.Target = targetTemperatureFor(dev)
	}
	spec.Sensors = sensorsFor(dev, kind)
	if exposeToggles {
		spec.Toggles = togglesFor(dev, o)
	}

	report.Kind = kind
	report.Mapped, report.Unmapped = describeMapping(dev, spec)
	return spec, report, true
}

// kindFor picks the HomeKit shape, honouring an explicit override first.
//
// Every switchable shape requires an on_off capability. Yandex types a device
// by what it is, not by what it can do: a Zigbee wall button reports as
// devices.types.switch but has no on_off at all, and exporting it as a HomeKit
// Switch produced a toggle that controlled nothing and never updated.
func kindFor(dev yandex.Device, o config.DeviceOverride) (Kind, bool) {
	hasOnOff := dev.HasCapability(yandex.CapabilityOnOff)
	hasTarget := targetTemperatureFor(dev) != nil
	hasSensors := len(sensorsFor(dev, KindSensor)) > 0

	if o.Type != config.TypeAuto {
		if kind, ok := overrideKind(o.Type, hasOnOff, hasTarget); ok {
			return kind, true
		}
		// The override cannot be honoured — usually a device with no on_off.
		// Fall through rather than publish a control backed by nothing.
	}

	if hasOnOff && hasTarget && heatsToTemperature(dev.Type) {
		return KindThermostat, true
	}

	if hasOnOff {
		switch dev.Type.Base() {
		case yandex.DeviceTypeLight:
			return KindLightbulb, true
		case yandex.DeviceTypeSocket:
			return KindOutlet, true
		case yandex.DeviceTypeSwitch:
			return KindSwitch, true
		}
		// An unrecognised type that can still be switched is exported as a
		// plain switch; guessing anything richer would misrepresent it.
		return KindSwitch, true
	}

	if hasSensors {
		return KindSensor, true
	}
	return "", false
}

// overrideKind resolves an explicit type from the config, reporting false when
// the device cannot back it.
func overrideKind(t config.HomeKitType, hasOnOff, hasTarget bool) (Kind, bool) {
	switch t {
	case config.TypeLightbulb:
		return KindLightbulb, hasOnOff
	case config.TypeOutlet:
		return KindOutlet, hasOnOff
	case config.TypeSwitch:
		return KindSwitch, hasOnOff
	case config.TypeFan:
		// A dumb fan plugged into a smart socket: HomeKit gets a Fan with a
		// single on/off control.
		return KindFan, hasOnOff
	case config.TypeThermostat:
		// Thermostat requires a target temperature characteristic; without a
		// range to drive it the accessory would show a dial that does nothing.
		return KindThermostat, hasOnOff && hasTarget
	default:
		return "", false
	}
}

// heatsToTemperature reports whether a device type is unambiguously something
// that heats to a set temperature.
//
// Air conditioners are excluded on purpose: they also carry a temperature
// range, but presenting one as heat-only would misrepresent it. Cooling
// support is a separate piece of work.
func heatsToTemperature(t yandex.DeviceType) bool {
	if t == yandex.DeviceTypeThermostatAC {
		return false
	}
	switch t.Base() {
	case yandex.DeviceTypeCooking, yandex.DeviceTypeThermostat:
		return true
	default:
		return false
	}
}

// skipReason explains why a device was not exported, for the inventory report.
func skipReason(dev yandex.Device) string {
	if hasButtonEvent(dev) {
		// Worth calling out: it looks switchable in the Yandex app but is an
		// input, and HomeKit models those as a different accessory type.
		return fmt.Sprintf("device type %s is a button (event:button) with no on_off; "+
			"HomeKit needs a stateless programmable switch for this, which the bridge does not build yet", dev.Type)
	}
	if !dev.HasCapability(yandex.CapabilityOnOff) && len(dev.Capabilities) > 0 {
		return fmt.Sprintf("device type %s has no on_off capability and no readings this bridge maps", dev.Type)
	}
	if len(dev.Capabilities) == 0 && len(dev.Properties) == 0 {
		return fmt.Sprintf("device type %s has no capabilities or properties", dev.Type)
	}
	return fmt.Sprintf("device type %s has nothing this bridge can map", dev.Type)
}

// hasButtonEvent reports whether the device is a physical button.
func hasButtonEvent(dev yandex.Device) bool {
	for _, p := range dev.Properties {
		if p.Type != yandex.PropertyEvent {
			continue
		}
		if p.Instance() == yandex.EventButton {
			return true
		}
	}
	return false
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

// targetTemperatureFor extracts the settable target temperature, if any.
func targetTemperatureFor(dev yandex.Device) *TemperatureRange {
	cap, ok := dev.RangeCapability(yandex.RangeTemperature)
	if !ok {
		return nil
	}
	params, err := cap.RangeParameters()
	if err != nil || params.Range == nil || params.Range.Max <= params.Range.Min {
		return nil
	}
	precision := params.Range.Precision
	if precision <= 0 {
		precision = 1
	}
	return &TemperatureRange{
		Min:       params.Range.Min,
		Max:       params.Range.Max,
		Precision: precision,
	}
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

// sensorsFor lists the readings exported as their own services. A device of
// any kind may carry them — a socket with a power meter, a lamp with a
// thermometer — so they are attached as extra services rather than becoming
// separate accessories.
func sensorsFor(dev yandex.Device, kind Kind) []SensorKind {
	var out []SensorKind
	if _, ok := dev.FloatProperty(yandex.FloatTemperature); ok {
		// A thermostat already carries CurrentTemperature inside its own
		// service; adding a sensor too would put the kettle back to two tiles.
		if kind != KindThermostat {
			out = append(out, SensorTemperature)
		}
	}
	if _, ok := dev.FloatProperty(yandex.FloatHumidity); ok {
		out = append(out, SensorHumidity)
	}
	if _, ok := dev.FloatProperty(yandex.FloatBatteryLevel); ok {
		out = append(out, SensorBattery)
	}
	// A device may report its battery as an event (low/normal) instead of a
	// percentage; HomeKit's BatteryService covers both.
	if _, ok := dev.EventProperty(yandex.EventBatteryLevel); ok && !slices.Contains(out, SensorBattery) {
		out = append(out, SensorBattery)
	}

	for _, p := range dev.Properties {
		if p.Type != yandex.PropertyEvent {
			continue
		}
		if kind, ok := eventSensors[p.Instance()]; ok && !slices.Contains(out, kind) {
			out = append(out, kind)
		}
	}
	return out
}

// togglesFor lists the toggle capabilities to expose, sorted by instance so
// the service order — and therefore the instance ids hap assigns — is stable.
func togglesFor(dev yandex.Device, o config.DeviceOverride) []ToggleSpec {
	var out []ToggleSpec
	for _, c := range dev.Capabilities {
		if c.Type != yandex.CapabilityToggle {
			continue
		}
		instance, err := c.ToggleInstance()
		if err != nil || instance == "" {
			continue
		}
		if o.TogglesHidden(instance) {
			continue
		}
		name, ok := toggleNames[instance]
		if !ok {
			name = instance
		}
		out = append(out, ToggleSpec{Instance: instance, Name: name})
	}
	slices.SortFunc(out, func(a, b ToggleSpec) int { return strings.Compare(a.Instance, b.Instance) })
	return out
}

// describeMapping records which capabilities and properties ended up driving a
// HomeKit characteristic, and which were left on the floor.
func describeMapping(dev yandex.Device, spec Spec) (mapped, unmapped []string) {
	note := func(used bool, label string) {
		if used {
			mapped = append(mapped, label)
		} else {
			unmapped = append(unmapped, label)
		}
	}

	for _, c := range dev.Capabilities {
		switch c.Type {
		case yandex.CapabilityOnOff:
			note(spec.HasOnOff, "on_off → On")

		case yandex.CapabilityRange:
			params, err := c.RangeParameters()
			if err != nil {
				unmapped = append(unmapped, "range (unreadable parameters)")
				continue
			}
			switch params.Instance {
			case yandex.RangeBrightness:
				note(spec.Brightness != nil, "range:brightness → Brightness")
			case yandex.RangeTemperature:
				note(spec.Target != nil, "range:temperature → TargetTemperature")
			default:
				unmapped = append(unmapped, "range:"+string(params.Instance))
			}

		case yandex.CapabilityColorSetting:
			switch spec.Color {
			case ColorHSV:
				mapped = append(mapped, "color_setting → Hue/Saturation")
			case ColorTemperature:
				mapped = append(mapped, "color_setting → ColorTemperature")
			default:
				// Say what it offers: a device can expose colour without an
				// on_off to hang a Lightbulb on, and the answer to whether
				// that is worth mapping depends on which model it supports.
				entry := "color_setting"
				if params, err := c.ColorParameters(); err == nil {
					if desc := params.Describe(); desc != "" {
						entry += " (" + desc + ")"
					}
				}
				unmapped = append(unmapped, entry)
			}

		case yandex.CapabilityToggle:
			instance, err := c.ToggleInstance()
			if err != nil || instance == "" {
				unmapped = append(unmapped, "toggle (unreadable parameters)")
				continue
			}
			exposed := slices.ContainsFunc(spec.Toggles, func(t ToggleSpec) bool {
				return t.Instance == instance
			})
			note(exposed, "toggle:"+instance+" → Switch")

		default:
			// Includes capability types Yandex does not document — identify
			// and zigbee_node show up on Zigbee devices. The instance is what
			// makes the entry actionable rather than just a name.
			unmapped = append(unmapped, withInstance(string(c.Type), c.Instance()))
		}
	}

	for _, p := range dev.Properties {
		if p.Type == yandex.PropertyEvent {
			instance := p.Instance()
			switch {
			case instance == yandex.EventBatteryLevel:
				note(spec.HasSensor(SensorBattery), "event:battery_level → StatusLowBattery")
			case eventSensors[instance] != "":
				kind := eventSensors[instance]
				note(spec.HasSensor(kind), fmt.Sprintf("event:%s → %s sensor", instance, kind))
			default:
				unmapped = append(unmapped, withInstance(string(p.Type), instance))
			}
			continue
		}
		if p.Type != yandex.PropertyFloat {
			unmapped = append(unmapped, withInstance(string(p.Type), p.Instance()))
			continue
		}
		params, err := p.FloatParameters()
		if err != nil {
			unmapped = append(unmapped, "float (unreadable parameters)")
			continue
		}
		switch params.Instance {
		case yandex.FloatTemperature:
			if spec.Kind == KindThermostat {
				mapped = append(mapped, "float:temperature → CurrentTemperature")
			} else {
				note(spec.HasSensor(SensorTemperature), "float:temperature → TemperatureSensor")
			}
		case yandex.FloatHumidity:
			note(spec.HasSensor(SensorHumidity), "float:humidity → HumiditySensor")
		case yandex.FloatBatteryLevel:
			note(spec.HasSensor(SensorBattery), "float:battery_level → BatteryService")
		default:
			unmapped = append(unmapped, "float:"+string(params.Instance))
		}
	}

	return mapped, unmapped
}

// withInstance appends an instance to a type name when there is one.
func withInstance(typ, instance string) string {
	if instance == "" {
		return typ
	}
	return typ + ":" + instance
}

// BuildSpecs maps a whole smart home, skipping anything unsupported.
//
// The specs are sorted by device id so that a caller comparing two polls sees
// a difference only when something genuinely changed. Reports cover every
// device, including the skipped ones, and are sorted by name because a human
// reads them.
func BuildSpecs(info *yandex.UserInfo, cfg config.Config) ([]Spec, []MappingReport) {
	if info == nil {
		return nil, nil
	}

	specs := make([]Spec, 0, len(info.Devices))
	reports := make([]MappingReport, 0, len(info.Devices))

	for _, dev := range info.Devices {
		room := info.RoomName(dev.Room)

		spec, report, ok := BuildSpec(dev, cfg.Override(dev.ID), cfg.ExposeToggles)
		report.Room = room
		reports = append(reports, report)
		if ok {
			spec.Room = room
			specs = append(specs, spec)
		}
	}

	slices.SortFunc(specs, func(a, b Spec) int { return strings.Compare(a.DeviceID, b.DeviceID) })
	slices.SortFunc(reports, func(a, b MappingReport) int { return strings.Compare(a.Name, b.Name) })
	return specs, reports
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
