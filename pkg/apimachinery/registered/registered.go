/*
Copyright 2015 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package to keep track of API Versions that can be registered and are enabled in api.Scheme.
package registered

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/golang/glog"

	"k8s.io/kubernetes/pkg/api/meta"
	"k8s.io/kubernetes/pkg/apimachinery"
	"k8s.io/kubernetes/pkg/runtime/schema"
	"k8s.io/kubernetes/pkg/util/sets"
)

var (
	//这个环境变量指定的KUBE_API_VERSIONS有何作用
	//KUBE_API_VERSIONS是一个","分隔的数组
	//如果没有指定KUBE_API_VERSIONS时,NewOrDie()只是返回初始化后的APIRegistrationManager
	DefaultAPIRegistrationManager = NewOrDie(os.Getenv("KUBE_API_VERSIONS"))
)

// APIRegistrationManager provides the concept of what API groups are enabled.
//
// TODO: currently, it also provides a "registered" concept. But it's wrong to
// have both concepts in the same object. Therefore the "announced" package is
// going to take over the registered concept. After all the install packages
// are switched to using the announce package instead of this package, then we
// can combine the registered/enabled concepts in this object. Simplifying this
// isn't easy right now because there are so many callers of this package.
//管理启动的API组
//将所有已经注册的，已经激活的，第三方的的GroupVersions进行了汇总，还包括了各个GroupVersion的GroupMeta(元数据)。
//参考自http://dockone.io/article/2161
/*
k8s现阶段，API一共分为13个Group：Core、apps、authentication、authorization、autoscaling、batch、certificates、componentconfig、extensions、imagepolicy、policy、rbac、storage。其中Core的Group Name为空，它包含的API是最核心的API,如Pod、Service等，它的代码位于pkg/api下面，其它12个Group代码位于pkg/apis。每个目录下都有一个install目录，里面有一个install.go文件，接着通过init()负责初始化。
所有的init()函数都会通过调用当前包的
	registered.RegisterVersions(availableVersions)注册到DefaultRegistrationManager中.
	如k8s.io/kubernetes/pkg/api/install/install.go
*/
type APIRegistrationManager struct {
	// registeredGroupVersions stores all API group versions for which RegisterGroup is called.
	registeredVersions map[schema.GroupVersion]struct{} //已注册的api组版本

	// thirdPartyGroupVersions are API versions which are dynamically
	// registered (and unregistered) via API calls to the apiserver
	// 第三方注册的GroupVersions,这些都向apiServer动态注册的
	thirdPartyGroupVersions []schema.GroupVersion

	// enabledVersions represents all enabled API versions. It should be a
	// subset of registeredVersions. Please call EnableVersions() to add
	// enabled versions.
	// 所有已经enable的GroupVersions，可以通过EnableVersions()将要enable的GroupVersion加入进来。
	// 只有enable了，才能使用对应的GroupVersion
	enabledVersions map[schema.GroupVersion]struct{}

	// map of group meta for all groups.
	//key为group名,所有api组的元数据
	groupMetaMap map[string]*apimachinery.GroupMeta

	// envRequestedVersions represents the versions requested via the
	// KUBE_API_VERSIONS environment variable. The install package of each group
	// checks this list before add their versions to the latest package and
	// Scheme.  This list is small and order matters, so represent as a slice
	//IsAllowedVersion()用于在各apigroup注册默认的apiverion时检测version是否允许
	//这个会影响到DefaultRESTMaper创建时的默认版本号
	//见pkg/api/install/install.go
	envRequestedVersions []schema.GroupVersion //KUBE_API_VERSIONS中指定的版本
}

