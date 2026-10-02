package common

type IStateTracker interface {
	ProcessNewState(newState any, publishEvent func(pmaasEntityId string, event any)) error
	// GetState returns a snapshot copy for rendering. The copy never carries the live stub.
	GetState() any
	// GetPmaasEntityId returns the container-assigned id, or "" if not registered.
	GetPmaasEntityId() string
	// PortableState returns the entity's kind, name and its state in the network-portable shape
	// used for net discovery announcements.
	PortableState() (kind string, name string, state any)
	// CloseStubIfPresent closes the entity's stub, if one was handed out; see Thermostat.CloseStubIfPresent.
	CloseStubIfPresent()
}
