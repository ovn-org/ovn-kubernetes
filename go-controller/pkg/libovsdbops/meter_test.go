// Unit tests for meter.go.
// This may be a bit finicky in the future as both the libovsdbclient.Client and libovsdbclient.ConditionalAPI
// unfortunately have big interfaces. Any change to these interfaces will break go test and will require an adjustment
// to the mock methods here.
package libovsdbops

import (
	"context"
	"fmt"
	"testing"

	"github.com/ovn-org/libovsdb/cache"
	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
)

// MockConditionalAPI is a mock implementation of interface libovsdbclient.Client.
type MockConditionalAPI struct {
	libovsdbclient.Client
}

// Update is a mock method.
// It returns nil, nil and does nothing in most cases.
// It returns nil, err if the provided model is an *nbdb.MeterBand and if *nbdb.MeterBand.UUID == "uuid2".
// For all other MeterBands, it updates the rate to the first numerical i value that can be found.
func (c *MockConditionalAPI) Update(m model.Model, i ...interface{}) ([]ovsdb.Operation, error) {
	if meterBand, ok := m.(*nbdb.MeterBand); ok {
		// uuid2 fails
		if meterBand.UUID == "uuid2" {
			return nil, fmt.Errorf("meterband uuid2 not found")
		}
		// simply assume that the first *int value that we find shall update the rate
		for _, v := range i {
			if rate, ok := v.(*int); ok {
				meterBand.Rate = *rate
				return nil, nil
			}
		}
	}
	return nil, nil
}

// List is an empty mock method.
func (c *MockConditionalAPI) List(ctx context.Context, result interface{}) error {
	return nil
}

// Mutate is an empty mock method.
func (c *MockConditionalAPI) Mutate(model.Model, ...model.Mutation) ([]ovsdb.Operation, error) {
	return nil, nil
}

// Delete is an empty mock method.
func (c *MockConditionalAPI) Delete() ([]ovsdb.Operation, error) {
	return nil, nil
}

// Wait is an empty mock method.
func (c *MockConditionalAPI) Wait(ovsdb.WaitCondition, *int, model.Model, ...interface{}) ([]ovsdb.Operation, error) {
	return nil, nil
}

// MockLibOvsDbClient is a mock implementation of libovsdbclient.ConditionalAPI.
type MockLibOvsDbClient struct {
	libovsdbclient.ConditionalAPI
}

// Get is a mock method.
// It returns nil in most cases.
// If the provided model.Model is an *nbdb.MeterBand and if its UUID == "uuid3", return an error.
func (c *MockLibOvsDbClient) Get(ctx context.Context, m model.Model) error {
	mbp, ok := m.(*nbdb.MeterBand)
	if ok {
		if mbp.UUID == "uuid3" {
			return fmt.Errorf("not found - uuid3")
		}
	}
	return nil
}

// Connect is an empty mock method.
func (c *MockLibOvsDbClient) Connect(ctx context.Context) error {
	return nil
}

// Disconnect is an empty mock method.
func (c *MockLibOvsDbClient) Disconnect() {
	return
}

// Close is an empty mock method.
func (c *MockLibOvsDbClient) Close() {
	return
}

// Schema is an empty mock method.
func (c *MockLibOvsDbClient) Schema() ovsdb.DatabaseSchema {
	return ovsdb.DatabaseSchema{}
}

// Cache is an empty mock method.
func (c *MockLibOvsDbClient) Cache() *cache.TableCache {
	return nil
}

// SetOption is an empty mock method.
func (c *MockLibOvsDbClient) SetOption(o libovsdbclient.Option) error {
	return nil
}

// Connected is an empty mock method.
func (c *MockLibOvsDbClient) Connected() bool {
	return true
}

// Disconnect is an empty mock method.
func (c *MockLibOvsDbClient) DisconnectNotify() chan struct{} {
	return nil
}

// Echo is an empty mock method.
func (c *MockLibOvsDbClient) Echo(ctx context.Context) error {
	return nil
}

// Transact is an empty mock method.
func (c *MockLibOvsDbClient) Transact(ctx context.Context, op ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	return nil, nil
}

// Monitor is an empty mock method.
func (c *MockLibOvsDbClient) Monitor(ctx context.Context, m *libovsdbclient.Monitor) (libovsdbclient.MonitorCookie, error) {
	return libovsdbclient.MonitorCookie{}, nil
}

// MonitorAll is an empty mock method.
func (c *MockLibOvsDbClient) MonitorAll(ctx context.Context) (libovsdbclient.MonitorCookie, error) {
	return libovsdbclient.MonitorCookie{}, nil
}

