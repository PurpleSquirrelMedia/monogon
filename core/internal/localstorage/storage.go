// Copyright 2020 The Monogon Project Authors.
//
// SPDX-License-Identifier: Apache-2.0
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
// limitations under the License.

package localstorage

// Localstorage is a replacement for the old 'storage' internal library. It is currently unused, but will become
// so as the node code gets rewritten.

// The library is centered around the idea of a declarative filesystem tree defined as mutually recursive Go structs.
// This structure is then Placed onto an abstract real filesystem (eg. a local POSIX filesystem at /), and a handle
// to that placed filesystem is then used by the consumers of this library to refer to subsets of the tree (that now
// correspond to locations on a filesystem).
//
// Every member of the storage hierarchy must either be, or inherit from Directory or File. In order to be placed
// correctly, Directory embedding structures must use `dir:` or `file:` tags for child Directories and files
// respectively. The content of the tag specifies the path part that this element will be placed at.
//
// Full placement path(available via FullPath()) format is placement implementation-specific. However, they're always
// strings.

import (
	"sync"

	"git.monogon.dev/source/nexantic.git/core/internal/localstorage/declarative"
)

type Root struct {
	declarative.Directory
	// UEFI ESP partition, mounted from plaintext storage.
	ESP ESPDirectory `dir:"esp"`
	// Persistent Data partition, mounted from encrypted and authenticated storage.
	Data DataDirectory `dir:"data"`
	// FHS-standard /etc directory, containes /etc/hosts, /etc/machine-id, and other compatibility files.
	Etc EtcDirectory `dir:"etc"`
	// Ephemeral data, used by runtime, stored in tmpfs. Things like sockets, temporary config files, etc.
	Ephemeral EphemeralDirectory `dir:"ephemeral"`
	// FHS-standard /tmp directory, used by ioutil.TempFile.
	Tmp TmpDirectory `dir:"tmp"`
}

type PKIDirectory struct {
	declarative.Directory
	CACertificate declarative.File `file:"ca.pem"`
	Certificate   declarative.File `file:"cert.pem"`
	Key           declarative.File `file:"cert-key.pem"`
}

// ESPDirectory is the EFI System Partition.
type ESPDirectory struct {
	declarative.Directory
	LocalUnlock ESPLocalUnlockFile `file:"local_unlock.bin"`
	// Enrolment is the configuration/provisioning file for this node, containing information required to begin
	// joining the cluster.
	Enrolment declarative.File `file:"enrolment.pb"`
}

// ESPLocalUnlockFile is the localUnlock file, encrypted by the TPM of this node. After decrypting by the TPM it is used
// in conjunction with the globalUnlock key (retrieved from the existing cluster) to decrypt the local data partition.
type ESPLocalUnlockFile struct {
	declarative.File
}

// DataDirectory is an xfs partition mounted via cryptsetup/LUKS, with a key derived from {global,local}Unlock keys.
type DataDirectory struct {
	declarative.Directory

	// flagLock locks canMount and mounted.
	flagLock sync.Mutex
	// canMount is set by Root when it is initialized. It is required to be set for mounting the data directory.
	canMount bool
	// mounted is set by DataDirectory when it is mounted. It ensures it's only mounted once.
	mounted bool

	Containerd declarative.Directory   `dir:"containerd"`
	Etcd       DataEtcdDirectory       `dir:"etcd"`
	Kubernetes DataKubernetesDirectory `dir:"kubernetes"`
	Node       PKIDirectory            `dir:"node_pki"`
	Volumes    DataVolumesDirectory    `dir:"volumes"`
}

type DataEtcdDirectory struct {
	declarative.Directory
	PeerPKI PKIDirectory          `dir:"peer_pki"`
	PeerCRL declarative.File      `file:"peer_crl"`
	Data    declarative.Directory `dir:"data"`
}

type DataKubernetesDirectory struct {
	declarative.Directory
	ClusterNetworking DataKubernetesClusterNetworkingDirectory `dir:"clusternet"`
	Kubelet           DataKubernetesKubeletDirectory           `dir:"kubelet"`
}

type DataKubernetesClusterNetworkingDirectory struct {
	declarative.Directory
	Key declarative.File `file:"private.key"`
}

type DataKubernetesKubeletDirectory struct {
	declarative.Directory
	Kubeconfig declarative.File `file:"kubeconfig"`
	PKI        PKIDirectory     `dir:"pki"`

	Plugins struct {
		declarative.Directory
		VFS declarative.File `file:"com.smalltown.vfs.sock"`
	} `dir:"plugins"`

	PluginsRegistry struct {
		declarative.Directory
		VFSReg declarative.File `file:"com.smalltown.vfs-reg.sock"`
	} `dir:"plugins_registry"`
}

type DataVolumesDirectory struct {
	declarative.Directory
}

type EtcDirectory struct {
	declarative.Directory
	Hosts     declarative.File `file:"hosts"`
	MachineID declarative.File `file:"machine-id"`
}

type EphemeralDirectory struct {
	declarative.Directory
	Consensus         EphemeralConsensusDirectory  `dir:"consensus"`
	Containerd        EphemeralContainerdDirectory `dir:"containerd"`
	FlexvolumePlugins declarative.Directory        `dir:"flexvolume_plugins"`
}

type EphemeralConsensusDirectory struct {
	declarative.Directory
	ClientSocket declarative.File `file:"client.sock"`
}

type EphemeralContainerdDirectory struct {
	declarative.Directory
	ClientSocket  declarative.File      `file:"client.sock"`
	RunSCLogsFIFO declarative.File      `file:"runsc-logs.fifo"`
	Tmp           declarative.Directory `dir:"tmp"`
	RunSC         declarative.Directory `dir:"runsc"`
	IPAM          declarative.Directory `dir:"ipam"`
}

type TmpDirectory struct {
	declarative.Directory
}
