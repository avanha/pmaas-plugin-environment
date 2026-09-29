package environment

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net"
	"reflect"
	"strings"
	"time"

	"github.com/avanha/pmaas-common/net/discovery"
	pluginconfig "github.com/avanha/pmaas-plugin-environment/config"
	"github.com/avanha/pmaas-plugin-environment/entities"
	"github.com/avanha/pmaas-plugin-environment/internal/common"
	"github.com/avanha/pmaas-plugin-environment/internal/thermometer"
	"github.com/avanha/pmaas-spi"
	spienvironment "github.com/avanha/pmaas-spi/environment"
	"github.com/avanha/pmaas-spi/tracking"
)

// netDiscoveryQueryResponseJitter bounds a random delay applied before responding to a Query,
// so a segment with several announcing peers doesn't have all of them reply in the same
// instant - the same thundering-herd concern mDNS (RFC 6762) addresses the same way for its own
// query responses.
const netDiscoveryQueryResponseJitter = 250 * time.Millisecond

// netDiscoveryStartupDelayMin/Max bound a random delay before this node's very first "anyone
// here?" Query, so a whole fleet of nodes coming up together (e.g. all rebooting after a power
// outage) doesn't have every one of them send it in the same instant - the same
// thundering-herd concern netDiscoveryQueryResponseJitter addresses for query *responses*, just
// at a much coarser, once-per-process-lifetime granularity appropriate to a startup event
// rather than a single query.
const (
	netDiscoveryStartupDelayMin = 20 * time.Second
	netDiscoveryStartupDelayMax = 1 * time.Minute
)

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

// netDiscoveryAnnounceInterval is both how often an EnableAnnounce node re-announces its full
// entity set (a heartbeat, on top of the change-triggered announces already sent by
// announceEntityChange's callers), and how often an EnableDiscover node sweeps for entities it
// hasn't heard about in netDiscoveryEntityTTL - see runNetDiscoveryTicker.
const netDiscoveryAnnounceInterval = 30 * time.Minute

// netDiscoveryEntityTTL is how long a mirrored entity is kept after the last Announce that
// mentioned it, before it's dropped as stale. This is what actually cleans up after a peer that
// disappears *without* sending Goodbye (a crash, a pulled cable, a network partition) -
// handleDiscoveryGoodbye handles the clean-shutdown case immediately, this handles everything
// else, just on a slower, TTL-driven schedule.
const netDiscoveryEntityTTL = 2 * time.Hour

// TODO: This is pretty tightly conflated with plugin.
// It uses the plugins's go routine and declares methods on the plugin struct.
// I think it should be a seperate component that communicates with the plugin.  It could still
// be an actor, but we want to move work off the Mailbox, like JSON decoding.

// netDiscovery holds the running discovery.Transport/Service for this plugin instance, when
// net discovery is enabled at all (see NetDiscoveryConfig). nil when neither EnableAnnounce
// nor EnableDiscover is set, so the rest of the plugin never has to check config in addition
// to a nil check.
type netDiscovery struct {
	instanceID discovery.InstanceID
	service    *discovery.Service

	// remoteMeta is bookkeeping for each mirrored entity that doesn't belong on the portable
	// spienvironment types themselves (those are shared with every producer plugin, not just
	// network-mirrored ones): when it was last seen (for TTL expiry) and the peer's most recent
	// source IP, kept purely as informational/debugging metadata - e.g. a future "mirrored from
	// 10.0.1.42" hover in the UI. It's *not* part of this entity's identity (see remoteEntityId)
	// precisely so it staying fresh/stale/changing doesn't affect that identity.
	remoteMeta map[string]*remoteEntityMeta

	// stopCh signals runNetDiscoveryTicker to exit; closed once, in stopNetDiscovery.
	stopCh chan struct{}
}

type remoteEntityMeta struct {
	sourceIP string
	lastSeen time.Time
}

