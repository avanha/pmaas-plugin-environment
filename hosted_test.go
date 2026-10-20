package environment

import (
	"log/slog"
	"reflect"
	"testing"

	"github.com/avanha/pmaas-plugin-environment/entities"
	"github.com/avanha/pmaas-plugin-environment/internal/common"
	"github.com/avanha/pmaas-spi"
	"github.com/avanha/pmaas-spi/group"
)

type sourceRef struct {
	id string
	ok bool
}

func (s *sourceRef) RegisteredId() (string, bool) { return s.id, s.ok }

type viewContainer struct {
	spi.IPMAASContainer
	views map[string]spi.EntityViewFunc
}

func (c *viewContainer) RegisterEntity(unique string, _ reflect.Type, _ string, _ spi.EntityStubFactoryFunc) (string, error) {
	return "environment_" + unique, nil
}

func (c *viewContainer) RegisterEntityView(id string, view spi.EntityViewFunc) error {
	c.views[id] = view
	return nil
}

func (c *viewContainer) DeregisterEntity(id string) error {
	delete(c.views, id)
	return nil
}

func TestOf_IsTheEntityThatHostsTheSourceOnceItIsRegisteredAndHasACardThatCoversTheSources(t *testing.T) {
	config := NewPluginConfig()
	source := &sourceRef{}
	hosted := config.Of(source)
	container := &viewContainer{views: map[string]spi.EntityViewFunc{}}
	p := &plugin{log: slog.New(slog.DiscardHandler), config: config}
	p.state.container = container
	p.state.entities = make(map[string]common.IStateTracker)

	if _, ok := hosted.RegisteredId(); ok {
		t.Fatal("has an id before the source is registered")
	}

	source.id, source.ok = "bluetooth_1", true

	if _, ok := hosted.RegisteredId(); ok {
		t.Fatal("has an id before the plugin has picked the source up")
	}

	p.processEntity("bluetooth_1", entities.WirelessThermometerType, "Master Bedroom", nil)

	id, ok := hosted.RegisteredId()
	if !ok || id == "" {
		t.Fatalf("%q %v", id, ok)
	}

	view, err := container.views[id]()
	if err != nil {
		t.Fatal(err)
	}

	if view.Key != "environment/bluetooth_1" || view.Category != group.CategoryThermometers ||
		!reflect.DeepEqual(view.Covers, []string{"bluetooth_1"}) || view.Value == nil {
		t.Errorf("%#v", view)
	}

	p.removeEntity("bluetooth_1")

	if _, ok := hosted.RegisteredId(); ok {
		t.Error("still has an id after the source was removed")
	}
}

func TestOf_NeedsAConfigFromNewPluginConfig(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("did not panic")
		}
	}()

	PluginConfig{}.Of(&sourceRef{})
}
