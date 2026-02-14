package thermometer

import (
	"fmt"
	"reflect"
	"time"

	"github.com/avanha/pmaas-plugin-environment/data"
	"github.com/avanha/pmaas-plugin-environment/entities"
	"github.com/avanha/pmaas-plugin-environment/internal/wrapper"
	"github.com/avanha/pmaas-spi"
	spicommon "github.com/avanha/pmaas-spi/common"
	spienvironment "github.com/avanha/pmaas-spi/environment"
	spievents "github.com/avanha/pmaas-spi/events"
	"github.com/avanha/pmaas-spi/tracking"
)

func CreateWirelessThermometer(
	instanceId int,
	targetEntityId string,
	name string,
	entityType reflect.Type,
	trackingConfig tracking.Config) *WirelessThermometer {
	return &WirelessThermometer{
		Thermometer: Thermometer{
			WrappedEntity: wrapper.CreateWrappedEntity(
				"WirelessThermometer", instanceId, targetEntityId, name, entityType),
			LowTemperature:  1000,
			HighTemperature: -1000,
			LowHumidity:     1000,
			HighHumidity:    -1000,
			trackingConfig:  trackingConfig,
		},
		BatteryData: spienvironment.BatteryData{},
		RSSIData:    spienvironment.RSSIData{},
	}
}

type WirelessThermometer struct {
	Thermometer
	BatteryData spienvironment.BatteryData
	RSSIData    spienvironment.RSSIData
	stub        *wirelessThermometerStub
}

func (wt *WirelessThermometer) GetStub(container spi.IPMAASContainer) entities.WirelessThermometer {
	if wt.stub == nil {
		wt.stub = newWirelessThermometerStub(
			wt.Id,
			&spicommon.ThreadSafeEntityWrapper[entities.WirelessThermometer]{
				Container: container,
				Entity:    wt,
			})
	}

	return wt.stub

}

func (wt *WirelessThermometer) TrackingConfig() tracking.Config {
	return wt.trackingConfig
}

func (wt *WirelessThermometer) Data() tracking.DataSample {
	return tracking.DataSample{
		LastUpdateTime: wt.SensorData.LastUpdateTime,
		Data: data.WirelessThermometerData{
			Temperature:    wt.SensorData.Temperature,
			HasHumidity:    wt.SensorData.HasHumidity,
			Humidity:       wt.SensorData.Humidity,
			BatteryLevel:   int32(wt.BatteryData.Level),
			RSSI:           int32(wt.RSSIData.RSSI),
			LastUpdateTime: wt.SensorData.LastUpdateTime,
		},
	}
}

func (wt *WirelessThermometer) GetSortKey() string {
	return wt.Name
}

func (wt *WirelessThermometer) GetState() any {
	return *wt
}

func (wt *WirelessThermometer) ProcessNewState(newState any, publishEventFunc func(pmassEntityId string, event any)) error {
	newWirelessThermometerState, ok := newState.(spienvironment.WirelessThermometer)

	if !ok {
		return fmt.Errorf(
			"unable to process state for WirelessThermomemter %s, unexpected incoming state type: %T",
			wt.Id, newState)
	}

	var entityEvent *spievents.EntityEvent = nil
	getEntityEvent := func() *spievents.EntityEvent {
		if entityEvent == nil {
			entityEvent = &spievents.EntityEvent{
				Id:         wt.PmaasEntityId,
				EntityType: wt.EntityType,
				Name:       wt.Name,
			}
		}
		return entityEvent
	}

	// TODO: This should be initialized once, when the device is first registered.
	wt.SensorData.HasHumidity = newWirelessThermometerState.SensorData.HasHumidity

	nameUpdated := false
	rssiUpdated := false
	batteryLevelUpdated := false
	temperatureUpdated := false
	humidityUpdated := false
	currentName := wt.Name
	currentTemperature := wt.SensorData.Temperature
	currentHumidity := wt.SensorData.Humidity
	now := time.Now()

	if currentName != newWirelessThermometerState.Name {
		wt.Name = newWirelessThermometerState.Name
		nameUpdated = true
	}

	if wt.RSSIData.RSSI != newWirelessThermometerState.RSSIData.RSSI {
		wt.RSSIData.RSSI = newWirelessThermometerState.RSSIData.RSSI
		wt.RSSIData.LastUpdateTime = newWirelessThermometerState.RSSIData.LastUpdateTime
		rssiUpdated = true
	}

	if wt.BatteryData.Level != newWirelessThermometerState.BatteryData.Level {
		wt.BatteryData.Level = newWirelessThermometerState.BatteryData.Level
		wt.BatteryData.LastUpdateTime = newWirelessThermometerState.BatteryData.LastUpdateTime
		batteryLevelUpdated = true
	}

	// Temperature
	if currentTemperature != newWirelessThermometerState.SensorData.Temperature {
		wt.SensorData.Temperature = newWirelessThermometerState.SensorData.Temperature
		wt.SensorData.LastUpdateTime = now
		temperatureUpdated = true

		if now.Day() != wt.HighTemperatureTime.Day() {
			wt.HighTemperature = -1000
			wt.LowTemperature = 1000
		}

		if newWirelessThermometerState.SensorData.Temperature > wt.HighTemperature {
			wt.HighTemperature = newWirelessThermometerState.SensorData.Temperature
			wt.HighTemperatureTime = now
		}

		if newWirelessThermometerState.SensorData.Temperature < wt.LowTemperature {
			wt.LowTemperature = newWirelessThermometerState.SensorData.Temperature
			wt.LowTemperatureTime = now
		}
	}

	// Humidity
	if currentHumidity != newWirelessThermometerState.SensorData.Humidity {
		wt.SensorData.Humidity = newWirelessThermometerState.SensorData.Humidity
		wt.SensorData.LastUpdateTime = now
		humidityUpdated = true

		if now.Day() != wt.HighHumidityTime.Day() {
			wt.HighHumidity = -1000
			wt.LowHumidity = 1000
		}

		if newWirelessThermometerState.SensorData.Humidity > wt.HighHumidity {
			wt.HighHumidity = newWirelessThermometerState.SensorData.Humidity
			wt.HighHumidityTime = now
		}

		if newWirelessThermometerState.SensorData.Humidity < wt.LowHumidity {
			wt.LowHumidity = newWirelessThermometerState.SensorData.Humidity
			wt.LowHumidityTime = now
		}
	}

	if nameUpdated {
		event := spievents.EntityNameChangedEvent{
			EntityEvent: *getEntityEvent(),
			NewName:     newWirelessThermometerState.Name,
			OldName:     currentName,
		}
		publishEventFunc(wt.PmaasEntityId, event)
	}

	if rssiUpdated {
		// TODO: Publish RSSIChangedEvent
	}

	if batteryLevelUpdated {
		// TODO: Publish BatteryLevelChangedEvent
	}

	if temperatureUpdated {
		event := spienvironment.TemperatureChangeEvent{
			EntityEvent: *getEntityEvent(),
			NewValue:    newWirelessThermometerState.SensorData.Temperature,
			OldValue:    currentTemperature,
		}
		publishEventFunc(wt.PmaasEntityId, event)
	}

	if humidityUpdated {
		event := spienvironment.HumidityChangeEvent{
			EntityEvent: *getEntityEvent(),
			NewValue:    newWirelessThermometerState.SensorData.Humidity,
			OldValue:    currentHumidity,
		}
		publishEventFunc(wt.PmaasEntityId, event)
	}

	if nameUpdated == false && temperatureUpdated == false && humidityUpdated {
		fmt.Printf("State change for %s, but no significant state change detected\n", wt.Id)
	}

	return nil
}
