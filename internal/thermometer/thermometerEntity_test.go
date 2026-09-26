package thermometer

import (
	"testing"
	"time"

	"github.com/avanha/pmaas-plugin-environment/data"
	spienvironment "github.com/avanha/pmaas-spi/environment"
	"github.com/avanha/pmaas-spi/tracking"
)

func TestThermometer_ImplementsExpectedInterfaces(t *testing.T) {
	tm := CreateThermometer(tracking.Config{})
	var _ tracking.Trackable = tm
}

func TestThermometer_TrackingConfig(t *testing.T) {
	// Arrange
	tm := CreateThermometer(tracking.Config{
		TrackingMode:        tracking.ModePoll,
		PollIntervalSeconds: 10,
	})

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
	tm := CreateThermometer(tracking.Config{})
	tm.SensorData = spienvironment.SensorData{
		LastUpdateTime: now,
		Temperature:    temperature,
	}

	// Act
	dataSample := tm.Data()

	// Assert
	if !dataSample.LastUpdateTime.Equal(now) {
		t.Fatalf("expected LastUpdateTime %v, got %v", now, dataSample.LastUpdateTime)
	}

	thermometerData := dataSample.Data.(data.ThermometerData)

	if thermometerData.Temperature != temperature {
		t.Fatalf("expected Temperature %v, got %v", temperature, thermometerData.Temperature)
	}
}
