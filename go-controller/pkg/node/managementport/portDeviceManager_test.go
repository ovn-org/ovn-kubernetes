// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package managementport

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/stretchr/testify/mock"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/deviceresource"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	kubeMocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube/mocks"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilMocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const testNodeName = "test-node"

// envVarForResource returns the device-plugin environment variable
// name that corresponds to the given resource name.
func envVarForResource(resourceName string) string {
	s := strings.ReplaceAll(resourceName, ".", "_")
	s = strings.ReplaceAll(s, "/", "_")
	return "PCIDEVICE_" + strings.ToUpper(s)
}

// setupInitTestEnv creates a DeviceResourceAllocator backed by the
// given PCI IDs list and a NodeWatchFactory whose fake client
// contains a single node with the provided management-port annotation.
// If annotation is nil the node is created without the annotation.
func setupInitTestEnv(
	resourceName string, availableDevices []string,
	annotation util.NetworkDeviceDetailsMap,
) (*deviceresource.DeviceResourceAllocator, factory.NodeWatchFactory) {
	envVarName := envVarForResource(resourceName)
	os.Setenv(envVarName, strings.Join(availableDevices, ","))
	DeferCleanup(os.Unsetenv, envVarName)

	allocator, err := deviceresource.DeviceResourceManager().GetDeviceResourceAllocator(resourceName)
	Expect(err).NotTo(HaveOccurred())

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        testNodeName,
			Annotations: map[string]string{},
		},
	}
	if annotation != nil {
		annotationBytes, err := json.Marshal(annotation)
		Expect(err).NotTo(HaveOccurred())
		node.Annotations[util.OvnNodeManagementPort] = string(annotationBytes)
	}

	fakeClient := fake.NewSimpleClientset(
		&corev1.NodeList{Items: []corev1.Node{*node}})
	fakeNodeClient := &util.OVNNodeClientset{KubeClient: fakeClient}
	wf, err := factory.NewNodeWatchFactory(fakeNodeClient, testNodeName)
	Expect(err).NotTo(HaveOccurred())
	Expect(wf.Start()).To(Succeed())

	return allocator, wf
}

// mockDeviceDetails sets up SriovnetOps mock expectations so that
// util.GetNetworkDeviceDetails(deviceId) returns the given pfId and
// funcId.
func mockDeviceDetails(
	sriovMock *utilMocks.SriovnetOps,
	deviceId string, pfId, funcId int,
) {
	sriovMock.On("GetVfIndexByPciAddress", deviceId).Return(funcId, nil)
	sriovMock.On("GetPfIndexByVfPciAddress", deviceId).Return(pfId, nil)
}