// NewAPIRegistrationManager constructs a new manager. The argument ought to be
// the value of the KUBE_API_VERSIONS env var, or a value of this which you
// wish to test.
//创建一个新的api注册管理器,
//只被NewOrDie()调用kubeAPiVersion
func NewAPIRegistrationManager(kubeAPIVersions string) (*APIRegistrationManager, error) {
	m := &APIRegistrationManager{
		registeredVersions:      map[schema.GroupVersion]struct{}{},
		thirdPartyGroupVersions: []schema.GroupVersion{},
		enabledVersions:         map[schema.GroupVersion]struct{}{},
		groupMetaMap:            map[string]*apimachinery.GroupMeta{},
		envRequestedVersions:    []schema.GroupVersion{},
	}

	//指定了api版本集
	if len(kubeAPIVersions) != 0 {
		for _, version := range strings.Split(kubeAPIVersions, ",") {
			//解析组版本
			gv, err := schema.ParseGroupVersion(version)
			if err != nil {
				return nil, fmt.Errorf("invalid api version: %s in KUBE_API_VERSIONS: %s.",
					version, kubeAPIVersions)
			}
			//
			m.envRequestedVersions = append(m.envRequestedVersions, gv)
		}
	}
	return m, nil
}

/* 测试指定一个不存在的版本,panic
wwh@wwh:~/kiongf/go/src/k8s.io/kubernetes$ kubectl get version
panic: KUBE_API_VERSIONS contains versions that are not installed: ["v2"].

goroutine 1 [running]:
k8s.io/kubernetes/pkg/client/clientset_generated/clientset.init.1()
	/go/src/k8s.io/kubernetes/_output/dockerized/go/src/k8s.io/kubernetes/pkg/client/clientset_generated/clientset/import_known_versions.go:40 +0x119
	k8s.io/kubernetes/pkg/client/clientset_generated/clientset.init()
*/
func NewOrDie(kubeAPIVersions string) *APIRegistrationManager {
	m, err := NewAPIRegistrationManager(kubeAPIVersions)
	if err != nil {
		glog.Fatalf("Could not construct version manager: %v (KUBE_API_VERSIONS=%q)", err, kubeAPIVersions)
	}
	return m
}

// People are calling global functions. Let them continue to do that (for now).
var (
	ValidateEnvRequestedVersions = DefaultAPIRegistrationManager.ValidateEnvRequestedVersions
	AllPreferredGroupVersions    = DefaultAPIRegistrationManager.AllPreferredGroupVersions
	//这里RESTMapper函数返回的RESTMapper interface的实现是k8s.io/kubernetes/pkg/api/meta/priority.go PriorityRESTMapper结构体
	RESTMapper                    = DefaultAPIRegistrationManager.RESTMapper
	GroupOrDie                    = DefaultAPIRegistrationManager.GroupOrDie
	AddThirdPartyAPIGroupVersions = DefaultAPIRegistrationManager.AddThirdPartyAPIGroupVersions
	IsThirdPartyAPIGroupVersion   = DefaultAPIRegistrationManager.IsThirdPartyAPIGroupVersion
	RegisteredGroupVersions       = DefaultAPIRegistrationManager.RegisteredGroupVersions
	IsRegisteredVersion           = DefaultAPIRegistrationManager.IsRegisteredVersion
	IsRegistered                  = DefaultAPIRegistrationManager.IsRegistered
	Group                         = DefaultAPIRegistrationManager.Group
	EnabledVersionsForGroup       = DefaultAPIRegistrationManager.EnabledVersionsForGroup
	EnabledVersions               = DefaultAPIRegistrationManager.EnabledVersions
	IsEnabledVersion              = DefaultAPIRegistrationManager.IsEnabledVersion
	IsAllowedVersion              = DefaultAPIRegistrationManager.IsAllowedVersion
	EnableVersions                = DefaultAPIRegistrationManager.EnableVersions
	RegisterGroup                 = DefaultAPIRegistrationManager.RegisterGroup
	RegisterVersions              = DefaultAPIRegistrationManager.RegisterVersions
	InterfacesFor                 = DefaultAPIRegistrationManager.InterfacesFor
)

