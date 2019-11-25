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
	"bufio"
	"bytes"
	"fmt"
	"io"
	"reflect"
	"strings"
	"text/template"
	"time"

	"github.com/ghodss/yaml"
	// TODO(nmittler): Remove this
	_ "github.com/golang/glog"
	"k8s.io/api/batch/v2alpha1"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	yamlDecoder "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"

	meshconfig "istio.io/api/mesh/v1alpha1"
	"istio.io/istio/pkg/log"
	"istio.io/istio/pkg/version"
)

// per-sidecar policy and status (deployment, job, statefulset, pod, etc)
const (
	istioSidecarAnnotationPolicyKey = "sidecar.istio.io/inject"
	istioSidecarAnnotationStatusKey = "sidecar.istio.io/status"
)

// InjectionPolicy determines the policy for injecting the
// sidecar proxy into the watched namespace(s).
type InjectionPolicy string

const (
	// InjectionPolicyDisabled specifies that the initializer will not
	// inject the sidecar into resources by default for the
	// namespace(s) being watched. Resources can enable injection
	// using the "sidecar.istio.io/inject" annotation with value of
	// true.
	InjectionPolicyDisabled InjectionPolicy = "disabled"

	// InjectionPolicyEnabled specifies that the initializer will
	// inject the sidecar into resources by default for the
	// namespace(s) being watched. Resources can disable injection
	// using the "sidecar.istio.io/inject" annotation with value of
	// false.
	InjectionPolicyEnabled InjectionPolicy = "enabled"

	// DefaultInjectionPolicy is the default injection policy.
	DefaultInjectionPolicy = InjectionPolicyEnabled
)

// Defaults values for injecting istio proxy into kubernetes
// resources.
const (
	DefaultSidecarProxyUID = int64(1337)
	DefaultVerbosity       = 2
	DefaultImagePullPolicy = "IfNotPresent"
)

const (
	// InitContainerName is the name for init container
	InitContainerName = "istio-init"

	// ProxyContainerName is the name for sidecar proxy container
	ProxyContainerName = "istio-proxy"

	// ConfigMapKey should match the expected MeshConfig file name
	ConfigMapKey = "mesh"

	// InitializerConfigMapKey is the key into the initailizer ConfigMap data.
	InitializerConfigMapKey = "config"

	// DefaultResyncPeriod specifies how frequently to retrieve the
	// full list of watched resources for initialization.
	DefaultResyncPeriod = 30 * time.Second

	// DefaultInitializerName specifies the name of the initializer.
	DefaultInitializerName = "sidecar.initializer.istio.io"
)

// SidecarConfig collects all container types and volumes for
// sidecar mesh injection
type SidecarConfig struct {
	InitContainers []v1.Container `yaml:"initContainers"`
	Containers     []v1.Container `yaml:"containers"`
	Volumes        []v1.Volume    `yaml:"volumes"`
}

// SidecarTemplate contains configurable settings for the sidecar mesh
// in a format necessary for proper template interpolation
type SidecarTemplate struct {
	Spec           *v1.PodSpec
	ServiceCluster string
	MConfig        *Params
	AuthPolicy     string
}

// InitImageName returns the fully qualified image name for the istio
// init image given a docker hub and tag and debug flag
func InitImageName(hub string, tag string, _ bool) string {
	return hub + "/proxy_init:" + tag
}

// ProxyImageName returns the fully qualified image name for the istio
// proxy image given a docker hub and tag and whether to use debug or not.
func ProxyImageName(hub string, tag string, debug bool) string {
	if debug {
		return hub + "/proxy_debug:" + tag
	}
	return hub + "/proxy:" + tag
}

// Params describes configurable parameters for injecting istio proxy
// into kubernetes resource.
type Params struct {
	InitImage       string                 `json:"initImage"`
	ProxyImage      string                 `json:"proxyImage"`
	Verbosity       int                    `json:"verbosity"`
	SidecarProxyUID int64                  `json:"sidecarProxyUID"`
	Version         string                 `json:"version"`
	EnableCoreDump  bool                   `json:"enableCoreDump"`
	DebugMode       bool                   `json:"debugMode"`
	Mesh            *meshconfig.MeshConfig `json:"-"`
	ImagePullPolicy string                 `json:"imagePullPolicy"`
	// Comma separated list of IP ranges in CIDR form. If set, only
	// redirect outbound traffic to Envoy for these IP
	// ranges. Otherwise all outbound traffic is redirected to Envoy.
	IncludeIPRanges string `json:"includeIPRanges"`
}

