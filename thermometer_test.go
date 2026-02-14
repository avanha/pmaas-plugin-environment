package environment

import (
	"testing"
	"time"

	spienvironment "pmaas.io/spi/environment"
	"pmaas.io/spi/tracking"
)

func TestThermometer_ImplementsExpectedInterfaces(t *testing.T) {
	tm := &Thermometer{}
	var _ tracking.Trackable = tm
}

func TestThermometer_TrackingConfig(t *testing.T) {
	// Arrange
	tm := &Thermometer{
		trackingConfig: tracking.Config{
			TrackingMode:        tracking.ModePoll,
			PollIntervalSeconds: 10,
		},
	}

	// Act
	cfg := tm.TrackingConfig()

	// Assert
	if cfg.TrackingMode != tracking.ModePoll {
		t.Fatalf("expected TrackingMode %v, got %v", tracking.ModePoll, cfg.TrackingMode)
	}

	if cfg.PollIntervalSeconds != 10 {
		t.Fatalf("expected PollIntervalSeconds %v, got %v", 10, cfg.PollIntervalSeconds)
	}
}

func TestThermometer_Data(t *testing.T) {
	// Arrange
	now := time.Now()
	var temperature float32 = 25.0
	tm := &Thermometer{
		SensorData: spienvironment.SensorData{
			LastUpdateTime: now,
			Temperature:    temperature,
		},
	}

	// Act
	data := tm.Data()

	// Assert
	if !data.LastUpdateTime.Equal(now) {
		t.Fatalf("expected LastUpdateTime %v, got %v", now, data.LastUpdateTime)
	}

	sensorData := data.Data.(spienvironment.SensorData)

	if sensorData.Temperature != temperature {
		t.Fatalf("expected Temperature %v, got %v", temperature, sensorData.Temperature)
	}

}
