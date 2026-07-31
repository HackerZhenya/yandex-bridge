package bridge

import (
	"encoding/json"
	"slices"
	"strings"
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

// home wraps devices in the response shape BuildSpecs consumes.
func home(devices ...yandex.Device) *yandex.UserInfo {
	return &yandex.UserInfo{Devices: devices}
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

	// A kettle's settable target: 40-100 °C in steps of 10.
	targetTempCap = `{"type":"devices.capabilities.range","retrievable":true,
	                  "parameters":{"instance":"temperature","unit":"unit.temperature.celsius",
	                                "random_access":true,"range":{"min":40,"max":100,"precision":10}},
	                  "state":{"instance":"temperature","value":80}}`

	keepWarmToggle = `{"type":"devices.capabilities.toggle","retrievable":true,
	                   "parameters":{"instance":"keep_warm"},
	                   "state":{"instance":"keep_warm","value":true}}`

	backlightToggle = `{"type":"devices.capabilities.toggle","retrievable":true,
	                    "parameters":{"instance":"backlight"},
	                    "state":{"instance":"backlight","value":false}}`

	teaModeCap = `{"type":"devices.capabilities.mode","retrievable":true,
	               "parameters":{"instance":"tea_mode","modes":[{"value":"black_tea"}]},
	               "state":{"instance":"tea_mode","value":"black_tea"}}`

	temperatureProp = `{"type":"devices.properties.float","retrievable":true,
	                    "parameters":{"instance":"temperature","unit":"unit.temperature.celsius"},
	                    "state":{"instance":"temperature","value":21.5}}`

	humidityProp = `{"type":"devices.properties.float","retrievable":true,
	                 "parameters":{"instance":"humidity","unit":"unit.percent"},
	                 "state":{"instance":"humidity","value":45}}`

	batteryProp = `{"type":"devices.properties.float","retrievable":true,
	                "parameters":{"instance":"battery_level","unit":"unit.percent"},
	                "state":{"instance":"battery_level","value":15}}`

	waterLevelProp = `{"type":"devices.properties.float","retrievable":true,
	                   "parameters":{"instance":"water_level","unit":"unit.percent"},
	                   "state":{"instance":"water_level","value":70}}`
)

// kettleJSON is the device this whole feature exists for.
const kettleJSON = `{"id":"kettle","name":"Чайник","type":"devices.types.cooking.kettle",
	"capabilities":[` + onOffCap + `,` + targetTempCap + `],
	"properties":[` + temperatureProp + `]}`

func buildSpec(t *testing.T, raw string, o config.DeviceOverride) (Spec, MappingReport, bool) {
	t.Helper()
	return BuildSpec(device(t, raw), o, true)
}

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
			// A dumb fan on a smart socket, exported as a HomeKit Fan with
			// nothing but on/off.
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
			// Without a temperature range there is nothing to put on a
			// thermostat dial, so a plain switch is the honest mapping.
			name:     "kettle without a temperature range is a switch",
			raw:      `{"id":"d","name":"Чайник","type":"devices.types.cooking.kettle","capabilities":[` + onOffCap + `]}`,
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
			spec, _, ok := buildSpec(t, tt.raw, tt.override)
			if ok != tt.wantOK {
				t.Fatalf("BuildSpec ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && spec.Kind != tt.wantKind {
				t.Errorf("Kind = %q, want %q", spec.Kind, tt.wantKind)
			}
		})
	}
}

