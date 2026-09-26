package environment

import (
	"testing"

	"github.com/avanha/pmaas-plugin-environment/entities"
	"github.com/avanha/pmaas-plugin-environment/internal/thermometer"
	"github.com/avanha/pmaas-spi/tracking"
)

func TestWirelessThermometer_ImplementsExpectedInterfaces(t *testing.T) {
	tm := thermometer.CreateWirelessThermometer(
		1,
		"targetEntityId",
		"name",
		entities.WirelessThermometerType,
		tracking.Config{})
	var _ entities.Thermometer = tm
	var _ entities.WirelessThermometer = tm
}
