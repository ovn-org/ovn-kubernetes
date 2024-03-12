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

package common

import (
	"errors"
)

var (
	// common field parsing
	ErrNoVariant      = errors.New("error parsing variant; must be specified")
	ErrInvalidVersion = errors.New("error parsing version; must be a valid semver")

	// high-level errors for fatal reports
	ErrInvalidSourceConfig    = errors.New("source config is invalid")
	ErrInvalidGeneratedConfig = errors.New("config generated was invalid")

	// deprecated variant/version
	ErrRhcosVariantUnsupported = errors.New("rhcos variant has been removed; use openshift variant instead: https://coreos.github.io/butane/upgrading-openshift/")

	// resources and trees
	ErrTooManyResourceSources = errors.New("only one of the following can be set: inline, local, source")
	ErrFilesDirEscape         = errors.New("local file path traverses outside the files directory")
	ErrFileType               = errors.New("trees may only contain files, directories, and symlinks")
	ErrNodeExists             = errors.New("matching filesystem node has existing contents or different type")
	ErrNoFilesDir             = errors.New("local file paths are relative to a files directory that must be specified with -d/--files-dir")
	ErrTreeNotDirectory       = errors.New("root of tree must be a directory")
	ErrTreeNoLocal            = errors.New("local is required")

	// filesystem nodes
	ErrDecimalMode = errors.New("unreasonable mode would be reasonable if specified in octal; remember to add a leading zero")

	// systemd
	ErrTooManySystemdSources = errors.New("only one of the following can be set: contents, contents_local")

	// mount units
	ErrMountUnitNoPath   = errors.New("path is required if with_mount_unit is true and format is not swap")
	ErrMountUnitNoFormat = errors.New("format is required if with_mount_unit is true")

	// boot device
	ErrUnknownBootDeviceLayout = errors.New("layout must be one of: aarch64, ppc64le, x86_64")
	ErrTooFewMirrorDevices     = errors.New("mirroring requires at least two devices")

	// partition
	ErrWrongPartitionNumber = errors.New("incorrect partition number; a new partition will be created using reserved label")

	// MachineConfigs
	ErrFieldElided            = errors.New("field ignored in raw mode")
	ErrNameRequired           = errors.New("metadata.name is required")
	ErrRoleRequired           = errors.New("machineconfiguration.openshift.io/role label is required")
	ErrInvalidKernelType      = errors.New("must be empty, \"default\", or \"realtime\"")
	ErrBtrfsSupport           = errors.New("btrfs is not supported in this spec version")
	ErrFilesystemNoneSupport  = errors.New("format \"none\" is not supported in this spec version")
	ErrFileSchemeSupport      = errors.New("file contents source must be data URL in this spec version")
	ErrFileAppendSupport      = errors.New("appending to files is not supported in this spec version")
	ErrFileCompressionSupport = errors.New("file compression is not supported in this spec version")
	ErrFileSpecialModeSupport = errors.New("special mode bits are not supported in this spec version")
	ErrGroupSupport           = errors.New("groups are not supported in this spec version")
	ErrUserFieldSupport       = errors.New("fields other than \"name\", \"ssh_authorized_keys\", and \"password_hash\" (4.13.0+) are not supported in this spec version")
	ErrUserNameSupport        = errors.New("users other than \"core\" are not supported in this spec version")
	ErrKernelArgumentSupport  = errors.New("this field cannot be used for kernel arguments in this spec version; use openshift.kernel_arguments instead")

	// Storage
	ErrClevisSupport     = errors.New("clevis is not supported in this spec version")
	ErrDirectorySupport  = errors.New("directories are not supported in this spec version")
	ErrDiskSupport       = errors.New("disk customization is not supported in this spec version")
	ErrFilesystemSupport = errors.New("filesystem customization is not supported in this spec version")
	ErrLinkSupport       = errors.New("links are not supported in this spec version")
	ErrLuksSupport       = errors.New("luks is not supported in this spec version")
	ErrRaidSupport       = errors.New("raid is not supported in this spec version")

	// Grub
	ErrGrubUserNameNotSpecified = errors.New("field \"name\" is required")
	ErrGrubPasswordNotSpecified = errors.New("field \"password_hash\" is required")

	// Kernel arguments
	ErrGeneralKernelArgumentSupport = errors.New("kernel argument customization is not supported in this spec version")
)
