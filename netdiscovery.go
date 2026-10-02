package environment

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net"
	"sync"
	"time"

	"github.com/avanha/pmaas-common/mailbox"
	"github.com/avanha/pmaas-common/net/discovery"
	spienvironment "github.com/avanha/pmaas-spi/environment"
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

// netDiscoveryAnnounceInterval is both how often an EnableAnnounce node re-announces its full
// entity set (a heartbeat, on top of the change-triggered announces already sent via
// EntityChanged), and how often an EnableDiscover node sweeps for entities it hasn't heard
// about in netDiscoveryEntityTTL - see runTicker.
const netDiscoveryAnnounceInterval = 30 * time.Minute

// netDiscoveryEntityTTL is how long a mirrored entity is kept after the last Announce that
// mentioned it, before it's dropped as stale. This is what actually cleans up after a peer that
// disappears *without* sending Goodbye (a crash, a pulled cable, a network partition) -
// handleGoodbye handles the clean-shutdown case immediately, this handles everything else, just
// on a slower, TTL-driven schedule.
const netDiscoveryEntityTTL = 2 * time.Hour

// EntitySnapshot is one of this node's own entities, in the plugin-agnostic shape netDiscovery
// needs to build an outgoing Announce - decoupled from both the wire format
// (discovery.EntityAnnouncement) and the plugin's own concrete thermometer/thermostat types, so
// netDiscovery never needs to import either.
type EntitySnapshot struct {
	RawId string
	Kind  string
	Name  string
	State any
	AsOf  time.Time
}

// RemoteEntityRef identifies one entity mirrored in from a peer, by the peer's persisted
// identity and the raw id it announced that entity under.
type RemoteEntityRef struct {
	Sender discovery.InstanceID
	RawId  string
}

// remoteEntitySink is how netDiscovery reaches back for the things only the plugin can decide -
// anything that touches the plugin's own entity map. Every method is invoked on the plugin's own
// goroutine (netDiscovery delivers each call through toPlugin, never directly from its own
// goroutine), so implementations touch plugin state freely and must not hop goroutines
// themselves. netDiscovery never waits for the result.
type remoteEntitySink interface {
	// ApplyRemoteAnnouncement mirrors one already-decoded entity announced by sender into the
	// plugin's own entity map, creating it on first sight and updating it thereafter.
	ApplyRemoteAnnouncement(sender discovery.InstanceID, kind, rawId, name string, decodedState any, asOf time.Time) error

	// RemoveRemoteEntitiesFromSender drops every entity previously mirrored in from sender -
	// called on a clean Goodbye.
	RemoveRemoteEntitiesFromSender(sender discovery.InstanceID) error

	// RemoveStaleRemoteEntities drops the mirrored entities identified by refs, because
	// netDiscovery hasn't heard an Announce mentioning them in netDiscoveryEntityTTL.
	RemoveStaleRemoteEntities(refs []RemoteEntityRef) error
}

// remoteEntityMeta is netDiscovery's own liveness bookkeeping for one mirrored entity: when it
// was last mentioned in an Announce (for TTL expiry) and the peer's most recent source IP, kept
// purely as informational/debugging metadata - e.g. a future "mirrored from 10.0.1.42" hover in
// the UI. It's tracked independently of whether the plugin actually materializes a shadow entity
// for it (see applyAnnouncement's hasLocalCopy note in remoteentities.go) - a harmless phantom
// entry for an id the plugin is shadowing with its own local copy just expires on its own via the
// same TTL sweep as everything else.
type remoteEntityMeta struct {
	sourceIP string
	lastSeen time.Time

	// lastAsOf is the newest EntitySnapshot.AsOf (the announcing node's own clock) applied for this
	// entity, used to drop an Announce that UDP delivered out of order - see applyAnnouncement. Only
	// ever compared against other values from the same sender, so clock skew between nodes doesn't
	// matter. Zero until an announcement carrying an AsOf has been applied.
	lastAsOf time.Time
}

