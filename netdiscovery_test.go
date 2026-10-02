package environment

import (
	"encoding/json"
	"errors"
	"io/fs"
	"sync"
	"testing"
	"time"

	"github.com/avanha/pmaas-common/mailbox"
	"github.com/avanha/pmaas-common/net/discovery"
	"github.com/avanha/pmaas-plugin-environment/config"
	spi "github.com/avanha/pmaas-spi"
	spienvironment "github.com/avanha/pmaas-spi/environment"
)

type recordingSink struct {
	mu      sync.Mutex
	applied []time.Time
}

func (r *recordingSink) appliedTimes() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]time.Time(nil), r.applied...)
}

func (r *recordingSink) RemoveRemoteEntitiesFromSender(discovery.InstanceID) error { return nil }
func (r *recordingSink) RemoveStaleRemoteEntities([]RemoteEntityRef) error         { return nil }
func (r *recordingSink) ApplyRemoteAnnouncement(
	_ discovery.InstanceID, _, _, _ string, _ any, asOf time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.applied = append(r.applied, asOf)
	return nil
}

func newTestAnnouncement(t *testing.T, asOf time.Time) discovery.EntityAnnouncement {
	t.Helper()

	state, err := json.Marshal(spienvironment.WirelessThermometer{Name: "x"})
	if err != nil {
		t.Fatal(err)
	}

	return discovery.EntityAnnouncement{
		EntityId: "dev1", EntityKind: "WirelessThermometer", Name: "x", State: state, AsOf: asOf}
}

func TestApplyAnnouncement_DropsOutOfOrder(t *testing.T) {
	sink := &recordingSink{}
	nd := &netDiscovery{
		sink:       sink,
		remoteMeta: make(map[discovery.InstanceID]map[string]*remoteEntityMeta),
		toPlugin:   newOutbox("test", func(f func()) error { f(); return nil }),
	}
	event := discovery.Event{Sender: "peer"}
	base := time.Now()

	nd.applyAnnouncement(event, newTestAnnouncement(t, base.Add(2*time.Second)))
	nd.applyAnnouncement(event, newTestAnnouncement(t, base.Add(1*time.Second))) // late, older
	nd.applyAnnouncement(event, newTestAnnouncement(t, base.Add(3*time.Second)))
	nd.applyAnnouncement(event, newTestAnnouncement(t, time.Time{})) // peer without AsOf: always applied

	nd.toPlugin.Stop()
	applied := sink.appliedTimes()

	if len(applied) != 3 {
		t.Fatalf("expected 3 applied announcements, got %d: %v", len(applied), applied)
	}
	if !applied[0].Equal(base.Add(2*time.Second)) || !applied[1].Equal(base.Add(3*time.Second)) {
		t.Fatalf("unexpected applied sequence: %v", applied)
	}
}

type configContainer struct {
	spi.IPMAASContainer
	loadResult any
	loadErr    error
	saved      []any
}

func (c *configContainer) LoadConfig(func(string) any) (any, error) { return c.loadResult, c.loadErr }
func (c *configContainer) SaveConfig(cfg any) error {
	c.saved = append(c.saved, cfg)
	return nil
}

func newConfigPlugin(container spi.IPMAASContainer) *plugin {
	p := &plugin{}
	p.state.container = container
	return p
}

func TestLoadOrCreateSenderId_ReusesSavedId(t *testing.T) {
	c := &configContainer{loadResult: &config.PersistentConfigV1{NetDiscoverySenderId: "abc"}}

	id, err := newConfigPlugin(c).loadOrCreateSenderId()

	if err != nil || id != "abc" || len(c.saved) != 0 {
		t.Fatalf("expected saved id reused without saving, got id=%q err=%v saved=%v", id, err, c.saved)
	}
}

func TestLoadOrCreateSenderId_FirstRunCreatesAndSaves(t *testing.T) {
	c := &configContainer{loadErr: errors.Join(errors.New("failed to read config"), fs.ErrNotExist)}

	id, err := newConfigPlugin(c).loadOrCreateSenderId()

	if err != nil || id == "" || len(c.saved) != 1 {
		t.Fatalf("expected new id saved, got id=%q err=%v saved=%v", id, err, c.saved)
	}
}

func TestLoadOrCreateSenderId_OtherLoadErrorDoesNotOverwrite(t *testing.T) {
	c := &configContainer{loadErr: errors.New("corrupted envelope structure")}

	_, err := newConfigPlugin(c).loadOrCreateSenderId()

	if err == nil || len(c.saved) != 0 {
		t.Fatalf("expected error and no save, got err=%v saved=%v", err, c.saved)
	}
}

