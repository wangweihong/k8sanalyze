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

package resource

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"

	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/errors"
	"k8s.io/kubernetes/pkg/api/meta"
	"k8s.io/kubernetes/pkg/api/validation"
	"k8s.io/kubernetes/pkg/labels"
	"k8s.io/kubernetes/pkg/runtime"
	"k8s.io/kubernetes/pkg/runtime/schema"
	utilerrors "k8s.io/kubernetes/pkg/util/errors"
	"k8s.io/kubernetes/pkg/util/yaml"
	"k8s.io/kubernetes/pkg/watch"
)

const (
	constSTDINstr       string = "STDIN"
	stopValidateMessage        = "if you choose to ignore these errors, turn validation off with --validate=false"
)

// Visitor lets clients walk a list of resources.
//
//因为Visitor会被层层Visitor封装.
//但调用顺序是最底层的Visitor,依次往外走
type Visitor interface {
	Visit(VisitorFunc) error
}

// VisitorFunc implements the Visitor interface for a matching function.
// If there was a problem walking a list of resources, the incoming error
// will describe the problem and the function can decide how to handle that error.
// A nil returned indicates to accept an error to continue loops even when errors happen.
// This is useful for ignoring certain kinds of errors or aggregating errors in some way.
type VisitorFunc func(*Info, error) error

// Watchable describes a resource that can be watched for changes that occur on the server,
// beginning after the provided resource version.
type Watchable interface {
	Watch(resourceVersion string) (watch.Interface, error)
}

// ResourceMapping allows an object to return the resource mapping associated with
// the resource or resources it represents.
type ResourceMapping interface {
	ResourceMapping() *meta.RESTMapping
}

// Info contains temporary info to execute a REST call, or show the results
// of an already completed REST call.
//初始化见k8s.io/kubernetes/pkg/kubectl/resource/mapper.go的InfoForData()
//测试参考:https://note.youdao.com/web/#/file/WEB2cb3ce540ae0e769d32485250d11127a/markdown/WEBc9e9d1ea1f005e47d2fd8a5ac87e5ec3
//这里是执行Result.Infos(),遍历Result的visitors后, 生成的用于向apiserver发送的http请求的临时信息
//每一项资源对应一个Info
type Info struct {
	Client    RESTClient //RESTClient,
	Mapping   *meta.RESTMapping
	Namespace string //namespace
	Name      string //资源名

	// Optional, Source is the filename or URL to template file (.json or .yaml),
	// or stdin to use to handle the resource
	Source string //文件名/URL路径对,只有通过文件传入的resource data才有值
	// Optional, this is the provided object in a versioned type before defaulting
	// and conversions into its corresponding internal type. This is useful for
	// reflecting on user intent which may be lost after defaulting and conversions.
	//更正,所有包含TypeMeta匿名结构体的k8s resource object的指针形式都实现了这个接口runtime.Object
	//所以这里的VersionObject很有可能是k8s resource object的指针值.
	VersionedObject runtime.Object //这里包含着resource通过map[string]interface{}解析后的值
	//&Pod{ObjectMeta:ObjectMeta{Name:nginx,GenerateName:,Namespace:,SelfLink:,UID:,ResourceVersion:,Generation:0,CreationTimestamp:0001-01-01 00:00:00 +0000 UTC,DeletionTimestamp:<nil>,DeletionGracePeriodSeconds:nil,Labels:map[string]string{},Annotations:map[string]string{},OwnerReferences:[],Finalizers:[],ClusterName:,},Spec:PodSpec{Volumes:[],Containers:[{nginx-test nginx:1.7.9 [] []  [{ 0 80  }] [] [] {map[] map[]} [] nil nil nil   nil false false false}],RestartPolicy:,TerminationGracePeriodSeconds:nil,ActiveDeadlineSeconds:nil,DNSPolicy:,NodeSelector:map[string]string{},ServiceAccountName:,DeprecatedServiceAccount:,NodeName:,HostNetwork:false,HostPID:false,HostIPC:false,SecurityContext:nil,ImagePullSecrets:[],Hostname:,Subdomain:,Affinity:nil,},Status:PodStatus{Phase:,Conditions:[],Message:,Reason:,HostIP:,PodIP:,StartTime:<nil>,ContainerStatuses:[],QOSClass:,},}

	// Optional, this is the most recent value returned by the server if available
	//同上
	//解析一个普通yaml的输出结果:&TypeMeta{Kind:,APIVersion:,}
	//这个有什么作用?
	Object runtime.Object //解析后的第一个对象
	// Optional, this is the most recent resource version the server knows about for
	// this type of resource. It may not match the resource version of the object,
	// but if set it should be equal to or newer than the resource version of the
	// object (however the server defines resource version).
	ResourceVersion string
	// Optional, should this resource be exported, stripped of cluster-specific and instance specific fields
	Export bool
}

