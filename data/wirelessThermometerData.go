package data

import (
	"reflect"
	"time"
)

type WirelessThermometerData struct {
	Temperature    float32 `track:"always"`
	HasHumidity    bool
	Humidity       float32   `track:"always,nullable"`
	BatteryLevel   int32     `track:"onchange,nullable"`
	RSSI           int32     `track:"always,nullable"`
	LastUpdateTime time.Time `track:"always"`
}

var WirelessThermometerDataType = reflect.TypeOf((*WirelessThermometerData)(nil)).Elem()

func WirelessThermometerDataToInsertArgs(anyData *any) ([]any, error) {
	sd := (*anyData).(WirelessThermometerData)
	var humidity any = nil
	var batteryLevel any = nil
	var rssi any = nil

	if sd.HasHumidity {
		humidity = sd.Humidity
	}

	if sd.BatteryLevel != 0 {
		batteryLevel = sd.BatteryLevel
	}

	if sd.RSSI != 0 {
		rssi = sd.RSSI
	}

	return []any{sd.Temperature, humidity, batteryLevel, rssi, sd.LastUpdateTime}, nil
}
