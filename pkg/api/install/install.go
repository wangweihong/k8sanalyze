/*
Copyright 2014 The Kubernetes Authors.

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

// Package install installs the v1 monolithic api, making it available as an
// option to all of the API encoding/decoding machinery.
package install

import (
	"fmt"

	"github.com/golang/glog"

	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/meta"
	"k8s.io/kubernetes/pkg/api/v1"
	"k8s.io/kubernetes/pkg/apimachinery"
	"k8s.io/kubernetes/pkg/apimachinery/registered"
	"k8s.io/kubernetes/pkg/runtime"
	"k8s.io/kubernetes/pkg/runtime/schema"
	"k8s.io/kubernetes/pkg/util/sets"
)

const importPrefix = "k8s.io/kubernetes/pkg/api"

//metadata accessor,提供方法来获取/设置resource的元数据
//也实现了pkg/runtime.SelfLinker接口
var accessor = meta.NewAccessor()

// availableVersions lists all known external versions for this group from most preferred to least preferred
//apigroup可以用的版本
//在创建DefaultRESTMapper时使用
var availableVersions = []schema.GroupVersion{v1.SchemeGroupVersion}

func init() {
	//注册apigroup到k8s.io/kubernetes/pkg/apimachinery/registered/registered.go 的DefaultAPIRegistrationManager
	registered.RegisterVersions(availableVersions)
	//添加所有被允许的apiversion
	externalVersions := []schema.GroupVersion{}
	for _, v := range availableVersions {
		//检测版本是否被注册器允许
		if registered.IsAllowedVersion(v) {
			externalVersions = append(externalVersions, v)
		}
	}
	//没有被允许的apigroup version,忽略
	if len(externalVersions) == 0 {
		glog.V(4).Infof("No version is registered for group %v", api.GroupName)
		return
	}

	//使能apiversions,如果有apiversion没有注册,则报错
	if err := registered.EnableVersions(externalVersions...); err != nil {
		glog.V(4).Infof("%v", err)
		return
	}
	if err := enableVersions(externalVersions); err != nil {
		glog.V(4).Infof("%v", err)
		return
	}
}

// TODO: enableVersions should be centralized rather than spread in each API
// group.
// We can combine registered.RegisterVersions, registered.EnableVersions and
// registered.RegisterGroup once we have moved enableVersions there.
func enableVersions(externalVersions []schema.GroupVersion) error {
	//对api.Scheme,添加apiGroup/version/Kind的映射
	addVersionsToScheme(externalVersions...)
	//第一个版本作为默认版本
	preferredExternalVersion := externalVersions[0]

	groupMeta := apimachinery.GroupMeta{
		GroupVersion:  preferredExternalVersion, //默认的版本
		GroupVersions: externalVersions,         //所有被APIRegistration允许的版本
		RESTMapper:    newRESTMapper(externalVersions),
		SelfLinker:    runtime.SelfLinker(accessor),
		InterfacesFor: interfacesFor,
	}

	//注册GroupMeta到APIRegistrationManager
	if err := registered.RegisterGroup(groupMeta); err != nil {
		return err
	}
	return nil
}

func newRESTMapper(externalVersions []schema.GroupVersion) meta.RESTMapper {
	// the list of kinds that are scoped at the root of the api hierarchy
	// if a kind is not enumerated here, it is assumed to have a namespace scope
	// 这些是API最顶层的对象，可以理解为没有namespace的对象
	// 根据有无namespace，对象分为两类：RESTScopeNamespace和RESTScopeRoot
	rootScoped := sets.NewString(
		"Node",
		"Namespace",
		"PersistentVolume",
		"ComponentStatus",
	)

	// these kinds should be excluded from the list of resources
	ignoredKinds := sets.NewString(
		"ListOptions",
		"DeleteOptions",
		"Status",
		"PodLogOptions",
		"PodExecOptions",
		"PodAttachOptions",
		"PodProxyOptions",
		"NodeProxyOptions",
		"ServiceProxyOptions",
		"ThirdPartyResource",
		"ThirdPartyResourceData",
		"ThirdPartyResourceList")

	//
	mapper := api.NewDefaultRESTMapper(externalVersions, interfacesFor, importPrefix, ignoredKinds, rootScoped)

	return mapper
}

// InterfacesFor returns the default Codec and ResourceVersioner for a given version
// string, or an error if the version is not known.
//获得VersionInteface
func interfacesFor(version schema.GroupVersion) (*meta.VersionInterfaces, error) {
	switch version {
	case v1.SchemeGroupVersion:
		return &meta.VersionInterfaces{
			ObjectConvertor:  api.Scheme,
			MetadataAccessor: accessor,
		}, nil
	default:
		//获得指定api组的元数据
		g, _ := registered.Group(api.GroupName)
		return nil, fmt.Errorf("unsupported storage version: %s (valid: %v)", version, g.GroupVersions)
	}
}

//>>>>
func addVersionsToScheme(externalVersions ...schema.GroupVersion) {
	// add the internal version to Scheme
	if err := api.AddToScheme(api.Scheme); err != nil {
		// Programmer error, detect immediately
		panic(err)
	}
	// add the enabled external versions to Scheme
	for _, v := range externalVersions {
		if !registered.IsEnabledVersion(v) {
			glog.Errorf("Version %s is not enabled, so it will not be added to the Scheme.", v)
			continue
		}
		switch v {
		case v1.SchemeGroupVersion:
			if err := v1.AddToScheme(api.Scheme); err != nil {
				// Programmer error, detect immediately
				panic(err)
			}
		}
	}
}