// NewInfo returns a new info object
func NewInfo(client RESTClient, mapping *meta.RESTMapping, namespace, name string, export bool) *Info {
	return &Info{
		Client:    client,
		Mapping:   mapping,
		Namespace: namespace,
		Name:      name,
		Export:    export,
	}
}

// Visit implements Visitor
func (i *Info) Visit(fn VisitorFunc) error {
	return fn(i, nil)
}

// Get retrieves the object from the Namespace and Name fields
//像apiserver获取info中相关object的信息
func (i *Info) Get() (err error) {
	//
	obj, err := NewHelper(i.Client, i.Mapping).Get(i.Namespace, i.Name, i.Export)
	if err != nil {
		if errors.IsNotFound(err) && len(i.Namespace) > 0 && i.Namespace != api.NamespaceDefault && i.Namespace != api.NamespaceAll {
			err2 := i.Client.Get().AbsPath("api", "v1", "namespaces", i.Namespace).Do().Error()
			if err2 != nil && errors.IsNotFound(err2) {
				return err2
			}
		}
		return err
	}
	i.Object = obj
	i.ResourceVersion, _ = i.Mapping.MetadataAccessor.ResourceVersion(obj)
	return nil
}

// Refresh updates the object with another object. If ignoreError is set
// the Object will be updated even if name, namespace, or resourceVersion
// attributes cannot be loaded from the object.
//根据object,更新Info中的object
func (i *Info) Refresh(obj runtime.Object, ignoreError bool) error {
	name, err := i.Mapping.MetadataAccessor.Name(obj)
	if err != nil {
		if !ignoreError {
			return err
		}
	} else {
		i.Name = name
	}
	namespace, err := i.Mapping.MetadataAccessor.Namespace(obj)
	if err != nil {
		if !ignoreError {
			return err
		}
	} else {
		i.Namespace = namespace
	}
	version, err := i.Mapping.MetadataAccessor.ResourceVersion(obj)
	if err != nil {
		if !ignoreError {
			return err
		}
	} else {
		i.ResourceVersion = version
	}
	i.Object = obj
	return nil
}

// Namespaced returns true if the object belongs to a namespace
func (i *Info) Namespaced() bool {
	return i.Mapping != nil && i.Mapping.Scope.Name() == meta.RESTScopeNameNamespace
}

// Watch returns server changes to this object after it was retrieved.
func (i *Info) Watch(resourceVersion string) (watch.Interface, error) {
	return NewHelper(i.Client, i.Mapping).WatchSingle(i.Namespace, i.Name, resourceVersion)
}

// ResourceMapping returns the mapping for this resource and implements ResourceMapping
func (i *Info) ResourceMapping() *meta.RESTMapping {
	return i.Mapping
}

// VisitorList implements Visit for the sub visitors it contains. The first error
// returned from a child Visitor will terminate iteration.
type VisitorList []Visitor