// startNetDiscovery brings up multicast discovery per p.config.NetDiscovery, if any of it is
// enabled. It's safe to call unconditionally from Start(); a fully-disabled config is a no-op.
//
// EnableAnnounce and EnableDiscover are independent at the config level (a node can announce
// without ever creating shadow entities from peers, or vice versa), but they share one
// Transport/Service, and the underlying transport's send/receive directions are derived from
// both flags together rather than mapped 1:1:
//   - send is needed whenever EITHER flag is set: EnableAnnounce needs it to publish Announce
//     and to answer a received Query with one, but EnableDiscover needs it too, on its own -
//     "anyone here with thermometers?" is itself an outbound message (the initial Query sent
//     below), so a discover-only node still has to be allowed to send, even though it never
//     announces anything of its own.
//   - receive is needed whenever EnableDiscover is set (to see peers' Announce), but *also*
//     whenever EnableAnnounce is set on its own (to see incoming Query requests to answer -
//     an announce-only node still needs to listen for the thing it's responding to).
func (p *plugin) startNetDiscovery() {
	config := p.config.NetDiscovery

	if !config.EnableAnnounce && !config.EnableDiscover {
		return
	}

	instanceID := p.loadOrCreateSenderId()
	transport, err := discovery.NewTransport(instanceID, discovery.Config{
		GroupAddress:  config.GroupAddress,
		InterfaceName: config.InterfaceName,
		EnableSend:    config.EnableAnnounce || config.EnableDiscover,
		EnableReceive: config.EnableDiscover || config.EnableAnnounce,
		TTL:           config.TTL,
	})
	if err != nil {
		// Net discovery is an optional add-on to a plugin that's otherwise fully functional
		// without it (local entities still work), so a misconfigured/unavailable network
		// interface here shouldn't take the whole plugin down - log and continue without it.
		fmt.Printf("%T startNetDiscovery: unable to start (continuing without it): %v\n", *p, err)
		return
	}

	nd := &netDiscovery{
		instanceID: instanceID,
		service:    discovery.NewService(transport),
		remoteMeta: make(map[string]*remoteEntityMeta),
		stopCh:     make(chan struct{}),
	}
	p.state.netDiscovery = nd

	go p.runNetDiscoveryEventLoop(nd.service)
	go p.runNetDiscoveryTicker(nd.stopCh)

	if config.EnableDiscover {
		// "Anyone here with thermometers?" - ask once at startup so entities from peers that
		// have been running for a while show up promptly, rather than waiting on the next
		// netDiscoveryAnnounceInterval heartbeat. Delayed rather than sent immediately - see
		// netDiscoveryStartupDelayMin/Max - and, like the query-response jitter, dispatched via
		// time.AfterFunc so the delay never blocks the plugin's own goroutine (which is what's
		// running startNetDiscovery right now, as part of Start()).
		delay := randomDelayIn(netDiscoveryStartupDelayMin, netDiscoveryStartupDelayMax)
		time.AfterFunc(delay, func() {
			err := p.state.container.EnqueueOnPluginGoRoutine(p.sendInitialDiscoveryQuery)
			if err != nil {
				fmt.Printf("%T startNetDiscovery: unable to enqueue initial query: %v\n", *p, err)
			}
		})
	}
}

func (p *plugin) sendInitialDiscoveryQuery() {
	if p.state.netDiscovery == nil {
		// Stopped before the startup delay elapsed - nothing to do.
		return
	}

	if err := p.state.netDiscovery.service.SendQuery(); err != nil {
		fmt.Printf("%T sendInitialDiscoveryQuery: unable to send: %v\n", *p, err)
	}
}

// randomDelayIn returns a random duration in [minDelay, maxDelay).
func randomDelayIn(minDelay, maxDelay time.Duration) time.Duration {
	return minDelay + time.Duration(rand.Int64N(int64(maxDelay-minDelay)))
}

// TODO: This should be done in the init method in plugin, and passed in here.
// loadOrCreateSenderId returns this installation's persisted net-discovery identity, creating
// and saving one on first use. It's deliberately separate from discovery.NewInstanceID's
// per-process randomness: reusing the same id across restarts is what lets a peer recognize
// "this is the same node I heard from before" rather than seeing what looks like a new peer,
// with a full new set of entities, every time this plugin restarts. See
// config.PersistentConfigV1.NetDiscoverySenderId for how to reset it.
func (p *plugin) loadOrCreateSenderId() discovery.InstanceID {
	persistentConfig, _ := p.state.container.LoadConfig(func(typeName string) any {
		v1Type := reflect.TypeFor[pluginconfig.PersistentConfigV1]()
		v1TypeName := v1Type.PkgPath() + "/" + v1Type.Name()

		if typeName == v1TypeName {
			return &pluginconfig.PersistentConfigV1{}
		}

		return nil
	})

	if persistentConfig != nil {
		if loaded, ok := persistentConfig.(*pluginconfig.PersistentConfigV1); ok && loaded.NetDiscoverySenderId != "" {
			return discovery.InstanceID(loaded.NetDiscoverySenderId)
		}
	}

	senderId := discovery.NewInstanceID()

	err := p.state.container.SaveConfig(pluginconfig.PersistentConfigV1{
		NetDiscoverySenderId: string(senderId),
	})
	if err != nil {
		fmt.Printf("%T loadOrCreateSenderId: unable to persist new sender id: %v\n", *p, err)
	}

	return senderId
}

