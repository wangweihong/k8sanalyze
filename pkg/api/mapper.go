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

package api

import (
	"strings"

	"k8s.io/kubernetes/pkg/api/meta"
	"k8s.io/kubernetes/pkg/runtime"
	"k8s.io/kubernetes/pkg/runtime/schema"
	"k8s.io/kubernetes/pkg/util/sets"
)

// Instantiates a DefaultRESTMapper based on types registered in api.Scheme
//调用参考k8s.io/kubernetes/pkg/api/install/install.go
func NewDefaultRESTMapper(defaultGroupVersions []schema.GroupVersion, interfacesFunc meta.VersionInterfacesFunc,
	importPathPrefix string, ignoredKinds, rootScoped sets.String) *meta.DefaultRESTMapper {
	return NewDefaultRESTMapperFromScheme(defaultGroupVersions, interfacesFunc, importPathPrefix, ignoredKinds, rootScoped, Scheme)
}

// Instantiates a DefaultRESTMapper based on types registered in the given scheme.
//这里创建一个meta.DefaultRESTMapper,其实现了meta.RESTMapper interface
func NewDefaultRESTMapperFromScheme(defaultGroupVersions []schema.GroupVersion, interfacesFunc meta.VersionInterfacesFunc,
	importPathPrefix string, ignoredKinds, rootScoped sets.String, scheme *runtime.Scheme) *meta.DefaultRESTMapper {

	mapper := meta.NewDefaultRESTMapper(defaultGroupVersions, interfacesFunc)
	// enumerate all supported versions, get the kinds, and register with the mapper how to address
	// our resources.
	//// 根据输入的defaultGroupVersions,比如"/api/v1"，从Scheme中遍历所有的kinds
	// 然后进行Add
	for _, gv := range defaultGroupVersions {
		for kind, oType := range scheme.KnownTypes(gv) {
			gvk := gv.WithKind(kind)
			// TODO: Remove import path check.
			// We check the import path because we currently stuff both "api" and "extensions" objects
			// into the same group within Scheme since Scheme has no notion of groups yet.
			// 过滤掉不属于"k8s.io/kubernetes/pkg/api"路径下的api，和ignoredKinds
			if !strings.Contains(oType.PkgPath(), importPathPrefix) || ignoredKinds.Has(kind) {
				continue
			}
			//判断该Kind是否有namespace属性
			scope := meta.RESTScopeNamespace
			if rootScoped.Has(kind) {
				scope = meta.RESTScopeRoot
			}
			//将gvk加到对应的组中
			mapper.Add(gvk, scope)
		}
	}
	return mapper
}
