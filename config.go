package environment

// NetDiscoveryConfig controls this plugin's optional multicast peer discovery: broadcasting
// the entities this instance knows about to the local network, and/or listening for other
// instances doing the same. See netdiscovery.go. Its zero value (both flags false) leaves net
// discovery fully disabled, matching every existing PluginConfig's zero-value default.
type NetDiscoveryConfig struct {
	// EnableAnnounce broadcasts this node's known entities to the local network - e.g. so an
	// environment plugin instance running elsewhere (a Mac dev machine that can't run the
	// Linux-only bluetooth plugin, for example) can discover and mirror them.
	EnableAnnounce bool

	// EnableDiscover listens for other nodes' announcements and queries for them on startup.
	// Independent of EnableAnnounce - a node can discover without ever announcing itself, or
	// announce without ever creating shadow entities from what it hears.
	EnableDiscover bool

	// GroupAddress is the multicast group and port used for discovery traffic, e.g.
	// "239.192.42.1:9999" - must be in the administratively-scoped range
	// 239.0.0.0/8-239.255.255.255. Required whenever EnableAnnounce or EnableDiscover is set.
	GroupAddress string

	// InterfaceName pins discovery traffic to one network interface (e.g. "en0"). Strongly
	// recommended on any multi-homed host - see discovery.Config.InterfaceName.
	InterfaceName string

	// TTL is the IP hop count on announced/sent packets. Left at its zero value, discovery
	// traffic never leaves the local network segment - see discovery.Config.TTL for why that's
	// the safe default, and what has to be true on the network (real multicast routing or an
	// IGMP-proxy feature bridging the relevant subnets, not just IGMP snooping/querier) before
	// raising this to let it cross a router onto another subnet/VLAN.
	TTL int
}

type PluginConfig struct {
	NetDiscovery NetDiscoveryConfig
}

func NewPluginConfig() PluginConfig {
	return PluginConfig{}
}
