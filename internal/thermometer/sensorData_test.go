package thermometer

import (
	"testing"
	"time"

	spienvironment "github.com/avanha/pmaas-spi/environment"
	"github.com/avanha/pmaas-spi/tracking"
)

func TestApplySensorData_FirstReadingOfZeroCountsAsChange(t *testing.T) {
	tm := CreateThermometer(tracking.Config{})
	now := time.Now()

	tempUpdated, humUpdated := tm.applySensorData(
		spienvironment.SensorData{HasData: true, Temperature: 0, HasHumidity: true, Humidity: 0, LastUpdateTime: now}, now)

	if !tempUpdated || !humUpdated {
		t.Fatalf("expected first reading to count as change, got temp=%v hum=%v", tempUpdated, humUpdated)
	}
	if tm.SensorData.IsEmpty() {
		t.Fatalf("expected sensor data to be recorded")
	}
}

func TestApplySensorData_UnchangedReadingStillRefreshesTimestamp(t *testing.T) {
	tm := CreateThermometer(tracking.Config{})
	t0 := time.Now().Add(-2 * time.Hour)
	tm.applySensorData(spienvironment.SensorData{HasData: true, Temperature: 21, LastUpdateTime: t0}, t0)

	t1 := time.Now()
	tempUpdated, _ := tm.applySensorData(spienvironment.SensorData{HasData: true, Temperature: 21, LastUpdateTime: t1}, t1)

	if tempUpdated {
		t.Fatalf("expected no temperature change")
	}
	if !tm.SensorData.LastUpdateTime.Equal(t1) {
		t.Fatalf("expected LastUpdateTime %v, got %v", t1, tm.SensorData.LastUpdateTime)
	}
}

func TestApplySensorData_EmptyReadingIgnored(t *testing.T) {
	tm := CreateThermometer(tracking.Config{})

	// Non-zero-looking values are still ignored without HasData: this is what a producer that hasn't
	// heard from its device yet publishes.
	tempUpdated, humUpdated := tm.applySensorData(
		spienvironment.SensorData{HasHumidity: true, LastUpdateTime: time.Now()}, time.Now())

	if tempUpdated || humUpdated || !tm.SensorData.IsEmpty() {
		t.Fatalf("expected reading without HasData to be ignored")
	}
}

func TestApplySensorData_ExtremesResetOnNewMonthSameDayOfMonth(t *testing.T) {
	tm := CreateThermometer(tracking.Config{})
	d1 := time.Date(2026, 1, 5, 12, 0, 0, 0, time.Local)
	d2 := time.Date(2026, 2, 5, 12, 0, 0, 0, time.Local)

	tm.applySensorData(spienvironment.SensorData{HasData: true, Temperature: 30, LastUpdateTime: d1}, d1)
	// Same temperature next month: the old extremes must not survive, and must be re-seeded.
	tm.applySensorData(spienvironment.SensorData{HasData: true, Temperature: 20, LastUpdateTime: d2}, d2)

	if tm.HighTemperature != 20 || tm.LowTemperature != 20 {
		t.Fatalf("expected extremes reset to 20/20, got high=%v low=%v", tm.HighTemperature, tm.LowTemperature)
	}
}

func TestApplySensorData_ExtremesTrackedOnUnchangedReadings(t *testing.T) {
	tm := CreateThermometer(tracking.Config{})
	now := time.Now()

	tm.applySensorData(spienvironment.SensorData{HasData: true, Temperature: 22, LastUpdateTime: now}, now)
	tm.applySensorData(spienvironment.SensorData{HasData: true, Temperature: 22, LastUpdateTime: now}, now)

	if tm.HighTemperature != 22 || tm.LowTemperature != 22 {
		t.Fatalf("expected extremes 22/22, got high=%v low=%v", tm.HighTemperature, tm.LowTemperature)
	}
}