// netDiscovery is its own actor - own Mailbox, own goroutines - running optional multicast
// discovery per NetDiscoveryConfig: broadcasting this node's entities to the local network,
// and/or listening for other instances doing the same. It owns the wire protocol end to end -
// the transport, JSON encode/decode, jitter/ticker timing, and its own remoteMeta liveness
// bookkeeping - and knows nothing about thermometer/thermostat types or the plugin's entity map;
// see remoteEntitySink for the narrow interface it uses instead of reaching into plugin state
// directly.
type netDiscovery struct {
	instanceID discovery.InstanceID
	service    *discovery.Service
	sink       remoteEntitySink

	announceEnabled bool
	discoverEnabled bool

	mailbox *mailbox.Mailbox

	// remoteMeta[sender][rawId] - see remoteEntityMeta. Only ever touched on the mailbox
	// goroutine.
	remoteMeta map[discovery.InstanceID]map[string]*remoteEntityMeta

	tickerStopCh chan struct{}

	// localEntities is netDiscovery's own copy of this node's locally-sourced entities, kept current
	// by the plugin pushing changes in (EntityChanged/EntityRemoved) rather than netDiscovery
	// calling back to ask. That's what keeps netDiscovery from ever blocking on the plugin
	// goroutine - see outbox. Only ever touched on the mailbox goroutine.
	localEntities map[string]EntitySnapshot

	// fromPlugin carries closures from the plugin goroutine onto netDiscovery's mailbox goroutine;
	// toPlugin carries them the other way. Both are non-blocking for the sender.
	fromPlugin *outbox
	toPlugin   *outbox

	// loops tracks runEventLoop and runTicker, so Stop can wait for them.
	loops    sync.WaitGroup
	stopOnce sync.Once

	// startupQueryTimer is the pending delayed initial Query, nil unless EnableDiscover.
	startupQueryTimer *time.Timer
}

// newNetDiscovery brings up multicast discovery per config, if any of it is enabled, and starts
// its own goroutines (event loop, ticker, and - if EnableDiscover - the delayed startup query).
// Returns a nil *netDiscovery and a nil error when neither flag is set, so a fully-disabled
// config is a no-op for the caller.
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
//
// runOnPlugin enqueues a function on the plugin's own goroutine - it may block until that goroutine
// is free, which is why it's only ever called from toPlugin's delivery goroutine.
func newNetDiscovery(
	instanceID discovery.InstanceID,
	config NetDiscoveryConfig,
	sink remoteEntitySink,
	runOnPlugin func(func()) error) (*netDiscovery, error) {
	if !config.EnableAnnounce && !config.EnableDiscover {
		return nil, nil
	}

	transport, err := discovery.NewTransport(instanceID, discovery.Config{
		GroupAddress:  config.GroupAddress,
		InterfaceName: config.InterfaceName,
		EnableSend:    config.EnableAnnounce || config.EnableDiscover,
		EnableReceive: config.EnableDiscover || config.EnableAnnounce,
		TTL:           config.TTL,
	})
	if err != nil {
		return nil, fmt.Errorf("unable to start transport: %w", err)
	}

	nd := &netDiscovery{
		instanceID:      instanceID,
		service:         discovery.NewService(transport),
		sink:            sink,
		announceEnabled: config.EnableAnnounce,
		discoverEnabled: config.EnableDiscover,
		mailbox:         mailbox.NewMailbox(),
		remoteMeta:      make(map[discovery.InstanceID]map[string]*remoteEntityMeta),
		localEntities:   make(map[string]EntitySnapshot),
		tickerStopCh:    make(chan struct{}),
	}

	nd.fromPlugin = newOutbox("fromPlugin", nd.mailbox.Send)
	nd.toPlugin = newOutbox("toPlugin", runOnPlugin)

	nd.loops.Go(nd.runEventLoop)
	nd.loops.Go(nd.runTicker)

	if config.EnableDiscover {
		// "Anyone here with thermometers?" - ask once at startup so entities from peers that
		// have been running for a while show up promptly, rather than waiting on the next
		// netDiscoveryAnnounceInterval heartbeat. Delayed rather than sent immediately - see
		// netDiscoveryStartupDelayMin/Max - and dispatched via time.AfterFunc so the delay never
		// blocks newNetDiscovery's caller.
		delay := randomDelayIn(netDiscoveryStartupDelayMin, netDiscoveryStartupDelayMax)
		nd.startupQueryTimer = time.AfterFunc(delay, func() {
			if err := nd.mailbox.Send(nd.sendInitialQuery); err != nil {
				// netDiscovery was stopped before the startup delay elapsed - nothing to do.
				fmt.Printf("netDiscovery: unable to enqueue initial query: %v\n", err)
			}
		})
	}

	return nd, nil
}

