package util

import (
	"time"

	"github.com/miekg/dns"
)

type DNSOps interface {
	ClientConfigFromFile(resolvconf string) (*dns.ClientConfig, error)
	Fqdn(s string) string
	Exchange(c *dns.Client, m *dns.Msg, a string) (r *dns.Msg, rtt time.Duration, err error)
	SetQuestion(msg *dns.Msg, z string, t uint16) *dns.Msg
}

type defaultDNSOps struct{}

var dnsOps DNSOps = &defaultDNSOps{}

func SetDNSLibOpsMockInst(mockInst DNSOps) {
	dnsOps = mockInst
}
func GetDNSLibOps() DNSOps {
	return dnsOps
}

func (defaultDNSOps) ClientConfigFromFile(resolveconf string) (*dns.ClientConfig, error) {
	return dns.ClientConfigFromFile(resolveconf)
}

func (defaultDNSOps) Fqdn(s string) string {
	return dns.Fqdn(s)
}

func (defaultDNSOps) Exchange(c *dns.Client, m *dns.Msg, a string) (r *dns.Msg, rtt time.Duration, err error) {
	return c.Exchange(m, a)
}

func (defaultDNSOps) SetQuestion(msg *dns.Msg, z string, t uint16) *dns.Msg {
	return msg.SetQuestion(z, t)
}
