package common

// EntityStore is how internal/http reaches back into the plugin for the entities to render -
// implemented by the plugin's entityStoreAdapter, which crosses onto the plugin's own goroutine
// internally (see spi.Exec).
type EntityStore interface {
	GetEntities() ([]any, error)
}
