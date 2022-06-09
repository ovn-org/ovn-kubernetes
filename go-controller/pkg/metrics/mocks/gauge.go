package mocks

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	io_prometheus_client "github.com/prometheus/client_model/go"
)

type GaugeMock struct {
	value float64
}

func NewGaugeMock() *GaugeMock {
	return &GaugeMock{}
}

func (GaugeMock) Desc() *prometheus.Desc {
	panic("unimplemented")
}

func (GaugeMock) Write(*io_prometheus_client.Metric) error {
	panic("unimplemented")
}

func (GaugeMock) Describe(chan<- *prometheus.Desc) {
	panic("unimplemented")
}

func (GaugeMock) Collect(chan<- prometheus.Metric) {
	panic("unimplemented")
}

func (h *GaugeMock) Observe(value float64) {
	h.value = value
}

func (gm *GaugeMock) Set(value float64) {
	gm.value = value
}

func (gm *GaugeMock) Inc() {
	gm.value++
}

func (gm *GaugeMock) Dec() {
	gm.value--
}

func (gm *GaugeMock) Add(value float64) {
	gm.value += value
}

func (gm *GaugeMock) Sub(value float64) {
	gm.value -= value
}

func (gm *GaugeMock) SetToCurrentTime() {
	gm.value = float64(time.Now().UnixMilli())
}

func (gm *GaugeMock) GetValue() float64 {
	return gm.value
}