func (p *plugin) stopNetDiscovery() {
	if p.state.netDiscovery == nil {
		return
	}

	close(p.state.netDiscovery.stopCh)

	if p.config.NetDiscovery.EnableAnnounce {
		if err := p.state.netDiscovery.service.SendGoodbye(); err != nil {
			fmt.Printf("%T stopNetDiscovery: unable to send goodbye: %v\n", *p, err)
		}
	}

	if err := p.state.netDiscovery.service.Close(); err != nil {
		fmt.Printf("%T stopNetDiscovery: error closing discovery service: %v\n", *p, err)
	}

	// Nil this out (rather than leaving a pointer to an already-closed Service/Transport
	// around) so anything that might still run after this point - notably a query response
	// jitter timer that was already pending when Stop was called - can tell discovery has
	// actually been torn down and skip touching it, instead of trying to send through a closed
	// connection and just logging the resulting error.
	p.state.netDiscovery = nil
}

// runNetDiscoveryEventLoop reads decoded Envelopes as they arrive on the discovery Service's
// own goroutine, and hands each one to the plugin's main goroutine before touching any plugin
// state - the same rule every other entry point into this plugin already follows (see
// handleHttpListRequest), since p.state is only safe to read/write from there.
func (p *plugin) runNetDiscoveryEventLoop(service *discovery.Service) {
	for event := range service.Events() {
		err := p.state.container.EnqueueOnPluginGoRoutine(func() {
			p.handleDiscoveryEvent(event)
		})
		if err != nil {
			// Plugin is shutting down (or its mailbox is otherwise closed) - the event loop
			// will exit on its own once Close() finishes closing service.Events().
			fmt.Printf("%T runNetDiscoveryEventLoop: unable to enqueue event: %v\n", *p, err)
		}
	}
}

// runNetDiscoveryTicker drives both the periodic re-announce heartbeat and the stale-entity
// sweep off one shared ticker - both are on the same netDiscoveryAnnounceInterval cadence, so
// there's no reason to run two separate timers for them. Each tick is handed to the plugin's
// own goroutine (announceEntityChange/expireStaleRemoteEntities both touch p.state) rather than
// acted on here directly.
func (p *plugin) runNetDiscoveryTicker(stopCh <-chan struct{}) {
	ticker := time.NewTicker(netDiscoveryAnnounceInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			err := p.state.container.EnqueueOnPluginGoRoutine(p.onNetDiscoveryTick)
			if err != nil {
				fmt.Printf("%T runNetDiscoveryTicker: unable to enqueue tick: %v\n", *p, err)
			}
		case <-stopCh:
			return
		}
	}
}

func (p *plugin) onNetDiscoveryTick() {
	p.announceEntityChange()
	p.expireStaleRemoteEntities()
}

func (p *plugin) handleDiscoveryEvent(event discovery.Event) {
	switch event.Envelope.Type {
	case discovery.MessageTypeQuery:
		p.handleDiscoveryQuery(event)
	case discovery.MessageTypeAnnounce:
		p.handleDiscoveryAnnounce(event)
	case discovery.MessageTypeGoodbye:
		p.handleDiscoveryGoodbye(event)
	default:
		fmt.Printf("%T handleDiscoveryEvent: unknown message type %q from %s\n", *p, event.Envelope.Type, event.Sender)
	}
}

// handleDiscoveryQuery answers a peer's "who's out there" with our current entities, if we're
// configured to announce at all. A node that only discovers (EnableDiscover, not
// EnableAnnounce) never reaches here, because its Transport isn't listening for Query in the
// first place (see startNetDiscovery's receive-direction comment).
func (p *plugin) handleDiscoveryQuery(event discovery.Event) {
	if !p.config.NetDiscovery.EnableAnnounce {
		return
	}

	// Respond after a random delay rather than immediately - see
	// netDiscoveryQueryResponseJitter. time.AfterFunc fires on its own goroutine, so this never
	// blocks the plugin's own goroutine (which is what's running handleDiscoveryQuery right
	// now) for the delay; the actual send is enqueued back onto it when the timer fires, the
	// same way every other access to plugin state already has to be.
	delay := randomDelayIn(0, netDiscoveryQueryResponseJitter)
	time.AfterFunc(delay, func() {
		err := p.state.container.EnqueueOnPluginGoRoutine(func() {
			p.respondToDiscoveryQuery(event)
		})
		if err != nil {
			fmt.Printf("%T handleDiscoveryQuery: unable to enqueue delayed response to %s: %v\n", *p, event.Sender, err)
		}
	})
}

