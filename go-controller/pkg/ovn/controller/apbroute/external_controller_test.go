package apbroute

import (
	"fmt"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/util/sets"
)

var (
	// Default comparable options to bypass comparing the mux object and define a less function to sort the slices.
	cmpOpts = []cmp.Option{
		cmpopts.IgnoreFields(syncSet{}, "mux"),
		cmpopts.SortSlices(func(x, y interface{}) bool {
			s1, ok1 := x.(*gatewayInfo)
			s2, ok2 := y.(*gatewayInfo)
			if !ok1 || !ok2 {
				return fmt.Sprint(x) < fmt.Sprint(y)
			}
			return fmt.Sprint(sets.List(s1.Gateways.items)) < fmt.Sprint(sets.List(s2.Gateways.items))
		})}
)

var _ = Describe("GatewayInfoList", func() {

	var _ = Context("Inserting", func() {

		It("Returns an unmodified list and the duplicated value when the inserting a slice with the same value already existing in the list", func() {
			s1 := gatewayInfoList{newGatewayInfo(sets.New("1.1.1.1"), false)}
			res, dup, err := s1.Insert(newGatewayInfo(sets.New("1.1.1.1"), false))
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(Equal(s1))
			Expect(dup).To(BeComparableTo(sets.New("1.1.1.1")))
		})

		It("Adds a new element in the slice when no duplicates are found", func() {
			s1 := gatewayInfoList{newGatewayInfo(sets.New("1.1.1.1"), false)}
			res, dup, err := s1.Insert(newGatewayInfo(sets.New("1.1.1.2"), false))
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(BeComparableTo(gatewayInfoList{newGatewayInfo(sets.New("1.1.1.1"), false), newGatewayInfo(sets.New("1.1.1.2"), false)}, cmpOpts...))
			Expect(dup).To(Equal(sets.Set[string]{}))
		})

		It("Adds a new element in the slice and returns the duplicated value found during insertion", func() {
			s1 := gatewayInfoList{newGatewayInfo(sets.New("1.1.1.1"), false)}
			res, dup, err := s1.Insert(newGatewayInfo(sets.New("1.1.1.1", "1.1.1.2"), false))
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(BeComparableTo(gatewayInfoList{newGatewayInfo(sets.New("1.1.1.1"), false), newGatewayInfo(sets.New("1.1.1.2"), false)}, cmpOpts...))
			Expect(dup).To(Equal(sets.New("1.1.1.1")))
		})

		It("Adds an empty element in the slice and returns no changes in the list and no duplicates", func() {
			s1 := gatewayInfoList{newGatewayInfo(sets.New("1.1.1.1"), false)}
			res, dup, err := s1.Insert(newGatewayInfo(sets.Set[string]{}, false))
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(BeComparableTo(s1, cmpOpts...))
			Expect(dup).To(Equal(sets.Set[string]{}))
		})

		It("Adds a new element with multiple duplicates found in one single entry in the slice", func() {
			s1 := gatewayInfoList{newGatewayInfo(sets.New("1.1.1.1", "1.1.1.2"), false)}
			res, dup, err := s1.Insert(newGatewayInfo(sets.New("1.1.1.1", "1.1.1.2", "1.1.1.3"), false))
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(BeComparableTo(gatewayInfoList{newGatewayInfo(sets.New("1.1.1.1", "1.1.1.2"), false), newGatewayInfo(sets.New("1.1.1.3"), false)}, cmpOpts...))
			Expect(dup).To(Equal(sets.New("1.1.1.2", "1.1.1.1")))
		})

		It("Adds a new element with multiple duplicates found in two different entries in the slice", func() {
			s1 := gatewayInfoList{newGatewayInfo(sets.New("1.1.1.1"), false), newGatewayInfo(sets.New("1.1.1.2"), false)}
			res, dup, err := s1.Insert(newGatewayInfo(sets.New("1.1.1.1", "1.1.1.2", "1.1.1.3"), false))
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(BeComparableTo(gatewayInfoList{newGatewayInfo(sets.New("1.1.1.1"), false), newGatewayInfo(sets.New("1.1.1.2"), false), newGatewayInfo(sets.New("1.1.1.3"), false)}, cmpOpts...))
			Expect(dup).To(Equal(sets.New("1.1.1.2", "1.1.1.1")))
		})

		It("Returns the same gatewayInfoList when adding a slice of gatewayInfos containing only duplicated IPs", func() {
			s1 := gatewayInfoList{newGatewayInfo(sets.New("1.1.1.1", "1.1.1.3"), false), newGatewayInfo(sets.New("1.1.1.2"), false)}
			res, dup, err := s1.Insert(
				newGatewayInfo(sets.New("1.1.1.2", "1.1.1.1"), false),
				newGatewayInfo(sets.New("1.1.1.1"), false),
				newGatewayInfo(sets.New("1.1.1.3"), false),
				newGatewayInfo(sets.New("1.1.1.1", "1.1.1.2"), false))
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(BeComparableTo(s1, cmpOpts...))
			Expect(dup).To(Equal(sets.New("1.1.1.2", "1.1.1.1", "1.1.1.3")))
		})

		It("Fails to insert when providing an IP with a different BFD state to the one already stored in the gatewayInfo", func() {
			s1 := gatewayInfoList{newGatewayInfo(sets.New("1.1.1.1"), false)}
			_, _, err := s1.Insert(newGatewayInfo(sets.New("1.1.1.1"), true))
			Expect(err).To(HaveOccurred())
			Expect(err).To(Equal(fmt.Errorf("attempting to insert duplicated IP 1.1.1.1 with different BFD states: enabled/disabled")))
		})
	})

	var _ = Context("Deleting", func() {
		It("deletes an existing element from the slice", func() {
			s1 := gatewayInfoList{newGatewayInfo(sets.New("1.1.1.1"), false), newGatewayInfo(sets.New("1.1.1.2"), false)}
			ret := s1.Delete(newGatewayInfo(sets.New("1.1.1.1"), false))
			Expect(ret).To(BeEquivalentTo(gatewayInfoList{newGatewayInfo(sets.New("1.1.1.2"), false)}))
		})

		It("fails to delete an element that does not have a match in the slice", func() {
			s1 := gatewayInfoList{newGatewayInfo(sets.New("1.1.1.2"), false)}
			ret := s1.Delete(newGatewayInfo(sets.New("1.1.1.1"), false))
			Expect(ret).To(BeEquivalentTo(s1))
		})

		It("fails to delete an element that matches only a subset of one of the elements in the slice", func() {
			s1 := gatewayInfoList{newGatewayInfo(sets.New("1.1.1.1", "1.1.1.2"), false)}
			ret := s1.Delete(newGatewayInfo(sets.New("1.1.1.1"), false))
			Expect(ret).To(BeEquivalentTo(s1))
		})

		It("NOP when the slice is empty", func() {
			s1 := gatewayInfoList{}
			ret := s1.Delete(newGatewayInfo(sets.New("1.1.1.1"), false))
			Expect(ret).To(BeEmpty())
		})
	})
})
