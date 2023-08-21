# package cidrtree
[![Go Reference](https://pkg.go.dev/badge/github.com/gaissmai/cidrtree.svg)](https://pkg.go.dev/github.com/gaissmai/cidrtree#section-documentation)
![GitHub release (latest SemVer)](https://img.shields.io/github/v/release/gaissmai/cidrtree)
[![CI](https://github.com/gaissmai/cidrtree/actions/workflows/go.yml/badge.svg)](https://github.com/gaissmai/cidrtree/actions/workflows/go.yml)
[![Coverage Status](https://coveralls.io/repos/github/gaissmai/cidrtree/badge.svg)](https://coveralls.io/github/gaissmai/cidrtree)
[![Stand With Ukraine](https://raw.githubusercontent.com/vshymanskyy/StandWithUkraine/main/badges/StandWithUkraine.svg)](https://stand-with-ukraine.pp.ua)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

## Overview

`package cidrtree` is an immutable datastructure for fast IP lookup (longest prefix match) in CIDR tables.

Immutability is achieved because insert/delete will return a new tree which will share some nodes with the original tree.
All nodes are read-only after creation, allowing concurrent readers to operate safely with concurrent writers.

This package is a specialization of the more generic [interval package] of the same author,
but explicit for CIDRs. It has a narrow focus with a smaller and simpler API.

[interval package]: https://github.com/gaissmai/interval

## API
```go
  import "github.com/gaissmai/cidrtree"

  type Tree struct{ ... }

  func New(cidrs ...netip.Prefix) Tree
  func NewConcurrent(jobs int, cidrs ...netip.Prefix) Tree

  func (t Tree) Lookup(ip netip.Addr) (cidr netip.Prefix, ok bool)

  func (t Tree) Insert(cidrs ...netip.Prefix) Tree
  func (t Tree) Delete(cidr netip.Prefix) (Tree, bool)

  func (t *Tree) InsertMutable(cidrs ...netip.Prefix)
  func (t *Tree) DeleteMutable(cidr netip.Prefix) bool

  func (t Tree) Union(other Tree, immutable bool) Tree
  func (t Tree) Clone() Tree

  func (t Tree) String() string
  func (t Tree) Fprint(w io.Writer) error
```
