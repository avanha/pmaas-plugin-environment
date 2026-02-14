package environment

import (
	"testing"

	"pmaas.io/plugins/environment/internal/thermometer"
	"pmaas.io/spi/tracking"
)

func TestWirelessThermometer_ImplementsExpectedInterfaces(t *testing.T) {
	tm := thermometer.CreateWirelessThermometer(
		1,
		"targetEntityId",
		"name",
		WirelessThermometerType,
		tracking.Config{})
	var _ Thermometer = tm
}
