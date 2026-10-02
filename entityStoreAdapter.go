package environment

import (
	"github.com/avanha/pmaas-plugin-environment/internal/common"
	spi "github.com/avanha/pmaas-spi"
)

// Force implementation of common.EntityStore
var _ common.EntityStore = (*entityStoreAdapter)(nil)

type entityStoreAdapter struct {
	parent *plugin
}

// GetEntities implements common.EntityStore. HTTP requests come in on arbitrary goroutines, so
// execute getEntities on the main plugin goroutine to get all states atomically.
func (e *entityStoreAdapter) GetEntities() ([]any, error) {
	return spi.Exec(e.parent.state.container, e.parent.getEntities)
}
