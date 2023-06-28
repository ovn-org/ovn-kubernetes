package apbroute

import (
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/util/sets"
)

var _ = Describe("GatewayInfoList", func() {

	var _ = Context("Inserting", func() {

		It("InsertOverwrite adds a new element in the slice when no duplicates are found", func() {
			s1 := newGatewayInfoList(newGatewayInfo(sets.New("1.1.1.1"), false))
			s1.InsertOverwrite(newGatewayInfo(sets.New("1.1.1.2"), false))
			Expect(s1.Equal(newGatewayInfoList(
				newGatewayInfo(sets.New("1.1.1.1"), false),
				newGatewayInfo(sets.New("1.1.1.2"), false)))).To(BeTrue())
		})

		It("InsertOverwrite adds a new element in the slice when the duplicated value found during insertion", func() {
			s1 := newGatewayInfoList(newGatewayInfo(sets.New("1.1.1.1"), false))
			s1.InsertOverwrite(newGatewayInfo(sets.New("1.1.1.1"), true))
			Expect(s1.Equal(newGatewayInfoList(
				newGatewayInfo(sets.New("1.1.1.1"), true)))).To(BeTrue())
		})

		It("InsertOverwrite adds an empty element in the slice and returns no changes in the list and no duplicates", func() {
			s1 := newGatewayInfoList(newGatewayInfo(sets.New("1.1.1.1"), false))
			s1.InsertOverwrite(newGatewayInfo(sets.Set[string]{}, false))
			Expect(s1.Equal(newGatewayInfoList(
				newGatewayInfo(sets.New("1.1.1.1"), false)))).To(BeTrue())
		})

		It("InsertOverwrite adds a new element with multiple duplicates found in one single entry in the slice", func() {
			s1 := newGatewayInfoList(newGatewayInfo(sets.New("1.1.1.1", "1.1.1.2"), false))
			s1.InsertOverwrite(newGatewayInfo(sets.New("1.1.1.1", "1.1.1.2", "1.1.1.3"), false))
			Expect(s1.Equal(newGatewayInfoList(
				newGatewayInfo(sets.New("1.1.1.1", "1.1.1.2", "1.1.1.3"), false)))).To(BeTrue())
		})

		It("InsertOverwrite adds a new element with multiple duplicates found in two different entries in the slice", func() {
			s1 := newGatewayInfoList(
				newGatewayInfo(sets.New("1.1.1.1"), false),
				newGatewayInfo(sets.New("1.1.1.2"), false))
			s1.InsertOverwrite(newGatewayInfo(sets.New("1.1.1.1", "1.1.1.2", "1.1.1.3"), false))
			Expect(s1.Equal(newGatewayInfoList(
				newGatewayInfo(sets.New("1.1.1.1", "1.1.1.2", "1.1.1.3"), false)))).To(BeTrue())
		})

		It("InsertOverwrite returns the same gatewayInfoList when adding a slice of gatewayInfos containing only duplicated IPs", func() {
			s1 := newGatewayInfoList(
				newGatewayInfo(sets.New("1.1.1.1", "1.1.1.3"), false),
				newGatewayInfo(sets.New("1.1.1.2"), false))
			s1.InsertOverwrite(
				newGatewayInfo(sets.New("1.1.1.2", "1.1.1.1"), false),
				newGatewayInfo(sets.New("1.1.1.1"), false),
				newGatewayInfo(sets.New("1.1.1.3"), false),
				newGatewayInfo(sets.New("1.1.1.1", "1.1.1.2"), false))
			Expect(s1.Equal(newGatewayInfoList(
				newGatewayInfo(sets.New("1.1.1.3"), false),
				newGatewayInfo(sets.New("1.1.1.1", "1.1.1.2"), false)))).To(BeTrue())
		})

		It("InsertOverwriteFailed updates status of the existing element", func() {
			s1 := newGatewayInfoList(newGatewayInfo(sets.New("1.1.1.1"), false))
			s1.InsertOverwriteFailed(newGatewayInfo(sets.New("1.1.1.1"), false))
			failedGwInfo := newGatewayInfo(sets.New("1.1.1.1"), false)
			failedGwInfo.applied = false
			Expect(s1.Equal(newGatewayInfoList(failedGwInfo))).To(BeTrue())
		})

	})

	var _ = Context("Deleting", func() {
		It("deletes an existing element from the slice", func() {
			s1 := newGatewayInfoList(
				newGatewayInfo(sets.New("1.1.1.1"), false),
				newGatewayInfo(sets.New("1.1.1.2"), false))
			s1.Delete(newGatewayInfo(sets.New("1.1.1.1"), false))
			Expect(s1.Equal(newGatewayInfoList(
				newGatewayInfo(sets.New("1.1.1.2"), false)))).To(BeTrue())
		})

		It("fails to delete an element that does not have a match in the slice", func() {
			s1 := newGatewayInfoList(newGatewayInfo(sets.New("1.1.1.2"), false))
			s1.Delete(newGatewayInfo(sets.New("1.1.1.1"), false))
			Expect(s1.Equal(newGatewayInfoList(
				newGatewayInfo(sets.New("1.1.1.2"), false)))).To(BeTrue())
		})

		It("fails to delete an element that matches only a subset of one of the elements in the slice", func() {
			s1 := newGatewayInfoList(newGatewayInfo(sets.New("1.1.1.1", "1.1.1.2"), false))
			s1.Delete(newGatewayInfo(sets.New("1.1.1.1"), false))
			Expect(s1.Equal(newGatewayInfoList(
				newGatewayInfo(sets.New("1.1.1.1", "1.1.1.2"), false)))).To(BeTrue())
		})

		It("NOP when the slice is empty", func() {
			s1 := newGatewayInfoList()
			s1.Delete(newGatewayInfo(sets.New("1.1.1.1"), false))
			Expect(s1.Equal(newGatewayInfoList())).To(BeTrue())
		})
	})
})
