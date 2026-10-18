package thermometer

import (
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/avanha/pmaas-plugin-environment/entities"
	"github.com/avanha/pmaas-spi/common"
	"github.com/avanha/pmaas-spi/tracking"
)

type wirelessThermometerStub struct {
	log                    *slog.Logger
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
	log *slog.Logger,
	id string,
	entityWrapper *common.ThreadSafeEntityWrapper[entities.WirelessThermometer]) *wirelessThermometerStub {
	instance := &wirelessThermometerStub{
		log: log,
		id:  id,
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
		s.log.Error("failed to close wirelessThermometer stub", "id", s.id, "error", err)
	}
}
