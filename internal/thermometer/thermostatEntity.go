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

func CreateThermostat(
	instanceId int,
	targetEntityId string,
	name string,
	entityType reflect.Type,
	trackingConfig tracking.Config) *Thermostat {
	return &Thermostat{
		Thermometer: Thermometer{
			WrappedEntity: wrapper.CreateWrappedEntity(
				"Thermostat", instanceId, targetEntityId, name, entityType),
			LowTemperature:  1000,
			HighTemperature: -1000,
			LowHumidity:     1000,
			HighHumidity:    -1000,
			trackingConfig:  trackingConfig,
		},
	}
}

// Thermostat re-hosts a thermostat-like device (advertised elsewhere via environment.IThermostat) as a
// tracked, renderable entity, the same way WirelessThermometer does for environment.IWirelessThermometer
// devices. It doesn't carry RSSIData/BatteryData like WirelessThermometer does — a thermostat isn't a
// battery-powered wireless sensor — just the HVAC-specific attributes on top of the shared Thermometer.
type Thermostat struct {
	Thermometer
	HvacStatus string
	// Mode is the configured mode (HEAT/COOL/HEATCOOL/OFF), independent of HvacStatus (what it's
	// actually doing right now). A HEATCOOL-mode thermostat has both setpoints meaningful at once.
	Mode         string
	EcoMode      string
	HeatSetpoint float32
	CoolSetpoint float32
	// OfflineSince and OnlineSince are when Connectivity last transitioned to
	// spienvironment.ConnectivityOffline/ConnectivityOnline respectively — computed by the producer
	// plugin (which knows when the transition actually happened, not just when we last heard about it)
	// and just relayed here as-is, the same way HeatSetpoint/CoolSetpoint are kept current without this
	// plugin re-deriving them.
	Connectivity spienvironment.Connectivity
	OfflineSince time.Time
	OnlineSince  time.Time
	stub         *thermostatStub
}

func (t *Thermostat) GetStub(container spi.IPMAASContainer) entities.Thermostat {
	if t.stub == nil {
		t.stub = newThermostatStub(
			t.Id,
			&spicommon.ThreadSafeEntityWrapper[entities.Thermostat]{
				Container: container,
				Entity:    t,
			})
	}

	return t.stub
}

// CloseStubIfPresent invalidates the stub handed out by GetStub, if any, so later calls through it
// fail fast rather than reaching a deregistered entity. Call after DeregisterEntity, on the plugin
// goroutine, like GetStub.
func (t *Thermostat) CloseStubIfPresent() {
	if t.stub != nil {
		t.stub.close()
		t.stub = nil
	}
}

func (t *Thermostat) TrackingConfig() tracking.Config {
	return t.trackingConfig
}

func (t *Thermostat) Data() tracking.DataSample {
	return tracking.DataSample{
		LastUpdateTime: t.SensorData.LastUpdateTime,
		Data: data.ThermostatData{
			Temperature:    t.SensorData.Temperature,
			HasHumidity:    t.SensorData.HasHumidity,
			Humidity:       t.SensorData.Humidity,
			HvacStatus:     t.HvacStatus,
			Mode:           t.Mode,
			EcoMode:        t.EcoMode,
			HeatSetpoint:   t.HeatSetpoint,
			CoolSetpoint:   t.CoolSetpoint,
			Connectivity:   t.Connectivity,
			LastUpdateTime: t.SensorData.LastUpdateTime,
		},
	}
}

func (t *Thermostat) GetSortKey() string {
	return t.Name
}

func (t *Thermostat) GetState() any {
	snapshot := *t
	snapshot.stub = nil

	return snapshot
}

func (t *Thermostat) PortableState() (string, string, any) {
	return "Thermostat", t.Name, spienvironment.Thermostat{
		Name:           t.Name,
		SensorData:     t.SensorData,
		HvacStatus:     t.HvacStatus,
		Mode:           t.Mode,
		EcoMode:        t.EcoMode,
		HeatSetpoint:   t.HeatSetpoint,
		CoolSetpoint:   t.CoolSetpoint,
		Connectivity:   t.Connectivity,
		OfflineSince:   t.OfflineSince,
		OnlineSince:    t.OnlineSince,
		LastUpdateTime: t.SensorData.LastUpdateTime,
	}
}