// TestKettleBecomesOneThermostat is the mapping the whole feature exists for:
// on/off, a target temperature and the current reading in a single service, so
// the Home app shows one tile instead of a switch plus a thermometer.
func TestKettleBecomesOneThermostat(t *testing.T) {
	spec, _, ok := buildSpec(t, kettleJSON, config.DeviceOverride{})
	if !ok {
		t.Fatal("BuildSpec returned false")
	}
	if spec.Kind != KindThermostat {
		t.Fatalf("Kind = %q, want thermostat", spec.Kind)
	}
	if spec.Target == nil {
		t.Fatal("Target is nil, want the 40..100 range")
	}
	if spec.Target.Min != 40 || spec.Target.Max != 100 || spec.Target.Precision != 10 {
		t.Errorf("Target = %+v, want 40..100 step 10", spec.Target)
	}
	// CurrentTemperature lives inside the Thermostat service; adding a sensor
	// service too would put the kettle back to two tiles.
	if spec.HasSensor(SensorTemperature) {
		t.Error("a separate TemperatureSensor was added alongside the thermostat")
	}
}

func TestKettleCanBeForcedBackToSwitchAndSensor(t *testing.T) {
	spec, _, ok := buildSpec(t, kettleJSON, config.DeviceOverride{Type: config.TypeSwitch})
	if !ok {
		t.Fatal("BuildSpec returned false")
	}
	if spec.Kind != KindSwitch {
		t.Errorf("Kind = %q, want switch", spec.Kind)
	}
	if !spec.HasSensor(SensorTemperature) {
		t.Error("the temperature reading was dropped instead of becoming a sensor")
	}
	if spec.Target != nil {
		t.Error("a switch should not carry a target temperature")
	}
}

// TestAirConditionerIsNotATermostat guards the one device that also carries a
// temperature range but must not be presented as heat-only.
func TestAirConditionerIsNotAThermostat(t *testing.T) {
	spec, _, ok := buildSpec(t,
		`{"id":"ac","name":"Кондиционер","type":"devices.types.thermostat.ac",
		  "capabilities":[`+onOffCap+`,`+targetTempCap+`]}`,
		config.DeviceOverride{})
	if !ok {
		t.Fatal("BuildSpec returned false")
	}
	if spec.Kind == KindThermostat {
		t.Error("an air conditioner was mapped as a heat-only thermostat")
	}
}

func TestThermostatOverrideNeedsATemperatureRange(t *testing.T) {
	// Forcing the type cannot conjure a range; a dial that does nothing would
	// be worse than a switch.
	spec, _, ok := buildSpec(t,
		`{"id":"d","name":"Штука","type":"devices.types.other","capabilities":[`+onOffCap+`]}`,
		config.DeviceOverride{Type: config.TypeThermostat})
	if !ok {
		t.Fatal("BuildSpec returned false")
	}
	if spec.Kind != KindSwitch {
		t.Errorf("Kind = %q, want switch as a fallback", spec.Kind)
	}
}

func TestTogglesBecomeSwitches(t *testing.T) {
	spec, _, ok := buildSpec(t,
		`{"id":"kettle","name":"Чайник","type":"devices.types.cooking.kettle",
		  "capabilities":[`+onOffCap+`,`+targetTempCap+`,`+keepWarmToggle+`,`+backlightToggle+`]}`,
		config.DeviceOverride{})
	if !ok {
		t.Fatal("BuildSpec returned false")
	}
	if len(spec.Toggles) != 2 {
		t.Fatalf("Toggles = %+v, want 2", spec.Toggles)
	}
	// Sorted by instance so the service order — and the instance ids hap
	// derives from it — stays stable across restarts.
	if spec.Toggles[0].Instance != "backlight" || spec.Toggles[1].Instance != "keep_warm" {
		t.Errorf("Toggles are not sorted by instance: %+v", spec.Toggles)
	}
	if spec.Toggles[0].Name != "Подсветка" {
		t.Errorf("backlight name = %q, want a readable label", spec.Toggles[0].Name)
	}
}