// Visit implements Visitor
//visitor出错则直接返回
func (l VisitorList) Visit(fn VisitorFunc) error {
	for i := range l {
		if err := l[i].Visit(fn); err != nil {
			return err
		}
	}
	return nil
}

// EagerVisitorList implements Visit for the sub visitors it contains. All errors
// will be captured and returned at the end of iteration.
//visitor出错,不会直接退出,在最后再返回
type EagerVisitorList []Visitor

// Visit implements Visitor, and gathers errors that occur during processing until
// all sub visitors have been visited.
func (l EagerVisitorList) Visit(fn VisitorFunc) error {
	errs := []error(nil)
	for i := range l {
		if err := l[i].Visit(func(info *Info, err error) error {
			if err != nil {
				errs = append(errs, err)
				return nil
			}
			if err := fn(info, nil); err != nil {
				errs = append(errs, err)
			}
			return nil
		}); err != nil {
			errs = append(errs, err)
		}
	}
	return utilerrors.NewAggregate(errs)
}

func ValidateSchema(data []byte, schema validation.Schema) error {
	if schema == nil {
		return nil
	}
	if err := schema.ValidateBytes(data); err != nil {
		return fmt.Errorf("error validating data: %v; %s", err, stopValidateMessage)
	}
	return nil
}

// URLVisitor downloads the contents of a URL, and if successful, returns
// an info object representing the downloaded object.
//visit时访问URL获取资源数据
type URLVisitor struct {
	URL *url.URL
	*StreamVisitor
	HttpAttemptCount int
}

//
func (v *URLVisitor) Visit(fn VisitorFunc) error {
	body, err := readHttpWithRetries(httpgetImpl, time.Second, v.URL.String(), v.HttpAttemptCount)
	if err != nil {
		return err
	}
	defer body.Close()
	v.StreamVisitor.Reader = body
	return v.StreamVisitor.Visit(fn)
}

// readHttpWithRetries tries to http.Get the v.URL retries times before giving up.
func readHttpWithRetries(get httpget, duration time.Duration, u string, attempts int) (io.ReadCloser, error) {
	var err error
	var body io.ReadCloser
	if attempts <= 0 {
		return nil, fmt.Errorf("http attempts must be greater than 0, was %d", attempts)
	}
	//尝试指定的次数
	for i := 0; i < attempts; i++ {
		var statusCode int
		var status string
		if i > 0 {
			time.Sleep(duration)
		}

		// Try to get the URL
		statusCode, status, body, err = get(u)

		// Retry Errors
		if err != nil {
			continue
		}

		// Error - Set the error condition from the StatusCode
		if statusCode != http.StatusOK {
			err = fmt.Errorf("unable to read URL %q, server reported %s, status code=%d", u, status, statusCode)
		}

		if statusCode >= 500 && statusCode < 600 {
			// Retry 500's
			continue
		} else {
			// Don't retry other StatusCodes
			break
		}
	}
	return body, err
}

// httpget Defines function to retrieve a url and return the results.  Exists for unit test stubbing.
type httpget func(url string) (int, string, io.ReadCloser, error)

// httpgetImpl Implements a function to retrieve a url and return the results.
func httpgetImpl(url string) (int, string, io.ReadCloser, error) {
	resp, err := http.Get(url)
	if err != nil {
		return 0, "", nil, err
	}
	return resp.StatusCode, resp.Status, resp.Body, nil
}

// DecoratedVisitor will invoke the decorators in order prior to invoking the visitor function
// passed to Visit. An error will terminate the visit.
type DecoratedVisitor struct {
	visitor    Visitor
	decorators []VisitorFunc //装饰函数,会对Info中的对象进行更改namespace等操作
}

// NewDecoratedVisitor will create a visitor that invokes the provided visitor functions before
// the user supplied visitor function is invoked, giving them the opportunity to mutate the Info
// object or terminate early with an error.
func NewDecoratedVisitor(v Visitor, fn ...VisitorFunc) Visitor {
	if len(fn) == 0 {
		return v
	}
	return DecoratedVisitor{v, fn}
}

