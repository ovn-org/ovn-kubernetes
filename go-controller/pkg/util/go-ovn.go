package util

import (
	goovn "github.com/ebay/go-ovn"
)

var OVNDBClient goovn.Client

func InitOVNDBClient() {
	OVNDBClient, err := goovn.NewClient(&goovn.Config{Addr: "good"})
}