func (t *Thermostat) ProcessNewState(newState any, publishEventFunc func(pmassEntityId string, event any)) error {
	newThermostatState, ok := newState.(spienvironment.Thermostat)

	if !ok {
		return fmt.Errorf(
			"unable to process state for Thermostat %s, unexpected incoming state type: %T",
			t.Id, newState)
	}

	var entityEvent *spievents.EntityEvent = nil
	getEntityEvent := func() *spievents.EntityEvent {
		if entityEvent == nil {
			entityEvent = &spievents.EntityEvent{
				Id:         t.PmaasEntityId,
				EntityType: t.EntityType,
				Name:       t.Name,
			}
		}
		return entityEvent
	}

	nameUpdated := false
	hvacStatusUpdated := false
	modeUpdated := false
	ecoModeUpdated := false
	connectivityUpdated := false
	currentName := t.Name
	currentTemperature := t.SensorData.Temperature
	currentHumidity := t.SensorData.Humidity
	currentHvacStatus := t.HvacStatus
	currentMode := t.Mode
	currentEcoMode := t.EcoMode
	currentConnectivity := t.Connectivity
	now := time.Now()

	if currentName != newThermostatState.Name {
		t.Name = newThermostatState.Name
		nameUpdated = true
	}

	if currentHvacStatus != newThermostatState.HvacStatus {
		t.HvacStatus = newThermostatState.HvacStatus
		hvacStatusUpdated = true
	}

	if currentMode != newThermostatState.Mode {
		t.Mode = newThermostatState.Mode
		modeUpdated = true
	}

	if currentEcoMode != newThermostatState.EcoMode {
		t.EcoMode = newThermostatState.EcoMode
		ecoModeUpdated = true
	}

	if currentConnectivity != newThermostatState.Connectivity {
		t.Connectivity = newThermostatState.Connectivity
		connectivityUpdated = true
	}

	// OfflineSince/OnlineSince are timestamps accompanying Connectivity, not independent state of their
	// own — no separate change event for them. The producer (which knows exactly when each transition
	// happened, not just when we last heard about it) already computed the correct values; just relay
	// them.
	t.OfflineSince = newThermostatState.OfflineSince
	t.OnlineSince = newThermostatState.OnlineSince

	// Setpoints aren't tracked for high/low extremes the way temperature/humidity are, and don't
	// currently have their own change event — just kept current.
	t.HeatSetpoint = newThermostatState.HeatSetpoint
	t.CoolSetpoint = newThermostatState.CoolSetpoint
	temperatureUpdated, humidityUpdated := t.applySensorData(newThermostatState.SensorData, now)

	if nameUpdated {
		event := spievents.EntityNameChangedEvent{
			EntityEvent: *getEntityEvent(),
			NewName:     newThermostatState.Name,
			OldName:     currentName,
		}
		publishEventFunc(t.PmaasEntityId, event)
	}

	if hvacStatusUpdated {
		// TODO: Publish HvacStatusChangedEvent
	}

	if modeUpdated {
		// TODO: Publish ModeChangedEvent
	}

	if ecoModeUpdated {
		// TODO: Publish EcoModeChangedEvent
	}

	if connectivityUpdated {
		// TODO: Publish ConnectivityChangedEvent
	}

	if temperatureUpdated {
		event := spienvironment.TemperatureChangeEvent{
			EntityEvent: *getEntityEvent(),
			NewValue:    newThermostatState.SensorData.Temperature,
			OldValue:    currentTemperature,
		}
		publishEventFunc(t.PmaasEntityId, event)
	}

	if humidityUpdated {
		event := spienvironment.HumidityChangeEvent{
			EntityEvent: *getEntityEvent(),
			NewValue:    newThermostatState.SensorData.Humidity,
			OldValue:    currentHumidity,
		}
		publishEventFunc(t.PmaasEntityId, event)
	}

	if !nameUpdated && !temperatureUpdated && !humidityUpdated && !hvacStatusUpdated && !modeUpdated &&
		!ecoModeUpdated && !connectivityUpdated {
		fmt.Printf("State change for %s, but no significant state change detected\n", t.Id)
	}

	return nil
}