// MonitorCancel is an empty mock method.
func (c *MockLibOvsDbClient) MonitorCancel(ctx context.Context, cookie libovsdbclient.MonitorCookie) error {
	return nil
}

// NewTableMonitor is an empty mock method.
func (c *MockLibOvsDbClient) NewTableMonitor(m model.Model, fields ...interface{}) libovsdbclient.TableMonitor {
	return libovsdbclient.TableMonitor{}
}

// List is an empty mock method.
func (c *MockLibOvsDbClient) List(ctx context.Context, result interface{}) error {
	return nil
}

// WhereCache is an empty mock method.
func (c *MockLibOvsDbClient) WhereCache(predicate interface{}) libovsdbclient.ConditionalAPI {
	return &MockConditionalAPI{}
}

// Where is an empty mock method.
func (c *MockLibOvsDbClient) Where(model.Model, ...model.Condition) libovsdbclient.ConditionalAPI {
	return &MockConditionalAPI{}
}

// WhereAll is an empty mock method.
func (c *MockLibOvsDbClient) WhereAll(model.Model, ...model.Condition) libovsdbclient.ConditionalAPI {
	return &MockConditionalAPI{}
}

// Create is an empty mock method.
func (c *MockLibOvsDbClient) Create(...model.Model) ([]ovsdb.Operation, error) {
	return nil, nil
}

// CurrentEndpoint is an empty mock method.
func (c *MockLibOvsDbClient) CurrentEndpoint() string {
	return ""
}

// NewMonitor is an empty mock method.
func (c *MockLibOvsDbClient) NewMonitor(...libovsdbclient.MonitorOption) *libovsdbclient.Monitor {
	return nil
}

// NewMockLibOvsDbClient returns a pointer to a new MockLibOvsDbClient.
func NewMockLibOvsDbClient() libovsdbclient.Client {
	return &MockLibOvsDbClient{}
}

// TestGetMeterBands implements the unit tests for GetMeterBands.
// Simulate 2 different meters:
// a) with Bands: []string{"uuid1", "uuid2"} -> no error and expect to the the same number
//    of MeterBands as len(bands []string).
// b) with Bands: []string{"uuid3", "uuid2"} -> error expected.
func TestGetMeterBands(t *testing.T) {
	// create a mock client
	nbClient := NewMockLibOvsDbClient()
	nbClient.Connect(context.Background())

	tcs := []struct {
		bands     []string
		expectErr bool
	}{
		{
			bands:     []string{"uuid1", "uuid2"},
			expectErr: false,
		},
		{
			bands:     []string{"uuid3", "uuid2"},
			expectErr: true,
		},
	}

	for i, tc := range tcs {
		meter := &nbdb.Meter{
			Bands: tc.bands,
		}
		receivedBands, err := GetMeterBands(nbClient, meter)
		if tc.expectErr {
			if err == nil {
				t.Errorf("Test %d: Expected error but got no error instead.", i)
			}
		} else {
			if err != nil {
				t.Errorf("Test %d: Expected no error but got error '%s' instead.", i, err.Error())
			}
			if len(receivedBands) != len(tc.bands) {
				t.Errorf("Test %d: Expected to receive %d bands, but got %d instead", i, len(tc.bands), len(receivedBands))
			}
		}
	}
}

// TestUpdateMeterBandRate tests UpdateMeterBandRate.
// If "uuid1" is to be updated, this expects no error. The rate should be updated to `rate`.
// If "uuid2" is to be updated, this expects an error.
func TestUpdateMeterBandRate(t *testing.T) {
	nbClient := NewMockLibOvsDbClient()
	nbClient.Connect(context.Background())

	tcs := []struct {
		meterBand *nbdb.MeterBand
		rate      int
		expectErr bool
	}{
		{
			meterBand: &nbdb.MeterBand{
				UUID: "uuid1",
			},
			rate:      10,
			expectErr: false,
		},
		{
			meterBand: &nbdb.MeterBand{
				UUID: "uuid2",
			},
			expectErr: true,
		},
	}

	for i, tc := range tcs {
		err := UpdateMeterBandRate(nbClient, tc.meterBand, tc.rate)
		if tc.expectErr {
			if err == nil {
				t.Errorf("Test %d: Expected error but got no error instead.", i)
			}
		} else {
			if err != nil {
				t.Errorf("Test %d: Expected no error but got error '%s' instead.", i, err.Error())
			}
			if tc.meterBand.Rate != tc.rate {
				t.Errorf("Test %d: Expected MeterBand rate to be set to %d, but got %d instead.", i, tc.rate, tc.meterBand.Rate)
			}
		}
	}
}
