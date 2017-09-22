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

package meta

import (
	metav1 "k8s.io/kubernetes/pkg/apis/meta/v1"
	"k8s.io/kubernetes/pkg/apis/meta/v1/unstructured"
	"k8s.io/kubernetes/pkg/runtime"
	"k8s.io/kubernetes/pkg/runtime/schema"
	"k8s.io/kubernetes/pkg/types"
)

// VersionInterfaces contains the interfaces one should use for dealing with types of a particular version.
//初始化由各apigroup注册时,创建RESTMapper时初始化
/*
如pkg/api/install/install.go的interfaceFor
return &meta.VersionInterfaces{
	129       ObjectConvertor:  api.Scheme,
	130       MetadataAccessor: accessor,
	131     }, nil
*/
type VersionInterfaces struct {
	//由runtime.Scheme实现
	runtime.ObjectConvertor //转换resource Object从一个版本到另一个版本
	MetadataAccessor        //获取/设置一个resource object的元数据
}

//获取一个Object Interface
//注意所有的k8s resource结构中都包含的ObjectMeta字段实现了这个interface
type ObjectMetaAccessor interface {
	GetObjectMeta() Object
}

// Object lets you work with object metadata from any of the versioned or
// internal API objects. Attempting to set or retrieve a field on an object that does
// not support that field (Name, UID, Namespace on lists) will be a no-op and return
// a default value.
//更新/设置resource object,功能比MetadataAccessor多
type Object interface {
	GetNamespace() string
	SetNamespace(namespace string)
	GetName() string
	SetName(name string)
	GetGenerateName() string
	SetGenerateName(name string)
	GetUID() types.UID
	SetUID(uid types.UID)
	GetResourceVersion() string
	SetResourceVersion(version string)
	GetSelfLink() string
	SetSelfLink(selfLink string)
	GetCreationTimestamp() metav1.Time
	SetCreationTimestamp(timestamp metav1.Time)
	GetDeletionTimestamp() *metav1.Time
	SetDeletionTimestamp(timestamp *metav1.Time)
	GetLabels() map[string]string
	SetLabels(labels map[string]string)
	GetAnnotations() map[string]string
	SetAnnotations(annotations map[string]string)
	GetFinalizers() []string
	SetFinalizers(finalizers []string)
	GetOwnerReferences() []metav1.OwnerReference
	SetOwnerReferences([]metav1.OwnerReference)
	GetClusterName() string
	SetClusterName(clusterName string)
}

// TODO: move me to pkg/apis/meta/v1/unstructured once Object is moved to pkg/apis/meta/v1
var _ Object = &unstructured.Unstructured{}

type ListMetaAccessor interface {
	GetListMeta() List
}

// List lets you work with list metadata from any of the versioned or
// internal API objects. Attempting to set or retrieve a field on an object that does
// not support that field will be a no-op and return a default value.
type List metav1.List

// Type exposes the type and APIVersion of versioned or internal API objects.
type Type metav1.Type

// MetadataAccessor lets you work with object and list metadata from any of the versioned or
// internal API objects. Attempting to set or retrieve a field on an object that does
// not support that field (Name, UID, Namespace on lists) will be a no-op and return
// a default value.
//
// MetadataAccessor exposes Interface in a way that can be used with multiple objects.
//访问/更改resource的元数据
//k8s.io/kubernetes/pkg/api/meta/meta.go的resourceAccessor实现了这个interface
type MetadataAccessor interface {
	APIVersion(obj runtime.Object) (string, error)
	SetAPIVersion(obj runtime.Object, version string) error

	Kind(obj runtime.Object) (string, error)
	SetKind(obj runtime.Object, kind string) error

	Namespace(obj runtime.Object) (string, error)
	SetNamespace(obj runtime.Object, namespace string) error

	Name(obj runtime.Object) (string, error)
	SetName(obj runtime.Object, name string) error

	GenerateName(obj runtime.Object) (string, error)
	SetGenerateName(obj runtime.Object, name string) error

	UID(obj runtime.Object) (types.UID, error)
	SetUID(obj runtime.Object, uid types.UID) error

	SelfLink(obj runtime.Object) (string, error)
	SetSelfLink(obj runtime.Object, selfLink string) error

	Labels(obj runtime.Object) (map[string]string, error)
	SetLabels(obj runtime.Object, labels map[string]string) error

	Annotations(obj runtime.Object) (map[string]string, error)
	SetAnnotations(obj runtime.Object, annotations map[string]string) error

	runtime.ResourceVersioner //获取和设置resource的版本
}

type RESTScopeName string

//RESTScopeRoot指那些像Node,Namespace,PersistentVolume这些没有namspace的对象的作用域
const (
	RESTScopeNameNamespace RESTScopeName = "namespace" //有命名空间的资源
	RESTScopeNameRoot      RESTScopeName = "root"      //没有命名空间
)