// RegisterVersions adds the given group versions to the list of registered group versions.
//获得所有已注册的apiGroupVersion
func (m *APIRegistrationManager) RegisterVersions(availableVersions []schema.GroupVersion) {
	for _, v := range availableVersions {
		m.registeredVersions[v] = struct{}{}
	}
}

// RegisterGroup adds the given group to the list of registered groups.
func (m *APIRegistrationManager) RegisterGroup(groupMeta apimachinery.GroupMeta) error {
	groupName := groupMeta.GroupVersion.Group
	if _, found := m.groupMetaMap[groupName]; found {
		return fmt.Errorf("group %v is already registered", m.groupMetaMap)
	}
	m.groupMetaMap[groupName] = &groupMeta
	return nil
}

// EnableVersions adds the versions for the given group to the list of enabled versions.
// Note that the caller should call RegisterGroup before calling this method.
// The caller of this function is responsible to add the versions to scheme and RESTMapper.
func (m *APIRegistrationManager) EnableVersions(versions ...schema.GroupVersion) error {
	var unregisteredVersions []schema.GroupVersion
	for _, v := range versions {
		if _, found := m.registeredVersions[v]; !found {
			unregisteredVersions = append(unregisteredVersions, v)
		}
		m.enabledVersions[v] = struct{}{}
	}
	if len(unregisteredVersions) != 0 {
		return fmt.Errorf("Please register versions before enabling them: %v", unregisteredVersions)
	}
	return nil
}

// IsAllowedVersion returns if the version is allowed by the KUBE_API_VERSIONS
// environment variable. If the environment variable is empty, then it always
// returns true.
//
func (m *APIRegistrationManager) IsAllowedVersion(v schema.GroupVersion) bool {
	if len(m.envRequestedVersions) == 0 {
		return true
	}
	for _, envGV := range m.envRequestedVersions {
		if v == envGV {
			return true
		}
	}
	return false
}

// IsEnabledVersion returns if a version is enabled.
func (m *APIRegistrationManager) IsEnabledVersion(v schema.GroupVersion) bool {
	_, found := m.enabledVersions[v]
	return found
}

// EnabledVersions returns all enabled versions.  Groups are randomly ordered, but versions within groups
// are priority order from best to worst
func (m *APIRegistrationManager) EnabledVersions() []schema.GroupVersion {
	ret := []schema.GroupVersion{}
	for _, groupMeta := range m.groupMetaMap {
		for _, version := range groupMeta.GroupVersions {
			if m.IsEnabledVersion(version) {
				ret = append(ret, version)
			}
		}
	}
	return ret
}

// EnabledVersionsForGroup returns all enabled versions for a group in order of best to worst
func (m *APIRegistrationManager) EnabledVersionsForGroup(group string) []schema.GroupVersion {
	groupMeta, ok := m.groupMetaMap[group]
	if !ok {
		return []schema.GroupVersion{}
	}

	ret := []schema.GroupVersion{}
	for _, version := range groupMeta.GroupVersions {
		if m.IsEnabledVersion(version) {
			ret = append(ret, version)
		}
	}
	return ret
}

// Group returns the metadata of a group if the group is registered, otherwise
// an error is returned.
//获得apigroup的元数据
func (m *APIRegistrationManager) Group(group string) (*apimachinery.GroupMeta, error) {
	groupMeta, found := m.groupMetaMap[group]
	if !found {
		return nil, fmt.Errorf("group %v has not been registered", group)
	}
	groupMetaCopy := *groupMeta
	return &groupMetaCopy, nil
}

// IsRegistered takes a string and determines if it's one of the registered groups
func (m *APIRegistrationManager) IsRegistered(group string) bool {
	_, found := m.groupMetaMap[group]
	return found
}

// IsRegisteredVersion returns if a version is registered.
func (m *APIRegistrationManager) IsRegisteredVersion(v schema.GroupVersion) bool {
	_, found := m.registeredVersions[v]
	return found
}

