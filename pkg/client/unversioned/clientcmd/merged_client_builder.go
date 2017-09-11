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

package clientcmd

import (
	"io"
	"sync"

	"github.com/golang/glog"

	"k8s.io/kubernetes/pkg/client/restclient"
	clientcmdapi "k8s.io/kubernetes/pkg/client/unversioned/clientcmd/api"
)

// DeferredLoadingClientConfig is a ClientConfig interface that is backed by a client config loader.
// It is used in cases where the loading rules may change after you've instantiated them and you want to be sure that
// the most recent rules are used.  This is useful in cases where you bind flags to loading rule parameters before
// the parse happens and you want your calling code to be ignorant of how the values are being mutated to avoid
// passing extraneous information down a call stack
//注意DeferredLoadingClientConfig实现了ClientConfig interface,其中字段clientConfig也是ClientConfig interface.
//DeferredLoadingClientConfig所有的ClientConfig的实现都是由clientConfig来完成, 如果在调用ClientConfig方法时,clientConfig为nil,则DeferredLoadingClientConfig将会
//根据loader/overrides来创建clientConfig.见DeferredLoadingClientConfig.createClientConfig()
//也即是说这个结构体只是如何生成真正的ClientAccess的实现对象DirectClientConfig
type DeferredLoadingClientConfig struct {
	loader         ClientConfigLoader //客户端配置加载规则
	overrides      *ConfigOverrides   // 配置覆盖项
	fallbackReader io.Reader

	clientConfig ClientConfig // 客户端配置生成接口,实现为k8s.io/kubernetes/pkg/client/unversioned/clientcmd/client_config.go的DirectClientConfig
	loadingLock  sync.Mutex

	// provided for testing
	icc InClusterConfig
}

// InClusterConfig abstracts details of whether the client is running in a cluster for testing.
type InClusterConfig interface {
	ClientConfig
	Possible() bool
}

// NewNonInteractiveDeferredLoadingClientConfig creates a ConfigClientClientConfig using the passed context name
func NewNonInteractiveDeferredLoadingClientConfig(loader ClientConfigLoader, overrides *ConfigOverrides) ClientConfig {
	return &DeferredLoadingClientConfig{loader: loader, overrides: overrides, icc: &inClusterClientConfig{overrides: overrides}}
}

// NewInteractiveDeferredLoadingClientConfig creates a ConfigClientClientConfig using the passed context name and the fallback auth reader
//生成ClientConfig,这时候的DeferredLoadingClientConfig还不具备真正的ClientConfig实现,只有调用ClientConfig中的方法时,才会生成真正的实现
//注意icc只是测试使用
func NewInteractiveDeferredLoadingClientConfig(loader ClientConfigLoader, overrides *ConfigOverrides, fallbackReader io.Reader) ClientConfig {
	return &DeferredLoadingClientConfig{loader: loader, overrides: overrides, icc: &inClusterClientConfig{overrides: overrides}, fallbackReader: fallbackReader}
}

//生成真正的ClientConfig
func (config *DeferredLoadingClientConfig) createClientConfig() (ClientConfig, error) {
	if config.clientConfig == nil {
		config.loadingLock.Lock()
		defer config.loadingLock.Unlock()

		//生成ClientConfig
		if config.clientConfig == nil {
			//按照配置加载规则加载配置文件,注意这时候还没有处理配置项覆盖等问题
			mergedConfig, err := config.loader.Load()
			if err != nil {
				return nil, err
			}

			var mergedClientConfig ClientConfig
			//根据是否有stdin,决定创建何种客户端配置
			if config.fallbackReader != nil {
				mergedClientConfig = NewInteractiveClientConfig(*mergedConfig, config.overrides.CurrentContext, config.overrides, config.fallbackReader, config.loader)
			} else {
				mergedClientConfig = NewNonInteractiveClientConfig(*mergedConfig, config.overrides.CurrentContext, config.overrides, config.loader)
			}

			config.clientConfig = mergedClientConfig
		}
	}

	return config.clientConfig, nil
}

func (config *DeferredLoadingClientConfig) RawConfig() (clientcmdapi.Config, error) {
	mergedConfig, err := config.createClientConfig()
	if err != nil {
		return clientcmdapi.Config{}, err
	}

	return mergedConfig.RawConfig()
}

// ClientConfig implements ClientConfig
func (config *DeferredLoadingClientConfig) ClientConfig() (*restclient.Config, error) {
	mergedClientConfig, err := config.createClientConfig()
	if err != nil {
		return nil, err
	}

	// load the configuration and return on non-empty errors and if the
	// content differs from the default config
	mergedConfig, err := mergedClientConfig.ClientConfig()
	switch {
	case err != nil:
		if !IsEmptyConfig(err) {
			// return on any error except empty config
			return nil, err
		}
	case mergedConfig != nil:
		// the configuration is valid, but if this is equal to the defaults we should try
		// in-cluster configuration
		if !config.loader.IsDefaultConfig(mergedConfig) {
			return mergedConfig, nil
		}
	}

	// check for in-cluster configuration and use it
	if config.icc.Possible() {
		glog.V(4).Infof("Using in-cluster configuration")
		return config.icc.ClientConfig()
	}

	// return the result of the merged client config
	return mergedConfig, err
}

// Namespace implements KubeConfig
func (config *DeferredLoadingClientConfig) Namespace() (string, bool, error) {
	mergedKubeConfig, err := config.createClientConfig()
	if err != nil {
		return "", false, err
	}

	ns, ok, err := mergedKubeConfig.Namespace()
	// if we get an error and it is not empty config, or if the merged config defined an explicit namespace, or
	// if in-cluster config is not possible, return immediately
	if (err != nil && !IsEmptyConfig(err)) || ok || !config.icc.Possible() {
		// return on any error except empty config
		return ns, ok, err
	}

	glog.V(4).Infof("Using in-cluster namespace")

	// allow the namespace from the service account token directory to be used.
	return config.icc.Namespace()
}

// ConfigAccess implements ClientConfig
func (config *DeferredLoadingClientConfig) ConfigAccess() ConfigAccess {
	return config.loader
}