func (nd *netDiscovery) sendInitialQuery() {
	if err := nd.service.SendQuery(); err != nil {
		fmt.Printf("netDiscovery: unable to send initial query: %v\n", err)
	}
}

// randomDelayIn returns a random duration in [minDelay, maxDelay).
func randomDelayIn(minDelay, maxDelay time.Duration) time.Duration {
	return minDelay + time.Duration(rand.Int64N(int64(maxDelay-minDelay)))
}

// Stop tears this actor down and blocks until it's completely finished, so it must NOT be called
// on the plugin goroutine: the toPlugin outbox needs that goroutine free to drain. Sequence: stop
// the ticker, send Goodbye if announcing, close the transport/service and wait for the loops that
// feed the mailbox, drain what the plugin already posted, stop the mailbox, then drain what
// netDiscovery already posted back to the plugin. Safe to call more than once.
func (nd *netDiscovery) Stop() {
	nd.stopOnce.Do(nd.stop)
}

func (nd *netDiscovery) stop() {
	close(nd.tickerStopCh)

	if nd.startupQueryTimer != nil {
		nd.startupQueryTimer.Stop()
	}

	if nd.announceEnabled {
		if err := nd.service.SendGoodbye(); err != nil {
			fmt.Printf("netDiscovery: unable to send goodbye: %v\n", err)
		}
	}

	if err := nd.service.Close(); err != nil {
		fmt.Printf("netDiscovery: error closing discovery service: %v\n", err)
	}

	nd.loops.Wait()
	nd.fromPlugin.Stop()
	nd.mailbox.Stop()
	nd.toPlugin.Stop()
}

// EntityChanged tells netDiscovery the current state of one of this node's locally-sourced
// entities - on registration and on every state change. It records the snapshot for future
// heartbeats and query responses, and announces just that entity promptly instead of waiting for
// the next heartbeat. Never blocks. Safe to call any time, including after Stop (the post just
// fails and is logged).
func (nd *netDiscovery) EntityChanged(snapshot EntitySnapshot) {
	err := nd.fromPlugin.Post(func() {
		nd.localEntities[snapshot.RawId] = snapshot
		nd.announceOne(snapshot)
	})
	if err != nil {
		fmt.Printf("netDiscovery: unable to post change for %s: %v\n", snapshot.RawId, err)
	}
}

// Seed records entities that were already tracked before netDiscovery started, without announcing
// them individually. Never blocks.
func (nd *netDiscovery) Seed(snapshots []EntitySnapshot) {
	err := nd.fromPlugin.Post(func() {
		for _, snapshot := range snapshots {
			nd.localEntities[snapshot.RawId] = snapshot
		}
	})
	if err != nil {
		fmt.Printf("netDiscovery: unable to post seed: %v\n", err)
	}
}

