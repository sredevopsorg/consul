// Code generated by mockery v1.0.0. DO NOT EDIT.

package autopilotevents

import (
	stream "github.com/hashicorp/consul/agent/consul/stream"
	mock "github.com/stretchr/testify/mock"
)

// MockPublisher is an autogenerated mock type for the Publisher type
type MockPublisher struct {
	mock.Mock
}

// Publish provides a mock function with given fields: _a0
func (_m *MockPublisher) Publish(_a0 []stream.Event) {
	_m.Called(_a0)
}
