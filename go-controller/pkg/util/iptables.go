// +build linux

package util

import (
	"fmt"
	"strings"

	"github.com/coreos/go-iptables/iptables"
)

// IPTablesHelper is an interface that wraps go-iptables to allow
// mock implementations for unti testing
type IPTablesHelper interface {
	// ListChains returns the names of all chains in the table
	ListChains(string) ([]string, error)
	// NewChain creates a new chain in the specified table
	NewChain(string, string) error
	// Exists checks if given rulespec in specified table/chain exists
	Exists(string, string, ...string) (bool, error)
	// Insert inserts a rule into the specified table/chain
	Insert(string, string, int, ...string) error
}

// NewWithProtocol creates a new IPTablesHelper wrapping "live" go-iptables
func NewWithProtocol(proto iptables.Protocol) (IPTablesHelper, error) {
	return iptables.NewWithProtocol(proto)
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
		return nil, fmt.Errorf("table %s does not exist", chainName)
	}
	return chain, nil
}

// FakeIPTables is a mock implementation of go-iptables
type FakeIPTables struct {
	proto  iptables.Protocol
	tables map[string]*FakeTable
}

// NewFakeWithProtocol creates a new IPTablesHelper wrapping a mock
// iptables implementation that can be used in unit tests
func NewFakeWithProtocol(proto iptables.Protocol) (*FakeIPTables, error) {
	ipt := &FakeIPTables{
		proto:  proto,
		tables: make(map[string]*FakeTable),
	}
	// Prepopulate some common tables
	ipt.tables["filter"] = newFakeTable()
	ipt.tables["nat"] = newFakeTable()
	return ipt, nil
}

func (f *FakeIPTables) getTable(tableName string) (*FakeTable, error) {
	table, ok := f.tables[tableName]
	if !ok {
		return nil, fmt.Errorf("table %s does not exist", tableName)
	}
	return table, nil
}

// ListChains returns the names of all chains in the table
func (f *FakeIPTables) ListChains(tableName string) ([]string, error) {
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

// Exists checks if given rulespec in specified table/chain exists
func (f *FakeIPTables) Exists(tableName, chainName string, rulespec ...string) (bool, error) {
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
	table, err := f.getTable(tableName)
	if err != nil {
		return err
	}
	if pos < 1 {
		return fmt.Errorf("invalid rule position %d", pos)
	}
	rule := strings.Join(rulespec, " ")
	chain, _ := table.getChain(chainName)
	if pos >= len(chain) {
		(*table)[chainName] = append(chain, rule)
	} else {
		first := append(chain[:pos-1], rule)
		(*table)[chainName] = append(first, chain[pos-1:]...)
	}
	return nil
}

// MatchState matches the expected state against the actual rules
// code under test added to iptables
func (f *FakeIPTables) MatchState(tables map[string]FakeTable) error {
	if len(tables) != len(f.tables) {
		return fmt.Errorf("expeted %d tables, got %d", len(tables), len(f.tables))
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
