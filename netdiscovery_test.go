package environment

import (
	"encoding/json"
	"errors"
	"io/fs"
	"testing"
	"time"

	"github.com/avanha/pmaas-common/net/discovery"
	"github.com/avanha/pmaas-plugin-environment/config"
	spi "github.com/avanha/pmaas-spi"
	spienvironment "github.com/avanha/pmaas-spi/environment"
)

type recordingSink struct {
	applied []time.Time
}

func (r *recordingSink) Snapshot() []EntitySnapshot                                { return nil }
func (r *recordingSink) SnapshotOne(string) (EntitySnapshot, bool)                 { return EntitySnapshot{}, false }
func (r *recordingSink) RemoveRemoteEntitiesFromSender(discovery.InstanceID) error { return nil }
func (r *recordingSink) RemoveStaleRemoteEntities([]RemoteEntityRef) error         { return nil }
func (r *recordingSink) ApplyRemoteAnnouncement(
	_ discovery.InstanceID, _, _, _ string, _ any, asOf time.Time) error {
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
	}
	event := discovery.Event{Sender: "peer"}
	base := time.Now()

	nd.applyAnnouncement(event, newTestAnnouncement(t, base.Add(2*time.Second)))
	nd.applyAnnouncement(event, newTestAnnouncement(t, base.Add(1*time.Second))) // late, older
	nd.applyAnnouncement(event, newTestAnnouncement(t, base.Add(3*time.Second)))
	nd.applyAnnouncement(event, newTestAnnouncement(t, time.Time{})) // peer without AsOf: always applied

	if len(sink.applied) != 3 {
		t.Fatalf("expected 3 applied announcements, got %d: %v", len(sink.applied), sink.applied)
	}
	if !sink.applied[0].Equal(base.Add(2*time.Second)) || !sink.applied[1].Equal(base.Add(3*time.Second)) {
		t.Fatalf("unexpected applied sequence: %v", sink.applied)
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
