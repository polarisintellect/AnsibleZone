// Copyright 2017 Istio Authors
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

package inject

// NOTE: This tool only exists because kubernetes does not support
// dynamic/out-of-tree admission controller for transparent proxy
// injection. This file should be removed as soon as a proper kubernetes
// admission controller is written for istio.

import (
	"fmt"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	meshconfig "istio.io/api/mesh/v1alpha1"
	"istio.io/istio/pilot/model"
)

// GetMeshConfig fetches the ProxyMesh configuration from Kubernetes ConfigMap.
func GetMeshConfig(kube kubernetes.Interface, namespace,
	name string) (*v1.ConfigMap, *meshconfig.MeshConfig, error) {

	config, err := kube.CoreV1().ConfigMaps(namespace).Get(name, metav1.GetOptions{})
	if err != nil {
		return nil, nil, err
	}

	// values in the data are strings, while proto might use a different data type.
	// therefore, we have to get a value by a key
	yaml, exists := config.Data[ConfigMapKey]
	if !exists {
		return nil, nil, fmt.Errorf("missing configuration map key %q", ConfigMapKey)
	}

	mesh, err := model.ApplyMeshConfigDefaults(yaml)
	if err != nil {
		return nil, nil, err
	}
	return config, mesh, nil
}
