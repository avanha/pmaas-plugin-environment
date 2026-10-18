package thermometer

import (
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/avanha/pmaas-plugin-environment/entities"
	"github.com/avanha/pmaas-spi/common"
	"github.com/avanha/pmaas-spi/tracking"
)

type thermostatStub struct {
	log                    *slog.Logger
	id                     string
	closeFn                func() error
	entityWrapperReference atomic.Pointer[common.ThreadSafeEntityWrapper[entities.Thermostat]]
}

func (s *thermostatStub) TrackingConfig() tracking.Config {
	return common.ThreadSafeEntityWrapperExecValueFunc(
		s.entityWrapperReference.Load(),
		func(target entities.Thermostat) tracking.Config { return target.TrackingConfig() })
}

func (s *thermostatStub) Data() tracking.DataSample {
	return common.ThreadSafeEntityWrapperExecValueFunc(
		s.entityWrapperReference.Load(),
		func(target entities.Thermostat) tracking.DataSample { return target.Data() })
}

func (s *thermostatStub) GetSortKey() string {
	return common.ThreadSafeEntityWrapperExecValueFunc(
		s.entityWrapperReference.Load(),
		func(target entities.Thermostat) string { return target.GetSortKey() })
}

func newThermostatStub(
	log *slog.Logger,
	id string,
	entityWrapper *common.ThreadSafeEntityWrapper[entities.Thermostat]) *thermostatStub {
	instance := &thermostatStub{
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

func (s *thermostatStub) close() {
	closeFn := s.closeFn

	if closeFn == nil {
		return
	}

	err := closeFn()

	if err != nil {
		s.log.Error("failed to close thermostat stub", "id", s.id, "error", err)
	}
}