// EntityRemoved tells netDiscovery that a locally-sourced entity no longer exists, so heartbeats
// and query responses stop advertising it. The protocol has no per-entity removal message, so
// peers drop their mirrored copy when it ages out (netDiscoveryEntityTTL). Also forgets any
// liveness bookkeeping for the raw id across every sender, so if a mirrored shadow of it ever
// reappears it starts from a clean TTL rather than a stale lastSeen. Never blocks.
func (nd *netDiscovery) EntityRemoved(rawId string) {
	err := nd.fromPlugin.Post(func() {
		delete(nd.localEntities, rawId)
		nd.forgetRawId(rawId)
	})
	if err != nil {
		fmt.Printf("netDiscovery: unable to post removal of %s: %v\n", rawId, err)
	}
}

// ForgetRawId drops liveness bookkeeping for rawId across every sender, without touching the
// local entity set. Called when a local entity for the same raw id is about to be registered, so
// that if the local copy is ever later removed, a shadow reappearing for it starts from a clean
// TTL. Never blocks.
func (nd *netDiscovery) ForgetRawId(rawId string) {
	if err := nd.fromPlugin.Post(func() { nd.forgetRawId(rawId) }); err != nil {
		fmt.Printf("netDiscovery: unable to post forgetting %s: %v\n", rawId, err)
	}
}

func (nd *netDiscovery) forgetRawId(rawId string) {
	for _, bySender := range nd.remoteMeta {
		delete(bySender, rawId)
	}
}

// runEventLoop reads decoded Envelopes as they arrive on the discovery Service's own goroutine,
// and hands each one to netDiscovery's own mailbox goroutine - decode, and every other access to
// netDiscovery's own state (remoteMeta), only ever happens there.
func (nd *netDiscovery) runEventLoop() {
	for event := range nd.service.Events() {
		if err := nd.mailbox.Send(func() { nd.handleEvent(event) }); err != nil {
			// netDiscovery is shutting down - this loop exits on its own once Close() finishes
			// closing service.Events().
			fmt.Printf("netDiscovery: unable to enqueue event: %v\n", err)
		}
	}
}

// runTicker drives both the periodic re-announce heartbeat and the stale-entity sweep off one
// shared ticker - both are on the same netDiscoveryAnnounceInterval cadence, so there's no reason
// to run two separate timers for them.
func (nd *netDiscovery) runTicker() {
	ticker := time.NewTicker(netDiscoveryAnnounceInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := nd.mailbox.Send(nd.onTick); err != nil {
				fmt.Printf("netDiscovery: unable to enqueue tick: %v\n", err)
			}
		case <-nd.tickerStopCh:
			return
		}
	}
}

func (nd *netDiscovery) onTick() {
	nd.announceNow()
	nd.sweepStale()
}

func (nd *netDiscovery) handleEvent(event discovery.Event) {
	switch event.Envelope.Type {
	case discovery.MessageTypeQuery:
		nd.handleQuery(event)
	case discovery.MessageTypeAnnounce:
		nd.handleAnnounce(event)
	case discovery.MessageTypeGoodbye:
		nd.handleGoodbye(event)
	default:
		fmt.Printf("netDiscovery: unknown message type %q from %s\n", event.Envelope.Type, event.Sender)
	}
}

// handleQuery answers a peer's "who's out there" with our current entities, if we're configured
// to announce at all. A node that only discovers (EnableDiscover, not EnableAnnounce) never
// reaches here, because its Transport isn't listening for Query in the first place (see
// newNetDiscovery's receive-direction comment).
func (nd *netDiscovery) handleQuery(event discovery.Event) {
	if !nd.announceEnabled {
		return
	}

	// Respond after a random delay rather than immediately - see netDiscoveryQueryResponseJitter.
	// time.AfterFunc fires on its own goroutine, so this never blocks the mailbox goroutine
	// (which is what's running handleQuery right now) for the delay.
	delay := randomDelayIn(0, netDiscoveryQueryResponseJitter)
	time.AfterFunc(delay, func() {
		if err := nd.mailbox.Send(nd.announceNow); err != nil {
			fmt.Printf("netDiscovery: unable to enqueue delayed response to %s: %v\n", event.Sender, err)
		}
	})
}

