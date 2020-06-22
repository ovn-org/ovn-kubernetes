GO OVN [![License](https://img.shields.io/:license-apache-blue.svg)](https://opensource.org/licenses/Apache-2.0) [![GoDoc](https://godoc.org/github.com/ebay/go-ovn?status.svg)](https://godoc.org/github.com/ebay/go-ovn) [![Travis CI](https://api.travis-ci.org/ebay/go-ovn.svg?branch=master)](https://travis-ci.org/ebay/go-ovn) [![Go Report Card](https://goreportcard.com/badge/ebay/go-ovn)](https://goreportcard.com/report/github.com/ebay/go-ovn)
========

A Go library for OVN DB access using native OVSDB protocol.
It is based on the [OVSDB Library](https://github.com/socketplane/libovsdb.git), but used own fork
https://github.com/ebay/libovsdb.git with patches.

## What is OVN?

OVN (Open Virtual Network) is a SDN solution built on top of OVS (Open vSwitch).
The interface of OVN is its northbound DB which is an OVSDB database.

## What is OVSDB?

OVSDB is a protocol for managing the configuration of OVS.
It's defined in [RFC 7047](http://tools.ietf.org/html/rfc7047).

## Why native OVSDB protocol?

There are projects accessing OVN DB based on the ovn-nbctl/sbctl CLI, which has some
problems. Here are the majors ones and how native OVSDB protocol based approach
solves them:

- Performance problem. Every CLI command would trigger a separate OVSDB connection setup/teardown,
  initial OVSDB client cache population, etc., which would impact performance significantly. This
  library uses OVSDB protocol directly so that the overhead happens only once for all OVSDB operations.

- Caching problem. When there is a change in desired state, which requires updates in OVN, we need
  to figure out first what's the current state in OVN, which requires either maintaining a client
  cache or executing a "list" command everytime. This library maintains an internal cache and ensures
  it is always up to date with the remote DB with the help of native OVSDB support.

- String parsing problem. CLI based implementation needs extra conversion from the string output
  to Go internal data types, while it is not necessary with this library since OVSDB JSON RPC takes
  care of it.

