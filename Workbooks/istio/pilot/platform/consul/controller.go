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

package consul

import (
	"time"
	// TODO(nmittler): Remove this
	_ "github.com/golang/glog"
	"github.com/hashicorp/consul/api"

	"istio.io/istio/pilot/model"
	"istio.io/istio/pkg/log"
)

// Controller communicates with Consul and monitors for changes
type Controller struct {
	client  *api.Client
	monitor Monitor
}

// NewController creates a new Consul controller
func NewController(addr string, interval time.Duration) (*Controller, error) {
	conf := api.DefaultConfig()
	conf.Address = addr

	client, err := api.NewClient(conf)
	return &Controller{
		monitor: NewConsulMonitor(client, interval),
		client:  client,
	}, err
}

// Services list declarations of all services in the system
func (c *Controller) Services() ([]*model.Service, error) {
	data, err := c.getServices()
	if err != nil {
		return nil, err
	}

	services := make([]*model.Service, 0, len(data))
	for name := range data {
		endpoints, err := c.getCatalogService(name, nil)
		if err != nil {
			return nil, err
		}
		services = append(services, convertService(endpoints))
	}

	return services, nil
}

// GetService retrieves a service by host name if it exists
func (c *Controller) GetService(hostname string) (*model.Service, error) {
	// Get actual service by name
	name, err := parseHostname(hostname)
	if err != nil {
		log.Infof("parseHostname(%s) => error %v", hostname, err)
		return nil, err
	}

	endpoints, err := c.getCatalogService(name, nil)
	if len(endpoints) == 0 || err != nil {
		return nil, err
	}

	return convertService(endpoints), nil
}

func (c *Controller) getServices() (map[string][]string, error) {
	data, _, err := c.client.Catalog().Services(nil)
	if err != nil {
		log.Warnf("Could not retrieve services from consul: %v", err)
		return nil, err
	}

	return data, nil
}

func (c *Controller) getCatalogService(name string, q *api.QueryOptions) ([]*api.CatalogService, error) {
	endpoints, _, err := c.client.Catalog().Service(name, "", q)
	if err != nil {
		log.Warnf("Could not retrieve service catalogue from consul: %v", err)
		return nil, err
	}

	return endpoints, nil
}

// ManagementPorts retries set of health check ports by instance IP.
// This does not apply to Consul service registry, as Consul does not
// manage the service instances. In future, when we integrate Nomad, we
// might revisit this function.
func (c *Controller) ManagementPorts(addr string) model.PortList {
	return nil
}

// Instances retrieves instances for a service and its ports that match
// any of the supplied labels. All instances match an empty tag list.
func (c *Controller) Instances(hostname string, ports []string,
	labels model.LabelsCollection) ([]*model.ServiceInstance, error) {
	// Get actual service by name
	name, err := parseHostname(hostname)
	if err != nil {
		log.Infof("parseHostname(%s) => error %v", hostname, err)
		return nil, err
	}

	portMap := make(map[string]bool)
	for _, port := range ports {
		portMap[port] = true
	}

	endpoints, err := c.getCatalogService(name, nil)
	if err != nil {
		return nil, err
	}

	instances := []*model.ServiceInstance{}
	for _, endpoint := range endpoints {
		instance := convertInstance(endpoint)
		if labels.HasSubsetOf(instance.Labels) && portMatch(instance, portMap) {
			instances = append(instances, instance)
		}
	}

	return instances, nil
}

// returns true if an instance's port matches with any in the provided list
func portMatch(instance *model.ServiceInstance, portMap map[string]bool) bool {
	if len(portMap) == 0 {
		return true
	}

	if portMap[instance.Endpoint.ServicePort.Name] {
		return true
	}

	return false
}

// HostInstances lists service instances for a given set of IPv4 addresses.
func (c *Controller) HostInstances(addrs map[string]*model.Node) ([]*model.ServiceInstance, error) {
	data, err := c.getServices()
	if err != nil {
		return nil, err
	}
	out := make([]*model.ServiceInstance, 0)
	for svcName := range data {
		endpoints, err := c.getCatalogService(svcName, nil)
		if err != nil {
			return nil, err
		}
		for _, endpoint := range endpoints {
			if addrs[endpoint.ServiceAddress] != nil {
				out = append(out, convertInstance(endpoint))
			}
		}
	}

	return out, nil
}

// Run all controllers until a signal is received
func (c *Controller) Run(stop <-chan struct{}) {
	c.monitor.Start(stop)
}

// AppendServiceHandler implements a service catalog operation
func (c *Controller) AppendServiceHandler(f func(*model.Service, model.Event)) error {
	c.monitor.AppendServiceHandler(func(instances []*api.CatalogService, event model.Event) error {
		f(convertService(instances), event)
		return nil
	})
	return nil
}

// AppendInstanceHandler implements a service catalog operation
func (c *Controller) AppendInstanceHandler(f func(*model.ServiceInstance, model.Event)) error {
	c.monitor.AppendInstanceHandler(func(instance *api.CatalogService, event model.Event) error {
		f(convertInstance(instance), event)
		return nil
	})
	return nil
}

// GetIstioServiceAccounts implements model.ServiceAccounts operation TODO
func (c *Controller) GetIstioServiceAccounts(hostname string, ports []string) []string {
	return nil
}
