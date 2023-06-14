//go:build linux
// +build linux

package util

import (
	"fmt"
	"strings"
	"sync"

	"github.com/coreos/go-iptables/iptables"
)

// IPTablesHelper is an interface that wraps go-iptables to allow
// mock implementations for unit testing
type IPTablesHelper interface {
	// List rules in specified table/chain
	List(table, chain string) ([]string, error)
	// ListChains returns the names of all chains in the table
	ListChains(string) ([]string, error)
	// ClearChain removes all rules in the specified table/chain.
	// If the chain does not exist, a new one will be created
	ClearChain(string, string) error
	// DeleteChain deletes the chain in the specified table.
	DeleteChain(string, string) error
	// NewChain creates a new chain in the specified table.
	// If the chain already exists, it will result in an error.
	NewChain(string, string) error
	// Exists checks if given rulespec in specified table/chain exists
	Exists(string, string, ...string) (bool, error)
	// Insert inserts a rule into the specified table/chain
	Insert(string, string, int, ...string) error
	// Append appends rulespec to specified table/chain
	Append(string, string, ...string) error
	// Delete removes rulespec in specified table/chain
	Delete(string, string, ...string) error
}

var helpers = make(map[iptables.Protocol]IPTablesHelper)

// SetIPTablesHelper sets the IPTablesHelper to be used
func SetIPTablesHelper(proto iptables.Protocol, ipt IPTablesHelper) {
	helpers[proto] = ipt
}

// GetIPTablesHelper returns an IPTablesHelper. If SetIPTablesHelper has not yet been
// called, it will create a new IPTablesHelper wrapping "live" go-iptables
func GetIPTablesHelper(proto iptables.Protocol) (IPTablesHelper, error) {
	if helpers[proto] == nil {
		ipt, err := iptables.NewWithProtocol(proto)
		if err != nil {
			return nil, fmt.Errorf("failed to create IPTablesHelper for proto %v: %v",
				proto, err)
		}
		SetIPTablesHelper(proto, ipt)
	}
	return helpers[proto], nil
}

// FakeTable represents a mock iptables table and can be used for
// unit tests to verify that the code creates the expected rules
type FakeTable map[string][]string

func newFakeTable() *FakeTable {
	return &FakeTable{}
}

func (t *FakeTable) String() string {
	return fmt.Sprintf("%v", *t)
}

func (t *FakeTable) getChain(chainName string) ([]string, error) {
	chain, ok := (*t)[chainName]
	if !ok {
		return nil, fmt.Errorf("chain %s does not exist", chainName)
	}
	return chain, nil
}

// FakeIPTables is a mock implementation of go-iptables
type FakeIPTables struct {
	proto  iptables.Protocol
	tables map[string]*FakeTable
	sync.Mutex
}

// SetFakeIPTablesHelpers populates `helpers` with FakeIPTablesHelper that can be used in unit tests
func SetFakeIPTablesHelpers() (IPTablesHelper, IPTablesHelper) {
	iptV4 := newFakeWithProtocol(iptables.ProtocolIPv4)
	SetIPTablesHelper(iptables.ProtocolIPv4, iptV4)
	iptV6 := newFakeWithProtocol(iptables.ProtocolIPv6)
	SetIPTablesHelper(iptables.ProtocolIPv6, iptV6)
	return iptV4, iptV6
}

func newFakeWithProtocol(protocol iptables.Protocol) *FakeIPTables {
	ipt := &FakeIPTables{
		proto:  protocol,
		tables: make(map[string]*FakeTable),
	}
	// Prepopulate some common tables
	ipt.tables["nat"] = newFakeTable()
	ipt.tables["filter"] = newFakeTable()
	ipt.tables["mangle"] = newFakeTable()
	return ipt
}

func (f *FakeIPTables) getTable(tableName string) (*FakeTable, error) {
	table, ok := f.tables[tableName]
	if !ok {
		return nil, fmt.Errorf("table %s does not exist", tableName)
	}
	return table, nil
}

func (f *FakeIPTables) newChain(tableName, chainName string) error {
	table, err := f.getTable(tableName)
	if err != nil {
		return err
	}
	if _, err := table.getChain(chainName); err == nil {
		// existing chain returns an error
		return err
	}
	(*table)[chainName] = nil
	return nil
}

// List rules in specified table/chain
func (f *FakeIPTables) List(tableName, chainName string) ([]string, error) {
	f.Lock()
	defer f.Unlock()
	table, err := f.getTable(tableName)
	if err != nil {
		return nil, err
	}
	chain, err := table.getChain(chainName)
	if err != nil {
		return nil, err
	}
	return chain, nil
}

// ListChains returns the names of all chains in the table
func (f *FakeIPTables) ListChains(tableName string) ([]string, error) {
	f.Lock()
	defer f.Unlock()
	table, ok := f.tables[tableName]
	if !ok {
		return nil, fmt.Errorf("table does not exist")
	}
	chains := make([]string, len(*table))
	for c := range *table {
		chains = append(chains, c)
	}
	return chains, nil
}