// Config specifies the initializer configuration for sidecar
// injection. This includes the sidear template and cluster-side
// injection policy. It is used by kube-inject, initializer, and http
// endpoint.
type Config struct {
	Policy InjectionPolicy `json:"policy"`

	// deprecate if InitializerConfiguration becomes namespace aware
	IncludeNamespaces []string `json:"namespaces"`

	// deprecate if InitializerConfiguration becomes namespace aware
	ExcludeNamespaces []string `json:"excludeNamespaces"`

	// Params specifies the parameters of the injected sidcar template
	Params Params `json:"params"`

	// InitializerName specifies the name of the initializer.
	InitializerName string `json:"initializerName"`
}

// GetInitializerConfig fetches the initializer configuration from a Kubernetes ConfigMap.
func GetInitializerConfig(kube kubernetes.Interface, namespace, injectConfigName string) (*Config, error) {
	var configMap *v1.ConfigMap
	var err error
	if errPoll := wait.Poll(500*time.Millisecond, 60*time.Second, func() (bool, error) {
		if configMap, err = kube.CoreV1().ConfigMaps(namespace).Get(injectConfigName, metav1.GetOptions{}); err != nil {
			return false, err
		}
		return true, nil
	}); errPoll != nil {
		return nil, errPoll
	}
	data, exists := configMap.Data[InitializerConfigMapKey]
	if !exists {
		return nil, fmt.Errorf("missing configuration map key %q", InitializerConfigMapKey)
	}

	var c Config
	if err := yaml.Unmarshal([]byte(data), &c); err != nil {
		return nil, err
	}

	if c.IncludeNamespaces != nil && c.ExcludeNamespaces != nil {
		return nil, fmt.Errorf("cannot configure both namespaces and excludeNamespaces")
	}

	if c.IncludeNamespaces == nil {
		c.IncludeNamespaces = []string{v1.NamespaceAll}
	}

	for _, excludeNamespace := range c.ExcludeNamespaces {
		if excludeNamespace == v1.NamespaceAll {
			return nil, fmt.Errorf("cannot configure ExcludeNamespaces as NamespaceAll")
		}
	}

	// apply safe defaults if not specified
	switch c.Policy {
	case InjectionPolicyDisabled, InjectionPolicyEnabled:
	default:
		c.Policy = DefaultInjectionPolicy
	}
	if c.Params.InitImage == "" {
		c.Params.InitImage = InitImageName(version.Info.DockerHub, version.Info.Version, c.Params.DebugMode)
	}
	if c.Params.ProxyImage == "" {
		c.Params.ProxyImage = ProxyImageName(version.Info.DockerHub, version.Info.Version, c.Params.DebugMode)
	}
	if c.Params.SidecarProxyUID == 0 {
		c.Params.SidecarProxyUID = DefaultSidecarProxyUID
	}
	if c.Params.ImagePullPolicy == "" {
		c.Params.ImagePullPolicy = DefaultImagePullPolicy
	}
	if c.InitializerName == "" {
		c.InitializerName = DefaultInitializerName
	}

	return &c, nil
}

func injectRequired(include, ignored, excluded []string, namespacePolicy InjectionPolicy, obj metav1.Object) bool {
	// skip special kubernetes system namespaces
	for _, namespace := range ignored {
		if obj.GetNamespace() == namespace {
			return false
		}
	}

	// skip customized exclude namespaces
	for _, excludeNamespace := range excluded {
		if obj.GetNamespace() == excludeNamespace {
			return false
		}
	}

	var included bool
IncludeNamespaceSearch:
	for _, namespace := range include {
		if namespace == v1.NamespaceAll {
			included = true
			break IncludeNamespaceSearch
		} else if obj.GetNamespace() == namespace {
			// Don't skip. The initializer should initialize this
			// resource.
			included = true
			break IncludeNamespaceSearch
		}
		// else, keep searching
	}
	if !included {
		return false
	}

	var useDefault bool
	var inject bool

	annotations := obj.GetAnnotations()
	if annotations == nil {
		useDefault = true
	} else {
		if value, ok := annotations[istioSidecarAnnotationPolicyKey]; !ok {
			useDefault = true
		} else {
			// http://yaml.org/type/bool.html
			switch strings.ToLower(value) {
			case "y", "yes", "true", "on":
				inject = true
			}
		}
	}

	var required bool

	switch namespacePolicy {
	default: // InjectionPolicyOff
		required = false
	case InjectionPolicyDisabled:
		if useDefault {
			required = false
		} else {
			required = inject
		}
	case InjectionPolicyEnabled:
		if useDefault {
			required = true
		} else {
			required = inject
		}
	}

	status, ok := annotations[istioSidecarAnnotationStatusKey]

	log.Infof("Sidecar injection policy for %v/%v: namespacePolicy:%v useDefault:%v inject:%v status:%q required:%v",
		obj.GetNamespace(), obj.GetName(), namespacePolicy, useDefault, inject, status, required)

	if !required {
		return false
	}

	// TODO - add version check for sidecar upgrade

	return !ok
}

