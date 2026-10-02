package environment

import (
	"fmt"
	"strings"
	"time"

	"github.com/avanha/pmaas-common/net/discovery"
	"github.com/avanha/pmaas-plugin-environment/entities"
	"github.com/avanha/pmaas-plugin-environment/internal/common"
	"github.com/avanha/pmaas-plugin-environment/internal/thermometer"
	"github.com/avanha/pmaas-spi"
	"github.com/avanha/pmaas-spi/tracking"
)

// Force implementation of remoteEntitySink
var _ remoteEntitySink = (*plugin)(nil)

// remoteEntityIdPrefix namespaces every entity mirrored in from a peer's Announce, so it can
// never collide with a locally-sourced entity's own id, and so the peer that owns it is always
// recoverable from the id alone (see remoteEntityId).
const remoteEntityIdPrefix = "net:"

// senderIdHexLength is the fixed length of every discovery.InstanceID (see NewInstanceID: 16
// random bytes, hex-encoded) - used to unambiguously pick the sender-id portion back out of a
// remoteEntityId (see remoteEntityIdsForRawId), rather than relying on a plain substring/suffix
// match that could in principle be fooled by a raw id that happens to end the same way as
// another device's raw id.
const senderIdHexLength = 32

// Snapshot implements remoteEntitySink. Reads p.state.entities, so it has to run on the plugin's
// own goroutine - see spi.Exec.
func (p *plugin) Snapshot() []EntitySnapshot {
	result, err := spi.Exec(p.state.container, p.buildEntitySnapshots)
	if err != nil {
		fmt.Printf("%T Snapshot: unable to read entities: %v\n", *p, err)
		return nil
	}
	return result
}

// buildEntitySnapshots converts this node's own entities into netDiscovery's portable shape,
// skipping any entity that was itself mirrored in from another peer (isRemoteEntityId) - relaying
// a peer's entities back out under our own announcement would let a third node see two different
// apparent owners for the same entity depending on which of us it happened to hear from first,
// and that peer is already the authoritative announcer for it regardless. Must run on the
// plugin's own goroutine - see Snapshot.
func (p *plugin) buildEntitySnapshots() []EntitySnapshot {
	snapshots := make([]EntitySnapshot, 0, len(p.state.entities))

	for id, tracked := range p.state.entities {
		if isRemoteEntityId(id) {
			continue
		}

		snapshots = append(snapshots, entitySnapshotFor(id, tracked))
	}

	return snapshots
}

type entityLookupResult struct {
	snapshot EntitySnapshot
	found    bool
}

// SnapshotOne implements remoteEntitySink. Reads p.state.entities, so it has to run on the
// plugin's own goroutine - see spi.Exec.
func (p *plugin) SnapshotOne(rawId string) (EntitySnapshot, bool) {
	result, err := spi.Exec(p.state.container, func() entityLookupResult {
		snapshot, found := p.buildEntitySnapshotFor(rawId)
		return entityLookupResult{snapshot: snapshot, found: found}
	})
	if err != nil {
		fmt.Printf("%T SnapshotOne: unable to read entity %s: %v\n", *p, rawId, err)
		return EntitySnapshot{}, false
	}
	return result.snapshot, result.found
}

// buildEntitySnapshotFor is SnapshotOne's single-entity counterpart to buildEntitySnapshots - see
// its doc for the isRemoteEntityId skip. Must run on the plugin's own goroutine - see SnapshotOne.
func (p *plugin) buildEntitySnapshotFor(rawId string) (EntitySnapshot, bool) {
	if isRemoteEntityId(rawId) {
		return EntitySnapshot{}, false
	}

	tracked, ok := p.state.entities[rawId]
	if !ok {
		return EntitySnapshot{}, false
	}

	return entitySnapshotFor(rawId, tracked), true
}

// entitySnapshotFor converts a single tracked entity into netDiscovery's portable shape.
func entitySnapshotFor(id string, tracked common.IStateTracker) EntitySnapshot {
	kind, name, portableState := tracked.PortableState()

	return EntitySnapshot{
		RawId: id,
		Kind:  kind,
		Name:  name,
		State: portableState,
		AsOf:  time.Now(),
	}
}

// ApplyRemoteAnnouncement implements remoteEntitySink.
func (p *plugin) ApplyRemoteAnnouncement(
	sender discovery.InstanceID, kind, rawId, name string, decodedState any, asOf time.Time) error {
	return p.state.container.EnqueueOnPluginGoRoutine(func() {
		p.applyRemoteAnnouncement(sender, kind, rawId, name, decodedState)
	})
}