// NewChain creates a new chain in the specified table
func (f *FakeIPTables) NewChain(tableName, chainName string) error {
	f.Lock()
	defer f.Unlock()
	return f.newChain(tableName, chainName)
}

// ClearChain removes all rules in the specified table/chain.
// If the chain does not exist, a new one will be created
func (f *FakeIPTables) ClearChain(tableName, chainName string) error {
	f.Lock()
	defer f.Unlock()
	table, err := f.getTable(tableName)
	if err != nil {
		return err
	}
	if _, err := table.getChain(chainName); err == nil {
		// chain exists, flush the rules
		(*table)[chainName] = nil
		return nil
	}
	return f.newChain(tableName, chainName)
}

// DeleteChain deletes the chain in the specified table.
// The chain must be empty
func (f *FakeIPTables) DeleteChain(tableName, chainName string) error {
	f.Lock()
	defer f.Unlock()
	table, err := f.getTable(tableName)
	if err != nil {
		return err
	}
	if chain, err := table.getChain(chainName); err == nil {
		if len(chain) != 0 {
			return fmt.Errorf("chain must be empty")
		}
		delete((*table), chainName)
		return nil
	} else {
		return err
	}
}

// Exists checks if given rulespec in specified table/chain exists
func (f *FakeIPTables) Exists(tableName, chainName string, rulespec ...string) (bool, error) {
	f.Lock()
	defer f.Unlock()
	table, err := f.getTable(tableName)
	if err != nil {
		return false, err
	}
	chain, err := table.getChain(chainName)
	if err != nil {
		return false, err
	}
	matchRule := strings.Join(rulespec, " ")
	for _, rule := range chain {
		if rule == matchRule {
			return true, nil
		}
	}
	return false, nil
}

// Insert inserts a rule into the specified table/chain
func (f *FakeIPTables) Insert(tableName, chainName string, pos int, rulespec ...string) error {
	f.Lock()
	defer f.Unlock()
	table, err := f.getTable(tableName)
	if err != nil {
		return err
	}
	if pos < 1 {
		return fmt.Errorf("invalid rule position %d", pos)
	}
	rule := strings.Join(rulespec, " ")
	chain, _ := table.getChain(chainName)
	if pos > len(chain) {
		(*table)[chainName] = append(chain, rule)
	} else {
		last := append([]string{rule}, chain[pos-1:]...)
		(*table)[chainName] = append(chain[:pos-1], last...)
	}
	return nil
}

// Append appends rulespec to specified table/chain
func (f *FakeIPTables) Append(tableName, chainName string, rulespec ...string) error {
	f.Lock()
	defer f.Unlock()
	table, err := f.getTable(tableName)
	if err != nil {
		return err
	}
	rule := strings.Join(rulespec, " ")
	chain, err := table.getChain(chainName)
	if err != nil {
		return err
	}
	(*table)[chainName] = append(chain, rule)
	return nil
}

// Delete removes a rule from the specified table/chain
func (f *FakeIPTables) Delete(tableName, chainName string, rulespec ...string) error {
	f.Lock()
	defer f.Unlock()
	table, err := f.getTable(tableName)
	if err != nil {
		return err
	}
	chain, err := table.getChain(chainName)
	if err != nil {
		return err
	}
	rule := strings.Join(rulespec, " ")
	for i, r := range chain {
		if r == rule {
			(*table)[chainName] = append(chain[:i], chain[i+1:]...)
			break
		}
	}
	return nil
}

// MatchState matches the expected state against the actual rules
// code under test added to iptables
func (f *FakeIPTables) MatchState(tables map[string]FakeTable) error {
	f.Lock()
	defer f.Unlock()
	if len(tables) != len(f.tables) {
		return fmt.Errorf("expected %d tables, got %d", len(tables), len(f.tables))
	}
	for tableName, table := range tables {
		foundTable, err := f.getTable(tableName)
		if err != nil {
			return err
		}
		if len(table) != len(*foundTable) {
			var keys, foundKeys []string
			for k := range table {
				keys = append(keys, k)
			}
			for k := range *foundTable {
				foundKeys = append(foundKeys, k)
			}
			return fmt.Errorf("expected %v chains from table %s, got %v", keys, tableName, foundKeys)
		}
		for chainName, chain := range table {
			foundChain, err := foundTable.getChain(chainName)
			if err != nil {
				return err
			}
			if len(chain) != len(foundChain) {
				return fmt.Errorf("expected %d %v rules in chain %s/%s, got %d %v", len(chain), chain, tableName, chainName, len(foundChain), foundChain)
			}
			for i, rule := range chain {
				if rule != foundChain[i] {
					return fmt.Errorf("expected rule %q at pos %d in chain %s/%s, got %q", rule, i, tableName, chainName, foundChain[i])
				}
			}
		}
	}
	return nil
}
