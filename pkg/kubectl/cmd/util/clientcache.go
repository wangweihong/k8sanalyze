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

package util

import (
	"sync"

	fedclientset "k8s.io/kubernetes/federation/client/clientset_generated/federation_internalclientset"
	"k8s.io/kubernetes/pkg/apimachinery/registered"
	"k8s.io/kubernetes/pkg/client/clientset_generated/internalclientset"
	"k8s.io/kubernetes/pkg/client/restclient"
	"k8s.io/kubernetes/pkg/client/typed/discovery"
	oldclient "k8s.io/kubernetes/pkg/client/unversioned"
	"k8s.io/kubernetes/pkg/client/unversioned/clientcmd"
	"k8s.io/kubernetes/pkg/runtime/schema"
)

//这个缓存的意义是什么?
//猜测,在不同的服务器版本中,有些资源的api组版本已经更改
//例如在1.5版本中Job资源还可通过clientset.Interface().BatchV2alpha1.*来操作资源
//但在1.7版本中,必须使用clientset.Interface().BatchV1来操作资源,否则会报resource not found.
func NewClientCache(loader clientcmd.ClientConfig, discoveryClientFactory DiscoveryClientFactory) *ClientCache {
	return &ClientCache{
		clientsets:             make(map[schema.GroupVersion]*internalclientset.Clientset), //不同api组版本的客户端
		configs:                make(map[schema.GroupVersion]*restclient.Config),           //不同api组版本的客户端配置
		fedClientSets:          make(map[schema.GroupVersion]fedclientset.Interface),       //不同api组版本的联邦客户端接口?
		loader:                 loader,                                                     //客户端配置文件处理
		discoveryClientFactory: discoveryClientFactory,
	}
}

// ClientCache caches previously loaded clients for reuse, and ensures MatchServerVersion
// is invoked only once
type ClientCache struct {
	loader        clientcmd.ClientConfig                               //
	clientsets    map[schema.GroupVersion]*internalclientset.Clientset //不同api组版本有不同的客户端接口?
	fedClientSets map[schema.GroupVersion]fedclientset.Interface
	configs       map[schema.GroupVersion]*restclient.Config

	matchVersion bool

	defaultConfigLock sync.Mutex
	defaultConfig     *restclient.Config
	// discoveryClientFactory comes as a factory method so that we can defer resolution until after
	// argument evaluation
	discoveryClientFactory DiscoveryClientFactory
	discoveryClient        discovery.DiscoveryInterface
}

// also looks up the discovery client.  We can't do this during init because the flags won't have been set
// because this is constructed pre-command execution before the command tree is even set up
func (c *ClientCache) getDefaultConfig() (restclient.Config, discovery.DiscoveryInterface, error) {
	c.defaultConfigLock.Lock()
	defer c.defaultConfigLock.Unlock()

	if c.defaultConfig != nil && c.discoveryClient != nil {
		return *c.defaultConfig, c.discoveryClient, nil
	}

	config, err := c.loader.ClientConfig()
	if err != nil {
		return restclient.Config{}, nil, err
	}
	discoveryClient, err := c.discoveryClientFactory.DiscoveryClient()
	if err != nil {
		return restclient.Config{}, nil, err
	}
	if c.matchVersion {
		if err := discovery.MatchesServerVersion(discoveryClient); err != nil {
			return restclient.Config{}, nil, err
		}
	}

	c.defaultConfig = config
	c.discoveryClient = discoveryClient
	return *c.defaultConfig, c.discoveryClient, nil
}

// ClientConfigForVersion returns the correct config for a server
func (c *ClientCache) ClientConfigForVersion(requiredVersion *schema.GroupVersion) (*restclient.Config, error) {
	// TODO: have a better config copy method
	config, discoveryClient, err := c.getDefaultConfig()
	if err != nil {
		return nil, err
	}
	if requiredVersion == nil && config.GroupVersion != nil {
		// if someone has set the values via flags, our config will have the groupVersion set
		// that means it is required.
		requiredVersion = config.GroupVersion
	}

	// required version may still be nil, since config.GroupVersion may have been nil.  Do the check
	// before looking up from the cache
	if requiredVersion != nil {
		if config, ok := c.configs[*requiredVersion]; ok {
			return config, nil
		}
	}

	negotiatedVersion, err := discovery.NegotiateVersion(discoveryClient, requiredVersion, registered.EnabledVersions())
	if err != nil {
		return nil, err
	}
	config.GroupVersion = negotiatedVersion

	// TODO this isn't what we want.  Each clientset should be setting defaults as it sees fit.
	oldclient.SetKubernetesDefaults(&config)

	if requiredVersion != nil {
		c.configs[*requiredVersion] = &config
	}

	// `version` does not necessarily equal `config.Version`.  However, we know that we call this method again with
	// `config.Version`, we should get the config we've just built.
	configCopy := config
	c.configs[*config.GroupVersion] = &configCopy

	return &config, nil
}

// ClientSetForVersion initializes or reuses a clientset for the specified version, or returns an
// error if that is not possible
func (c *ClientCache) ClientSetForVersion(requiredVersion *schema.GroupVersion) (*internalclientset.Clientset, error) {
	if requiredVersion != nil {
		if clientset, ok := c.clientsets[*requiredVersion]; ok {
			return clientset, nil
		}
	}
	config, err := c.ClientConfigForVersion(requiredVersion)
	if err != nil {
		return nil, err
	}

	clientset, err := internalclientset.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	c.clientsets[*config.GroupVersion] = clientset

	// `version` does not necessarily equal `config.Version`.  However, we know that if we call this method again with
	// `version`, we should get a client based on the same config we just found.  There's no guarantee that a client
	// is copiable, so create a new client and save it in the cache.
	if requiredVersion != nil {
		configCopy := *config
		clientset, err := internalclientset.NewForConfig(&configCopy)
		if err != nil {
			return nil, err
		}
		c.clientsets[*requiredVersion] = clientset
	}

	return clientset, nil
}

func (c *ClientCache) FederationClientSetForVersion(version *schema.GroupVersion) (fedclientset.Interface, error) {
	if version != nil {
		if clientSet, found := c.fedClientSets[*version]; found {
			return clientSet, nil
		}
	}
	config, err := c.ClientConfigForVersion(version)
	if err != nil {
		return nil, err
	}

	// TODO: support multi versions of client with clientset
	clientSet, err := fedclientset.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	c.fedClientSets[*config.GroupVersion] = clientSet

	if version != nil {
		configCopy := *config
		clientSet, err := fedclientset.NewForConfig(&configCopy)
		if err != nil {
			return nil, err
		}
		c.fedClientSets[*version] = clientSet
	}

	return clientSet, nil
}

func (c *ClientCache) FederationClientForVersion(version *schema.GroupVersion) (*restclient.RESTClient, error) {
	fedClientSet, err := c.FederationClientSetForVersion(version)
	if err != nil {
		return nil, err
	}
	return fedClientSet.Federation().RESTClient().(*restclient.RESTClient), nil
}
