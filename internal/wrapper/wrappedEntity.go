package wrapper

import (
	"fmt"
	"reflect"
	"time"
)

func CreateWrappedEntity(
	idPrefix string,
	instanceId int,
	targetEntityId string,
	name string,
	entityType reflect.Type) WrappedEntity {
	return WrappedEntity{
		Id:             fmt.Sprintf("%s_%d", idPrefix, instanceId),
		TargetEntityId: targetEntityId,
		Name:           name,
		EntityType:     entityType,
		LastUpdateTime: time.Now(),
	}
}

type WrappedEntity struct {
	Id             string
	TargetEntityId string
	PmaasEntityId  string
	Name           string
	EntityType     reflect.Type
	LastUpdateTime time.Time
}

func (e *WrappedEntity) GetSortKey() string {
	if e.Name == "" {
		return e.Id
	}

	return e.Name
}
