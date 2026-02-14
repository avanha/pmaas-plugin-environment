package data

import "time"

type ThermometerData struct {
	Temperature    float32 `track:"always"`
	HasHumidity    bool
	Humidity       float32   `track:"always,nullable"`
	LastUpdateTime time.Time `track:"always"`
}

func ThermometerDataToInsertArgs(anyData *any) ([]any, error) {
	sd := (*anyData).(ThermometerData)
	var humidity any = nil

	if sd.HasHumidity {
		humidity = sd.Humidity
	}

	return []any{sd.Temperature, humidity, sd.LastUpdateTime}, nil
}
