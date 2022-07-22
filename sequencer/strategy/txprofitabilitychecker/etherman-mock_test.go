// Code generated by mockery v2.13.1. DO NOT EDIT.

package txprofitabilitychecker_test

import (
	context "context"
	big "math/big"

	mock "github.com/stretchr/testify/mock"

	types "github.com/ethereum/go-ethereum/core/types"
)

// etherman is an autogenerated mock type for the etherman type
type etherman struct {
	mock.Mock
}

// EstimateSendBatchCost provides a mock function with given fields: ctx, txs, maticAmount
func (_m *etherman) EstimateSendBatchCost(ctx context.Context, txs []*types.Transaction, maticAmount *big.Int) (*big.Int, error) {
	ret := _m.Called(ctx, txs, maticAmount)

	var r0 *big.Int
	if rf, ok := ret.Get(0).(func(context.Context, []*types.Transaction, *big.Int) *big.Int); ok {
		r0 = rf(ctx, txs, maticAmount)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*big.Int)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func(context.Context, []*types.Transaction, *big.Int) error); ok {
		r1 = rf(ctx, txs, maticAmount)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// GetCurrentSequencerCollateral provides a mock function with given fields:
func (_m *etherman) GetCurrentSequencerCollateral() (*big.Int, error) {
	ret := _m.Called()

	var r0 *big.Int
	if rf, ok := ret.Get(0).(func() *big.Int); ok {
		r0 = rf()
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*big.Int)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func() error); ok {
		r1 = rf()
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

type mockConstructorTestingTnewEtherman interface {
	mock.TestingT
	Cleanup(func())
}

// newEtherman creates a new instance of etherman. It also registers a testing interface on the mock and a cleanup function to assert the mocks expectations.
func newEtherman(t mockConstructorTestingTnewEtherman) *etherman {
	mock := &etherman{}
	mock.Mock.Test(t)

	t.Cleanup(func() { mock.AssertExpectations(t) })

	return mock
}