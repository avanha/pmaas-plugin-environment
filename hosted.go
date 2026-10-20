package environment

import (
	"sync"

	"github.com/avanha/pmaas-spi/entity"
)

// hostedEntities is which entity of this plugin hosts each source entity (the Bluetooth plugin's thermometer, say): the plugin
// re-hosts them, with a fuller card than the source's, and an assembly that wants that card in a group names the source.
type hostedEntities struct {
	mu  sync.Mutex
	ids map[string]string
}

func newHostedEntities() *hostedEntities {
	return &hostedEntities{ids: make(map[string]string)}
}

// set records that the plugin's entity hosts the source's. Safe on nil.
func (h *hostedEntities) set(sourceId string, hostId string) {
	if h == nil {
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	h.ids[sourceId] = hostId
}

// remove forgets the source. Safe on nil.
func (h *hostedEntities) remove(sourceId string) {
	if h == nil {
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	delete(h.ids, sourceId)
}

func (h *hostedEntities) lookup(sourceId string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	id, ok := h.ids[sourceId]

	return id, ok
}

// hostedRef is the entity of this plugin that hosts a source entity.
type hostedRef struct {
	source entity.Ref
	hosted *hostedEntities
}

var _ entity.Ref = (*hostedRef)(nil)

// RegisteredId is the id of the entity that hosts the source, or false until the source is registered and this plugin has
// picked it up, and after either has gone.
func (r *hostedRef) RegisteredId() (string, bool) {
	sourceId, ok := r.source.RegisteredId()
	if !ok {
		return "", false
	}

	return r.hosted.lookup(sourceId)
}