// announceNow sends every locally-sourced entity as one Announce - the periodic heartbeat (onTick)
// and a query response (handleQuery), since both send the full set.
func (nd *netDiscovery) announceNow() {
	if !nd.announceEnabled {
		return
	}

	if err := nd.service.SendAnnounce(nd.buildEntityAnnouncements()); err != nil {
		fmt.Printf("netDiscovery: unable to announce: %v\n", err)
	}
}

func (nd *netDiscovery) buildEntityAnnouncements() []discovery.EntityAnnouncement {
	announcements := make([]discovery.EntityAnnouncement, 0, len(nd.localEntities))

	for _, entity := range nd.localEntities {
		announcement, err := encodeAnnouncement(entity)
		if err != nil {
			fmt.Printf("netDiscovery: unable to encode state for %s: %v\n", entity.RawId, err)
			continue
		}

		announcements = append(announcements, announcement)
	}

	return announcements
}

// announceOne sends exactly one entity as a single-element Announce - the change-triggered
// counterpart to announceNow's full heartbeat, so one state change doesn't require re-encoding and
// re-sending every other entity this node knows about.
func (nd *netDiscovery) announceOne(snapshot EntitySnapshot) {
	if !nd.announceEnabled {
		return
	}

	announcement, err := encodeAnnouncement(snapshot)
	if err != nil {
		fmt.Printf("netDiscovery: unable to encode state for %s: %v\n", snapshot.RawId, err)
		return
	}

	if err := nd.service.SendAnnounce([]discovery.EntityAnnouncement{announcement}); err != nil {
		fmt.Printf("netDiscovery: unable to announce %s: %v\n", snapshot.RawId, err)
	}
}

func encodeAnnouncement(entity EntitySnapshot) (discovery.EntityAnnouncement, error) {
	encodedState, err := json.Marshal(entity.State)
	if err != nil {
		return discovery.EntityAnnouncement{}, err
	}

	return discovery.EntityAnnouncement{
		EntityId:   entity.RawId,
		EntityKind: entity.Kind,
		Name:       entity.Name,
		State:      encodedState,
		AsOf:       entity.AsOf,
	}, nil
}

// handleAnnounce mirrors a peer's announced entities in, decoding each one's state here - on
// netDiscovery's own goroutine, not the plugin's - before handing the already-decoded value to
// the sink.
func (nd *netDiscovery) handleAnnounce(event discovery.Event) {
	if !nd.discoverEnabled {
		return
	}

	var announcements []discovery.EntityAnnouncement
	if err := json.Unmarshal(event.Envelope.Body, &announcements); err != nil {
		fmt.Printf("netDiscovery: malformed announce body from %s: %v\n", event.Sender, err)
		return
	}

	for _, announcement := range announcements {
		nd.applyAnnouncement(event, announcement)
	}
}