func (p *plugin) respondToDiscoveryQuery(event discovery.Event) {
	if p.state.netDiscovery == nil || !p.config.NetDiscovery.EnableAnnounce {
		// Net discovery (or specifically announcing) may have been stopped in the time it took
		// this response's jitter delay to elapse - nothing to do in that case.
		return
	}

	if err := p.state.netDiscovery.service.SendAnnounce(p.buildEntityAnnouncements()); err != nil {
		fmt.Printf("%T respondToDiscoveryQuery: unable to respond to query from %s: %v\n", *p, event.Sender, err)
	}
}

// handleDiscoveryAnnounce mirrors a peer's announced entities in as local entities, keyed by
// remoteEntityId (namespaced by event.Sender - the peer's persisted identity, not its
// possibly-changing source IP; see remoteEntityId). Each one is created, the first time it's
// seen, with tracking.Config{} - remote entities are never auto-tracked into dblog: the peer
// that actually owns the device is responsible for tracking it (if it chooses to), and tracking
// the same readings a second time here, under a second and strictly less direct source, would
// just duplicate that history rather than add anything.
func (p *plugin) handleDiscoveryAnnounce(event discovery.Event) {
	if !p.config.NetDiscovery.EnableDiscover {
		return
	}

	var announcements []discovery.EntityAnnouncement
	if err := json.Unmarshal(event.Envelope.Body, &announcements); err != nil {
		fmt.Printf("%T handleDiscoveryAnnounce: malformed body from %s: %v\n", *p, event.Sender, err)
		return
	}

	for _, announcement := range announcements {
		if err := p.applyRemoteAnnouncement(event, announcement); err != nil {
			fmt.Printf("%T handleDiscoveryAnnounce: unable to apply %s %q from %s: %v\n",
				*p, announcement.EntityKind, announcement.EntityId, event.Sender, err)
		}
	}
}

func (p *plugin) applyRemoteAnnouncement(event discovery.Event, announcement discovery.EntityAnnouncement) error {
	id := remoteEntityId(event.Sender, announcement.EntityId)

	// Prefer a local copy of the same device over mirroring it: announcement.EntityId is the
	// raw id the announcing node itself registered it under (see buildEntityAnnouncements),
	// and for every current producer that's already a globally-stable identity in its own
	// right - an SDM device resource string, a Bluetooth MAC address - not a locally-scoped
	// counter, despite EntityId only being *documented* as unique within the announcing node
	// (true for a hypothetical future producer that isn't so lucky). A raw id can only ever
	// collide with a LOCAL entity's map key here, never another peer's mirrored one, since
	// every mirrored key carries the remoteEntityIdPrefix and a raw device id realistically
	// never does. This is exactly the case that bit us with Nest thermostats: a node running
	// its own nestthermostat plugin also hearing the *same* thermostat announced by a peer that
	// also runs it - without this, both ended up in p.state.entities as two distinct entries
	// for the same physical device.
	if _, hasLocalCopy := p.state.entities[announcement.EntityId]; hasLocalCopy {
		// If an earlier Announce already created a mirrored duplicate (e.g. before this node's
		// own local copy existed, or before this check existed), remove it now rather than
		// leaving a stale duplicate sitting around indefinitely.
		if _, staleShadowExists := p.state.entities[id]; staleShadowExists {
			p.removeRemoteEntity(id)
		}
		return nil
	}

	tracked, exists := p.state.entities[id]
	if !exists {
		switch announcement.EntityKind {
		case "WirelessThermometer":
			tracked = p.createRemoteWirelessThermometer(id, announcement.Name)
		case "Thermostat":
			tracked = p.createRemoteThermostat(id, announcement.Name)
		default:
			return fmt.Errorf("unknown entity kind %q", announcement.EntityKind)
		}
		p.state.entities[id] = tracked
	}

	// Refresh TTL/display metadata regardless of what happens below - hearing about this
	// entity at all, even if its payload turns out malformed, is still evidence the peer and
	// this entity are alive.
	p.state.netDiscovery.remoteMeta[id] = &remoteEntityMeta{
		sourceIP: sourceIPString(event.From),
		lastSeen: time.Now(),
	}

	newState, err := decodeAnnouncementState(announcement)
	if err != nil {
		return err
	}

	return tracked.ProcessNewState(newState, func(pmaasEntityId string, event any) {
		if err := p.state.container.BroadcastEvent(pmaasEntityId, event); err != nil {
			fmt.Printf("%T applyRemoteAnnouncement: error broadcasting event %v: %v\n", *p, event, err)
		}
	})
}

