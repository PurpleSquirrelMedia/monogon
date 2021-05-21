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

package reconciler

import (
	"context"

	storage "k8s.io/api/storage/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// TODO(q3k): this is duplicated with
// //metropolis/node/kubernetes:provisioner.go; integrate this once
// provisioner.go gets moved into a subpackage.
// ONCHANGE(//metropolis/node/kubernetes:provisioner.go): needs to match
// csiProvisionerName declared.
const csiProvisionerName = "dev.monogon.metropolis.vfs"

type resourceCSIDrivers struct {
	kubernetes.Interface
}

func (r resourceCSIDrivers) List(ctx context.Context) ([]string, error) {
	res, err := r.StorageV1().CSIDrivers().List(ctx, listBuiltins)
	if err != nil {
		return nil, err
	}
	objs := make([]string, len(res.Items))
	for i, el := range res.Items {
		objs[i] = el.ObjectMeta.Name
	}
	return objs, nil
}

func (r resourceCSIDrivers) Create(ctx context.Context, el interface{}) error {
	_, err := r.StorageV1().CSIDrivers().Create(ctx, el.(*storage.CSIDriver), meta.CreateOptions{})
	return err
}

func (r resourceCSIDrivers) Delete(ctx context.Context, name string) error {
	return r.StorageV1().CSIDrivers().Delete(ctx, name, meta.DeleteOptions{})
}

func (r resourceCSIDrivers) Expected() map[string]interface{} {
	return map[string]interface{}{
		csiProvisionerName: &storage.CSIDriver{
			ObjectMeta: meta.ObjectMeta{
				Name:   csiProvisionerName,
				Labels: builtinLabels(nil),
			},
			Spec: storage.CSIDriverSpec{
				AttachRequired:       False(),
				PodInfoOnMount:       False(),
				VolumeLifecycleModes: []storage.VolumeLifecycleMode{storage.VolumeLifecyclePersistent},
			},
		},
	}
}
