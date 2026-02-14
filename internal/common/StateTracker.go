package common

type IStateTracker interface {
	ProcessNewState(newState any, publishEvent func(pmaasEntityId string, event any)) error
	GetState() any
}