func injectIntoSpec(p *Params, spec *v1.PodSpec, metadata *metav1.ObjectMeta) {

	st := SidecarTemplate{spec, p.Mesh.DefaultConfig.ServiceCluster, p, p.Mesh.DefaultConfig.ControlPlaneAuthPolicy.String()}

	// If 'app' label is available, use it as the default service cluster
	if val, ok := metadata.GetLabels()["app"]; ok {
		st.ServiceCluster = val
	}

	var tmpl bytes.Buffer

	t := template.Must(template.New("inject").Parse(productionTemplate))
	if err := t.Execute(&tmpl, &st); err != nil {
		log.Errora(err)
	}

	sc := SidecarConfig{}
	if err := yaml.Unmarshal(tmpl.Bytes(), &sc); err != nil {
		log.Warnf(err.Error())
	}

	spec.InitContainers = append(spec.InitContainers, sc.InitContainers...)
	spec.Containers = append(spec.Containers, sc.Containers...)
	spec.Volumes = append(spec.Volumes, sc.Volumes...)
}

func intoObject(c *Config, in runtime.Object) (interface{}, error) {
	obj, err := meta.Accessor(in)
	if err != nil {
		return nil, err
	}

	out := in.DeepCopyObject()

	if !injectRequired(c.IncludeNamespaces, ignoredNamespaces, c.ExcludeNamespaces, c.Policy, obj) {
		log.Infof("Skipping %s/%s due to policy check", obj.GetNamespace(), obj.GetName())
		return out, nil
	}

	// `in` is a pointer to an Object. Dereference it.
	outValue := reflect.ValueOf(out).Elem()

	var objectMeta *metav1.ObjectMeta
	var templateObjectMeta *metav1.ObjectMeta
	var templatePodSpec *v1.PodSpec
	// CronJobs have JobTemplates in them, instead of Templates, so we
	// special case them.
	if job, ok := out.(*v2alpha1.CronJob); ok {
		objectMeta = &job.ObjectMeta
		templateObjectMeta = &job.Spec.JobTemplate.ObjectMeta
		templatePodSpec = &job.Spec.JobTemplate.Spec.Template.Spec
	} else {
		templateValue := outValue.FieldByName("Spec").FieldByName("Template")
		// `Template` is defined as a pointer in some older API
		// definitions, e.g. ReplicationController
		if templateValue.Kind() == reflect.Ptr {
			templateValue = templateValue.Elem()
		}
		objectMeta = outValue.FieldByName("ObjectMeta").Addr().Interface().(*metav1.ObjectMeta)
		templateObjectMeta = templateValue.FieldByName("ObjectMeta").Addr().Interface().(*metav1.ObjectMeta)
		templatePodSpec = templateValue.FieldByName("Spec").Addr().Interface().(*v1.PodSpec)
	}

	// Skip injection when host networking is enabled. The problem is
	// that the iptable changes are assumed to be within the pod when,
	// in fact, they are changing the routing at the host level. This
	// often results in routing failures within a node which can
	// affect the network provider within the cluster causing
	// additional pod failures.
	if templatePodSpec.HostNetwork {
		return out, nil
	}

	for _, m := range []*metav1.ObjectMeta{objectMeta, templateObjectMeta} {
		if m.Annotations == nil {
			m.Annotations = make(map[string]string)
		}
		m.Annotations[istioSidecarAnnotationStatusKey] = "injected-version-" + c.Params.Version
	}

	injectIntoSpec(&c.Params, templatePodSpec, templateObjectMeta)

	return out, nil
}

// IntoResourceFile injects the istio proxy into the specified
// kubernetes YAML file.
func IntoResourceFile(c *Config, in io.Reader, out io.Writer) error {
	reader := yamlDecoder.NewYAMLReader(bufio.NewReaderSize(in, 4096))
	for {
		raw, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		var typeMeta metav1.TypeMeta
		if err = yaml.Unmarshal(raw, &typeMeta); err != nil {
			return err
		}

		gvk := schema.FromAPIVersionAndKind(typeMeta.APIVersion, typeMeta.Kind)
		obj, err := injectScheme.New(gvk)
		var updated []byte
		if err == nil {
			if err = yaml.Unmarshal(raw, obj); err != nil {
				return err
			}
			out, err := intoObject(c, obj) // nolint: vetshadow
			if err != nil {
				return err
			}
			if updated, err = yaml.Marshal(out); err != nil {
				return err
			}
		} else {
			updated = raw // unchanged
		}
		if _, err = out.Write(updated); err != nil {
			return err
		}
		if _, err = fmt.Fprint(out, "---\n"); err != nil {
			return err
		}
	}
	return nil
}
