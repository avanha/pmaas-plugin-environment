package config

// PersistentConfigV1 is the plugin's state persisted across restarts via
// IPMAASContainer.SaveConfig/LoadConfig.
type PersistentConfigV1 struct {
	// NetDiscoverySenderId is this installation's stable identity for outbound net-discovery
	// messages (see netdiscovery.go), generated once and reused for as long as this file
	// exists. Deliberately separate from discovery.InstanceID's own randomness guarantee:
	// InstanceID only needs to be unique for one process's lifetime (so a Transport can filter
	// out its own packets), but a peer's remote-entity bookkeeping needs an identity that
	// survives this plugin restarting - otherwise every restart would make every one of this
	// node's entities look like a brand new peer to everyone listening. Cleared by deleting
	// this plugin's persisted config file, which causes a fresh id to be generated and saved on
	// the next start.
	NetDiscoverySenderId string
}
