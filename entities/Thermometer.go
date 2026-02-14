package entities

import (
	"github.com/avanha/pmaas-plugin-environment/internal/common"
	"github.com/avanha/pmaas-spi/tracking"
)

type Thermometer interface {
	tracking.Trackable
	common.ISortable
}
