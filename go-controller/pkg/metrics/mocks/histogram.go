package mocks

import (
	"github.com/prometheus/client_golang/prometheus"
	io_prometheus_client "github.com/prometheus/client_model/go"
)

type HistogramMock struct {
	ch chan float64
}

func NewHistogramMock() *HistogramMock {
	return &HistogramMock{ch: make(chan float64, 10)}
}

func (HistogramMock) Desc() *prometheus.Desc {
	panic("unimplemented")
}

func (HistogramMock) Write(*io_prometheus_client.Metric) error {
	panic("unimplemented")
}

func (HistogramMock) Describe(chan<- *prometheus.Desc) {
	panic("unimplemented")
}

func (HistogramMock) Collect(chan<- prometheus.Metric) {
	panic("implement me")
}

func (h HistogramMock) Observe(value float64) {
	h.ch <- value
}

type HistorgramVecMock struct {
	mock HistogramMock
}

func (m *HistorgramVecMock) Describe(chan<- *prometheus.Desc) {
	panic("unimplemented")
}

func (m *HistorgramVecMock) With(prometheus.Labels) prometheus.Observer {
	return m.mock
}

func (m *HistorgramVecMock) GetMetricWith(prometheus.Labels) (prometheus.Observer, error) {
	panic("unimplemented")
}

func (m *HistorgramVecMock) GetMetricWithLabelValues(_ ...string) (prometheus.Observer, error) {
	panic("unimplemented")
}

func (m *HistorgramVecMock) WithLabelValues(...string) prometheus.Observer {
	return m.mock
}

func (m *HistorgramVecMock) MustCurryWith(prometheus.Labels) prometheus.ObserverVec {
	panic("unimplemented")
}

func (m *HistorgramVecMock) CurryWith(prometheus.Labels) (prometheus.ObserverVec, error) {
	panic("unimplemented")
}

func (m *HistorgramVecMock) Collect(chan<- prometheus.Metric) {
	panic("unimplemented")
}

func (m *HistorgramVecMock) GetCh() chan float64 {
	return m.mock.ch
}

func (m *HistorgramVecMock) Cleanup() {
	close(m.mock.ch)
}

func NewHistogramVecMock() *HistorgramVecMock {
	return &HistorgramVecMock{mock: *NewHistogramMock()}
}