// Visit implements Visitor
//
func (v DecoratedVisitor) Visit(fn VisitorFunc) error {
	return v.visitor.Visit(func(info *Info, err error) error {
		if err != nil {
			return err
		}
		for i := range v.decorators {
			if err := v.decorators[i](info, nil); err != nil {
				return err
			}
		}
		return fn(info, nil)
	})
}

// ContinueOnErrorVisitor visits each item and, if an error occurs on
// any individual item, returns an aggregate error after all items
// are visited.
type ContinueOnErrorVisitor struct {
	Visitor
}

// Visit returns nil if no error occurs during traversal, a regular
// error if one occurs, or if multiple errors occur, an aggregate
// error.  If the provided visitor fails on any individual item it
// will not prevent the remaining items from being visited. An error
// returned by the visitor directly may still result in some items
// not being visited.
func (v ContinueOnErrorVisitor) Visit(fn VisitorFunc) error {
	errs := []error{}
	err := v.Visitor.Visit(func(info *Info, err error) error {
		if err != nil {
			errs = append(errs, err)
			return nil
		}
		if err := fn(info, nil); err != nil {
			errs = append(errs, err)
		}
		return nil
	})
	if err != nil {
		errs = append(errs, err)
	}
	if len(errs) == 1 {
		return errs[0]
	}
	return utilerrors.NewAggregate(errs)
}

// FlattenListVisitor flattens any objects that runtime.ExtractList recognizes as a list
// - has an "Items" public field that is a slice of runtime.Objects or objects satisfying
// that interface - into multiple Infos. An error on any sub item (for instance, if a List
// contains an object that does not have a registered client or resource) will terminate
// the visit.
// TODO: allow errors to be aggregated?
type FlattenListVisitor struct {
	Visitor
	*Mapper
}

// NewFlattenListVisitor creates a visitor that will expand list style runtime.Objects
// into individual items and then visit them individually.
func NewFlattenListVisitor(v Visitor, mapper *Mapper) Visitor {
	return FlattenListVisitor{v, mapper}
}

func (v FlattenListVisitor) Visit(fn VisitorFunc) error {
	return v.Visitor.Visit(func(info *Info, err error) error {
		if err != nil {
			return err
		}
		//??
		if info.Object == nil {
			return fn(info, nil)
		}
		items, err := meta.ExtractList(info.Object)
		if err != nil {
			return fn(info, nil)
		}
		if errs := runtime.DecodeList(items, struct {
			runtime.ObjectTyper
			runtime.Decoder
		}{v.Mapper, v.Mapper.Decoder}); len(errs) > 0 {
			return utilerrors.NewAggregate(errs)
		}

		// If we have a GroupVersionKind on the list, prioritize that when asking for info on the objects contained in the list
		var preferredGVKs []schema.GroupVersionKind
		if info.Mapping != nil && !info.Mapping.GroupVersionKind.Empty() {
			preferredGVKs = append(preferredGVKs, info.Mapping.GroupVersionKind)
		}

		for i := range items {
			item, err := v.InfoForObject(items[i], preferredGVKs)
			if err != nil {
				return err
			}
			if len(info.ResourceVersion) != 0 {
				item.ResourceVersion = info.ResourceVersion
			}
			if err := fn(item, nil); err != nil {
				return err
			}
		}
		return nil
	})
}

func ignoreFile(path string, extensions []string) bool {
	if len(extensions) == 0 {
		return false
	}
	ext := filepath.Ext(path)
	for _, s := range extensions {
		if s == ext {
			return false
		}
	}
	return true
}

// FileVisitorForSTDIN return a special FileVisitor just for STDIN
func FileVisitorForSTDIN(mapper *Mapper, schema validation.Schema) Visitor {
	return &FileVisitor{
		Path:          constSTDINstr,
		StreamVisitor: NewStreamVisitor(nil, mapper, constSTDINstr, schema),
	}
}

