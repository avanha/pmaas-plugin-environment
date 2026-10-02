package environment

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/avanha/pmaas-common/net/discovery"
	"github.com/avanha/pmaas-plugin-environment/config"
	"github.com/avanha/pmaas-plugin-environment/data"
	"github.com/avanha/pmaas-plugin-environment/entities"
	"github.com/avanha/pmaas-plugin-environment/internal/common"
	httphandler "github.com/avanha/pmaas-plugin-environment/internal/http"
	"github.com/avanha/pmaas-plugin-environment/internal/thermometer"
	environmental "github.com/avanha/pmaas-spi/environment"
	"github.com/avanha/pmaas-spi/events"
	"github.com/avanha/pmaas-spi/tracking"

	"github.com/avanha/pmaas-spi"
)

var IWirelessThermometerType = reflect.TypeFor[environmental.IWirelessThermometer]()
var IThermostatType = reflect.TypeFor[environmental.IThermostat]()

type state struct {
	container            spi.IPMAASContainer
	entities             map[string]common.IStateTracker
	entityCounter        int
	eventReceiverHandles map[string]int
	netDiscovery         *netDiscovery
	// stopped is set once Stop begins, so work already queued on the plugin goroutine (e.g. a
	// remote announcement) doesn't re-register entities after they've been torn down.
	stopped bool
}

func (s *state) nextEntityId() int {
	s.entityCounter = s.entityCounter + 1
	return s.entityCounter
}

type plugin struct {
	config      PluginConfig
	state       state
	httpHandler *httphandler.Handler
}

type Plugin interface {
	spi.IPMAASPlugin
}

func NewPlugin(config PluginConfig) Plugin {
	fmt.Printf("New, config: %v\n", config)
	instance := &plugin{
		config: config,
		state: state{
			container:            nil,
			entities:             make(map[string]common.IStateTracker),
			entityCounter:        0,
			eventReceiverHandles: make(map[string]int),
		},
		httpHandler: httphandler.NewHandler(),
	}

	return instance
}

// Force implementation of spi.IPMAASPlugin
var _ spi.IPMAASPlugin = (*plugin)(nil)

func (p *plugin) ShortName() string {
	return "environment"
}

func (p *plugin) Init(container spi.IPMAASContainer) {
	p.state.container = container
	p.httpHandler.Init(container, &entityStoreAdapter{parent: p})
}

func (p *plugin) Start() {
	fmt.Printf("%T Starting...\n", *p)

	p.registerEventHandlers()
	// TODO: Retrieve the list of possible entities to add to our map.
	// Without it, we depend on the plugin ordering to ensure we get any devices in existence prior to our registration.

	p.startNetDiscovery()
}

func (p *plugin) Stop() chan func() {
	fmt.Printf("%T Stopping...\n", *p)

	p.state.stopped = true

	// Stop event delivery first so nothing new arrives while tearing down.
	p.deregisterEventHandlers()
	p.stopNetDiscovery()
	p.deregisterAllEntities()

	return p.state.container.ClosedCallbackChannel()
}

func (p *plugin) deregisterEventHandlers() {
	for name, handle := range p.state.eventReceiverHandles {
		if err := p.state.container.DeregisterEventReceiver(handle); err != nil {
			fmt.Printf("%T Stop: unable to deregister event receiver %s: %v\n", *p, name, err)
		}
	}

	clear(p.state.eventReceiverHandles)
}

// deregisterAllEntities removes every entity this plugin registered with the container, local
// and remote-mirrored alike.
func (p *plugin) deregisterAllEntities() {
	for id := range p.state.entities {
		p.removeRemoteEntity(id)
	}
}

// startNetDiscovery brings up this plugin's netDiscovery actor per p.config.NetDiscovery, if any
// of it is enabled - a fully-disabled config is a no-op. The actual discovery protocol lives
// entirely in netDiscovery (see netdiscovery.go); this is just the plugin-side wiring: resolving
// this installation's persisted sender identity and passing *p as the remoteEntitySink.
func (p *plugin) startNetDiscovery() {
	netDiscoveryConfig := p.config.NetDiscovery
	if !netDiscoveryConfig.EnableAnnounce && !netDiscoveryConfig.EnableDiscover {
		return
	}

	instanceID := p.loadOrCreateSenderId()

	nd, err := newNetDiscovery(instanceID, netDiscoveryConfig, p)
	if err != nil {
		// Net discovery is an optional add-on to a plugin that's otherwise fully functional
		// without it (local entities still work), so a misconfigured/unavailable network
		// interface here shouldn't take the whole plugin down - log and continue without it.
		fmt.Printf("%T startNetDiscovery: unable to start (continuing without it): %v\n", *p, err)
		return
	}

	p.state.netDiscovery = nd
}

