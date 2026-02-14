package thermometer

import (
	"reflect"
	"testing"
	"time"

	data "github.com/avanha/pmaas-plugin-environment/data"
	"github.com/avanha/pmaas-spi/tracking"
)

func TestWirelessThermometer_TrackingConfig(t *testing.T) {
	// Arrange
	tm := CreateWirelessThermometer(
		1,
		"targetEntityId",
		"name",
		reflect.TypeOf((*WirelessThermometer)(nil)).Elem(),
		tracking.Config{
			TrackingMode:        tracking.ModePoll,
			PollIntervalSeconds: 10,
		})

	expectedTrackingConfig := tracking.Config{
		TrackingMode:        tracking.ModePoll,
		PollIntervalSeconds: 10,
	}

	// Act
	result := tm.TrackingConfig()

	// Assert
	if result.TrackingMode != expectedTrackingConfig.TrackingMode ||
		result.PollIntervalSeconds != expectedTrackingConfig.PollIntervalSeconds {
		t.Fatalf("expected %+v, got %+v", expectedTrackingConfig, result)
	}
}

func TestWirelessThermometer_Data(t *testing.T) {
	// Arrange
	now := time.Now()
	tm := CreateWirelessThermometer(1,
		"targetEntityId",
		"name",
		reflect.TypeOf((*WirelessThermometer)(nil)).Elem(),
		tracking.Config{})
	tm.SensorData.LastUpdateTime = now
	tm.SensorData.Temperature = 25.0

	expectedSensorData := data.WirelessThermometerData{
		LastUpdateTime: now,
		Temperature:    25.0,
	}

	// Act
	dataSample := tm.Data()

	// Assert

	if !dataSample.LastUpdateTime.Equal(now) {
		t.Fatalf("expected LastUpdateTime %v, got %v", now, dataSample.LastUpdateTime)
	}

	if dataSample.Data.(data.WirelessThermometerData) != expectedSensorData {
		t.Fatalf("expected %+v, got %+v", expectedSensorData, dataSample.Data)
	}
}
