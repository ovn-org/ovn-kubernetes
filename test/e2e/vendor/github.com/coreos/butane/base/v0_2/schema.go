// Copyright 2019 Red Hat, Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.)

package v0_2

type Config struct {
	Version  string   `yaml:"version"`
	Variant  string   `yaml:"variant"`
	Ignition Ignition `yaml:"ignition"`
	Passwd   Passwd   `yaml:"passwd"`
	Storage  Storage  `yaml:"storage"`
	Systemd  Systemd  `yaml:"systemd"`
}

type Device string

type Directory struct {
	Group     NodeGroup `yaml:"group"`
	Overwrite *bool     `yaml:"overwrite"`
	Path      string    `yaml:"path"`
	User      NodeUser  `yaml:"user"`
	Mode      *int      `yaml:"mode"`
}

type Disk struct {
	Device     string      `yaml:"device"`
	Partitions []Partition `yaml:"partitions"`
	WipeTable  *bool       `yaml:"wipe_table"`
}

type Dropin struct {
	Contents *string `yaml:"contents"`
	Name     string  `yaml:"name"`
}

type File struct {
	Group     NodeGroup  `yaml:"group"`
	Overwrite *bool      `yaml:"overwrite"`
	Path      string     `yaml:"path"`
	User      NodeUser   `yaml:"user"`
	Append    []Resource `yaml:"append"`
	Contents  Resource   `yaml:"contents"`
	Mode      *int       `yaml:"mode"`
}

type Filesystem struct {
	Device         string   `yaml:"device"`
	Format         *string  `yaml:"format"`
	Label          *string  `yaml:"label"`
	MountOptions   []string `yaml:"mount_options"`
	Options        []string `yaml:"options"`
	Path           *string  `yaml:"path"`
	UUID           *string  `yaml:"uuid"`
	WipeFilesystem *bool    `yaml:"wipe_filesystem"`
	WithMountUnit  *bool    `yaml:"with_mount_unit" butane:"auto_skip"` // Added, not in Ignition spec
}

type FilesystemOption string

type Group string

type HTTPHeader struct {
	Name  string  `yaml:"name"`
	Value *string `yaml:"value"`
}

type HTTPHeaders []HTTPHeader

type Ignition struct {
	Config   IgnitionConfig `yaml:"config"`
	Proxy    Proxy          `yaml:"proxy"`
	Security Security       `yaml:"security"`
	Timeouts Timeouts       `yaml:"timeouts"`
}

type IgnitionConfig struct {
	Merge   []Resource `yaml:"merge"`
	Replace Resource   `yaml:"replace"`
}

type Link struct {
	Group     NodeGroup `yaml:"group"`
	Overwrite *bool     `yaml:"overwrite"`
	Path      string    `yaml:"path"`
	User      NodeUser  `yaml:"user"`
	Hard      *bool     `yaml:"hard"`
	Target    string    `yaml:"target"`
}

type NodeGroup struct {
	ID   *int    `yaml:"id"`
	Name *string `yaml:"name"`
}

type NodeUser struct {
	ID   *int    `yaml:"id"`
	Name *string `yaml:"name"`
}

type Partition struct {
	GUID               *string `yaml:"guid"`
	Label              *string `yaml:"label"`
	Number             int     `yaml:"number"`
	ShouldExist        *bool   `yaml:"should_exist"`
	SizeMiB            *int    `yaml:"size_mib"`
	StartMiB           *int    `yaml:"start_mib"`
	TypeGUID           *string `yaml:"type_guid"`
	WipePartitionEntry *bool   `yaml:"wipe_partition_entry"`
}

type Passwd struct {
	Groups []PasswdGroup `yaml:"groups"`
	Users  []PasswdUser  `yaml:"users"`
}

type PasswdGroup struct {
	Gid          *int    `yaml:"gid"`
	Name         string  `yaml:"name"`
	PasswordHash *string `yaml:"password_hash"`
	System       *bool   `yaml:"system"`
}

type PasswdUser struct {
	Gecos             *string            `yaml:"gecos"`
	Groups            []Group            `yaml:"groups"`
	HomeDir           *string            `yaml:"home_dir"`
	Name              string             `yaml:"name"`
	NoCreateHome      *bool              `yaml:"no_create_home"`
	NoLogInit         *bool              `yaml:"no_log_init"`
	NoUserGroup       *bool              `yaml:"no_user_group"`
	PasswordHash      *string            `yaml:"password_hash"`
	PrimaryGroup      *string            `yaml:"primary_group"`
	SSHAuthorizedKeys []SSHAuthorizedKey `yaml:"ssh_authorized_keys"`
	Shell             *string            `yaml:"shell"`
	System            *bool              `yaml:"system"`
	UID               *int               `yaml:"uid"`
}

type Proxy struct {
	HTTPProxy  *string  `yaml:"http_proxy"`
	HTTPSProxy *string  `yaml:"https_proxy"`
	NoProxy    []string `yaml:"no_proxy"`
}

type Raid struct {
	Devices []Device     `yaml:"devices"`
	Level   string       `yaml:"level"`
	Name    string       `yaml:"name"`
	Options []RaidOption `yaml:"options"`
	Spares  *int         `yaml:"spares"`
}

type RaidOption string

type Resource struct {
	Compression  *string      `yaml:"compression"`
	HTTPHeaders  HTTPHeaders  `yaml:"http_headers"`
	Source       *string      `yaml:"source"`
	Inline       *string      `yaml:"inline"` // Added, not in ignition spec
	Local        *string      `yaml:"local"`  // Added, not in ignition spec
	Verification Verification `yaml:"verification"`
}

type SSHAuthorizedKey string

type Security struct {
	TLS TLS `yaml:"tls"`
}

type Storage struct {
	Directories []Directory  `yaml:"directories"`
	Disks       []Disk       `yaml:"disks"`
	Files       []File       `yaml:"files"`
	Filesystems []Filesystem `yaml:"filesystems"`
	Links       []Link       `yaml:"links"`
	Raid        []Raid       `yaml:"raid"`
	Trees       []Tree       `yaml:"trees" butane:"auto_skip"` // Added, not in ignition spec
}

type Systemd struct {
	Units []Unit `yaml:"units"`
}

type TLS struct {
	CertificateAuthorities []Resource `yaml:"certificate_authorities"`
}

type Timeouts struct {
	HTTPResponseHeaders *int `yaml:"http_response_headers"`
	HTTPTotal           *int `yaml:"http_total"`
}

type Tree struct {
	Local string  `yaml:"local"`
	Path  *string `yaml:"path"`
}

type Unit struct {
	Contents *string  `yaml:"contents"`
	Dropins  []Dropin `yaml:"dropins"`
	Enabled  *bool    `yaml:"enabled"`
	Mask     *bool    `yaml:"mask"`
	Name     string   `yaml:"name"`
}

type Verification struct {
	Hash *string `yaml:"hash"`
}
