/**
 * Tencent is pleased to support the open source community by making Polaris available.
 *
 * Copyright (C) 2019 THL A29 Limited, a Tencent company. All rights reserved.
 *
 * Licensed under the BSD 3-Clause License (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * https://opensource.org/licenses/BSD-3-Clause
 *
 * Unless required by applicable law or agreed to in writing, software distributed
 * under the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR
 * CONDITIONS OF ANY KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations under the License.
 */

package healthcheck

import (
	api "github.com/polarismesh/polaris-server/common/api/v1"
	"github.com/polarismesh/polaris-server/common/log"
	"github.com/polarismesh/polaris-server/common/model"
	"github.com/polarismesh/polaris-server/plugin"
	"sync"
)

// CacheProvider provider health check objects for service cache
type CacheProvider struct {
	healthCheckInstances map[string]*InstanceWithChecker
	healthCheckMutex     *sync.RWMutex
	selfService          string
	selfServiceInstances map[string]*InstanceWithChecker
	selfServiceMutex     *sync.RWMutex
}

// CacheEvent provide the event for cache changes
type CacheEvent struct {
	healthCheckInstancesChanged bool
	selfServiceInstancesChanged bool
}

func newCacheProvider(selfService string) *CacheProvider {
	return &CacheProvider{
		healthCheckInstances: make(map[string]*InstanceWithChecker),
		healthCheckMutex:     &sync.RWMutex{},
		selfServiceInstances: make(map[string]*InstanceWithChecker),
		selfServiceMutex:     &sync.RWMutex{},
		selfService:          selfService,
	}
}

func (c *CacheProvider) isSelfServiceInstance(instance *api.Instance) bool {
	metadata := instance.GetMetadata()
	if svcName, ok := metadata[model.MetaKeyPolarisService]; ok {
		return svcName == c.selfService
	}
	return false
}

func (c *CacheProvider) sendEvent(event CacheEvent) {
	server.dispatcher.UpdateStatusByEvent(event)
}

func compareAndStoreServiceInstance(
	instanceWithChecker *InstanceWithChecker, mutex *sync.RWMutex, values map[string]*InstanceWithChecker) bool {
	mutex.Lock()
	defer mutex.Unlock()
	instanceId := instanceWithChecker.instance.GetId().GetValue()
	value, ok := values[instanceId]
	if !ok {
		log.Infof("[Health Check][Cache]create service instance is %s:%d, id is %s",
			instanceWithChecker.instance.GetHost().GetValue(), instanceWithChecker.instance.GetPort().GetValue(),
			instanceId)
		values[instanceId] = instanceWithChecker
		return true
	}
	lastInstance := value.instance
	if lastInstance.GetRevision().GetValue() == instanceWithChecker.instance.GetRevision().GetValue() {
		return false
	}
	log.Infof("[Health Check][Cache]update service instance is %s:%d, id is %s",
		instanceWithChecker.instance.GetHost().GetValue(), instanceWithChecker.instance.GetPort().GetValue(), instanceId)
	values[instanceId] = instanceWithChecker
	return true
}

func storeServiceInstance(
	instanceWithChecker *InstanceWithChecker, mutex *sync.RWMutex, values map[string]*InstanceWithChecker) bool {
	mutex.Lock()
	defer mutex.Unlock()
	log.Infof("[Health Check][Cache]create service instance is %s:%d, id is %s",
		instanceWithChecker.instance.GetHost().GetValue(), instanceWithChecker.instance.GetPort().GetValue(),
		instanceWithChecker.instance.GetId())
	instanceId := instanceWithChecker.instance.GetId().GetValue()
	values[instanceId] = instanceWithChecker
	return true
}

func deleteServiceInstance(instance *api.Instance, mutex *sync.RWMutex, values map[string]*InstanceWithChecker) bool {
	mutex.Lock()
	defer mutex.Unlock()
	instanceId := instance.GetId().GetValue()
	_, ok := values[instanceId]
	if ok {
		log.Infof("[Health Check][Cache]delete service instance is %s:%d, id is %s",
			instance.GetHost().GetValue(), instance.GetPort().GetValue(), instanceId)
		delete(values, instanceId)
	}
	return true
}

// InstanceWithChecker instance and checker combine
type InstanceWithChecker struct {
	instance  *api.Instance
	checker   plugin.HealthChecker
	hashValue uint
}

func newInstanceWithChecker(instance *api.Instance, checker plugin.HealthChecker) *InstanceWithChecker {
	return &InstanceWithChecker{
		instance:  instance,
		checker:   checker,
		hashValue: hashString(instance.GetId().GetValue()),
	}
}

