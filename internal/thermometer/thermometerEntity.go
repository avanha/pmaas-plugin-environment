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
	// extremesDate is the calendar day the High/Low values above belong to; they're reset when a
	// reading arrives on a different day.
	extremesDate   time.Time
	trackingConfig tracking.Config
}

const (
	extremeLowSentinel  = 1000
	extremeHighSentinel = -1000
)

func sameCalendarDay(a, b time.Time) bool {
	aYear, aMonth, aDay := a.Date()
	bYear, bMonth, bDay := b.Date()

	return aYear == bYear && aMonth == bMonth && aDay == bDay
}

// applySensorData folds a newly received reading into the entity and reports whether the
// temperature and humidity values actually changed. now is when the reading was received; it
// drives the daily extremes. Unlike the old per-change handling:
//   - LastUpdateTime advances on every reading, so a steady value isn't mistaken for a dead
//     sensor. It uses the producer's own timestamp when it supplied a plausible one, and now
//     otherwise.
//   - The very first reading always counts as a change, so a legitimate 0 degrees / 0% isn't
//     swallowed by the zero-value initial state.
//   - The high/low extremes reset when the calendar day changes (full date, not day-of-month),
//     and are checked on every reading rather than only when a value changes.
//
// An entirely empty incoming reading (producer has no data yet) is ignored.
func (t *Thermometer) applySensorData(incoming spienvironment.SensorData, now time.Time) (temperatureUpdated, humidityUpdated bool) {
	t.SensorData.HasHumidity = incoming.HasHumidity

	if incoming.IsEmpty() {
		return false, false
	}

	firstReading := t.SensorData.LastUpdateTime.IsZero()

	if !sameCalendarDay(t.extremesDate, now) {
		t.HighTemperature = extremeHighSentinel
		t.LowTemperature = extremeLowSentinel
		t.HighHumidity = extremeHighSentinel
		t.LowHumidity = extremeLowSentinel
		t.HighTemperatureTime = time.Time{}
		t.LowTemperatureTime = time.Time{}
		t.HighHumidityTime = time.Time{}
		t.LowHumidityTime = time.Time{}
		t.extremesDate = now
	}

	readingTime := incoming.LastUpdateTime

	if readingTime.IsZero() || readingTime.After(now) {
		readingTime = now
	}

	temperatureUpdated = firstReading || t.SensorData.Temperature != incoming.Temperature
	humidityUpdated = incoming.HasHumidity && (firstReading || t.SensorData.Humidity != incoming.Humidity)

	t.SensorData.Temperature = incoming.Temperature
	t.SensorData.Humidity = incoming.Humidity
	t.SensorData.LastUpdateTime = readingTime

	if incoming.Temperature > t.HighTemperature {
		t.HighTemperature = incoming.Temperature
		t.HighTemperatureTime = now
	}

	if incoming.Temperature < t.LowTemperature {
		t.LowTemperature = incoming.Temperature
		t.LowTemperatureTime = now
	}

	if incoming.HasHumidity {
		if incoming.Humidity > t.HighHumidity {
			t.HighHumidity = incoming.Humidity
			t.HighHumidityTime = now
		}

		if incoming.Humidity < t.LowHumidity {
			t.LowHumidity = incoming.Humidity
			t.LowHumidityTime = now
		}
	}

	return temperatureUpdated, humidityUpdated
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