// ExpandPathsToFileVisitors will return a slice of FileVisitors that will handle files from the provided path.
// After FileVisitors open the files, they will pass an io.Reader to a StreamVisitor to do the reading. (stdin
// is also taken care of). Paths argument also accepts a single file, and will return a single visitor
func ExpandPathsToFileVisitors(mapper *Mapper, paths string, recursive bool, extensions []string, schema validation.Schema) ([]Visitor, error) {
	var visitors []Visitor
	err := filepath.Walk(paths, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if fi.IsDir() {
			if path != paths && !recursive {
				return filepath.SkipDir
			}
			return nil
		}
		// Don't check extension if the filepath was passed explicitly
		if path != paths && ignoreFile(path, extensions) {
			return nil
		}

		//返回一个vistor
		visitor := &FileVisitor{
			Path:          path,
			StreamVisitor: NewStreamVisitor(nil, mapper, path, schema),
		}

		visitors = append(visitors, visitor)
		return nil
	})

	if err != nil {
		return nil, err
	}
	return visitors, nil
}

// FileVisitor is wrapping around a StreamVisitor, to handle open/close files
type FileVisitor struct {
	Path string
	*StreamVisitor
}

// Visit in a FileVisitor is just taking care of opening/closing files
func (v *FileVisitor) Visit(fn VisitorFunc) error {
	var f *os.File
	if v.Path == constSTDINstr {
		f = os.Stdin
	} else {
		var err error
		if f, err = os.Open(v.Path); err != nil {
			return err
		}
	}
	defer f.Close()

	// TODO: Consider adding a flag to force to UTF16, apparently some
	// Windows tools don't write the BOM
	utf16bom := unicode.BOMOverride(unicode.UTF8.NewDecoder())
	v.StreamVisitor.Reader = transform.NewReader(f, utf16bom)

	return v.StreamVisitor.Visit(fn)
}

// StreamVisitor reads objects from an io.Reader and walks them. A stream visitor can only be
// visited once.
// TODO: depends on objects being in JSON format before being passed to decode - need to implement
// a stream decoder method on runtime.Codec to properly handle this.
type StreamVisitor struct {
	io.Reader //数据读入
	*Mapper   //这个的作用?

	Source string            //资源名
	Schema validation.Schema //用于检测
}

// NewStreamVisitor is a helper function that is useful when we want to change the fields of the struct but keep calls the same.
func NewStreamVisitor(r io.Reader, mapper *Mapper, source string, schema validation.Schema) *StreamVisitor {
	return &StreamVisitor{
		Reader: r,
		Mapper: mapper,
		Source: source,
		Schema: schema,
	}
}

// Visit implements Visitor over a stream. StreamVisitor is able to distinct multiple resources in one stream.
//解析输入流内容
//真正处理数据,然后将生成的resource.Info交由交由其他装饰visitor去处理
func (v *StreamVisitor) Visit(fn VisitorFunc) error {
	//创建一个解析器来解析传递过来的数据流
	d := yaml.NewYAMLOrJSONDecoder(v.Reader, 4096)
	for {
		ext := runtime.RawExtension{}
		//将数据转换成指定的对象
		if err := d.Decode(&ext); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		// TODO: This needs to be able to handle object in other encodings and schemas.
		//去除空数据
		ext.Raw = bytes.TrimSpace(ext.Raw)
		if len(ext.Raw) == 0 || bytes.Equal(ext.Raw, []byte("null")) {
			continue
		}
		//检查是否合法
		if err := ValidateSchema(ext.Raw, v.Schema); err != nil {
			return fmt.Errorf("error validating %q: %v", v.Source, err)
		}
		//解析数据,构建成一个resource.Info对象
		info, err := v.InfoForData(ext.Raw, v.Source)
		if err != nil {
			//调用装饰器函数
			if fnErr := fn(info, err); fnErr != nil {
				return fnErr
			}
			continue
		}
		if err := fn(info, nil); err != nil {
			return err
		}
	}
}

