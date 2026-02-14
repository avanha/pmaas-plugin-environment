package entities

import (
	"pmaas.io/plugins/environment/internal/common"
	"pmaas.io/spi/tracking"
)

type Thermometer interface {
	tracking.Trackable
	common.ISortable
}
