package data

import (
	"reflect"
	"time"
)

type ThermostatData struct {
	Temperature    float32 `track:"always"`
	HasHumidity    bool
	Humidity       float32   `track:"always,nullable"`
	HvacStatus     string    `track:"always"`
	EcoMode        string    `track:"always"`
	HeatSetpoint   float32   `track:"onchange,nullable"`
	CoolSetpoint   float32   `track:"onchange,nullable"`
	LastUpdateTime time.Time `track:"always"`
}

var ThermostatDataType = reflect.TypeOf((*ThermostatData)(nil)).Elem()

func ThermostatDataToInsertArgs(anyData *any) ([]any, error) {
	sd := (*anyData).(ThermostatData)
	var humidity any = nil
	var heatSetpoint any = nil
	var coolSetpoint any = nil

	if sd.HasHumidity {
		humidity = sd.Humidity
	}

	// Only the setpoint matching the currently active HVAC action is meaningful — a HEATCOOL-mode
	// thermostat sitting idle could report both, but neither is actually "in effect" right now.
	switch sd.HvacStatus {
	case "HEATING":
		heatSetpoint = sd.HeatSetpoint
	case "COOLING":
		coolSetpoint = sd.CoolSetpoint
	}

	return []any{sd.Temperature, humidity, sd.HvacStatus, sd.EcoMode, heatSetpoint, coolSetpoint, sd.LastUpdateTime}, nil
}
