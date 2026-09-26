package data

import (
	"reflect"
	"time"
)

type ThermostatData struct {
	Temperature float32 `track:"always"`
	HasHumidity bool
	Humidity    float32 `track:"always,nullable"`
	// HvacStatus is what the system is actually doing right now: "OFF", "HEATING", "COOLING".
	HvacStatus string `track:"always"`
	// Mode is the configured mode, independent of HvacStatus: "HEAT", "COOL", "HEATCOOL", "OFF".
	Mode           string    `track:"always"`
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

	// A setpoint is meaningful whenever its mode is enabled, regardless of whether the system is
	// actively running right now (HvacStatus) — a HEATCOOL-mode thermostat has both setpoints in effect
	// simultaneously even while idle.
	if sd.Mode == "HEAT" || sd.Mode == "HEATCOOL" {
		heatSetpoint = sd.HeatSetpoint
	}

	if sd.Mode == "COOL" || sd.Mode == "HEATCOOL" {
		coolSetpoint = sd.CoolSetpoint
	}

	return []any{sd.Temperature, humidity, sd.HvacStatus, sd.Mode, sd.EcoMode, heatSetpoint, coolSetpoint, sd.LastUpdateTime}, nil
}