func decodeAnnouncementState(announcement discovery.EntityAnnouncement) (any, error) {
	switch announcement.EntityKind {
	case "WirelessThermometer":
		var decoded spienvironment.WirelessThermometer
		if err := json.Unmarshal(announcement.State, &decoded); err != nil {
			return nil, fmt.Errorf("decode WirelessThermometer state: %w", err)
		}
		return decoded, nil
	case "Thermostat":
		var decoded spienvironment.Thermostat
		if err := json.Unmarshal(announcement.State, &decoded); err != nil {
			return nil, fmt.Errorf("decode Thermostat state: %w", err)
		}
		return decoded, nil
	default:
		return nil, fmt.Errorf("unknown entity kind %q", announcement.EntityKind)
	}
}

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
	} else {
		instance.PmaasEntityId = pmaasEntityId
	}

	return instance
}

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
	} else {
		instance.PmaasEntityId = pmaasEntityId
	}

	return instance
}

// handleDiscoveryGoodbye drops every locally-mirrored entity that came from the departing peer,
// immediately rather than waiting for netDiscoveryEntityTTL to expire it. It's the fast path for
// a clean shutdown; expireStaleRemoteEntities is the slower fallback for everything else (a
// crash, a pulled cable, a network partition) that never gets to send Goodbye at all.
func (p *plugin) handleDiscoveryGoodbye(event discovery.Event) {
	if !p.config.NetDiscovery.EnableDiscover {
		return
	}

	for _, id := range p.remoteEntityIdsForSender(event.Sender) {
		p.removeRemoteEntity(id)
	}
}

// expireStaleRemoteEntities drops any mirrored entity not mentioned in an Announce for at least
// netDiscoveryEntityTTL - see runNetDiscoveryTicker.
func (p *plugin) expireStaleRemoteEntities() {
	if p.state.netDiscovery == nil || !p.config.NetDiscovery.EnableDiscover {
		return
	}

	cutoff := time.Now().Add(-netDiscoveryEntityTTL)

	for id, meta := range p.state.netDiscovery.remoteMeta {
		if meta.lastSeen.After(cutoff) {
			continue
		}

		fmt.Printf("%T expireStaleRemoteEntities: %s not seen since %s, removing\n", *p, id, meta.lastSeen)
		p.removeRemoteEntity(id)
	}
}

func (p *plugin) removeRemoteEntity(id string) {
	tracked, ok := p.state.entities[id]
	if !ok {
		return
	}

	if pmaasEntityId := remotePmaasEntityId(tracked); pmaasEntityId != "" {
		if err := p.state.container.DeregisterEntity(pmaasEntityId); err != nil {
			fmt.Printf("%T removeRemoteEntity: unable to deregister %s: %v\n", *p, id, err)
		}
	}

	delete(p.state.entities, id)
	if p.state.netDiscovery != nil {
		delete(p.state.netDiscovery.remoteMeta, id)
	}
}

func remotePmaasEntityId(tracked common.IStateTracker) string {
	switch state := tracked.GetState().(type) {
	case thermometer.WirelessThermometer:
		return state.PmaasEntityId
	case thermometer.Thermostat:
		return state.PmaasEntityId
	default:
		return ""
	}
}

func sourceIPString(addr *net.UDPAddr) string {
	if addr == nil {
		return ""
	}
	return addr.IP.String()
}

// remoteEntityId builds the local entity-map key for an entity announced by senderId,
// namespaced by that sender so it can never collide with another peer reusing the same
// EntityAnnouncement.EntityId (which is only unique within its own announcing node). senderId
// is the peer's persisted discovery.InstanceID (see loadOrCreateSenderId), not its source IP:
// unlike an IP, which can change under a peer that keeps running the whole time (DHCP renewal,
// moving networks), the persisted id is exactly the identity meant to stay stable for as long as
// that peer keeps existing - the IP is tracked alongside it as metadata (remoteEntityMeta) for
// display, but deliberately isn't part of the key itself.
func remoteEntityId(senderId discovery.InstanceID, announcedId string) string {
	return remoteEntityIdPrefix + string(senderId) + ":" + announcedId
}