// RegisteredGroupVersions returns all registered group versions.
func (m *APIRegistrationManager) RegisteredGroupVersions() []schema.GroupVersion {
	ret := []schema.GroupVersion{}
	for groupVersion := range m.registeredVersions {
		ret = append(ret, groupVersion)
	}
	return ret
}

// IsThirdPartyAPIGroupVersion returns true if the api version is a user-registered group/version.
//检测是否是第三方ApiGroupVersion
func (m *APIRegistrationManager) IsThirdPartyAPIGroupVersion(gv schema.GroupVersion) bool {
	for ix := range m.thirdPartyGroupVersions {
		if m.thirdPartyGroupVersions[ix] == gv {
			return true
		}
	}
	return false
}

// AddThirdPartyAPIGroupVersions sets the list of third party versions,
// registers them in the API machinery and enables them.
// Skips GroupVersions that are already registered.
// Returns the list of GroupVersions that were skipped.
//添加第三方资源版本
func (m *APIRegistrationManager) AddThirdPartyAPIGroupVersions(gvs ...schema.GroupVersion) []schema.GroupVersion {
	filteredGVs := []schema.GroupVersion{}
	skippedGVs := []schema.GroupVersion{}
	for ix := range gvs {
		if !m.IsRegisteredVersion(gvs[ix]) {
			filteredGVs = append(filteredGVs, gvs[ix])
		} else {
			glog.V(3).Infof("Skipping %s, because its already registered", gvs[ix].String())
			skippedGVs = append(skippedGVs, gvs[ix])
		}
	}
	if len(filteredGVs) == 0 {
		return skippedGVs
	}
	m.RegisterVersions(filteredGVs)
	m.EnableVersions(filteredGVs...)
	m.thirdPartyGroupVersions = append(m.thirdPartyGroupVersions, filteredGVs...)

	return skippedGVs
}

// InterfacesFor is a union meta.VersionInterfacesFunc func for all registered types
func (m *APIRegistrationManager) InterfacesFor(version schema.GroupVersion) (*meta.VersionInterfaces, error) {
	groupMeta, err := m.Group(version.Group)
	if err != nil {
		return nil, err
	}
	return groupMeta.InterfacesFor(version)
}

// TODO: This is an expedient function, because we don't check if a Group is
// supported throughout the code base. We will abandon this function and
// checking the error returned by the Group() function.
func (m *APIRegistrationManager) GroupOrDie(group string) *apimachinery.GroupMeta {
	groupMeta, found := m.groupMetaMap[group]
	if !found {
		if group == "" {
			panic("The legacy v1 API is not registered.")
		} else {
			panic(fmt.Sprintf("Group %s is not registered.", group))
		}
	}
	groupMetaCopy := *groupMeta
	return &groupMetaCopy
}

