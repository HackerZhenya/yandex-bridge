package bridge

import (
	"encoding/json"
	"testing"

	"yandex-bridge/internal/config"
	"yandex-bridge/internal/yandex"
)

// device builds a yandex.Device from JSON, which keeps the test fixtures in
// the same shape the API actually returns.
func device(t *testing.T, raw string) yandex.Device {
	t.Helper()
	var d yandex.Device
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatalf("build device: %v", err)
	}
	return d
}

const (
	onOffCap = `{"type":"devices.capabilities.on_off","retrievable":true,
	             "parameters":{"split":false},"state":{"instance":"on","value":true}}`

	brightnessCap = `{"type":"devices.capabilities.range","retrievable":true,
	                  "parameters":{"instance":"brightness","unit":"unit.percent","random_access":true,
	                                "range":{"min":1,"max":100,"precision":1}},
	                  "state":{"instance":"brightness","value":50}}`

	hsvCap = `{"type":"devices.capabilities.color_setting","retrievable":true,
	           "parameters":{"color_model":"hsv","temperature_k":{"min":2700,"max":6500}},
	           "state":{"instance":"hsv","value":{"h":120,"s":50,"v":80}}}`

	temperatureOnlyCap = `{"type":"devices.capabilities.color_setting","retrievable":true,
	                       "parameters":{"temperature_k":{"min":2700,"max":6500}},
	                       "state":{"instance":"temperature_k","value":4000}}`

	temperatureProp = `{"type":"devices.properties.float","retrievable":true,
	                    "parameters":{"instance":"temperature","unit":"unit.temperature.celsius"},
	                    "state":{"instance":"temperature","value":21.5}}`

	humidityProp = `{"type":"devices.properties.float","retrievable":true,
	                 "parameters":{"instance":"humidity","unit":"unit.percent"},
	                 "state":{"instance":"humidity","value":45}}`

	batteryProp = `{"type":"devices.properties.float","retrievable":true,
	                "parameters":{"instance":"battery_level","unit":"unit.percent"},
	                "state":{"instance":"battery_level","value":15}}`
)

func TestBuildSpecKinds(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		override config.DeviceOverride
		wantKind Kind
		wantOK   bool
	}{
		{
			name:     "ceiling light subtype maps like a light",
			raw:      `{"id":"d","name":"Люстра","type":"devices.types.light.ceiling","capabilities":[` + onOffCap + `]}`,
			wantKind: KindLightbulb,
			wantOK:   true,
		},
		{
			name:     "socket",
			raw:      `{"id":"d","name":"Розетка","type":"devices.types.socket","capabilities":[` + onOffCap + `]}`,
			wantKind: KindOutlet,
			wantOK:   true,
		},
		{
			name:     "switch relay subtype",
			raw:      `{"id":"d","name":"Реле","type":"devices.types.switch.relay","capabilities":[` + onOffCap + `]}`,
			wantKind: KindSwitch,
			wantOK:   true,
		},
		{
			name:     "climate sensor",
			raw:      `{"id":"d","name":"Датчик","type":"devices.types.sensor.climate","properties":[` + temperatureProp + `]}`,
			wantKind: KindSensor,
			wantOK:   true,
		},
		{
			// The user's case: a dumb fan on a smart socket, exported as a
			// HomeKit Fan with nothing but on/off.
			name:     "socket overridden to a fan",
			raw:      `{"id":"d","name":"Розетка","type":"devices.types.socket","capabilities":[` + onOffCap + `]}`,
			override: config.DeviceOverride{Type: config.TypeFan},
			wantKind: KindFan,
			wantOK:   true,
		},
		{
			name:     "excluded device is skipped",
			raw:      `{"id":"d","name":"Розетка","type":"devices.types.socket","capabilities":[` + onOffCap + `]}`,
			override: config.DeviceOverride{Exclude: true},
			wantOK:   false,
		},
		{
			name:     "unknown type with on_off falls back to a switch",
			raw:      `{"id":"d","name":"Нечто","type":"devices.types.cooking.kettle","capabilities":[` + onOffCap + `]}`,
			wantKind: KindSwitch,
			wantOK:   true,
		},
		{
			name:   "unknown type with nothing usable is skipped",
			raw:    `{"id":"d","name":"Нечто","type":"devices.types.vacuum_cleaner"}`,
			wantOK: false,
		},
		{
			name:   "sensor with no mappable readings is skipped",
			raw:    `{"id":"d","name":"Датчик","type":"devices.types.sensor.gas"}`,
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, ok := BuildSpec(device(t, tt.raw), tt.override)
			if ok != tt.wantOK {
				t.Fatalf("BuildSpec ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && spec.Kind != tt.wantKind {
				t.Errorf("Kind = %q, want %q", spec.Kind, tt.wantKind)
			}
		})
	}
}