func TestStartNetDiscovery_LoadFailureDoesNotStart(t *testing.T) {
	c := &configContainer{loadErr: errors.New("boom")}
	p := newConfigPlugin(c)
	p.config.NetDiscovery = NetDiscoveryConfig{EnableAnnounce: true, GroupAddress: "239.192.42.1:9999"}

	p.startNetDiscovery()

	if p.state.netDiscovery != nil || len(c.saved) != 0 {
		t.Fatalf("expected discovery not started and nothing saved")
	}
}

// TestNetDiscovery_NoDeadlockBetweenPluginAndDiscoveryGoroutines drives both directions at once:
// the "plugin" goroutine keeps posting entity changes to discovery while discovery's goroutine
// keeps delivering applied announcements back onto the plugin goroutine. With direct blocking
// sends in both directions (both mailboxes are unbuffered) this wedges; with the outboxes it must
// finish.
func TestNetDiscovery_NoDeadlockBetweenPluginAndDiscoveryGoroutines(t *testing.T) {
	const rounds = 500

	pluginMailbox := mailbox.NewMailbox()
	defer pluginMailbox.Stop()

	var sink recordingSink
	nd := &netDiscovery{
		sink:          &sink,
		mailbox:       mailbox.NewMailbox(),
		remoteMeta:    make(map[discovery.InstanceID]map[string]*remoteEntityMeta),
		localEntities: make(map[string]EntitySnapshot),
	}
	nd.fromPlugin = newOutbox("fromPlugin", nd.mailbox.Send)
	nd.toPlugin = newOutbox("toPlugin", pluginMailbox.Send)

	done := make(chan struct{})

	go func() {
		defer close(done)

		var wg sync.WaitGroup

		// Plugin side: each change is posted from the plugin goroutine itself.
		wg.Go(func() {
			for i := 0; i < rounds; i++ {
				if err := pluginMailbox.Send(func() {
					nd.EntityChanged(EntitySnapshot{RawId: "local", Kind: "WirelessThermometer", AsOf: time.Now()})
				}); err != nil {
					t.Error(err)
					return
				}
			}
		})

		// Discovery side: announcements arriving on the discovery goroutine, each applied via the
		// plugin goroutine.
		wg.Go(func() {
			event := discovery.Event{Sender: "peer"}
			for i := 0; i < rounds; i++ {
				announcement := newTestAnnouncement(t, time.Now())
				if err := nd.mailbox.Send(func() { nd.applyAnnouncement(event, announcement) }); err != nil {
					t.Error(err)
					return
				}
			}
		})

		wg.Wait()

		// Shut down in the same order as stop(), minus the network parts.
		nd.fromPlugin.Stop()
		nd.mailbox.Stop()
		nd.toPlugin.Stop()
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("deadlock: plugin and discovery goroutines did not finish")
	}

	if got := len(sink.appliedTimes()); got != rounds {
		t.Fatalf("expected %d announcements applied, got %d", rounds, got)
	}
}

func TestNetDiscovery_RemovedEntityStopsBeingAdvertised(t *testing.T) {
	nd := &netDiscovery{
		sink:          &recordingSink{},
		mailbox:       mailbox.NewMailbox(),
		remoteMeta:    make(map[discovery.InstanceID]map[string]*remoteEntityMeta),
		localEntities: make(map[string]EntitySnapshot),
	}
	nd.fromPlugin = newOutbox("fromPlugin", nd.mailbox.Send)
	nd.toPlugin = newOutbox("toPlugin", func(f func()) error { f(); return nil })

	nd.EntityChanged(EntitySnapshot{RawId: "a", Kind: "Thermostat", State: spienvironment.Thermostat{}})
	nd.EntityChanged(EntitySnapshot{RawId: "b", Kind: "Thermostat", State: spienvironment.Thermostat{}})
	nd.EntityRemoved("a")

	nd.fromPlugin.Stop() // everything posted has been delivered to the mailbox
	announcements, err := nd.mailbox.Exec(nd.buildEntityAnnouncements)
	nd.mailbox.Stop()
	nd.toPlugin.Stop()

	if err != nil {
		t.Fatal(err)
	}
	if len(announcements) != 1 || announcements[0].EntityId != "b" {
		t.Fatalf("expected only entity b to be advertised, got %+v", announcements)
	}
}
