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

// CloseStubIfPresent invalidates the stub handed out by GetStub, if any, so later calls through it
// fail fast rather than reaching a deregistered entity. Call after DeregisterEntity, on the plugin
// goroutine, like GetStub.
func (wt *WirelessThermometer) CloseStubIfPresent() {
	if wt.stub != nil {
		wt.stub.close()
		wt.stub = nil
	}
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
	snapshot := *wt
	snapshot.stub = nil

	return snapshot
}

func (wt *WirelessThermometer) PortableState() (string, string, any) {
	return "WirelessThermometer", wt.Name, spienvironment.WirelessThermometer{
		Name:        wt.Name,
		RSSIData:    wt.RSSIData,
		BatteryData: wt.BatteryData,
		SensorData:  wt.SensorData,
	}
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

	nameUpdated := false
	rssiUpdated := false
	batteryLevelUpdated := false
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
	temperatureUpdated, humidityUpdated := wt.applySensorData(newWirelessThermometerState.SensorData, now)

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

	if !nameUpdated && !temperatureUpdated && !humidityUpdated && !rssiUpdated && !batteryLevelUpdated {
		fmt.Printf("State change for %s, but no significant state change detected\n", wt.Id)
	}

	return nil
}
