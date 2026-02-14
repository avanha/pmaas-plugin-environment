package thermometer

import (
	"fmt"
	"sync/atomic"

	"pmaas.io/plugins/environment/entities"
	"pmaas.io/spi/common"
	"pmaas.io/spi/tracking"
)

type wirelessThermometerStub struct {
	id                     string
	closeFn                func() error
	entityWrapperReference atomic.Pointer[common.ThreadSafeEntityWrapper[entities.WirelessThermometer]]
}

func (s *wirelessThermometerStub) TrackingConfig() tracking.Config {
	return common.ThreadSafeEntityWrapperExecValueFunc(
		s.entityWrapperReference.Load(),
		func(target entities.WirelessThermometer) tracking.Config { return target.TrackingConfig() })
}

func (s *wirelessThermometerStub) Data() tracking.DataSample {
	return common.ThreadSafeEntityWrapperExecValueFunc(
		s.entityWrapperReference.Load(),
		func(target entities.WirelessThermometer) tracking.DataSample { return target.Data() })
}

func (s *wirelessThermometerStub) GetSortKey() string {
	return common.ThreadSafeEntityWrapperExecValueFunc(
		s.entityWrapperReference.Load(),
		func(target entities.WirelessThermometer) string { return target.GetSortKey() })
}

func newWirelessThermometerStub(
	id string,
	entityWrapper *common.ThreadSafeEntityWrapper[entities.WirelessThermometer]) *wirelessThermometerStub {
	instance := &wirelessThermometerStub{
		id: id,
	}

	instance.entityWrapperReference.Store(entityWrapper)

	instance.closeFn = func() error {
		if instance.entityWrapperReference.CompareAndSwap(entityWrapper, nil) {
			instance.closeFn = nil
			return nil
		}

		return fmt.Errorf("failed to clear entity wrapper, current value does not match expected value")
	}

	return instance
}

func (s *wirelessThermometerStub) close() {
	closeFn := s.closeFn

	if closeFn == nil {
		return
	}

	err := closeFn()

	if err != nil {
		fmt.Printf("Failed to close wirelessThermometer stub %s: %v", s.id, err)
	}
}
