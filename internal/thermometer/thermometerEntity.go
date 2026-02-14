package thermometer

import (
	"time"

	"github.com/avanha/pmaas-plugin-environment/data"
	"github.com/avanha/pmaas-plugin-environment/internal/wrapper"
	spienvironment "github.com/avanha/pmaas-spi/environment"
	"github.com/avanha/pmaas-spi/tracking"
)

func CreateThermometer(trackingConfig tracking.Config) *Thermometer {
	return &Thermometer{
		trackingConfig: trackingConfig,
	}
}

type Thermometer struct {
	wrapper.WrappedEntity
	spienvironment.SensorData
	HighTemperature     float32
	HighTemperatureTime time.Time
	LowTemperature      float32
	LowTemperatureTime  time.Time
	HighHumidity        float32
	HighHumidityTime    time.Time
	LowHumidity         float32
	LowHumidityTime     time.Time
	trackingConfig      tracking.Config
}

func (t *Thermometer) TrackingConfig() tracking.Config {
	return t.trackingConfig
}

func (t *Thermometer) Data() tracking.DataSample {
	return tracking.DataSample{
		LastUpdateTime: t.SensorData.LastUpdateTime,
		Data: data.ThermometerData{
			Temperature:    t.SensorData.Temperature,
			HasHumidity:    t.SensorData.HasHumidity,
			Humidity:       t.SensorData.Humidity,
			LastUpdateTime: t.SensorData.LastUpdateTime,
		},
	}
}