func (p *plugin) stopNetDiscovery() {
	if p.state.netDiscovery == nil {
		return
	}

	p.state.netDiscovery.Stop()
	p.state.netDiscovery = nil
}

// loadOrCreateSenderId returns this installation's persisted net-discovery identity, creating
// and saving one on first use. It's deliberately separate from discovery.NewInstanceID's
// per-process randomness: reusing the same id across restarts is what lets a peer recognize
// "this is the same node I heard from before" rather than seeing what looks like a new peer,
// with a full new set of entities, every time this plugin restarts. See
// config.PersistentConfigV1.NetDiscoverySenderId for how to reset it.
func (p *plugin) loadOrCreateSenderId() discovery.InstanceID {
	persistentConfig, _ := p.state.container.LoadConfig(func(typeName string) any {
		v1Type := reflect.TypeFor[config.PersistentConfigV1]()
		v1TypeName := v1Type.PkgPath() + "/" + v1Type.Name()

		if typeName == v1TypeName {
			return &config.PersistentConfigV1{}
		}

		return nil
	})

	if persistentConfig != nil {
		if loaded, ok := persistentConfig.(*config.PersistentConfigV1); ok && loaded.NetDiscoverySenderId != "" {
			return discovery.InstanceID(loaded.NetDiscoverySenderId)
		}
	}

	senderId := discovery.NewInstanceID()

	err := p.state.container.SaveConfig(config.PersistentConfigV1{
		NetDiscoverySenderId: string(senderId),
	})
	if err != nil {
		fmt.Printf("%T loadOrCreateSenderId: unable to persist new sender id: %v\n", *p, err)
	}

	return senderId
}

// notifyEntityChanged tells netDiscovery (if running) that the entity identified by rawId
// changed, so it re-announces just that entity promptly instead of waiting for its next
// heartbeat.
func (p *plugin) notifyEntityChanged(rawId string) {
	if p.state.netDiscovery != nil {
		p.state.netDiscovery.NotifyEntityChanged(rawId)
	}
}

func (p *plugin) registerEventHandlers() {
	var handle int
	var err error

	handle, err = p.state.container.RegisterEventReceiver(
		func(eventInfo *events.EventInfo) bool {
			// We only want entity registrations
			entityRegisteredEvent, ok := eventInfo.Event.(events.EntityRegisteredEvent)

			if !ok {
				return false
			}

			return isCompatibleEntityType(entityRegisteredEvent.EntityType)
		},
		p.onEntityRegistered,
	)

	if err != nil {
		panic(fmt.Sprintf("Unable to register for entity registration events: %v", err))
	}

	p.state.eventReceiverHandles["onEntityRegistered"] = handle

	handle, err = p.state.container.RegisterEventReceiver(
		func(eventInfo *events.EventInfo) bool {
			entityStateChangedEvent, ok := eventInfo.Event.(events.EntityStateChangedEvent)

			if !ok {
				return false
			}

			return isCompatibleEntityType(entityStateChangedEvent.EntityType)
		},
		p.onEntityStateChanged,
	)

	if err != nil {
		panic(fmt.Sprintf("Unable to register for entity state change events: %v", err))
	}

	p.state.eventReceiverHandles["onEntityStateChange"] = handle
}

func (p *plugin) getEntities() []any {
	var entityList = make([]any, len(p.state.entities))
	i := 0
	for _, stateTrackingEntity := range p.state.entities {
		entityList[i] = stateTrackingEntity.GetState()
		i = i + 1
	}
	//fmt.Printf("getEntities(), list: %v\n", entityList)
	return entityList
}

func (p *plugin) onEntityRegistered(eventInfo *events.EventInfo) error {
	if p.state.stopped {
		return nil
	}

	fmt.Printf("%T onEntityRegistered(%v)\n", *p, eventInfo)
	event := eventInfo.Event.(events.EntityRegisteredEvent)
	_, ok := p.state.entities[event.Id]

	if ok {
		return fmt.Errorf("Entity %s already tracked", event.Id)
	}

	// A remote-mirrored shadow of this exact device may already exist (announced by a peer
	// before this node's own local copy got registered) - remove it now that a local,
	// authoritative copy is arriving, so the local one always wins over a mirrored one
	// regardless of which arrived first.
	p.removeRemoteShadowsOf(event.Id)

	if event.EntityType.AssignableTo(IThermostatType) {
		p.registerThermostat(event)
	} else {
		p.registerWirelessThermometer(event)
	}

	return nil
}