func TestBuildSpecBrightness(t *testing.T) {
	spec, ok := BuildSpec(device(t,
		`{"id":"d","name":"L","type":"devices.types.light","capabilities":[`+onOffCap+`,`+brightnessCap+`]}`),
		config.DeviceOverride{})
	if !ok {
		t.Fatal("BuildSpec returned false")
	}
	if spec.Brightness == nil {
		t.Fatal("Brightness is nil, want a range")
	}
	if spec.Brightness.Min != 1 || spec.Brightness.Max != 100 {
		t.Errorf("Brightness = %+v, want 1..100", spec.Brightness)
	}
}

// TestColorModeIsExclusive covers the Home app misbehaving when a Lightbulb
// exposes ColorTemperature alongside Hue and Saturation.
func TestColorModeIsExclusive(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		override   config.DeviceOverride
		wantColor  ColorSupport
		wantKelvin bool
	}{
		{
			name:      "device supporting both defaults to hue and saturation",
			raw:       `{"id":"d","name":"L","type":"devices.types.light","capabilities":[` + onOffCap + `,` + hsvCap + `]}`,
			wantColor: ColorHSV,
		},
		{
			name:       "override forces colour temperature",
			raw:        `{"id":"d","name":"L","type":"devices.types.light","capabilities":[` + onOffCap + `,` + hsvCap + `]}`,
			override:   config.DeviceOverride{ColorMode: config.ColorTemperature},
			wantColor:  ColorTemperature,
			wantKelvin: true,
		},
		{
			name:       "temperature-only device gets colour temperature",
			raw:        `{"id":"d","name":"L","type":"devices.types.light","capabilities":[` + onOffCap + `,` + temperatureOnlyCap + `]}`,
			wantColor:  ColorTemperature,
			wantKelvin: true,
		},
		{
			name:      "plain light has no colour controls",
			raw:       `{"id":"d","name":"L","type":"devices.types.light","capabilities":[` + onOffCap + `]}`,
			wantColor: ColorNone,
		},
		{
			// Asking for colour on a white-only lamp cannot be honoured, so
			// the temperature control is kept rather than dropping colour
			// support altogether.
			name:       "colour override on a temperature-only device falls back",
			raw:        `{"id":"d","name":"L","type":"devices.types.light","capabilities":[` + onOffCap + `,` + temperatureOnlyCap + `]}`,
			override:   config.DeviceOverride{ColorMode: config.ColorHSV},
			wantColor:  ColorTemperature,
			wantKelvin: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, ok := BuildSpec(device(t, tt.raw), tt.override)
			if !ok {
				t.Fatal("BuildSpec returned false")
			}
			if spec.Color != tt.wantColor {
				t.Errorf("Color = %q, want %q", spec.Color, tt.wantColor)
			}
			if (spec.Kelvin != nil) != tt.wantKelvin {
				t.Errorf("Kelvin = %+v, want present=%v", spec.Kelvin, tt.wantKelvin)
			}
		})
	}
}

func TestSensorsAttachToAnyKind(t *testing.T) {
	// A socket that also measures temperature stays one accessory rather than
	// becoming a socket plus a separate sensor tile.
	spec, ok := BuildSpec(device(t,
		`{"id":"d","name":"Розетка","type":"devices.types.socket","capabilities":[`+onOffCap+`],
		  "properties":[`+temperatureProp+`,`+humidityProp+`,`+batteryProp+`]}`),
		config.DeviceOverride{})
	if !ok {
		t.Fatal("BuildSpec returned false")
	}
	if spec.Kind != KindOutlet {
		t.Errorf("Kind = %q, want outlet", spec.Kind)
	}
	for _, want := range []SensorKind{SensorTemperature, SensorHumidity, SensorBattery} {
		if !spec.HasSensor(want) {
			t.Errorf("missing sensor %q", want)
		}
	}
}

func TestOverrideRenamesDevice(t *testing.T) {
	spec, ok := BuildSpec(device(t,
		`{"id":"d","name":"Розетка 3","type":"devices.types.socket","capabilities":[`+onOffCap+`]}`),
		config.DeviceOverride{Type: config.TypeFan, Name: "Вентилятор в спальне"})
	if !ok {
		t.Fatal("BuildSpec returned false")
	}
	if spec.Name != "Вентилятор в спальне" {
		t.Errorf("Name = %q, want the override", spec.Name)
	}
}

func TestUnnamedDeviceFallsBackToItsID(t *testing.T) {
	// hap refuses an accessory with an empty name, which would take down the
	// whole server rather than just one device.
	spec, ok := BuildSpec(device(t,
		`{"id":"device-42","type":"devices.types.socket","capabilities":[`+onOffCap+`]}`),
		config.DeviceOverride{})
	if !ok {
		t.Fatal("BuildSpec returned false")
	}
	if spec.Name != "device-42" {
		t.Errorf("Name = %q, want the device id as a fallback", spec.Name)
	}
}