// OnCreated callback when cache value created
func (c *CacheProvider) OnCreated(value interface{}) {
	if instance, ok := value.(*api.Instance); ok {
		if c.isSelfServiceInstance(instance) {
			storeServiceInstance(
				newInstanceWithChecker(instance, nil), c.selfServiceMutex, c.selfServiceInstances)
			c.sendEvent(CacheEvent{selfServiceInstancesChanged: true})
			return
		}
		hcEnable, checker := isHealthCheckEnable(instance)
		if !hcEnable {
			return
		}
		storeServiceInstance(newInstanceWithChecker(instance, checker), c.healthCheckMutex, c.healthCheckInstances)
		c.sendEvent(CacheEvent{healthCheckInstancesChanged: true})
	}
}

func isHealthCheckEnable(instance *api.Instance) (bool, plugin.HealthChecker) {
	if !instance.GetEnableHealthCheck().GetValue() || nil == instance.GetHealthCheck() {
		return false, nil
	}
	checker, ok := server.checkers[int32(instance.GetHealthCheck().GetType())]
	if !ok {
		return false, nil
	}
	return true, checker
}

// OnUpdated callback when cache value updated
func (c *CacheProvider) OnUpdated(value interface{}) {
	if instance, ok := value.(*api.Instance); ok {
		if c.isSelfServiceInstance(instance) {
			if compareAndStoreServiceInstance(
				newInstanceWithChecker(instance, nil), c.selfServiceMutex, c.selfServiceInstances) {
				c.sendEvent(CacheEvent{selfServiceInstancesChanged: true})
			}
			return
		}
		//check exists
		c.healthCheckMutex.Lock()
		defer c.healthCheckMutex.Unlock()
		instanceId := instance.GetId().GetValue()
		healthCheckInstanceValue, exists := c.healthCheckInstances[instanceId]
		hcEnable, checker := isHealthCheckEnable(instance)
		if !hcEnable {
			if !exists {
				return
			}
			log.Infof("[Health Check][Cache]delete health check disabled instance is %s:%d, id is %s",
				instance.GetHost().GetValue(), instance.GetPort().GetValue(), instanceId)
			delete(c.healthCheckInstances, instanceId)
			c.sendEvent(CacheEvent{healthCheckInstancesChanged: true})
			return
		}
		var noChanged bool
		if exists {
			healthCheckInstance := healthCheckInstanceValue.instance
			noChanged = healthCheckInstance.GetRevision().GetValue() == instance.GetRevision().GetValue()
		}
		if !noChanged {
			log.Infof("[Health Check][Cache]update service instance is %s:%d, id is %s",
				instance.GetHost().GetValue(), instance.GetPort().GetValue(), instanceId)
			c.healthCheckInstances[instanceId] = newInstanceWithChecker(instance, checker)
			c.sendEvent(CacheEvent{healthCheckInstancesChanged: true})
		}
	}
}

// OnDeleted callback when cache value deleted
func (c *CacheProvider) OnDeleted(value interface{}) {
	if instance, ok := value.(*api.Instance); ok {
		if c.isSelfServiceInstance(instance) {
			deleteServiceInstance(instance, c.selfServiceMutex, c.selfServiceInstances)
			c.sendEvent(CacheEvent{selfServiceInstancesChanged: true})
			return
		}
		if !instance.GetEnableHealthCheck().GetValue() || nil == instance.GetHealthCheck() {
			return
		}
		deleteServiceInstance(instance, c.healthCheckMutex, c.healthCheckInstances)
		c.sendEvent(CacheEvent{healthCheckInstancesChanged: true})
	}
}

// RangeHealthCheckInstances range loop healthCheckInstances
func (c *CacheProvider) RangeHealthCheckInstances(check func(instance *InstanceWithChecker)) {
	c.healthCheckMutex.RLock()
	defer c.healthCheckMutex.RUnlock()
	for _, value := range c.healthCheckInstances {
		check(value)
	}
}

// RangeSelfServiceInstances range loop selfServiceInstances
func (c *CacheProvider) RangeSelfServiceInstances(check func(instance *api.Instance)) {
	c.selfServiceMutex.RLock()
	defer c.selfServiceMutex.RUnlock()
	for _, value := range c.selfServiceInstances {
		check(value.instance)
	}
}

// GetInstance get instance by id
func (c *CacheProvider) GetInstance(id string) *api.Instance {
	c.healthCheckMutex.RLock()
	defer c.healthCheckMutex.RUnlock()
	value, ok := c.healthCheckInstances[id]
	if !ok {
		return nil
	}
	return value.instance
}