func TestHiddenTogglesAreDropped(t *testing.T) {
	spec, _, ok := buildSpec(t,
		`{"id":"kettle","name":"Чайник","type":"devices.types.cooking.kettle",
		  "capabilities":[`+onOffCap+`,`+targetTempCap+`,`+keepWarmToggle+`,`+backlightToggle+`]}`,
		config.DeviceOverride{HideToggles: []string{"backlight"}})
	if !ok {
		t.Fatal("BuildSpec returned false")
	}
	if len(spec.Toggles) != 1 || spec.Toggles[0].Instance != "keep_warm" {
		t.Errorf("Toggles = %+v, want only keep_warm", spec.Toggles)
	}
}

func TestTogglesCanBeDisabledGlobally(t *testing.T) {
	spec, _, ok := BuildSpec(device(t,
		`{"id":"kettle","name":"Чайник","type":"devices.types.cooking.kettle",
		  "capabilities":[`+onOffCap+`,`+targetTempCap+`,`+keepWarmToggle+`]}`),
		config.DeviceOverride{}, false)
	if !ok {
		t.Fatal("BuildSpec returned false")
	}
	if len(spec.Toggles) != 0 {
		t.Errorf("Toggles = %+v, want none when expose_toggles is off", spec.Toggles)
	}
}

func TestUnknownToggleKeepsItsRawName(t *testing.T) {
	spec, _, ok := buildSpec(t,
		`{"id":"d","name":"Штука","type":"devices.types.other","capabilities":[`+onOffCap+`,
		  {"type":"devices.capabilities.toggle","parameters":{"instance":"warp_drive"},
		   "state":{"instance":"warp_drive","value":false}}]}`,
		config.DeviceOverride{})
	if !ok {
		t.Fatal("BuildSpec returned false")
	}
	// Dropping it would silently lose a control the user can see in the
	// Yandex app.
	if len(spec.Toggles) != 1 || spec.Toggles[0].Name != "warp_drive" {
		t.Errorf("Toggles = %+v, want the raw instance as a label", spec.Toggles)
	}
}

// TestMappingReportNamesWhatIsLost is what makes config.yaml writable: an
// unsupported capability is otherwise invisible.
func TestMappingReportNamesWhatIsLost(t *testing.T) {
	_, report, ok := buildSpec(t,
		`{"id":"kettle","name":"Чайник","type":"devices.types.cooking.kettle",
		  "capabilities":[`+onOffCap+`,`+targetTempCap+`,`+teaModeCap+`],
		  "properties":[`+temperatureProp+`,`+waterLevelProp+`]}`,
		config.DeviceOverride{})
	if !ok {
		t.Fatal("BuildSpec returned false")
	}

	joinedUnmapped := strings.Join(report.Unmapped, " ")
	for _, want := range []string{"devices.capabilities.mode", "float:water_level"} {
		if !strings.Contains(joinedUnmapped, want) {
			t.Errorf("unmapped = %v, want it to mention %q", report.Unmapped, want)
		}
	}

	joinedMapped := strings.Join(report.Mapped, " ")
	for _, want := range []string{"on_off", "range:temperature", "float:temperature"} {
		if !strings.Contains(joinedMapped, want) {
			t.Errorf("mapped = %v, want it to mention %q", report.Mapped, want)
		}
	}
}

// buttonJSON is a Zigbee wall button as Yandex reports it: typed as a switch,
// but with no on_off at all — only an identify capability and a button event.
const buttonJSON = `{"id":"button","name":"Левая нижняя кнопка","type":"devices.types.switch",
	"capabilities":[{"type":"devices.capabilities.identify","retrievable":false,
	                 "parameters":{"instance":"identify"}}],
	"properties":[{"type":"devices.properties.event","retrievable":false,
	               "parameters":{"instance":"button","events":[{"value":"click"}]},
	               "state":{"instance":"button","value":"click"}}]}`

