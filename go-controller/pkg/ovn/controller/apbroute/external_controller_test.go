package apbroute

import (
	"sort"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/util/sets"
)

var _ = Describe("GatewayInfoList", func() {

	var _ = Context("Sorting", func() {

		It("NOOP when the list is empty", func() {
			s1 := gatewayInfoList{}
			sort.Sort(s1)
			Expect(s1).To(BeEquivalentTo(gatewayInfoList{}))
		})

		It("sorts the IPs when only one gatewayInfo element exists and the IPs come unsorted", func() {

			s1 := gatewayInfoList{{gws: sets.Set[string]{"2.1.1.1": sets.Empty{}, "1.11.1.1": sets.Empty{}, "1.1.1.1": sets.Empty{}, "1.1.1.2": sets.Empty{}}}}
			sort.Sort(s1)
			Expect(s1).To(BeEquivalentTo(gatewayInfoList{{gws: sets.Set[string]{"1.1.1.1": sets.Empty{}, "1.1.1.2": sets.Empty{}, "1.11.1.1": sets.Empty{}, "2.1.1.1": sets.Empty{}}}}))
		})

		It("sorts the IPs with equal results regardless of which is the first gatewayInfo element is in the slice", func() {
			s1 := gatewayInfoList{
				{gws: sets.Set[string]{"2.1.1.1": sets.Empty{}, "1.11.1.1": sets.Empty{}, "1.11.2.1": sets.Empty{}}},
				{gws: sets.Set[string]{"1.1.2.2": sets.Empty{}, "1.1.1.1": sets.Empty{}, "1.1.2.1": sets.Empty{}}}}
			s2 := gatewayInfoList{
				{gws: sets.Set[string]{"1.1.1.1": sets.Empty{}, "1.1.2.2": sets.Empty{}, "1.1.2.1": sets.Empty{}}},
				{gws: sets.Set[string]{"2.1.1.1": sets.Empty{}, "1.11.1.1": sets.Empty{}, "1.11.2.1": sets.Empty{}}}}
			sort.Sort(s1)
			sort.Sort(s2)
			Expect(s1).To(BeEquivalentTo(gatewayInfoList{
				{gws: sets.Set[string]{"1.1.1.1": sets.Empty{}, "1.1.2.1": sets.Empty{}, "1.1.2.2": sets.Empty{}}},
				{gws: sets.Set[string]{"1.11.1.1": sets.Empty{}, "1.11.2.1": sets.Empty{}, "2.1.1.1": sets.Empty{}}}}))
			Expect(s1).To(BeEquivalentTo(s2))
		})

		It("sorts the IPs when the second element is empty and the first one is unsorted", func() {
			s1 := gatewayInfoList{
				{gws: sets.Set[string]{"2.1.1.1": sets.Empty{}, "1.11.1.1": sets.Empty{}, "1.11.2.1": sets.Empty{}}},
				{gws: sets.Set[string]{}}}
			sort.Sort(s1)
			Expect(s1).To(BeEquivalentTo(gatewayInfoList{
				{gws: sets.Set[string]{}},
				{gws: sets.Set[string]{"1.11.1.1": sets.Empty{}, "1.11.2.1": sets.Empty{}, "2.1.1.1": sets.Empty{}}}}))

		})

		It("leaves the order unchanged when the slice is already sorted", func() {
			s1 := gatewayInfoList{
				{gws: sets.Set[string]{"1.11.1.1": sets.Empty{}, "1.11.2.1": sets.Empty{}, "2.1.1.1": sets.Empty{}}}}
			sort.Sort(s1)
			Expect(s1).To(BeEquivalentTo(gatewayInfoList{
				{gws: sets.Set[string]{"1.11.1.1": sets.Empty{}, "1.11.2.1": sets.Empty{}, "2.1.1.1": sets.Empty{}}}}))
		})

	})

	var _ = Context("Inserting", func() {

		It("Returns an unmodified list and the duplicated value when the inserting a slice with the same value already existing in the list", func() {
			s1 := gatewayInfoList{{gws: sets.Set[string]{"1.1.1.1": sets.Empty{}}}}
			res, dup := s1.Insert(&gatewayInfo{gws: sets.Set[string]{"1.1.1.1": sets.Empty{}}})
			Expect(res).To(Equal(s1))
			Expect(dup).To(BeEquivalentTo(sets.Set[string]{"1.1.1.1": sets.Empty{}}))
		})

		It("Adds a new element in the slice when no duplicates are found", func() {
			s1 := gatewayInfoList{{gws: sets.Set[string]{"1.1.1.1": sets.Empty{}}}}
			res, dup := s1.Insert(&gatewayInfo{gws: sets.Set[string]{"1.1.1.2": sets.Empty{}}})
			Expect(res).To(BeEquivalentTo(gatewayInfoList{{gws: sets.Set[string]{"1.1.1.1": sets.Empty{}}}, {gws: sets.Set[string]{"1.1.1.2": sets.Empty{}}}}))
			Expect(dup).To(Equal(sets.Set[string]{}))
		})

		It("Adds a new element in the slice and returns the duplicated value found during insertion", func() {
			s1 := gatewayInfoList{{gws: sets.Set[string]{"1.1.1.1": sets.Empty{}}}}
			res, dup := s1.Insert(&gatewayInfo{gws: sets.Set[string]{"1.1.1.2": sets.Empty{}, "1.1.1.1": sets.Empty{}}})
			Expect(res).To(BeEquivalentTo(gatewayInfoList{{gws: sets.Set[string]{"1.1.1.1": sets.Empty{}}}, {gws: sets.Set[string]{"1.1.1.2": sets.Empty{}}}}))
			Expect(dup).To(Equal(sets.Set[string]{"1.1.1.1": sets.Empty{}}))
		})

		It("Adds an empty element in the slice and returns no changes in the list and no duplicates", func() {
			s1 := gatewayInfoList{{gws: sets.Set[string]{"1.1.1.1": sets.Empty{}}}}
			res, dup := s1.Insert(&gatewayInfo{gws: sets.Set[string]{}})
			Expect(res).To(BeEquivalentTo(gatewayInfoList{{gws: sets.Set[string]{"1.1.1.1": sets.Empty{}}}}))
			Expect(dup).To(Equal(sets.Set[string]{}))
		})

		It("Adds a new element with multiple duplicates found in one single entry in the slice", func() {
			s1 := gatewayInfoList{{gws: sets.Set[string]{"1.1.1.1": sets.Empty{}, "1.1.1.2": sets.Empty{}}}}
			res, dup := s1.Insert(&gatewayInfo{gws: sets.Set[string]{"1.1.1.2": sets.Empty{}, "1.1.1.1": sets.Empty{}, "1.1.1.3": sets.Empty{}}})
			Expect(res).To(BeEquivalentTo(gatewayInfoList{{gws: sets.Set[string]{"1.1.1.1": sets.Empty{}, "1.1.1.2": sets.Empty{}}}, {gws: sets.Set[string]{"1.1.1.3": sets.Empty{}}}}))
			Expect(dup).To(Equal(sets.Set[string]{"1.1.1.2": sets.Empty{}, "1.1.1.1": sets.Empty{}}))
		})

		It("Adds a new element with multiple duplicates found in two different entries in the slice", func() {
			s1 := gatewayInfoList{{gws: sets.Set[string]{"1.1.1.1": sets.Empty{}}}, {gws: sets.Set[string]{"1.1.1.2": sets.Empty{}}}}
			res, dup := s1.Insert(&gatewayInfo{gws: sets.Set[string]{"1.1.1.2": sets.Empty{}, "1.1.1.1": sets.Empty{}, "1.1.1.3": sets.Empty{}}})
			Expect(res).To(BeEquivalentTo(gatewayInfoList{{gws: sets.Set[string]{"1.1.1.1": sets.Empty{}}}, {gws: sets.Set[string]{"1.1.1.2": sets.Empty{}}}, {gws: sets.Set[string]{"1.1.1.3": sets.Empty{}}}}))
			Expect(dup).To(Equal(sets.Set[string]{"1.1.1.2": sets.Empty{}, "1.1.1.1": sets.Empty{}}))
		})
	})

	var _ = Context("Deleting", func() {
		It("deletes an existing element from the slice", func() {
			s1 := gatewayInfoList{{gws: sets.Set[string]{"1.1.1.1": sets.Empty{}}}, {gws: sets.Set[string]{"1.1.1.2": sets.Empty{}}}}
			ret := s1.Delete(&gatewayInfo{gws: sets.Set[string]{"1.1.1.1": sets.Empty{}}})
			Expect(ret).To(BeEquivalentTo(gatewayInfoList{{gws: sets.Set[string]{"1.1.1.2": sets.Empty{}}}}))
		})

		It("fails to delete an element that does not have a match in the slice", func() {
			s1 := gatewayInfoList{{gws: sets.Set[string]{"1.1.1.2": sets.Empty{}}}}
			ret := s1.Delete(&gatewayInfo{gws: sets.Set[string]{"1.1.1.1": sets.Empty{}}})
			Expect(ret).To(BeEquivalentTo(gatewayInfoList{{gws: sets.Set[string]{"1.1.1.2": sets.Empty{}}}}))
		})

		It("fails to delete an element that matches only a subset of one of the elements in the slice", func() {
			s1 := gatewayInfoList{{gws: sets.Set[string]{"1.1.1.1": sets.Empty{}, "1.1.1.2": sets.Empty{}}}}
			ret := s1.Delete(&gatewayInfo{gws: sets.Set[string]{"1.1.1.1": sets.Empty{}}})
			Expect(ret).To(BeEquivalentTo(gatewayInfoList{{gws: sets.Set[string]{"1.1.1.1": sets.Empty{}, "1.1.1.2": sets.Empty{}}}}))
		})

		It("NOP when the slice is empty", func() {
			s1 := gatewayInfoList{}
			ret := s1.Delete(&gatewayInfo{gws: sets.Set[string]{"1.1.1.1": sets.Empty{}}})
			Expect(ret).To(BeEmpty())
		})
	})
})