//更新Info中包含的runtime object的namespace
func UpdateObjectNamespace(info *Info, err error) error {
	if err != nil {
		return err
	}
	if info.Object != nil {
		return info.Mapping.MetadataAccessor.SetNamespace(info.Object, info.Namespace)
	}
	return nil
}

// FilterNamespace omits the namespace if the object is not namespace scoped
func FilterNamespace(info *Info, err error) error {
	if err != nil {
		return err
	}
	if !info.Namespaced() {
		info.Namespace = ""
		UpdateObjectNamespace(info, nil)
	}
	return nil
}

// SetNamespace ensures that every Info object visited will have a namespace
// set. If info.Object is set, it will be mutated as well.
//更新Info中的namespace
func SetNamespace(namespace string) VisitorFunc {
	return func(info *Info, err error) error {
		if err != nil {
			return err
		}
		if !info.Namespaced() {
			return nil
		}
		if len(info.Namespace) == 0 {
			info.Namespace = namespace
			UpdateObjectNamespace(info, nil)
		}
		return nil
	}
}

// RequireNamespace will either set a namespace if none is provided on the
// Info object, or if the namespace is set and does not match the provided
// value, returns an error. This is intended to guard against administrators
// accidentally operating on resources outside their namespace.
func RequireNamespace(namespace string) VisitorFunc {
	return func(info *Info, err error) error {
		if err != nil {
			return err
		}
		if !info.Namespaced() {
			return nil
		}
		if len(info.Namespace) == 0 {
			info.Namespace = namespace
			UpdateObjectNamespace(info, nil)
			return nil
		}
		if info.Namespace != namespace {
			return fmt.Errorf("the namespace from the provided object %q does not match the namespace %q. You must pass '--namespace=%s' to perform this operation.", info.Namespace, namespace, info.Namespace)
		}
		return nil
	}
}

// RetrieveLatest updates the Object on each Info by invoking a standard client
// Get.
//
func RetrieveLatest(info *Info, err error) error {
	if err != nil {
		return err
	}
	if meta.IsListType(info.Object) {
		return fmt.Errorf("watch is only supported on individual resources and resource collections, but a list of resources is found")
	}
	if len(info.Name) == 0 {
		return nil
	}
	if info.Namespaced() && len(info.Namespace) == 0 {
		return fmt.Errorf("no namespace set on resource %s %q", info.Mapping.Resource, info.Name)
	}
	return info.Get()
}

// RetrieveLazy updates the object if it has not been loaded yet.
func RetrieveLazy(info *Info, err error) error {
	if err != nil {
		return err
	}
	if info.Object == nil {
		return info.Get()
	}
	return nil
}

type FilterFunc func(info *Info, err error) (bool, error)

type FilteredVisitor struct {
	visitor Visitor
	filters []FilterFunc
}

func NewFilteredVisitor(v Visitor, fn ...FilterFunc) Visitor {
	if len(fn) == 0 {
		return v
	}
	return FilteredVisitor{v, fn}
}

func (v FilteredVisitor) Visit(fn VisitorFunc) error {
	return v.visitor.Visit(func(info *Info, err error) error {
		if err != nil {
			return err
		}
		for _, filter := range v.filters {
			ok, err := filter(info, nil)
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}
		}
		return fn(info, nil)
	})
}

//检测标签是否匹配
func FilterBySelector(s labels.Selector) FilterFunc {
	return func(info *Info, err error) (bool, error) {
		if err != nil {
			return false, err
		}
		a, err := meta.Accessor(info.Object)
		if err != nil {
			return false, err
		}
		if !s.Matches(labels.Set(a.GetLabels())) {
			return false, nil
		}
		return true, nil
	}
}