var _ = Describe("MgmtPortDeviceManager tests", func() {
	var (
		sriovnetOpsMock *utilMocks.SriovnetOps
		origSriovnetOps util.SriovnetOps
		kubeMock        *kubeMocks.Interface
	)
	BeforeEach(func() {
		origSriovnetOps = util.GetSriovnetOps()
		Expect(config.PrepareTestConfig()).To(Succeed())
		sriovnetOpsMock = &utilMocks.SriovnetOps{}
		util.SetSriovnetOpsInst(sriovnetOpsMock)
		kubeMock = &kubeMocks.Interface{}
	})

	AfterEach(func() {
		util.SetSriovnetOpsInst(origSriovnetOps)
		sriovnetOpsMock.AssertExpectations(GinkgoT())
		kubeMock.AssertExpectations(GinkgoT())
	})

	Context("Init", func() {
		It("Succeeds with no management port annotation", func() {
			allocator, wf := setupInitTestEnv(
				"example.com/pool_no_annotation",
				[]string{"0000:05:00.0"},
				nil,
			)
			DeferCleanup(wf.Shutdown)
			mpdm := NewMgmtPortDeviceManager(kubeMock, wf, testNodeName, allocator)
			Expect(mpdm.Init()).NotTo(HaveOccurred())
			Expect(mpdm.mgmtPortDetails).To(BeEmpty())
		})
		It("Restores valid DeviceId with matching PfId/FuncId", func() {
			const (
				device0 = "0000:03:00.0"
				device1 = "0000:03:00.1"
			)
			allocator, wf := setupInitTestEnv(
				"example.com/pool_valid_restore",
				[]string{device0, device1},
				util.NetworkDeviceDetailsMap{
					"default": {DeviceId: device0, PfId: 0, FuncId: 4},
				},
			)
			DeferCleanup(wf.Shutdown)
			mockDeviceDetails(sriovnetOpsMock, device0, 0, 4)
			mpdm := NewMgmtPortDeviceManager(kubeMock, wf, testNodeName, allocator)
			Expect(mpdm.Init()).NotTo(HaveOccurred())
			Expect(mpdm.mgmtPortDetails["default"].DeviceId).To(Equal(device0))
		})
		It("Restores legacy annotation without DeviceId by PfId/FuncId match", func() {
			const (
				device0 = "0000:04:00.0"
				device1 = "0000:04:00.1"
			)
			allocator, wf := setupInitTestEnv(
				"example.com/pool_legacy_restore",
				[]string{device0, device1},
				util.NetworkDeviceDetailsMap{"default": {PfId: 0, FuncId: 4}},
			)
			DeferCleanup(wf.Shutdown)
			mockDeviceDetails(sriovnetOpsMock, device0, 0, 4)

			kubeMock.On("SetAnnotationsOnNode", mock.Anything, testNodeName,
				mock.Anything).Return(nil).Once()

			mpdm := NewMgmtPortDeviceManager(kubeMock, wf, testNodeName, allocator)
			Expect(mpdm.Init()).NotTo(HaveOccurred())
			Expect(mpdm.mgmtPortDetails["default"].DeviceId).
				To(Equal(device0))
		})
		It("Recovers by PfId/FuncId when annotated DeviceId is stale", func() {
			const (
				staleDevice = "0000:01:01.0"
				matchDevice = "0000:01:00.0"
				otherDevice = "0000:01:00.1"
			)
			allocator, wf := setupInitTestEnv(
				"example.com/pool_stale_recovery",
				[]string{matchDevice, otherDevice},
				util.NetworkDeviceDetailsMap{
					"default": {DeviceId: staleDevice, PfId: 3, FuncId: 5},
				},
			)
			DeferCleanup(wf.Shutdown)
			mockDeviceDetails(sriovnetOpsMock, matchDevice, 3, 5)
			kubeMock.On("SetAnnotationsOnNode", mock.Anything, testNodeName,
				mock.Anything).Return(nil).Once()

			mpdm := NewMgmtPortDeviceManager(kubeMock, wf, testNodeName, allocator)
			Expect(mpdm.Init()).NotTo(HaveOccurred())
			Expect(mpdm.mgmtPortDetails["default"].DeviceId).
				To(Equal(matchDevice))
		})
		It("Fails when no PfId/FuncId match after ignoring stale DeviceId", func() {
			const (
				staleDevice = "0000:02:01.0"
				device1     = "0000:02:00.0"
				device2     = "0000:02:00.1"
			)
			allocator, wf := setupInitTestEnv(
				"example.com/pool_stale_no_match",
				[]string{device1, device2},
				util.NetworkDeviceDetailsMap{
					"default": {DeviceId: staleDevice, PfId: 3, FuncId: 5},
				},
			)
			DeferCleanup(wf.Shutdown)
			// Neither device matches PfId=3, FuncId=5
			mockDeviceDetails(sriovnetOpsMock, device1, 1, 1)
			mockDeviceDetails(sriovnetOpsMock, device2, 2, 2)

			mpdm := NewMgmtPortDeviceManager(kubeMock, wf, testNodeName, allocator)
			err := mpdm.Init()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to find match manage port device"))
		})
	})
})