// RESTMapper returns a union RESTMapper of all known types with priorities chosen in the following order:
//  1. if KUBE_API_VERSIONS is specified, then KUBE_API_VERSIONS in order, OR
//  1. legacy kube group preferred version, extensions preferred version, metrics perferred version, legacy
//     kube any version, extensions any version, metrics any version, all other groups alphabetical preferred version,
//     all other groups alphabetical.
func (m *APIRegistrationManager) RESTMapper(versionPatterns ...schema.GroupVersion) meta.RESTMapper {
	//meta.RSETMapper数组
	unionMapper := meta.MultiRESTMapper{}
	unionedGroups := sets.NewString()

	//检测apigroup注册管理器中注册了多少apigroup/version
	for enabledVersion := range m.enabledVersions {
		//插入为存在的group
		if !unionedGroups.Has(enabledVersion.Group) {
			unionedGroups.Insert(enabledVersion.Group)
			groupMeta := m.groupMetaMap[enabledVersion.Group]
			//添加所有注册的apigroup的RESTMapper
			unionMapper = append(unionMapper, groupMeta.RESTMapper)
		}
	}

	//
	if len(versionPatterns) != 0 {
		resourcePriority := []schema.GroupVersionResource{}
		kindPriority := []schema.GroupVersionKind{}
		for _, versionPriority := range versionPatterns {
			resourcePriority = append(resourcePriority, versionPriority.WithResource(meta.AnyResource))
			kindPriority = append(kindPriority, versionPriority.WithKind(meta.AnyKind))
		}

		return meta.PriorityRESTMapper{Delegate: unionMapper, ResourcePriority: resourcePriority, KindPriority: kindPriority}
	}

	//???
	if len(m.envRequestedVersions) != 0 {
		resourcePriority := []schema.GroupVersionResource{}
		kindPriority := []schema.GroupVersionKind{}

		for _, versionPriority := range m.envRequestedVersions {
			resourcePriority = append(resourcePriority, versionPriority.WithResource(meta.AnyResource))
			kindPriority = append(kindPriority, versionPriority.WithKind(meta.AnyKind))
		}

		return meta.PriorityRESTMapper{Delegate: unionMapper, ResourcePriority: resourcePriority, KindPriority: kindPriority}
	}

	prioritizedGroups := []string{"", "extensions", "metrics"}
	resourcePriority, kindPriority := m.prioritiesForGroups(prioritizedGroups...)

	prioritizedGroupsSet := sets.NewString(prioritizedGroups...)
	remainingGroups := sets.String{}
	for enabledVersion := range m.enabledVersions {
		if !prioritizedGroupsSet.Has(enabledVersion.Group) {
			remainingGroups.Insert(enabledVersion.Group)
		}
	}

	remainingResourcePriority, remainingKindPriority := m.prioritiesForGroups(remainingGroups.List()...)
	resourcePriority = append(resourcePriority, remainingResourcePriority...)
	kindPriority = append(kindPriority, remainingKindPriority...)

	return meta.PriorityRESTMapper{Delegate: unionMapper, ResourcePriority: resourcePriority, KindPriority: kindPriority}
}

// prioritiesForGroups returns the resource and kind priorities for a PriorityRESTMapper, preferring the preferred version of each group first,
// then any non-preferred version of the group second.
func (m *APIRegistrationManager) prioritiesForGroups(groups ...string) ([]schema.GroupVersionResource, []schema.GroupVersionKind) {
	resourcePriority := []schema.GroupVersionResource{}
	kindPriority := []schema.GroupVersionKind{}

	for _, group := range groups {
		availableVersions := m.EnabledVersionsForGroup(group)
		if len(availableVersions) > 0 {
			resourcePriority = append(resourcePriority, availableVersions[0].WithResource(meta.AnyResource))
			kindPriority = append(kindPriority, availableVersions[0].WithKind(meta.AnyKind))
		}
	}
	for _, group := range groups {
		resourcePriority = append(resourcePriority, schema.GroupVersionResource{Group: group, Version: meta.AnyVersion, Resource: meta.AnyResource})
		kindPriority = append(kindPriority, schema.GroupVersionKind{Group: group, Version: meta.AnyVersion, Kind: meta.AnyKind})
	}

	return resourcePriority, kindPriority
}

// AllPreferredGroupVersions returns the preferred versions of all registered
// groups in the form of "group1/version1,group2/version2,..."
func (m *APIRegistrationManager) AllPreferredGroupVersions() string {
	if len(m.groupMetaMap) == 0 {
		return ""
	}
	var defaults []string
	for _, groupMeta := range m.groupMetaMap {
		defaults = append(defaults, groupMeta.GroupVersion.String())
	}
	sort.Strings(defaults)
	return strings.Join(defaults, ",")
}

// ValidateEnvRequestedVersions returns a list of versions that are requested in
// the KUBE_API_VERSIONS environment variable, but not enabled.
func (m *APIRegistrationManager) ValidateEnvRequestedVersions() []schema.GroupVersion {
	var missingVersions []schema.GroupVersion
	for _, v := range m.envRequestedVersions {
		if _, found := m.enabledVersions[v]; !found {
			missingVersions = append(missingVersions, v)
		}
	}
	return missingVersions
}
