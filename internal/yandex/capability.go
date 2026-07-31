package yandex

import (
	"encoding/json"
	"fmt"
)

// CapabilityType identifies a controllable device function.
type CapabilityType string

const (
	CapabilityOnOff        CapabilityType = "devices.capabilities.on_off"
	CapabilityColorSetting CapabilityType = "devices.capabilities.color_setting"
	CapabilityMode         CapabilityType = "devices.capabilities.mode"
	CapabilityRange        CapabilityType = "devices.capabilities.range"
	CapabilityToggle       CapabilityType = "devices.capabilities.toggle"
	CapabilityVideoStream  CapabilityType = "devices.capabilities.video_stream"
)

// RangeInstance names a range capability's parameter.
type RangeInstance string

const (
	RangeBrightness  RangeInstance = "brightness"
	RangeChannel     RangeInstance = "channel"
	RangeHumidity    RangeInstance = "humidity"
	RangeOpen        RangeInstance = "open"
	RangeTemperature RangeInstance = "temperature"
	RangeVolume      RangeInstance = "volume"
)

// ColorInstance names a color_setting capability's parameter.
type ColorInstance string

const (
	ColorHSV          ColorInstance = "hsv"
	ColorRGB          ColorInstance = "rgb"
	ColorTemperatureK ColorInstance = "temperature_k"
	ColorScene        ColorInstance = "scene"
)

// PropertyType identifies a read-only device reading.
type PropertyType string

const (
	PropertyFloat PropertyType = "devices.properties.float"
	PropertyEvent PropertyType = "devices.properties.event"
)

// FloatInstance names a float property's reading.
type FloatInstance string

const (
	FloatTemperature  FloatInstance = "temperature"
	FloatHumidity     FloatInstance = "humidity"
	FloatBatteryLevel FloatInstance = "battery_level"
	FloatIllumination FloatInstance = "illumination"
	FloatCO2Level     FloatInstance = "co2_level"
	FloatPressure     FloatInstance = "pressure"
	FloatVoltage      FloatInstance = "voltage"
	FloatPower        FloatInstance = "power"
	FloatAmperage     FloatInstance = "amperage"
	FloatPM25Density  FloatInstance = "pm2.5_density"
)

// Capability is a controllable device function. Parameters and State are kept
// raw because their shape depends on Type; use the typed accessors below.
type Capability struct {
	Type        CapabilityType  `json:"type"`
	Retrievable bool            `json:"retrievable"`
	Reportable  bool            `json:"reportable"`
	Parameters  json.RawMessage `json:"parameters"`
	State       json.RawMessage `json:"state"`
	LastUpdated float64         `json:"last_updated"`
}

// ErrNoState is returned when a capability carries no state. This is normal
// rather than exceptional: a capability with retrievable=false never reports
// one, and Yandex also omits state for a device it has not heard from yet.
var ErrNoState = fmt.Errorf("capability has no state")

// OnOffState decodes the on_off state.
func (c Capability) OnOffState() (bool, error) {
	if c.Type != CapabilityOnOff {
		return false, fmt.Errorf("capability is %s, not on_off", c.Type)
	}
	if rawOrNull(c.State) {
		return false, ErrNoState
	}
	var s struct {
		Instance string `json:"instance"`
		Value    bool   `json:"value"`
	}
	if err := json.Unmarshal(c.State, &s); err != nil {
		return false, fmt.Errorf("decode on_off state: %w", err)
	}
	return s.Value, nil
}

// Range describes the accepted values of a range capability.
type Range struct {
	Min       float64 `json:"min"`
	Max       float64 `json:"max"`
	Precision float64 `json:"precision"`
}

// RangeParams are the parameters of a range capability.
type RangeParams struct {
	Instance     RangeInstance `json:"instance"`
	Unit         string        `json:"unit"`
	RandomAccess bool          `json:"random_access"`
	Range        *Range        `json:"range"`
}

// RangeParameters decodes the parameters of a range capability.
func (c Capability) RangeParameters() (RangeParams, error) {
	var p RangeParams
	if c.Type != CapabilityRange {
		return p, fmt.Errorf("capability is %s, not range", c.Type)
	}
	if rawOrNull(c.Parameters) {
		return p, fmt.Errorf("range capability has no parameters")
	}
	if err := json.Unmarshal(c.Parameters, &p); err != nil {
		return p, fmt.Errorf("decode range parameters: %w", err)
	}
	return p, nil
}

// RangeState decodes the current value of a range capability.
func (c Capability) RangeState() (RangeInstance, float64, error) {
	if c.Type != CapabilityRange {
		return "", 0, fmt.Errorf("capability is %s, not range", c.Type)
	}
	if rawOrNull(c.State) {
		return "", 0, ErrNoState
	}
	var s struct {
		Instance RangeInstance `json:"instance"`
		Value    float64       `json:"value"`
	}
	if err := json.Unmarshal(c.State, &s); err != nil {
		return "", 0, fmt.Errorf("decode range state: %w", err)
	}
	return s.Instance, s.Value, nil
}