// TestButtonIsNotExportedAsADeadSwitch is a regression: Yandex types a button
// as devices.types.switch, and mapping on device type alone produced a HomeKit
// toggle with nothing behind it — it controlled nothing and never updated.
func TestButtonIsNotExportedAsADeadSwitch(t *testing.T) {
	spec, report, ok := buildSpec(t, buttonJSON, config.DeviceOverride{})
	if ok {
		t.Fatalf("a button with no on_off was exported as %q", spec.Kind)
	}
	// The reason has to name the cause, or the device just looks arbitrarily
	// missing from the Home app.
	if !strings.Contains(report.Reason, "button") {
		t.Errorf("reason = %q, want it to identify the device as a button", report.Reason)
	}
}

func TestOverrideCannotConjureASwitch(t *testing.T) {
	// Forcing a type does not give the device an on_off capability, and a
	// control backed by nothing is worse than no control.
	_, _, ok := buildSpec(t, buttonJSON, config.DeviceOverride{Type: config.TypeSwitch})
	if ok {
		t.Error("a type override produced a switch on a device with no on_off")
	}
}

// TestSkippedDeviceStillListsItsFeatures is what makes the inventory useful for
// deciding whether a device is worth supporting.
func TestSkippedDeviceStillListsItsFeatures(t *testing.T) {
	_, report, ok := buildSpec(t, buttonJSON, config.DeviceOverride{})
	if ok {
		t.Fatal("BuildSpec exported the button")
	}

	joined := strings.Join(report.Unmapped, " ")
	for _, want := range []string{"identify", "button"} {
		if !strings.Contains(joined, want) {
			t.Errorf("unmapped = %v, want it to mention %q", report.Unmapped, want)
		}
	}
}

// TestUndocumentedCapabilitiesCarryTheirInstance matters because Yandex adds
// capability types it does not document — identify and zigbee_node appear on
// Zigbee devices — and the type name alone is not enough to act on.
func TestUndocumentedCapabilitiesCarryTheirInstance(t *testing.T) {
	_, report, ok := buildSpec(t,
		`{"id":"lamp","name":"Люстра","type":"devices.types.light","capabilities":[`+onOffCap+`,
		  {"type":"devices.capabilities.identify","parameters":{"instance":"identify"}},
		  {"type":"devices.capabilities.zigbee_node","parameters":{"instance":"node"}}]}`,
		config.DeviceOverride{})
	if !ok {
		t.Fatal("BuildSpec returned false")
	}

	joined := strings.Join(report.Unmapped, " ")
	if !strings.Contains(joined, "devices.capabilities.identify:identify") {
		t.Errorf("unmapped = %v, want the instance alongside the type", report.Unmapped)
	}
	if !strings.Contains(joined, "zigbee_node:node") {
		t.Errorf("unmapped = %v, want zigbee_node with its instance", report.Unmapped)
	}
}

func TestExcludedDeviceStillListsItsFeatures(t *testing.T) {
	_, report, _ := buildSpec(t,
		`{"id":"d","name":"Розетка","type":"devices.types.socket","capabilities":[`+onOffCap+`]}`,
		config.DeviceOverride{Exclude: true})

	// Knowing what an excluded device offers is how you decide to un-exclude it.
	if len(report.Unmapped) == 0 {
		t.Error("an excluded device reported nothing about its capabilities")
	}
}

func TestSkippedDeviceCarriesAReason(t *testing.T) {
	_, report, ok := buildSpec(t,
		`{"id":"d","name":"Пылесос","type":"devices.types.vacuum_cleaner"}`,
		config.DeviceOverride{})
	if ok {
		t.Fatal("BuildSpec exported a device it cannot map")
	}
	if !report.Skipped || report.Reason == "" {
		t.Errorf("report = %+v, want skipped with a reason", report)
	}
}

func TestExcludedDeviceSaysSo(t *testing.T) {
	_, report, _ := buildSpec(t,
		`{"id":"d","name":"Розетка","type":"devices.types.socket","capabilities":[`+onOffCap+`]}`,
		config.DeviceOverride{Exclude: true})
	if !strings.Contains(report.Reason, "config") {
		t.Errorf("reason = %q, want it to point at the config", report.Reason)
	}
}