func (nd *netDiscovery) applyAnnouncement(event discovery.Event, announcement discovery.EntityAnnouncement) {
	decodedState, err := decodeAnnouncementState(announcement)
	if err != nil {
		fmt.Printf("netDiscovery: unable to decode %s %q from %s: %v\n",
			announcement.EntityKind, announcement.EntityId, event.Sender, err)
		return
	}

	// Refresh TTL/display metadata regardless of what the sink ultimately does with this
	// announcement (e.g. it may be shadowed by a local copy and discarded) - hearing about it at
	// all is still evidence the peer and this entity are alive. See remoteEntityMeta's doc.
	meta := nd.touch(event.Sender, announcement.EntityId, event.From)

	// UDP doesn't preserve order: a change-triggered Announce can be overtaken by an earlier one
	// (or a heartbeat snapshot taken before the change), and applying it would roll the mirrored
	// state back until the next announce. A peer that doesn't send AsOf is always applied.
	if !announcement.AsOf.IsZero() {
		if announcement.AsOf.Before(meta.lastAsOf) {
			fmt.Printf("netDiscovery: dropping stale announce for %s %q from %s (as of %s, already applied %s)\n",
				announcement.EntityKind, announcement.EntityId, event.Sender, announcement.AsOf, meta.lastAsOf)
			return
		}

		meta.lastAsOf = announcement.AsOf
	}

	nd.postToPlugin(func() {
		err := nd.sink.ApplyRemoteAnnouncement(
			event.Sender, announcement.EntityKind, announcement.EntityId, announcement.Name, decodedState, announcement.AsOf)
		if err != nil {
			fmt.Printf("netDiscovery: unable to apply %s %q from %s: %v\n",
				announcement.EntityKind, announcement.EntityId, event.Sender, err)
		}
	})
}

// touch records that rawId from sender was just heard from, creating its bookkeeping on first
// sight, and returns it.
func (nd *netDiscovery) touch(sender discovery.InstanceID, rawId string, from *net.UDPAddr) *remoteEntityMeta {
	bySender, ok := nd.remoteMeta[sender]
	if !ok {
		bySender = make(map[string]*remoteEntityMeta)
		nd.remoteMeta[sender] = bySender
	}

	meta, ok := bySender[rawId]
	if !ok {
		meta = &remoteEntityMeta{}
		bySender[rawId] = meta
	}

	meta.sourceIP = sourceIPString(from)
	meta.lastSeen = time.Now()

	return meta
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

// handleGoodbye drops every locally-mirrored entity that came from the departing peer,
// immediately rather than waiting for netDiscoveryEntityTTL to expire it. It's the fast path for
// a clean shutdown; sweepStale is the slower fallback for everything else (a crash, a pulled
// cable, a network partition) that never gets to send Goodbye at all.
func (nd *netDiscovery) handleGoodbye(event discovery.Event) {
	if !nd.discoverEnabled {
		return
	}

	delete(nd.remoteMeta, event.Sender)

	nd.postToPlugin(func() {
		if err := nd.sink.RemoveRemoteEntitiesFromSender(event.Sender); err != nil {
			fmt.Printf("netDiscovery: unable to remove entities from %s: %v\n", event.Sender, err)
		}
	})
}

// sweepStale drops any mirrored entity not mentioned in an Announce for at least
// netDiscoveryEntityTTL - see runTicker.
func (nd *netDiscovery) sweepStale() {
	if !nd.discoverEnabled {
		return
	}

	cutoff := time.Now().Add(-netDiscoveryEntityTTL)
	var stale []RemoteEntityRef

	for sender, bySender := range nd.remoteMeta {
		for rawId, meta := range bySender {
			if meta.lastSeen.After(cutoff) {
				continue
			}
			stale = append(stale, RemoteEntityRef{Sender: sender, RawId: rawId})
			delete(bySender, rawId)
		}
	}

	if len(stale) == 0 {
		return
	}

	fmt.Printf("netDiscovery: %d entities not seen since %s, removing\n", len(stale), cutoff)

	nd.postToPlugin(func() {
		if err := nd.sink.RemoveStaleRemoteEntities(stale); err != nil {
			fmt.Printf("netDiscovery: unable to remove stale entities: %v\n", err)
		}
	})
}

// postToPlugin queues f to run on the plugin's goroutine, in order with everything else posted.
func (nd *netDiscovery) postToPlugin(f func()) {
	if err := nd.toPlugin.Post(f); err != nil {
		fmt.Printf("netDiscovery: unable to post to plugin: %v\n", err)
	}
}

func sourceIPString(addr *net.UDPAddr) string {
	if addr == nil {
		return ""
	}
	return addr.IP.String()
}
