package entities

import (
	"reflect"
)

type WirelessThermometer interface {
	Thermometer
}

var WirelessThermometerType = reflect.TypeOf((*WirelessThermometer)(nil)).Elem()
