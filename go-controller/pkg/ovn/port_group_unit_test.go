package ovn

import (
	"fmt"
	"testing"

	. "github.com/onsi/gomega"
	libovsdbclient "github.com/ovn-org/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	. "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
)

const (
	pgName     = "TestPortGroupName"
	pgHashName = "TestPortGroupHashName"
	pgUUUID    = "0d3b392e-15a0-46be-9b37-f9cb0dd4f9ec"
	lspUUID    = "e72eb0bb-a1ce-45d5-b8a0-3200aed7e5e9"
	lspName    = "TestLSPName"
)

func TestCreatePortGroup(t *testing.T) {
	tests := []struct {
		desc        string
		name        string
		hashName    string
		errMatch    error
		initialData []TestData
	}{
		{
			desc:     "positive test case",
			name:     pgName,
			hashName: pgHashName,
			errMatch: nil,
		}, /* TODO: there seems to be a bug with already exists in libovsdb client
		{
			desc:     "port group already exists",
			name:     pgName,
			hashName: pgHashName,
			errMatch: nil,
			initialData: []TestData{
				&nbdb.PortGroup{
					Name: pgHashName,
					ExternalIDs: map[string]string{
						"name": pgName,
					},
				},
			},
		}, */
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			g := NewGomegaWithT(t)
			stopChan := make(chan struct{})
			defer close(stopChan)

			nbClient, err := NewNBTestHarness(TestSetup{NBData: tc.initialData}, stopChan)
			if err != nil {
				t.Fatalf("Error creating northbound ovsdb test harness: %v", err)
			}

			uuid, err := createPortGroup(nbClient, tc.name, tc.hashName)

			if tc.errMatch != nil {
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(ContainSubstring(tc.errMatch.Error()))
			} else {
				g.Expect(err).ToNot(HaveOccurred())
				expected := &nbdb.PortGroup{
					UUID: uuid,
					Name: tc.hashName,
					ExternalIDs: map[string]string{
						"name": tc.name,
					},
				}

				g.Eventually(nbClient).Should(HaveTestData(expected))
			}
		})
	}
}

func TestDeletePortGroup(t *testing.T) {
	tests := []struct {
		desc        string
		hashName    string
		errMatch    error
		initialData []TestData
	}{
		{
			desc:     "positive test case",
			hashName: pgHashName,
			errMatch: nil,
			initialData: []TestData{
				&nbdb.PortGroup{
					Name: pgHashName,
					ExternalIDs: map[string]string{
						"name": pgName,
					},
				},
			},
		},
		{
			desc:        "port group not found",
			hashName:    pgHashName,
			errMatch:    nil,
			initialData: nil,
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			g := NewGomegaWithT(t)
			stopChan := make(chan struct{})
			defer close(stopChan)

			nbClient, err := NewNBTestHarness(TestSetup{NBData: tc.initialData}, stopChan)
			if err != nil {
				t.Fatalf("Error creating northbound ovsdb test harness: %v", err)
			}

			err = deletePortGroup(nbClient, tc.hashName)

			if tc.errMatch != nil {
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(ContainSubstring(tc.errMatch.Error()))
			} else {
				g.Expect(err).ToNot(HaveOccurred())
				g.Eventually(nbClient).Should(HaveEmptyTestData())
			}
		})
	}
}

func TestAddToPortGroup(t *testing.T) {
	tests := []struct {
		desc         string
		name         string
		portInfo     *lpInfo
		errMatch     error
		initialData  []TestData
		expectedData []TestData
	}{
		{
			desc:     "positive test case",
			name:     pgHashName,
			portInfo: &lpInfo{name: lspName, uuid: lspUUID},
			errMatch: nil,
			initialData: []TestData{
				&nbdb.PortGroup{
					Name: pgHashName,
				},
			},
			expectedData: []TestData{
				&nbdb.PortGroup{
					Name: pgHashName,
					Ports: []string{
						lspUUID,
					},
				},
			},
		},
		{
			desc:     "positive idempotency",
			name:     pgHashName,
			portInfo: &lpInfo{name: lspName, uuid: lspUUID},
			errMatch: nil,
			initialData: []TestData{
				&nbdb.PortGroup{
					Name: pgHashName,
					Ports: []string{
						lspUUID,
					},
				},
			},
			expectedData: []TestData{
				&nbdb.PortGroup{
					Name: pgHashName,
					Ports: []string{
						lspUUID,
					},
				},
			},
		},
		{
			desc:         "port group not found",
			name:         pgHashName,
			portInfo:     &lpInfo{name: lspName, uuid: lspUUID},
			errMatch:     fmt.Errorf("Error getting port group %s for mutate: %s", pgHashName, libovsdbclient.ErrNotFound.Error()),
			initialData:  nil,
			expectedData: nil,
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			g := NewGomegaWithT(t)
			stopChan := make(chan struct{})
			defer close(stopChan)

			nbClient, err := NewNBTestHarness(TestSetup{NBData: tc.initialData}, stopChan)
			if err != nil {
				t.Fatalf("Error creating northbound ovsdb test harness: %v", err)
			}

			err = addToPortGroup(nbClient, tc.name, tc.portInfo)

			if tc.errMatch != nil {
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(ContainSubstring(tc.errMatch.Error()))
			} else {
				g.Expect(err).ToNot(HaveOccurred())
				g.Eventually(nbClient).Should(HaveTestDataIgnoringUUIDs(tc.expectedData...))
			}
		})
	}
}

func TestDeleteFromPortGroup(t *testing.T) {
	tests := []struct {
		desc         string
		name         string
		portInfo     *lpInfo
		errMatch     error
		initialData  []TestData
		expectedData []TestData
	}{
		{
			desc:     "positive test case",
			name:     pgHashName,
			portInfo: &lpInfo{name: lspName, uuid: lspUUID},
			errMatch: nil,
			initialData: []TestData{
				&nbdb.PortGroup{
					Name: pgHashName,
					Ports: []string{
						lspUUID,
					},
				},
			},
			expectedData: []TestData{
				&nbdb.PortGroup{
					Name: pgHashName,
				},
			},
		},
		{
			desc:         "port group not found",
			name:         pgHashName,
			portInfo:     &lpInfo{name: lspName, uuid: lspUUID},
			errMatch:     fmt.Errorf("Error getting port group %s for mutate: %s", pgHashName, libovsdbclient.ErrNotFound.Error()),
			initialData:  nil,
			expectedData: nil,
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			g := NewGomegaWithT(t)
			stopChan := make(chan struct{})
			defer close(stopChan)

			nbClient, err := NewNBTestHarness(TestSetup{NBData: tc.initialData}, stopChan)
			if err != nil {
				t.Fatalf("Error creating northbound ovsdb test harness: %v", err)
			}

			err = deleteFromPortGroup(nbClient, tc.name, tc.portInfo)

			if tc.errMatch != nil {
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(ContainSubstring(tc.errMatch.Error()))
			} else {
				g.Expect(err).ToNot(HaveOccurred())
				g.Eventually(nbClient).Should(HaveTestDataIgnoringUUIDs(tc.expectedData...))
			}
		})
	}
}
