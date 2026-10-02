package environment

import (
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"strings"

	"github.com/avanha/pmaas-common/net/discovery"
	"github.com/avanha/pmaas-plugin-environment/config"
	"github.com/avanha/pmaas-plugin-environment/data"
	"github.com/avanha/pmaas-plugin-environment/entities"
	"github.com/avanha/pmaas-plugin-environment/internal/common"
	httphandler "github.com/avanha/pmaas-plugin-environment/internal/http"
	"github.com/avanha/pmaas-plugin-environment/internal/thermometer"
	"github.com/avanha/pmaas-spi/entity"
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

	// Handlers first, then the scan: an entity registered between the two is seen by both, which
	// processEntity tolerates, whereas the reverse order could miss it entirely.
	p.registerEventHandlers()
	p.scanCurrentEntities()
	p.startNetDiscovery()
}

// Stop runs on the plugin goroutine, so it must not wait on anything that itself needs that
// goroutine. Net discovery does (its outbound queue delivers onto it), so its shutdown happens on a
// background goroutine and Stop returns the callback channel the core waits on, closing it once
// that's finished. The plugin goroutine stays free to run what discovery still has queued
// (which does nothing, see state.stopped) in the meantime.
func (p *plugin) Stop() chan func() {
	fmt.Printf("%T Stopping...\n", *p)

	p.state.stopped = true

	// Stop event delivery first so nothing new arrives while tearing down.
	p.deregisterEventHandlers()
	p.deregisterAllEntities()

	nd := p.state.netDiscovery
	p.state.netDiscovery = nil

	if nd == nil {
		return p.state.container.ClosedCallbackChannel()
	}

	callbackCh := make(chan func())

	go func() {
		nd.Stop()
		close(callbackCh)
	}()

	return callbackCh
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
		p.removeEntity(id)
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

	instanceID, err := p.loadOrCreateSenderId()
	if err != nil {
		// Without the persisted id we can't tell "first run" from "couldn't read it", and guessing
		// would mint a new identity and overwrite the saved one, making every peer see this node
		// as brand new.
		fmt.Printf("%T WARNING: not starting net discovery, unable to load persisted config: %v\n", *p, err)
		return
	}

	nd, err := newNetDiscovery(instanceID, netDiscoveryConfig, p, p.state.container.EnqueueOnPluginGoRoutine)
	if err != nil {
		// Net discovery is an optional add-on to a plugin that's otherwise fully functional
		// without it (local entities still work), so a misconfigured/unavailable network
		// interface here shouldn't take the whole plugin down - log and continue without it.
		fmt.Printf("%T startNetDiscovery: unable to start (continuing without it): %v\n", *p, err)
		return
	}

	p.state.netDiscovery = nd

	// Entities tracked before discovery came up (e.g. found by the startup scan) have already
	// had their change notifications dropped, so hand them over now. Not announced individually:
	// the first heartbeat or a peer's query will carry them.
	nd.Seed(p.buildEntitySnapshots())
}

// loadOrCreateSenderId returns this installation's persisted net-discovery identity, creating
// and saving one on first use. It's deliberately separate from discovery.NewInstanceID's
// per-process randomness: reusing the same id across restarts is what lets a peer recognize
// "this is the same node I heard from before" rather than seeing what looks like a new peer,
// with a full new set of entities, every time this plugin restarts. See
// config.PersistentConfigV1.NetDiscoverySenderId for how to reset it.
//
// A missing config file is the normal first-run case and creates a new id. Any other failure to
// load (unreadable or corrupt file, unrecognized payload type) is returned as an error rather
// than treated as "no id yet", so a transient problem can never overwrite a good saved id.
func (p *plugin) loadOrCreateSenderId() (discovery.InstanceID, error) {
	persistentConfig, err := p.state.container.LoadConfig(func(typeName string) any {
		v1Type := reflect.TypeFor[config.PersistentConfigV1]()
		v1TypeName := v1Type.PkgPath() + "/" + v1Type.Name()

		if typeName == v1TypeName {
			return &config.PersistentConfigV1{}
		}

		return nil
	})

	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}

	if err == nil {
		if loaded, ok := persistentConfig.(*config.PersistentConfigV1); ok && loaded.NetDiscoverySenderId != "" {
			return discovery.InstanceID(loaded.NetDiscoverySenderId), nil
		}
	}

	senderId := discovery.NewInstanceID()

	err = p.state.container.SaveConfig(config.PersistentConfigV1{
		NetDiscoverySenderId: string(senderId),
	})
	if err != nil {
		fmt.Printf("%T loadOrCreateSenderId: unable to persist new sender id: %v\n", *p, err)
	}

	return senderId, nil
}