// TemperatureKRange is the supported colour temperature span in Kelvin.
type TemperatureKRange struct {
	Min float64 `json:"min"`
	Max float64 `json:"max"`
}

// ColorParams are the parameters of a color_setting capability.
type ColorParams struct {
	// ColorModel is "hsv" or "rgb" when the device supports arbitrary colours.
	ColorModel string `json:"color_model"`
	// TemperatureK is set when the device supports white temperature.
	TemperatureK *TemperatureKRange `json:"temperature_k"`
}

// SupportsColor reports whether arbitrary colours can be set.
func (p ColorParams) SupportsColor() bool { return p.ColorModel != "" }

// SupportsTemperature reports whether white temperature can be set.
func (p ColorParams) SupportsTemperature() bool { return p.TemperatureK != nil }

// ColorParameters decodes the parameters of a color_setting capability.
func (c Capability) ColorParameters() (ColorParams, error) {
	var p ColorParams
	if c.Type != CapabilityColorSetting {
		return p, fmt.Errorf("capability is %s, not color_setting", c.Type)
	}
	if rawOrNull(c.Parameters) {
		return p, fmt.Errorf("color_setting capability has no parameters")
	}
	if err := json.Unmarshal(c.Parameters, &p); err != nil {
		return p, fmt.Errorf("decode color_setting parameters: %w", err)
	}
	return p, nil
}

// HSV is a colour in the Yandex hue/saturation/value model, with hue in
// degrees (0-360) and saturation and value in percent (0-100).
type HSV struct {
	H float64 `json:"h"`
	S float64 `json:"s"`
	V float64 `json:"v"`
}

// ColorState is the current colour of a device. Exactly one field is set,
// selected by Instance, because color_setting reports whichever colour mode
// the device is currently in.
type ColorState struct {
	Instance     ColorInstance
	HSV          HSV
	RGB          uint32
	TemperatureK float64
	Scene        string
}

// ColorState decodes the state of a color_setting capability.
func (c Capability) ColorState() (ColorState, error) {
	var out ColorState
	if c.Type != CapabilityColorSetting {
		return out, fmt.Errorf("capability is %s, not color_setting", c.Type)
	}
	if rawOrNull(c.State) {
		return out, ErrNoState
	}
	var s struct {
		Instance ColorInstance   `json:"instance"`
		Value    json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(c.State, &s); err != nil {
		return out, fmt.Errorf("decode color_setting state: %w", err)
	}
	out.Instance = s.Instance

	switch s.Instance {
	case ColorHSV:
		if err := json.Unmarshal(s.Value, &out.HSV); err != nil {
			return out, fmt.Errorf("decode hsv value: %w", err)
		}
	case ColorRGB:
		if err := json.Unmarshal(s.Value, &out.RGB); err != nil {
			return out, fmt.Errorf("decode rgb value: %w", err)
		}
	case ColorTemperatureK:
		if err := json.Unmarshal(s.Value, &out.TemperatureK); err != nil {
			return out, fmt.Errorf("decode temperature_k value: %w", err)
		}
	case ColorScene:
		if err := json.Unmarshal(s.Value, &out.Scene); err != nil {
			return out, fmt.Errorf("decode scene value: %w", err)
		}
	default:
		return out, fmt.Errorf("unknown color_setting instance %q", s.Instance)
	}
	return out, nil
}

// Property is a read-only device reading.
type Property struct {
	Type        PropertyType    `json:"type"`
	Retrievable bool            `json:"retrievable"`
	Reportable  bool            `json:"reportable"`
	Parameters  json.RawMessage `json:"parameters"`
	State       json.RawMessage `json:"state"`
	LastUpdated float64         `json:"last_updated"`
}

// FloatParams are the parameters of a float property.
type FloatParams struct {
	Instance FloatInstance `json:"instance"`
	Unit     string        `json:"unit"`
}

// FloatParameters decodes the parameters of a float property.
func (p Property) FloatParameters() (FloatParams, error) {
	var out FloatParams
	if p.Type != PropertyFloat {
		return out, fmt.Errorf("property is %s, not float", p.Type)
	}
	if rawOrNull(p.Parameters) {
		return out, fmt.Errorf("float property has no parameters")
	}
	if err := json.Unmarshal(p.Parameters, &out); err != nil {
		return out, fmt.Errorf("decode float parameters: %w", err)
	}
	return out, nil
}

// FloatState decodes the current value of a float property.
func (p Property) FloatState() (float64, error) {
	if p.Type != PropertyFloat {
		return 0, fmt.Errorf("property is %s, not float", p.Type)
	}
	if rawOrNull(p.State) {
		return 0, ErrNoState
	}
	var s struct {
		Instance FloatInstance `json:"instance"`
		Value    float64       `json:"value"`
	}
	if err := json.Unmarshal(p.State, &s); err != nil {
		return 0, fmt.Errorf("decode float state: %w", err)
	}
	return s.Value, nil
}