func TestBuildSpecBrightness(t *testing.T) {
	spec, _, ok := buildSpec(t,
		`{"id":"d","name":"L","type":"devices.types.light","capabilities":[`+onOffCap+`,`+brightnessCap+`]}`,
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
			spec, _, ok := buildSpec(t, tt.raw, tt.override)
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
	spec, _, ok := buildSpec(t,
		`{"id":"d","name":"Розетка","type":"devices.types.socket","capabilities":[`+onOffCap+`],
		  "properties":[`+temperatureProp+`,`+humidityProp+`,`+batteryProp+`]}`,
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
	spec, _, ok := buildSpec(t,
		`{"id":"d","name":"Розетка 3","type":"devices.types.socket","capabilities":[`+onOffCap+`]}`,
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
	spec, _, ok := buildSpec(t,
		`{"id":"device-42","type":"devices.types.socket","capabilities":[`+onOffCap+`]}`,
		config.DeviceOverride{})
	if !ok {
		t.Fatal("BuildSpec returned false")
	}
	if spec.Name != "device-42" {
		t.Errorf("Name = %q, want the device id as a fallback", spec.Name)
	}
}

func TestShapeHashIgnoresName(t *testing.T) {
	a, _, _ := buildSpec(t,
		`{"id":"d","name":"Лампа","type":"devices.types.light","capabilities":[`+onOffCap+`]}`,
		config.DeviceOverride{})
	b, _, _ := buildSpec(t,
		`{"id":"d","name":"Совсем другая лампа","type":"devices.types.light","capabilities":[`+onOffCap+`]}`,
		config.DeviceOverride{})

	// A rename must not count as a structural change: HomeKit keeps the user's
	// own name anyway, so rebuilding the server for it is pure disruption.
	if a.ShapeHash() != b.ShapeHash() {
		t.Errorf("ShapeHash changed on rename: %s vs %s", a.ShapeHash(), b.ShapeHash())
	}
}

func TestShapeHashTracksStructure(t *testing.T) {
	plain, _, _ := buildSpec(t,
		`{"id":"d","name":"L","type":"devices.types.light","capabilities":[`+onOffCap+`]}`,
		config.DeviceOverride{})
	dimmable, _, _ := buildSpec(t,
		`{"id":"d","name":"L","type":"devices.types.light","capabilities":[`+onOffCap+`,`+brightnessCap+`]}`,
		config.DeviceOverride{})

	if plain.ShapeHash() == dimmable.ShapeHash() {
		t.Error("ShapeHash is the same with and without brightness")
	}
}

// TestShapeHashTracksToggles matters because a toggle appearing or being
// hidden changes the service list, and hap numbers instance ids by position.
func TestShapeHashTracksToggles(t *testing.T) {
	raw := `{"id":"kettle","name":"Чайник","type":"devices.types.cooking.kettle",
	         "capabilities":[` + onOffCap + `,` + targetTempCap + `,` + keepWarmToggle + `]}`

	with, _, _ := buildSpec(t, raw, config.DeviceOverride{})
	without, _, _ := buildSpec(t, raw, config.DeviceOverride{HideToggles: []string{"keep_warm"}})

	if with.ShapeHash() == without.ShapeHash() {
		t.Error("hiding a toggle did not change ShapeHash")
	}
}

func TestShapeHashTracksTargetTemperatureRange(t *testing.T) {
	narrow, _, _ := buildSpec(t, kettleJSON, config.DeviceOverride{})
	wide, _, _ := buildSpec(t,
		`{"id":"kettle","name":"Чайник","type":"devices.types.cooking.kettle","capabilities":[`+onOffCap+`,
		  {"type":"devices.capabilities.range","retrievable":true,
		   "parameters":{"instance":"temperature","unit":"unit.temperature.celsius",
		                 "range":{"min":30,"max":100,"precision":5}},
		   "state":{"instance":"temperature","value":80}}]}`,
		config.DeviceOverride{})

	// The range drives the characteristic's advertised bounds, which HomeKit
	// caches.
	if narrow.ShapeHash() == wide.ShapeHash() {
		t.Error("a different target temperature range did not change ShapeHash")
	}
}

func TestShapeHashIgnoresLiveValues(t *testing.T) {
	on := `{"id":"d","name":"L","type":"devices.types.light","capabilities":[
	        {"type":"devices.capabilities.on_off","retrievable":true,"state":{"instance":"on","value":true}}]}`
	off := `{"id":"d","name":"L","type":"devices.types.light","capabilities":[
	         {"type":"devices.capabilities.on_off","retrievable":true,"state":{"instance":"on","value":false}}]}`

	a, _, _ := buildSpec(t, on, config.DeviceOverride{})
	b, _, _ := buildSpec(t, off, config.DeviceOverride{})

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
	forwardSpecs, _ := BuildSpecs(home(lamp, socket), cfg)
	reverseSpecs, _ := BuildSpecs(home(socket, lamp), cfg)

	// Yandex is under no obligation to keep its ordering stable, and a
	// reorder must never be mistaken for a topology change.
	if TopologyHash(forwardSpecs) != TopologyHash(reverseSpecs) {
		t.Error("TopologyHash depends on device order")
	}
}

func TestTopologyHashDetectsAddedAndRemovedDevices(t *testing.T) {
	lamp := device(t, `{"id":"lamp","name":"L","type":"devices.types.light","capabilities":[`+onOffCap+`]}`)
	socket := device(t, `{"id":"socket","name":"S","type":"devices.types.socket","capabilities":[`+onOffCap+`]}`)

	cfg := config.Defaults()
	one, _ := BuildSpecs(home(lamp), cfg)
	two, _ := BuildSpecs(home(lamp, socket), cfg)

	if TopologyHash(one) == TopologyHash(two) {
		t.Error("TopologyHash did not change when a device was added")
	}
}

func TestBuildSpecsSkipsExcludedButStillReportsIt(t *testing.T) {
	lamp := device(t, `{"id":"lamp","name":"L","type":"devices.types.light","capabilities":[`+onOffCap+`]}`)
	socket := device(t, `{"id":"socket","name":"S","type":"devices.types.socket","capabilities":[`+onOffCap+`]}`)

	cfg := config.Defaults()
	cfg.Devices = map[string]config.DeviceOverride{"socket": {Exclude: true}}

	specs, reports := BuildSpecs(home(lamp, socket), cfg)
	if len(specs) != 1 || specs[0].DeviceID != "lamp" {
		t.Errorf("specs = %+v, want only the lamp", specs)
	}
	// The report covers every device, so an excluded one does not simply
	// vanish from the inventory.
	if len(reports) != 2 {
		t.Errorf("reports = %d, want one per device", len(reports))
	}
	if !slices.ContainsFunc(reports, func(r MappingReport) bool {
		return r.DeviceID == "socket" && r.Skipped
	}) {
		t.Error("the excluded device is missing from the report")
	}
}

func TestBuildSpecsResolvesRoomNames(t *testing.T) {
	lamp := device(t, `{"id":"lamp","name":"L","type":"devices.types.light","room":"r1","capabilities":[`+onOffCap+`]}`)
	info := &yandex.UserInfo{
		Rooms:   []yandex.Room{{ID: "r1", Name: "Гостиная"}},
		Devices: []yandex.Device{lamp},
	}

	specs, reports := BuildSpecs(info, config.Defaults())
	if len(specs) != 1 || specs[0].Room != "Гостиная" {
		t.Errorf("spec room = %q, want Гостиная", specs[0].Room)
	}
	if reports[0].Room != "Гостиная" {
		t.Errorf("report room = %q, want Гостиная", reports[0].Room)
	}
}