// RESTScope contains the information needed to deal with REST resources that are in a resource hierarchy
//只被k8s.io/kubernetes/pkg/api/meta/restmapper.go restScope实现
//// 根据有无namespace，对象分为两类：RESTScopeNamespace和RESTScopeRoot
//RESTScopeRoot指那些像Node,Namespace,PersistentVolume这些没有namspace的对象
type RESTScope interface {
	// Name of the scope
	Name() RESTScopeName //返回的值是RESTScopeNameNamespace
	// ParamName is the optional name of the parameter that should be inserted in the resource url
	// If empty, no param will be inserted
	ParamName() string //返回的是namespaces
	// ArgumentName is the optional name that should be used for the variable holding the value.
	ArgumentName() string //返回的值是namespace
	// ParamDescription is the optional description to use to document the parameter in api documentation
	ParamDescription() string //一些帮助信息
}

// RESTMapping contains the information needed to deal with objects of a specific
// resource and kind in a RESTful manner.
//见k8s.io/kubernetes/pkg/kubectl/resource/helper.go使用RESTMapping来创建资源
//resource和这个结构体之间的关系?
//k8s.io/kubernetes/pkg/kubectl/resource/visitor.go UpdateObjectNamespace()中RestMapping利用MetadataAccessor来更该
//resource object的namespace
//k8s.io/kubernetes/pkg/kubectl/resource/interfaces.go 中ClientMapper,根据RESTMapping生成一个RESTClient(包含url,以及http.Client)
//猜想,一个resource object在不同版本的apiserver中其apigroup/version是不同的,apiserver处理该资源的URL也是不同的.
//因此RESTMapping根据从服务端获得的信息,将资源转换成特定的URL格式,让http.Client提供进行请求
//测试方法:参考https://note.youdao.com/web/#/file/WEB2cb3ce540ae0e769d32485250d11127a/markdown/WEBc9e9d1ea1f005e47d2fd8a5ac87e5ec3
type RESTMapping struct {
	// Resource is a string representing the name of this resource as a REST client would see it
	Resource string //资源的名字, 例如pods

	GroupVersionKind schema.GroupVersionKind //api组版本,对象类型 //例如输出的结果为:"/v1, Kind=Pod"

	// Scope contains the information needed to deal with REST Resources that are in a resource hierarchy
	// 根据有无namespace，对象分为两类：RESTScopeNamespace和RESTScopeRoot
	//RESTScopeRoot指那些像Node,Namespace,PersistentVolume这些没有namspace的对象
	Scope RESTScope

	runtime.ObjectConvertor //转换resource object成不同api版本
	MetadataAccessor        //获取/更改resource object的元数据
}

// RESTMapper allows clients to map resources to kind, and map kind and version
// to interfaces for manipulating those objects. It is primarily intended for
// consumers of Kubernetes compatible REST APIs as defined in docs/devel/api-conventions.md.
//
// The Kubernetes API provides versioned resources and object kinds which are scoped
// to API groups. In other words, kinds and resources should not be assumed to be
// unique across groups.
//
// TODO: split into sub-interfaces
//k8s.io/kubernetes/pkg/api/meta/restmapper.go的DefaultRESTMapper实现了这个
type RESTMapper interface {
	// KindFor takes a partial resource and returns the single match.  Returns an error if there are multiple matches
	//获取指定resource对应的resource kind
	KindFor(resource schema.GroupVersionResource) (schema.GroupVersionKind, error)

	// KindsFor takes a partial resource and returns the list of potential kinds in priority order
	//获取指定resource所有Kind??
	KindsFor(resource schema.GroupVersionResource) ([]schema.GroupVersionKind, error)

	// ResourceFor takes a partial resource and returns the single match.  Returns an error if there are multiple matches
	//??
	ResourceFor(input schema.GroupVersionResource) (schema.GroupVersionResource, error)

	// ResourcesFor takes a partial resource and returns the list of potential resource in priority order
	ResourcesFor(input schema.GroupVersionResource) ([]schema.GroupVersionResource, error)

	// RESTMapping identifies a preferred resource mapping for the provided group kind.
	//根据gk,version获得相应的RESTMapping
	RESTMapping(gk schema.GroupKind, versions ...string) (*RESTMapping, error)
	// RESTMappings returns all resource mappings for the provided group kind if no
	// version search is provided. Otherwise identifies a preferred resource mapping for
	// the provided version(s).
	RESTMappings(gk schema.GroupKind, versions ...string) ([]*RESTMapping, error)

	AliasesForResource(resource string) ([]string, bool)
	ResourceSingularizer(resource string) (singular string, err error)
}