// applyRemoteAnnouncement does the actual entity-map mutation - must run on the plugin's own
// goroutine (see ApplyRemoteAnnouncement).
func (p *plugin) applyRemoteAnnouncement(sender discovery.InstanceID, kind, rawId, name string, decodedState any) {
	if p.state.stopped {
		return
	}

	id := remoteEntityId(sender, rawId)

	// Prefer a local copy of the same device over mirroring it: rawId is the id the announcing
	// node itself registered it under, and for every current producer that's already a
	// globally-stable identity in its own right - an SDM device resource string, a Bluetooth MAC
	// address - not a locally-scoped counter, despite rawId only being *documented* as unique
	// within the announcing node (true for a hypothetical future producer that isn't so lucky). A
	// raw id can only ever collide with a LOCAL entity's map key here, never another peer's
	// mirrored one, since every mirrored key carries the remoteEntityIdPrefix and a raw device id
	// realistically never does. This is exactly the case that bit us with Nest thermostats: a
	// node running its own nestthermostat plugin also hearing the *same* thermostat announced by
	// a peer that also runs it - without this, both ended up in p.state.entities as two distinct
	// entries for the same physical device.
	if _, hasLocalCopy := p.state.entities[rawId]; hasLocalCopy {
		// If an earlier Announce already created a mirrored duplicate (e.g. before this node's
		// own local copy existed, or before this check existed), remove it now rather than
		// leaving a stale duplicate sitting around indefinitely.
		if _, staleShadowExists := p.state.entities[id]; staleShadowExists {
			p.removeRemoteEntity(id)
		}
		return
	}

	tracked, exists := p.state.entities[id]
	if !exists {
		switch kind {
		case "WirelessThermometer":
			if created := p.createRemoteWirelessThermometer(id, name); created != nil {
				tracked = created
			}
		case "Thermostat":
			if created := p.createRemoteThermostat(id, name); created != nil {
				tracked = created
			}
		default:
			fmt.Printf("%T applyRemoteAnnouncement: unknown entity kind %q for %s from %s\n", *p, kind, rawId, sender)
			return
		}

		if tracked == nil {
			// Registration failed (already logged); don't track it.
			return
		}

		p.state.entities[id] = tracked
	}

	err := tracked.ProcessNewState(decodedState, func(pmaasEntityId string, event any) {
		if err := p.state.container.BroadcastEvent(pmaasEntityId, event); err != nil {
			fmt.Printf("%T applyRemoteAnnouncement: error broadcasting event %v: %v\n", *p, event, err)
		}
	})
	if err != nil {
		fmt.Printf("%T applyRemoteAnnouncement: unable to apply %s %q from %s: %v\n", *p, kind, rawId, sender, err)
	}
}

// createRemoteWirelessThermometer returns nil if the entity couldn't be registered; a later Announce retries.
func (p *plugin) createRemoteWirelessThermometer(id string, name string) *thermometer.WirelessThermometer {
	instance := thermometer.CreateWirelessThermometer(
		p.state.nextEntityId(), id, name, entities.WirelessThermometerType, tracking.Config{})

	var stubFactoryFn spi.EntityStubFactoryFunc = func() (any, error) {
		return instance.GetStub(p.state.container), nil
	}

	pmaasEntityId, err := p.state.container.RegisterEntity(
		instance.Id, entities.WirelessThermometerType, instance.Name, stubFactoryFn)
	if err != nil {
		fmt.Printf("%T createRemoteWirelessThermometer: %s could not be registered: %v\n", *p, id, err)
		return nil
	}

	instance.PmaasEntityId = pmaasEntityId

	return instance
}

// createRemoteThermostat returns nil if the entity couldn't be registered; a later Announce retries.
func (p *plugin) createRemoteThermostat(id string, name string) *thermometer.Thermostat {
	instance := thermometer.CreateThermostat(
		p.state.nextEntityId(), id, name, entities.ThermostatType, tracking.Config{})

	var stubFactoryFn spi.EntityStubFactoryFunc = func() (any, error) {
		return instance.GetStub(p.state.container), nil
	}

	pmaasEntityId, err := p.state.container.RegisterEntity(
		instance.Id, entities.ThermostatType, instance.Name, stubFactoryFn)
	if err != nil {
		fmt.Printf("%T createRemoteThermostat: %s could not be registered: %v\n", *p, id, err)
		return nil
	}

	instance.PmaasEntityId = pmaasEntityId

	return instance
}

