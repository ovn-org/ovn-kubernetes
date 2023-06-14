package services

import (
	"net"
	"sort"
	"testing"

	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
)

func Test_NodeIPTemplates_SingleIP(t *testing.T) {
	g := gomega.NewGomegaWithT(t)

	// System Under Test
	sut := NewNodeIPsTemplates(v1.IPv4Protocol)

	sut.AddIP("ch1", net.ParseIP("11.11.0.1"))
	sut.AddIP("ch2", net.ParseIP("11.11.0.2"))

	g.Expect(sut.Len()).To(gomega.Equal(1))
	g.Expect(sut.AsTemplates()).
		To(gomega.Equal(
			[]*Template{{
				Name: "NODEIP_IPv4_0",
				Value: map[string]string{
					"ch1": "11.11.0.1",
					"ch2": "11.11.0.2",
				},
			}},
		))
}

func Test_NodeIPTemplates_DifferentIPCount(t *testing.T) {
	g := gomega.NewGomegaWithT(t)

	// System Under Test
	sut := NewNodeIPsTemplates(v1.IPv4Protocol)

	sut.AddIP("ch1", net.ParseIP("11.11.0.1"))
	sut.AddIP("ch2", net.ParseIP("11.11.0.2"))
	sut.AddIP("ch2", net.ParseIP("22.22.0.2"))
	sut.AddIP("ch3", net.ParseIP("11.11.0.3"))
	sut.AddIP("ch3", net.ParseIP("22.22.0.3"))
	sut.AddIP("ch3", net.ParseIP("33.33.0.3"))

	g.Expect(sut.Len()).To(gomega.Equal(3))

	templates := sut.AsTemplates()
	sortTemplateSliceByName(templates)
	g.Expect(templates).
		To(gomega.BeEquivalentTo(
			[]*Template{{
				Name: "NODEIP_IPv4_0",
				Value: map[string]string{
					"ch1": "11.11.0.1",
					"ch2": "11.11.0.2",
					"ch3": "11.11.0.3",
				},
			}, {
				Name: "NODEIP_IPv4_1",
				Value: map[string]string{
					"ch2": "22.22.0.2",
					"ch3": "22.22.0.3",
				},
			}, {
				Name: "NODEIP_IPv4_2",
				Value: map[string]string{
					"ch3": "33.33.0.3",
				},
			}},
		))
}

func sortTemplateSliceByName(input []*Template) {
	sort.Slice(input, func(i, j int) bool {
		return input[i].Name < input[j].Name
	})
}