// notifyEntityChanged hands netDiscovery (if running) the current state of the locally-sourced
// entity identified by rawId, so it announces it promptly and keeps it in its heartbeats. Runs on
// the plugin goroutine, where the entity's state may be read directly.
func (p *plugin) notifyEntityChanged(rawId string) {
	if p.state.netDiscovery == nil || isRemoteEntityId(rawId) {
		return
	}

	if tracked, ok := p.state.entities[rawId]; ok {
		p.state.netDiscovery.EntityChanged(entitySnapshotFor(rawId, tracked))
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

	handle, err = p.state.container.RegisterEventReceiver(
		func(eventInfo *events.EventInfo) bool {
			entityDeregisteredEvent, ok := eventInfo.Event.(events.EntityDeregisteredEvent)

			if !ok {
				return false
			}

			return isCompatibleEntityType(entityDeregisteredEvent.EntityType)
		},
		p.onEntityDeregistered,
	)

	if err != nil {
		panic(fmt.Sprintf("Unable to register for entity deregistration events: %v", err))
	}

	p.state.eventReceiverHandles["onEntityDeregistered"] = handle
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

// scanCurrentEntities picks up compatible entities that were registered before this plugin
// started, which no registration event will ever be delivered for.
func (p *plugin) scanCurrentEntities() {
	registered, err := p.state.container.GetEntities(
		func(info *entity.RegisteredEntityInfo) bool {
			return isCompatibleEntityType(info.EntityType)
		})
	if err != nil {
		fmt.Printf("%T scanCurrentEntities: unable to scan registered entities: %v\n", *p, err)
		return
	}

	for _, e := range registered {
		p.processEntity(e.Id, e.EntityType, e.Name, e.StubFactoryFn)
	}
}

func (p *plugin) onEntityRegistered(eventInfo *events.EventInfo) error {
	if p.state.stopped {
		return nil
	}

	fmt.Printf("%T onEntityRegistered(%v)\n", *p, eventInfo)
	event := eventInfo.Event.(events.EntityRegisteredEvent)
	p.processEntity(event.Id, event.EntityType, event.Name, event.StubFactoryFn)

	return nil
}

// processEntity starts tracking a source entity, whether it was learned from a registration event
// or from the startup scan. Already-tracked entities are ignored, since the same entity can
// legitimately arrive via both. sourceStubFactory is the producer's stub factory, nil for a producer
// that doesn't publish a stub (e.g. bluetooth today).
func (p *plugin) processEntity(
	id string, entityType reflect.Type, name string, sourceStubFactory func() (any, error)) {
	if _, ok := p.state.entities[id]; ok {
		return
	}

	// A remote-mirrored shadow of this exact device may already exist (announced by a peer
	// before this node's own local copy got registered) - remove it now that a local,
	// authoritative copy is arriving, so the local one always wins over a mirrored one
	// regardless of which arrived first.
	p.removeRemoteShadowsOf(id)

	var tracked common.IStateTracker

	if entityType.AssignableTo(IThermostatType) {
		tracked = p.registerThermostat(id, name)
	} else {
		tracked = p.registerWirelessThermometer(id, name)
	}

	if tracked != nil {
		p.loadInitialState(id, tracked, sourceStubFactory)
	}
}

// loadInitialState seeds a newly tracked entity from its source's current state, so an entity
// registered before this plugin started doesn't show no data until the source's next state change.
// Needs the producer to publish a stub implementing the environment interface; one that doesn't
// (nil factory) is simply skipped and the entity waits for its first state change event. The source
// stub is owned by the producer and must not be closed here.
//
// This blocks on the producer's goroutine twice (stub factory, then the state read), so it must not
// be called from anything the producer could be waiting on.
func (p *plugin) loadInitialState(id string, tracked common.IStateTracker, sourceStubFactory func() (any, error)) {
	if sourceStubFactory == nil {
		return
	}

	sourceStub, err := sourceStubFactory()
	if err != nil {
		fmt.Printf("%T loadInitialState: unable to create stub for %s: %v\n", *p, id, err)
		return
	}

	var initialState any

	switch typedStub := sourceStub.(type) {
	case environmental.IThermostat:
		initialState = typedStub.GetThermostatData()
	case environmental.IWirelessThermometer:
		initialState = typedStub.GetWirelessThermometerData()
	default:
		return
	}

	if err := p.applyState(tracked, initialState); err != nil {
		fmt.Printf("%T loadInitialState: unable to apply initial state for %s: %v\n", *p, id, err)
	}
}

func (p *plugin) applyState(tracked common.IStateTracker, newState any) error {
	return tracked.ProcessNewState(newState, func(pmaasEntityId string, event any) {
		if err := p.state.container.BroadcastEvent(pmaasEntityId, event); err != nil {
			fmt.Printf("%T Error broadcasting event %v: %v\n", *p, event, err)
		}
	})
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

// registerWirelessThermometer returns nil if the entity couldn't be registered with the container.
func (p *plugin) registerWirelessThermometer(id string, name string) common.IStateTracker {
	trackingConfig := buildTrackingConfig(
		"WirelessThermometer", name, data.WirelessThermometerDataType, data.WirelessThermometerDataToInsertArgs)

	instance := thermometer.CreateWirelessThermometer(
		p.state.nextEntityId(), id, name, entities.WirelessThermometerType, trackingConfig)

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
		return nil
	}

	instance.PmaasEntityId = pmaasEntityId
	p.state.entities[id] = instance
	p.notifyEntityChanged(id)

	return instance
}

// registerThermostat returns nil if the entity couldn't be registered with the container.
func (p *plugin) registerThermostat(id string, name string) common.IStateTracker {
	trackingConfig := buildTrackingConfig(
		"Thermostat", name, data.ThermostatDataType, data.ThermostatDataToInsertArgs)

	instance := thermometer.CreateThermostat(
		p.state.nextEntityId(), id, name, entities.ThermostatType, trackingConfig)

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
		return nil
	}

	instance.PmaasEntityId = pmaasEntityId
	p.state.entities[id] = instance
	p.notifyEntityChanged(id)

	return instance
}

func buildTrackingName(prefix string, name string) string {
	result := fmt.Sprintf("%s_%s", prefix, name)
	result = strings.ReplaceAll(result, " ", "_")
	result = strings.ReplaceAll(result, "'", "")

	return result
}

// onEntityDeregistered stops tracking a source entity that its producer removed (e.g. bluetooth
// trimming a device), instead of leaving a stale copy rendering and announcing forever.
func (p *plugin) onEntityDeregistered(eventInfo *events.EventInfo) error {
	if p.state.stopped {
		return nil
	}

	event := eventInfo.Event.(events.EntityDeregisteredEvent)

	if _, ok := p.state.entities[event.Id]; !ok {
		return nil
	}

	fmt.Printf("%T onEntityDeregistered(%v)\n", *p, eventInfo)
	p.removeEntity(event.Id)

	if p.state.netDiscovery != nil {
		p.state.netDiscovery.EntityRemoved(event.Id)
	}

	return nil
}

func (p *plugin) onEntityStateChanged(eventInfo *events.EventInfo) error {
	fmt.Printf("%T onEntityStateChanged(%v)\n", *p, eventInfo)
	event := eventInfo.Event.(events.EntityStateChangedEvent)
	sourceEntityId := event.Id
	entity, ok := p.state.entities[sourceEntityId]

	if !ok {
		return fmt.Errorf("Entity %s is not tracked", event.Id)
	}

	err := p.applyState(entity, event.NewState)

	p.notifyEntityChanged(sourceEntityId)

	return err
}

func isCompatibleEntityType(entityType reflect.Type) bool {
	result := entityType.AssignableTo(IWirelessThermometerType) || entityType.AssignableTo(IThermostatType)
	return result
}