// RemoveRemoteEntitiesFromSender implements remoteEntitySink.
func (p *plugin) RemoveRemoteEntitiesFromSender(sender discovery.InstanceID) error {
	return p.state.container.EnqueueOnPluginGoRoutine(func() {
		for _, id := range p.remoteEntityIdsForSender(sender) {
			p.removeRemoteEntity(id)
		}
	})
}

// RemoveStaleRemoteEntities implements remoteEntitySink.
func (p *plugin) RemoveStaleRemoteEntities(refs []RemoteEntityRef) error {
	return p.state.container.EnqueueOnPluginGoRoutine(func() {
		for _, ref := range refs {
			p.removeRemoteEntity(remoteEntityId(ref.Sender, ref.RawId))
		}
	})
}

// removeRemoteEntity deregisters and forgets the tracked entity with the given map key. Despite
// the name it works for any tracked entity, local or mirrored - see deregisterAllEntities.
func (p *plugin) removeRemoteEntity(id string) {
	tracked, ok := p.state.entities[id]
	if !ok {
		return
	}

	if pmaasEntityId := tracked.GetPmaasEntityId(); pmaasEntityId != "" {
		if err := p.state.container.DeregisterEntity(pmaasEntityId); err != nil {
			fmt.Printf("%T removeRemoteEntity: unable to deregister %s: %v\n", *p, id, err)
		}
	}

	tracked.CloseStubIfPresent()
	delete(p.state.entities, id)
}

// remoteEntityId builds the local entity-map key for an entity announced by senderId,
// namespaced by that sender so it can never collide with another peer reusing the same raw id
// (which is only unique within its own announcing node). senderId is the peer's persisted
// discovery.InstanceID, not its source IP: unlike an IP, which can change under a peer that keeps
// running the whole time (DHCP renewal, moving networks), the persisted id is exactly the
// identity meant to stay stable for as long as that peer keeps existing.
func remoteEntityId(senderId discovery.InstanceID, rawId string) string {
	return remoteEntityIdPrefix + string(senderId) + ":" + rawId
}

func isRemoteEntityId(id string) bool {
	return strings.HasPrefix(id, remoteEntityIdPrefix)
}

// removeRemoteShadowsOf removes every remote-mirrored shadow entity for rawId, from whichever
// peer(s) announced it, if any exist, and tells netDiscovery to forget its own liveness
// bookkeeping for rawId too. Called when a LOCAL entity for the exact same raw id is about to be
// registered (see onEntityRegistered), so the local, authoritative copy always wins over a
// mirrored one - the reverse of the ordering applyRemoteAnnouncement already handles (there, the
// local copy already existed when the remote Announce arrived; here, the remote shadow already
// exists when the local copy arrives).
func (p *plugin) removeRemoteShadowsOf(rawId string) {
	if !p.config.NetDiscovery.EnableDiscover {
		// No remote shadow could exist at all without EnableDiscover.
		return
	}

	for _, id := range p.remoteEntityIdsForRawId(rawId) {
		p.removeRemoteEntity(id)
	}

	if p.state.netDiscovery != nil {
		p.state.netDiscovery.ForgetRawId(rawId)
	}
}

// remoteEntityIdsForRawId returns every locally-mirrored entity id whose raw, un-namespaced
// device id is exactly rawId, regardless of which peer announced it. There's realistically at
// most one, but this doesn't assume that.
func (p *plugin) remoteEntityIdsForRawId(rawId string) []string {
	suffix := ":" + rawId
	var ids []string

	for id := range p.state.entities {
		if !isRemoteEntityId(id) || !strings.HasSuffix(id, suffix) {
			continue
		}

		// Confirm what's between "net:" and this suffix is exactly one sender id, not just a
		// coincidental substring match.
		senderPortion := strings.TrimSuffix(strings.TrimPrefix(id, remoteEntityIdPrefix), suffix)
		if len(senderPortion) != senderIdHexLength {
			continue
		}

		ids = append(ids, id)
	}

	return ids
}

func (p *plugin) remoteEntityIdsForSender(senderId discovery.InstanceID) []string {
	prefix := remoteEntityIdPrefix + string(senderId) + ":"
	var ids []string

	for id := range p.state.entities {
		if strings.HasPrefix(id, prefix) {
			ids = append(ids, id)
		}
	}

	return ids
}