func TestShapeHashIgnoresName(t *testing.T) {
	base := `{"id":"d","name":%q,"type":"devices.types.light","capabilities":[` + onOffCap + `]}`

	a, _ := BuildSpec(device(t, jsonf(base, "Лампа")), config.DeviceOverride{})
	b, _ := BuildSpec(device(t, jsonf(base, "Совсем другая лампа")), config.DeviceOverride{})

	// A rename must not count as a structural change: HomeKit keeps the user's
	// own name anyway, so rebuilding the server for it is pure disruption.
	if a.ShapeHash() != b.ShapeHash() {
		t.Errorf("ShapeHash changed on rename: %s vs %s", a.ShapeHash(), b.ShapeHash())
	}
}

func TestShapeHashTracksStructure(t *testing.T) {
	plain, _ := BuildSpec(device(t,
		`{"id":"d","name":"L","type":"devices.types.light","capabilities":[`+onOffCap+`]}`),
		config.DeviceOverride{})
	dimmable, _ := BuildSpec(device(t,
		`{"id":"d","name":"L","type":"devices.types.light","capabilities":[`+onOffCap+`,`+brightnessCap+`]}`),
		config.DeviceOverride{})

	if plain.ShapeHash() == dimmable.ShapeHash() {
		t.Error("ShapeHash is the same with and without brightness")
	}
}

func TestShapeHashIgnoresLiveValues(t *testing.T) {
	on := `{"id":"d","name":"L","type":"devices.types.light","capabilities":[
	        {"type":"devices.capabilities.on_off","retrievable":true,"state":{"instance":"on","value":true}}]}`
	off := `{"id":"d","name":"L","type":"devices.types.light","capabilities":[
	         {"type":"devices.capabilities.on_off","retrievable":true,"state":{"instance":"on","value":false}}]}`

	a, _ := BuildSpec(device(t, on), config.DeviceOverride{})
	b, _ := BuildSpec(device(t, off), config.DeviceOverride{})

	// Turning a lamp on must never look like a structural change; if it did,
	// the supervisor would rebuild the HAP server every time somebody used a
	// light switch.
	if a.ShapeHash() != b.ShapeHash() {
		t.Error("ShapeHash changed when the lamp was switched on")
	}
}

func TestTopologyHashIgnoresDeviceOrder(t *testing.T) {
	lamp := device(t, `{"id":"lamp","name":"L","type":"devices.types.light","capabilities":[`+onOffCap+`]}`)
	socket := device(t, `{"id":"socket","name":"S","type":"devices.types.socket","capabilities":[`+onOffCap+`]}`)

	cfg := config.Defaults()
	forward := TopologyHash(BuildSpecs([]yandex.Device{lamp, socket}, cfg))
	reverse := TopologyHash(BuildSpecs([]yandex.Device{socket, lamp}, cfg))

	// Yandex is under no obligation to keep its ordering stable, and a
	// reorder must never be mistaken for a topology change.
	if forward != reverse {
		t.Errorf("TopologyHash depends on order: %s vs %s", forward, reverse)
	}
}

func TestTopologyHashDetectsAddedAndRemovedDevices(t *testing.T) {
	lamp := device(t, `{"id":"lamp","name":"L","type":"devices.types.light","capabilities":[`+onOffCap+`]}`)
	socket := device(t, `{"id":"socket","name":"S","type":"devices.types.socket","capabilities":[`+onOffCap+`]}`)

	cfg := config.Defaults()
	one := TopologyHash(BuildSpecs([]yandex.Device{lamp}, cfg))
	two := TopologyHash(BuildSpecs([]yandex.Device{lamp, socket}, cfg))

	if one == two {
		t.Error("TopologyHash did not change when a device was added")
	}
}

func TestBuildSpecsSkipsExcluded(t *testing.T) {
	lamp := device(t, `{"id":"lamp","name":"L","type":"devices.types.light","capabilities":[`+onOffCap+`]}`)
	socket := device(t, `{"id":"socket","name":"S","type":"devices.types.socket","capabilities":[`+onOffCap+`]}`)

	cfg := config.Defaults()
	cfg.Devices = map[string]config.DeviceOverride{"socket": {Exclude: true}}

	specs := BuildSpecs([]yandex.Device{lamp, socket}, cfg)
	if len(specs) != 1 || specs[0].DeviceID != "lamp" {
		t.Errorf("specs = %+v, want only the lamp", specs)
	}
}

func jsonf(format string, args ...any) string {
	b, _ := json.Marshal(args[0])
	return replaceFirst(format, "%q", string(b))
}

func replaceFirst(s, old, new string) string {
	for i := 0; i+len(old) <= len(s); i++ {
		if s[i:i+len(old)] == old {
			return s[:i] + new + s[i+len(old):]
		}
	}
	return s
}