func isRemoteEntityId(id string) bool {
	return strings.HasPrefix(id, remoteEntityIdPrefix)
}

// removeRemoteShadowsOf removes every remote-mirrored shadow entity for rawId, from whichever
// peer(s) announced it, if any exist. Called when a LOCAL entity for the exact same raw id is
// about to be registered (see onEntityRegistered), so the local, authoritative copy always
// wins over a mirrored one - the reverse of the ordering applyRemoteAnnouncement already
// handles (there, the local copy already existed when the remote Announce arrived; here, the
// remote shadow already exists when the local copy arrives).
func (p *plugin) removeRemoteShadowsOf(rawId string) {
	if !p.config.NetDiscovery.EnableDiscover {
		// No remote shadow could exist at all without EnableDiscover - see
		// handleDiscoveryAnnounce's own identical guard.
		return
	}

	for _, id := range p.remoteEntityIdsForRawId(rawId) {
		p.removeRemoteEntity(id)
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

// TODO: Implement the deboaunce.  Do we send all states whenever one changes or only the one that changed?
// announceEntityChange re-announces this node's full current entity set if EnableAnnounce is
// set. Called both on the netDiscoveryAnnounceInterval heartbeat (runNetDiscoveryTicker) and
// after any local change to p.state.entities (see registerWirelessThermometer,
// registerThermostat, onEntityStateChanged), so peers see a change promptly without waiting for
// the next heartbeat, while the heartbeat itself guarantees they never wait longer than
// netDiscoveryAnnounceInterval even if nothing changes - a real implementation would likely want
// to debounce/coalesce the change-triggered announces rather than send one on every single state
// change (e.g. every temperature reading).
func (p *plugin) announceEntityChange() {
	if p.state.netDiscovery == nil || !p.config.NetDiscovery.EnableAnnounce {
		return
	}

	if err := p.state.netDiscovery.service.SendAnnounce(p.buildEntityAnnouncements()); err != nil {
		fmt.Printf("%T announceEntityChange: unable to announce: %v\n", *p, err)
	}
}

// buildEntityAnnouncements converts this node's own entities into the wire format, skipping
// any entity that was itself mirrored in from another peer (isRemoteEntityId) - relaying a
// peer's entities back out under our own announcement would let a third node see two different
// apparent owners for the same entity depending on which of us it happened to hear from first,
// and that peer is already the authoritative announcer for it regardless.
func (p *plugin) buildEntityAnnouncements() []discovery.EntityAnnouncement {
	announcements := make([]discovery.EntityAnnouncement, 0, len(p.state.entities))

	for id, tracked := range p.state.entities {
		if isRemoteEntityId(id) {
			continue
		}

		var kind, name string
		var portableState any

		switch state := tracked.GetState().(type) {
		case thermometer.WirelessThermometer:
			kind, name, portableState = "WirelessThermometer", state.Name, spienvironment.WirelessThermometer{
				Name:        state.Name,
				RSSIData:    state.RSSIData,
				BatteryData: state.BatteryData,
				SensorData:  state.SensorData,
			}
		case thermometer.Thermostat:
			kind, name, portableState = "Thermostat", state.Name, spienvironment.Thermostat{
				Name:           state.Name,
				SensorData:     state.SensorData,
				HvacStatus:     state.HvacStatus,
				Mode:           state.Mode,
				EcoMode:        state.EcoMode,
				HeatSetpoint:   state.HeatSetpoint,
				CoolSetpoint:   state.CoolSetpoint,
				Connectivity:   state.Connectivity,
				OfflineSince:   state.OfflineSince,
				OnlineSince:    state.OnlineSince,
				LastUpdateTime: state.SensorData.LastUpdateTime,
			}
		default:
			continue
		}

		encodedState, err := json.Marshal(portableState)
		if err != nil {
			fmt.Printf("%T buildEntityAnnouncements: unable to encode state for %s: %v\n", *p, id, err)
			continue
		}

		announcements = append(announcements, discovery.EntityAnnouncement{
			EntityId:   id,
			EntityKind: kind,
			Name:       name,
			State:      encodedState,
			AsOf:       time.Now(),
		})
	}

	return announcements
}
