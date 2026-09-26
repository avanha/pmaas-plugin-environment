package entities

import "reflect"

type Thermostat interface {
	Thermometer
}

var ThermostatType = reflect.TypeOf((*Thermostat)(nil)).Elem()