// buildTrackingConfig builds the tracking.Config shared by every re-hosted entity kind: unnamed
// devices aren't tracked at all, everything else polls its own stub every 5 minutes under a name
// prefixed by the entity kind.
func buildTrackingConfig(
	kindPrefix string, name string, dataType reflect.Type, insertArgFn tracking.InsertArgFactoryFunc,
) tracking.Config {
	if name == "" {
		// Do not track unnamed devices
		return tracking.Config{}
	}

	return tracking.Config{
		TrackingMode:        tracking.ModePoll,
		PollIntervalSeconds: 300,
		Name:                buildTrackingName(kindPrefix, name),
		Schema: tracking.Schema{
			DataStructType:     dataType,
			InsertArgFactoryFn: insertArgFn,
		},
	}
}

func (p *plugin) registerWirelessThermometer(event events.EntityRegisteredEvent) {
	trackingConfig := buildTrackingConfig(
		"WirelessThermometer", event.Name, data.WirelessThermometerDataType, data.WirelessThermometerDataToInsertArgs)

	instance := thermometer.CreateWirelessThermometer(
		p.state.nextEntityId(), event.Id, event.Name, entities.WirelessThermometerType, trackingConfig)

	// This lambda captures both the plugin instance and the thermometer instance
	// and passes it to the entity manager.  However, since entities are deregistered on plugin
	// stop, so this is OK and doesn't leak memory.
	var stubFactoryFn spi.EntityStubFactoryFunc = func() (any, error) {
		return instance.GetStub(p.state.container), nil
	}
	pmaasEntityId, err := p.state.container.RegisterEntity(
		instance.Id,
		entities.WirelessThermometerType,
		instance.Name,
		stubFactoryFn)

	if err != nil {
		// Don't track an entity the container doesn't know about: it would have no PmaasEntityId,
		// so any event published for it would carry an empty id.
		fmt.Printf("Device %s could not be registered: %v\n", instance.Id, err)
		return
	}

	instance.PmaasEntityId = pmaasEntityId
	p.state.entities[event.Id] = instance
	p.notifyEntityChanged(event.Id)
}

func (p *plugin) registerThermostat(event events.EntityRegisteredEvent) {
	trackingConfig := buildTrackingConfig(
		"Thermostat", event.Name, data.ThermostatDataType, data.ThermostatDataToInsertArgs)

	instance := thermometer.CreateThermostat(
		p.state.nextEntityId(), event.Id, event.Name, entities.ThermostatType, trackingConfig)

	var stubFactoryFn spi.EntityStubFactoryFunc = func() (any, error) {
		return instance.GetStub(p.state.container), nil
	}
	pmaasEntityId, err := p.state.container.RegisterEntity(
		instance.Id,
		entities.ThermostatType,
		instance.Name,
		stubFactoryFn)

	if err != nil {
		// See registerWirelessThermometer.
		fmt.Printf("Device %s could not be registered: %v\n", instance.Id, err)
		return
	}

	instance.PmaasEntityId = pmaasEntityId
	p.state.entities[event.Id] = instance
	p.notifyEntityChanged(event.Id)
}

func buildTrackingName(prefix string, name string) string {
	result := fmt.Sprintf("%s_%s", prefix, name)
	result = strings.ReplaceAll(result, " ", "_")
	result = strings.ReplaceAll(result, "'", "")

	return result
}

func (p *plugin) onEntityStateChanged(eventInfo *events.EventInfo) error {
	fmt.Printf("%T onEntityStateChanged(%v)\n", *p, eventInfo)
	event := eventInfo.Event.(events.EntityStateChangedEvent)
	sourceEntityId := event.Id
	entity, ok := p.state.entities[sourceEntityId]

	if !ok {
		return fmt.Errorf("Entity %s is not tracked", event.Id)
	}

	err := entity.ProcessNewState(event.NewState, func(pmassEntityId string, event any) {
		err := p.state.container.BroadcastEvent(pmassEntityId, event)
		if err != nil {
			fmt.Printf("%T Error broadcasting event %v", p, event)
		}
	})

	p.notifyEntityChanged(sourceEntityId)

	return err
}

func isCompatibleEntityType(entityType reflect.Type) bool {
	result := entityType.AssignableTo(IWirelessThermometerType) || entityType.AssignableTo(IThermostatType)
	return result
}
