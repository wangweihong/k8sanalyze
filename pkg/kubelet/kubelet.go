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

package kubelet

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang/glog"
	cadvisorapi "github.com/google/cadvisor/info/v1"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/resource"
	"k8s.io/kubernetes/pkg/api/v1"
	"k8s.io/kubernetes/pkg/apis/componentconfig"
	componentconfigv1alpha1 "k8s.io/kubernetes/pkg/apis/componentconfig/v1alpha1"
	"k8s.io/kubernetes/pkg/client/cache"
	clientset "k8s.io/kubernetes/pkg/client/clientset_generated/clientset"
	"k8s.io/kubernetes/pkg/client/record"
	"k8s.io/kubernetes/pkg/cloudprovider"
	"k8s.io/kubernetes/pkg/fields"
	internalapi "k8s.io/kubernetes/pkg/kubelet/api"
	"k8s.io/kubernetes/pkg/kubelet/cadvisor"
	"k8s.io/kubernetes/pkg/kubelet/cm"
	"k8s.io/kubernetes/pkg/kubelet/config"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/dockershim"
	dockerremote "k8s.io/kubernetes/pkg/kubelet/dockershim/remote"
	"k8s.io/kubernetes/pkg/kubelet/dockertools"
	"k8s.io/kubernetes/pkg/kubelet/events"
	"k8s.io/kubernetes/pkg/kubelet/eviction"
	"k8s.io/kubernetes/pkg/kubelet/images"
	"k8s.io/kubernetes/pkg/kubelet/kuberuntime"
	"k8s.io/kubernetes/pkg/kubelet/lifecycle"
	"k8s.io/kubernetes/pkg/kubelet/metrics"
	"k8s.io/kubernetes/pkg/kubelet/network"
	"k8s.io/kubernetes/pkg/kubelet/pleg"
	kubepod "k8s.io/kubernetes/pkg/kubelet/pod"
	"k8s.io/kubernetes/pkg/kubelet/prober"
	proberesults "k8s.io/kubernetes/pkg/kubelet/prober/results"
	"k8s.io/kubernetes/pkg/kubelet/remote"
	"k8s.io/kubernetes/pkg/kubelet/rkt"
	"k8s.io/kubernetes/pkg/kubelet/server"
	"k8s.io/kubernetes/pkg/kubelet/server/stats"
	"k8s.io/kubernetes/pkg/kubelet/server/streaming"
	"k8s.io/kubernetes/pkg/kubelet/status"
	"k8s.io/kubernetes/pkg/kubelet/sysctl"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
	"k8s.io/kubernetes/pkg/kubelet/util/queue"
	"k8s.io/kubernetes/pkg/kubelet/util/sliceutils"
	"k8s.io/kubernetes/pkg/kubelet/volumemanager"
	"k8s.io/kubernetes/pkg/labels"
	"k8s.io/kubernetes/pkg/security/apparmor"
	"k8s.io/kubernetes/pkg/types"
	"k8s.io/kubernetes/pkg/util/bandwidth"
	"k8s.io/kubernetes/pkg/util/clock"
	utilconfig "k8s.io/kubernetes/pkg/util/config"
	utildbus "k8s.io/kubernetes/pkg/util/dbus"
	utilexec "k8s.io/kubernetes/pkg/util/exec"
	"k8s.io/kubernetes/pkg/util/flowcontrol"
	"k8s.io/kubernetes/pkg/util/integer"
	kubeio "k8s.io/kubernetes/pkg/util/io"
	utilipt "k8s.io/kubernetes/pkg/util/iptables"
	"k8s.io/kubernetes/pkg/util/mount"
	nodeutil "k8s.io/kubernetes/pkg/util/node"
	"k8s.io/kubernetes/pkg/util/oom"
	"k8s.io/kubernetes/pkg/util/procfs"
	utilruntime "k8s.io/kubernetes/pkg/util/runtime"
	"k8s.io/kubernetes/pkg/util/sets"
	"k8s.io/kubernetes/pkg/util/wait"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/plugin/pkg/scheduler/algorithm/predicates"
)

const (
	// Max amount of time to wait for the container runtime to come up.
	maxWaitForContainerRuntime = 30 * time.Second

	// nodeStatusUpdateRetry specifies how many times kubelet retries when posting node status failed.
	nodeStatusUpdateRetry = 5

	// Location of container logs.
	ContainerLogsDir = "/var/log/containers"

	// max backoff period, exported for the e2e test
	MaxContainerBackOff = 300 * time.Second

	// Capacity of the channel for storing pods to kill. A small number should
	// suffice because a goroutine is dedicated to check the channel and does
	// not block on anything else.
	podKillingChannelCapacity = 50

	// Period for performing global cleanup tasks.
	housekeepingPeriod = time.Second * 2

	// Period for performing eviction monitoring.
	// TODO ensure this is in sync with internal cadvisor housekeeping.
	evictionMonitoringPeriod = time.Second * 10

	// The path in containers' filesystems where the hosts file is mounted.
	etcHostsPath = "/etc/hosts"

	// Capacity of the channel for receiving pod lifecycle events. This number
	// is a bit arbitrary and may be adjusted in the future.
	plegChannelCapacity = 1000

	// Generic PLEG relies on relisting for discovering container events.
	// A longer period means that kubelet will take longer to detect container
	// changes and to update pod status. On the other hand, a shorter period
	// will cause more frequent relisting (e.g., container runtime operations),
	// leading to higher cpu usage.
	// Note that even though we set the period to 1s, the relisting itself can
	// take more than 1s to finish if the container runtime responds slowly
	// and/or when there are many container changes in one cycle.
	plegRelistPeriod = time.Second * 1 //pleg relist周期

	// backOffPeriod is the period to back off when pod syncing results in an
	// error. It is also used as the base period for the exponential backoff
	// container restarts and image pulls.
	backOffPeriod = time.Second * 10 //10秒重试周期

	// Period for performing container garbage collection.
	ContainerGCPeriod = time.Minute
	// Period for performing image garbage collection.
	ImageGCPeriod = 5 * time.Minute

	// Minimum number of dead containers to keep in a pod
	minDeadContainerInPod = 1
)

// SyncHandler is an interface implemented by Kubelet, for testability
//只被kkubelet实现
type SyncHandler interface {
	HandlePodAdditions(pods []*v1.Pod)
	HandlePodUpdates(pods []*v1.Pod)
	HandlePodRemoves(pods []*v1.Pod)
	HandlePodReconcile(pods []*v1.Pod)
	HandlePodSyncs(pods []*v1.Pod)
	HandlePodCleanups() error //kubelet_pod.go中实现
}

// Option is a functional option type for Kubelet
type Option func(*Kubelet)

// bootstrapping interface for kubelet, targets the initialization protocol
//kubelet如何进行自举?这个接口负责的是启动kubelet?
//这是kubelet对象实现的接口
type KubeletBootstrap interface {
	GetConfiguration() componentconfig.KubeletConfiguration
	BirthCry()
	StartGarbageCollection()
	ListenAndServe(address net.IP, port uint, tlsOptions *server.TLSOptions, auth server.AuthInterface, enableDebuggingHandlers bool)
	ListenAndServeReadOnly(address net.IP, port uint)
	Run(<-chan kubetypes.PodUpdate)
	RunOnce(<-chan kubetypes.PodUpdate) ([]RunPodResult, error) ///??kublet在哪里实现了这个方法?在当前包的runonce.go中实现了.
}

// create and initialize a Kubelet instance
//返回一个Interface??这个函数是构建了kubelet对象,kubelet对象实现了这个KubeletBootstrap接口
type KubeletBuilder func(kubeCfg *componentconfig.KubeletConfiguration, kubeDeps *KubeletDeps, standaloneMode bool) (KubeletBootstrap, error)

// KubeletDeps is a bin for things we might consider "injected注入的 dependencies" -- objects constructed
// at runtime that are necessary for running the Kubelet. This is a temporary solution for grouping
// these objects while we figure out a more comprehensive dependency injection story for the Kubelet.
//kubelet依赖的其他组件?这个是什么情况下如何使用? kubelet二进制程序默认该项为nil.但后面检测到为nil时,通过cmd/kubelet/app/server.go中额UnsecuredKubeletDeps创建了新的kubedeps对象
type KubeletDeps struct {
	// TODO(mtaufen): KubeletBuilder:
	//                Mesos currently uses this as a hook to let them make their own call to
	//                let them wrap the KubeletBootstrap that CreateAndInitKubelet returns with
	//                their own KubeletBootstrap. It's a useful hook. I need to think about what
	//                a nice home for it would be. There seems to be a trend, between this and
	//                the Options fields below, of providing hooks where you can add extra functionality
	//                to the Kubelet for your solution. Maybe we should centralize these sorts of things?
	Builder KubeletBuilder //描述如何创建Kubelet对象,如果没有指定,则会使用CreateAndInitKubelet来构建Kubelet对象

	// TODO(mtaufen): ContainerRuntimeOptions and Options:
	//                Arrays of functions that can do arbitrary things to the Kubelet and the Runtime
	//                seem like a difficult path to trace when it's time to debug something.
	//                I'm leaving these fields here for now, but there is likely an easier-to-follow
	//                way to support their intended use cases. E.g. ContainerRuntimeOptions
	//                is used by Mesos to set an environment variable in containers which has
	//                some connection to their container GC. It seems that Mesos intends to use
	//                Options to add additional node conditions that are updated as part of the
	//                Kubelet lifecycle (see https://github.com/kubernetes/kubernetes/pull/21521).
	//                We should think about providing more explicit ways of doing these things.
	ContainerRuntimeOptions []kubecontainer.Option
	Options                 []Option

	// Injected Dependencies
	Auth              server.AuthInterface
	CAdvisorInterface cadvisor.Interface
	Cloud             cloudprovider.Interface
	ContainerManager  cm.ContainerManager
	DockerClient      dockertools.DockerInterface
	EventClient       *clientset.Clientset
	//k8s的客户端
	KubeClient     *clientset.Clientset
	Mounter        mount.Interface
	NetworkPlugins []network.NetworkPlugin
	OOMAdjuster    *oom.OOMAdjuster
	OSInterface    kubecontainer.OSInterface
	PodConfig      *config.PodConfig    //这个和pod的删除/创建的请求有关,这是在哪里初始化的?unsecureKubeletDeps并没有创建该项
	Recorder       record.EventRecorder ///事件记录器
	Writer         kubeio.Writer
	VolumePlugins  []volume.VolumePlugin
	TLSOptions     *server.TLSOptions
}

// makePodSourceConfig creates a config.PodConfig from the given
// KubeletConfiguration or returns an error.
//这个Pod配置做什么用的?
//用于创建static pod使用,static pod 创建源
func makePodSourceConfig(kubeCfg *componentconfig.KubeletConfiguration, kubeDeps *KubeletDeps, nodeName types.NodeName) (*config.PodConfig, error) {
	manifestURLHeader := make(http.Header)
	if kubeCfg.ManifestURLHeader != "" {
		pieces := strings.Split(kubeCfg.ManifestURLHeader, ":")
		if len(pieces) != 2 {
			return nil, fmt.Errorf("manifest-url-header must have a single ':' key-value separator, got %q", kubeCfg.ManifestURLHeader)
		}
		manifestURLHeader.Set(pieces[0], pieces[1])
	}

	// source of all configuration
	cfg := config.NewPodConfig(config.PodConfigNotificationIncremental, kubeDeps.Recorder)

	// define file config source
	//static pod的config file创建源
	if kubeCfg.PodManifestPath != "" {
		glog.Infof("Adding manifest file: %v", kubeCfg.PodManifestPath)
		config.NewSourceFile(kubeCfg.PodManifestPath, nodeName, kubeCfg.FileCheckFrequency.Duration, cfg.Channel(kubetypes.FileSource))
	}

	// define url config source
	//static Pod的pod的http创建源
	if kubeCfg.ManifestURL != "" {
		glog.Infof("Adding manifest url %q with HTTP header %v", kubeCfg.ManifestURL, manifestURLHeader)
		config.NewSourceURL(kubeCfg.ManifestURL, manifestURLHeader, nodeName, kubeCfg.HTTPCheckFrequency.Duration, cfg.Channel(kubetypes.HTTPSource))
	}
	if kubeDeps.KubeClient != nil {
		glog.Infof("Watching apiserver")
		config.NewSourceApiserver(kubeDeps.KubeClient, nodeName, cfg.Channel(kubetypes.ApiserverSource))
	}
	return cfg, nil
}

func getRuntimeAndImageServices(config *componentconfig.KubeletConfiguration) (internalapi.RuntimeService, internalapi.ImageManagerService, error) {
	//远程镜像服务?
	rs, err := remote.NewRemoteRuntimeService(config.RemoteRuntimeEndpoint, config.RuntimeRequestTimeout.Duration)
	if err != nil {
		return nil, nil, err
	}
	is, err := remote.NewRemoteImageService(config.RemoteImageEndpoint, config.RuntimeRequestTimeout.Duration)
	if err != nil {
		return nil, nil, err
	}
	return rs, is, err
}

// NewMainKubelet instantiates a new Kubelet object along with all the required internal modules.
// No initialization of Kubelet and its modules should happen here.
func NewMainKubelet(kubeCfg *componentconfig.KubeletConfiguration, kubeDeps *KubeletDeps, standaloneMode bool) (*Kubelet, error) {
	if kubeCfg.RootDirectory == "" {
		return nil, fmt.Errorf("invalid root directory %q", kubeCfg.RootDirectory)
	}
	if kubeCfg.SyncFrequency.Duration <= 0 {
		return nil, fmt.Errorf("invalid sync frequency %d", kubeCfg.SyncFrequency.Duration)
	}

	if kubeCfg.MakeIPTablesUtilChains {
		if kubeCfg.IPTablesMasqueradeBit > 31 || kubeCfg.IPTablesMasqueradeBit < 0 {
			return nil, fmt.Errorf("iptables-masquerade-bit is not valid. Must be within [0, 31]")
		}
		if kubeCfg.IPTablesDropBit > 31 || kubeCfg.IPTablesDropBit < 0 {
			return nil, fmt.Errorf("iptables-drop-bit is not valid. Must be within [0, 31]")
		}
		if kubeCfg.IPTablesDropBit == kubeCfg.IPTablesMasqueradeBit {
			return nil, fmt.Errorf("iptables-masquerade-bit and iptables-drop-bit must be different")
		}
	}

	//通过uname -n来获取主机名
	hostname := nodeutil.GetHostname(kubeCfg.HostnameOverride)
	// Query the cloud provider for our node name, default to hostname
	nodeName := types.NodeName(hostname)
	//这个云实例有什么作用?
	if kubeDeps.Cloud != nil {
		var err error
		instances, ok := kubeDeps.Cloud.Instances()
		if !ok {
			return nil, fmt.Errorf("failed to get instances from cloud provider")
		}

		nodeName, err = instances.CurrentNodeName(hostname)
		if err != nil {
			return nil, fmt.Errorf("error fetching current instance name from cloud provider: %v", err)
		}

		glog.V(2).Infof("cloud provider determined current node name to be %s", nodeName)
	}

	// TODO: KubeletDeps.KubeClient should be a client interface, but client interface misses certain methods
	// used by kubelet. Since NewMainKubelet expects a client interface, we need to make sure we are not passing
	// a nil pointer to it when what we really want is a nil interface.
	//这个kubeClient用于和k8s server进行通信
	var kubeClient clientset.Interface
	if kubeDeps.KubeClient != nil {
		kubeClient = kubeDeps.KubeClient
		// TODO: remove this when we've refactored kubelet to only use clientset.
	}

	//??这个PodConfig有什么作用? static pod创建源
	if kubeDeps.PodConfig == nil {
		var err error
		kubeDeps.PodConfig, err = makePodSourceConfig(kubeCfg, kubeDeps, nodeName)
		if err != nil {
			return nil, err
		}
	}

	//用于管理容器如何进行回收
	containerGCPolicy := kubecontainer.ContainerGCPolicy{
		MinAge:             kubeCfg.MinimumGCAge.Duration,
		MaxPerPodContainer: int(kubeCfg.MaxPerPodContainerCount),
		MaxContainers:      int(kubeCfg.MaxContainerCount),
	}

	//kubelet的服务端口?
	daemonEndpoints := &v1.NodeDaemonEndpoints{
		KubeletEndpoint: v1.DaemonEndpoint{Port: kubeCfg.Port},
	}

	//容器镜像回收策略
	imageGCPolicy := images.ImageGCPolicy{
		MinAge:               kubeCfg.ImageMinimumGCAge.Duration,
		HighThresholdPercent: int(kubeCfg.ImageGCHighThresholdPercent),
		LowThresholdPercent:  int(kubeCfg.ImageGCLowThresholdPercent),
	}

	//指定硬盘空间策略
	diskSpacePolicy := DiskSpacePolicy{
		DockerFreeDiskMB: int(kubeCfg.LowDiskSpaceThresholdMB),
		RootFreeDiskMB:   int(kubeCfg.LowDiskSpaceThresholdMB),
	}

	//驱逐pod资源,下面的EvictHard/EvicionSoft..等参数都有kubelet的启动参数提供
	thresholds, err := eviction.ParseThresholdConfig(kubeCfg.EvictionHard, kubeCfg.EvictionSoft, kubeCfg.EvictionSoftGracePeriod, kubeCfg.EvictionMinimumReclaim)
	if err != nil {
		return nil, err
	}
	//驱逐配置
	evictionConfig := eviction.Config{
		PressureTransitionPeriod: kubeCfg.EvictionPressureTransitionPeriod.Duration, //由kubelet的启动参数--eviction-pressure-transition-period 提供
		MaxPodGracePeriodSeconds: int64(kubeCfg.EvictionMaxPodGracePeriod),          //有kubelet的启动参数--eviction-max-pod-grace-period提供
		Thresholds:               thresholds,
		KernelMemcgNotification:  kubeCfg.ExperimentalKernelMemcgNotification, //由kubelet的启动参数--experimental-kernel-memcg-notification提供
	}

	//用于记录kubelet和非kubelet组件保留的资源数量,达到了资源上限,将会做什么样的处理?
	reservation, err := ParseReservation(kubeCfg.KubeReserved, kubeCfg.SystemReserved)
	if err != nil {
		return nil, err
	}

	//指定容器处理时的控制器,CROI/docker/rkt,所作的处理都不一样
	var dockerExecHandler dockertools.ExecHandler
	switch kubeCfg.DockerExecHandlerName {
	case "native":
		dockerExecHandler = &dockertools.NativeExecHandler{}
	case "nsenter":
		dockerExecHandler = &dockertools.NsenterExecHandler{}
	default:
		glog.Warningf("Unknown Docker exec handler %q; defaulting to native", kubeCfg.DockerExecHandlerName)
		dockerExecHandler = &dockertools.NativeExecHandler{}
	}

	//??服务存储?访问apiserverz中的services资源
	//如果kubeClient为空?那么是那么是如何获取service资源的?
	//如果没有配置apiserver参数(standalone模式?),或者创建k8sclient失败时,才会导致kubeClient为nil
	serviceStore := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	if kubeClient != nil {
		serviceLW := cache.NewListWatchFromClient(kubeClient.Core().RESTClient(), "services", v1.NamespaceAll, fields.Everything())
		//创建一个反射器,一但监控的资源改变,立即更改当前的数据
		cache.NewReflector(serviceLW, &v1.Service{}, serviceStore, 0).Run()
	}
	serviceLister := &cache.StoreToServiceLister{Indexer: serviceStore}

	//获取当前节点在apiserver中节点信息
	nodeStore := cache.NewStore(cache.MetaNamespaceKeyFunc)
	if kubeClient != nil {
		fieldSelector := fields.Set{api.ObjectNameField: string(nodeName)}.AsSelector()
		nodeLW := cache.NewListWatchFromClient(kubeClient.Core().RESTClient(), "nodes", v1.NamespaceAll, fieldSelector)
		cache.NewReflector(nodeLW, &v1.Node{}, nodeStore, 0).Run()
	}
	nodeLister := &cache.StoreToNodeLister{Store: nodeStore}
	nodeInfo := &predicates.CachedNodeInfo{StoreToNodeLister: nodeLister}

	// TODO: get the real node object of ourself,
	// and use the real node name and UID.
	// TODO: what is namespace for node?
	//??这里的作用?节点有命名空间?
	nodeRef := &v1.ObjectReference{
		Kind:      "Node",
		Name:      string(nodeName),
		UID:       types.UID(nodeName),
		Namespace: "",
	}

	//用于管理硬盘空间,
	diskSpaceManager, err := newDiskSpaceManager(kubeDeps.CAdvisorInterface, diskSpacePolicy)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize disk manager: %v", err)
	}
	//管理容器的引用计数,这个引用计数有什么作用?会对Pod造成什么样的影响
	containerRefManager := kubecontainer.NewRefManager()

	//监控系统 OOM
	oomWatcher := NewOOMWatcher(kubeDeps.CAdvisorInterface, kubeDeps.Recorder)

	klet := &Kubelet{
		hostname:            hostname,
		nodeName:            nodeName,
		dockerClient:        kubeDeps.DockerClient,
		kubeClient:          kubeClient,
		rootDirectory:       kubeCfg.RootDirectory,
		resyncInterval:      kubeCfg.SyncFrequency.Duration,
		containerRefManager: containerRefManager,
		httpClient:          &http.Client{},
		sourcesReady:        config.NewSourcesReady(kubeDeps.PodConfig.SeenAllSources),
		registerNode:        kubeCfg.RegisterNode,
		registerSchedulable: kubeCfg.RegisterSchedulable,
		standaloneMode:      standaloneMode,
		clusterDomain:       kubeCfg.ClusterDomain,
		//这个DNS?
		clusterDNS:                     net.ParseIP(kubeCfg.ClusterDNS),
		serviceLister:                  serviceLister,
		nodeLister:                     nodeLister,
		nodeInfo:                       nodeInfo,
		masterServiceNamespace:         kubeCfg.MasterServiceNamespace,
		streamingConnectionIdleTimeout: kubeCfg.StreamingConnectionIdleTimeout.Duration,
		recorder:                       kubeDeps.Recorder,
		cadvisor:                       kubeDeps.CAdvisorInterface,
		diskSpaceManager:               diskSpaceManager,
		cloud:                          kubeDeps.Cloud,
		autoDetectCloudProvider:   (componentconfigv1alpha1.AutoDetectCloudProvider == kubeCfg.CloudProvider),
		nodeRef:                   nodeRef, //当前节点在apiserver数据?
		nodeLabels:                kubeCfg.NodeLabels,
		nodeStatusUpdateFrequency: kubeCfg.NodeStatusUpdateFrequency.Duration,
		os:                kubeDeps.OSInterface,
		oomWatcher:        oomWatcher,
		cgroupsPerQOS:     kubeCfg.ExperimentalCgroupsPerQOS,
		cgroupRoot:        kubeCfg.CgroupRoot,
		mounter:           kubeDeps.Mounter,
		writer:            kubeDeps.Writer,
		nonMasqueradeCIDR: kubeCfg.NonMasqueradeCIDR,
		maxPods:           int(kubeCfg.MaxPods),
		podsPerCore:       int(kubeCfg.PodsPerCore),
		nvidiaGPUs:        int(kubeCfg.NvidiaGPUs),
		syncLoopMonitor:   atomic.Value{},
		resolverConfig:    kubeCfg.ResolverConfig,
		cpuCFSQuota:       kubeCfg.CPUCFSQuota,
		daemonEndpoints:   daemonEndpoints,
		containerManager:  kubeDeps.ContainerManager,
		nodeIP:            net.ParseIP(kubeCfg.NodeIP),
		clock:             clock.RealClock{},
		outOfDiskTransitionFrequency:            kubeCfg.OutOfDiskTransitionFrequency.Duration,
		reservation:                             *reservation,
		enableCustomMetrics:                     kubeCfg.EnableCustomMetrics,
		babysitDaemons:                          kubeCfg.BabysitDaemons,
		enableControllerAttachDetach:            kubeCfg.EnableControllerAttachDetach,
		iptClient:                               utilipt.New(utilexec.New(), utildbus.New(), utilipt.ProtocolIpv4),
		makeIPTablesUtilChains:                  kubeCfg.MakeIPTablesUtilChains,
		iptablesMasqueradeBit:                   int(kubeCfg.IPTablesMasqueradeBit),
		iptablesDropBit:                         int(kubeCfg.IPTablesDropBit),
		experimentalHostUserNamespaceDefaulting: utilconfig.DefaultFeatureGate.ExperimentalHostUserNamespaceDefaulting(),
	}

	if klet.experimentalHostUserNamespaceDefaulting {
		glog.Infof("Experimental host user namespace defaulting is enabled.")
	}

	//??发夹模式?
	if mode, err := effectiveHairpinMode(componentconfig.HairpinMode(kubeCfg.HairpinMode), kubeCfg.ContainerRuntime, kubeCfg.NetworkPluginName); err != nil {
		// This is a non-recoverable error. Returning it up the callstack will just
		// lead to retries of the same failure, so just fail hard.
		glog.Fatalf("Invalid hairpin mode: %v", err)
	} else {
		klet.hairpinMode = mode
	}
	glog.Infof("Hairpin mode set to %q", klet.hairpinMode)

	//初始化网络插件
	if plug, err := network.InitNetworkPlugin(kubeDeps.NetworkPlugins, kubeCfg.NetworkPluginName, &criNetworkHost{&networkHost{klet}}, klet.hairpinMode, klet.nonMasqueradeCIDR, int(kubeCfg.NetworkPluginMTU)); err != nil {
		return nil, err
	} else {
		klet.networkPlugin = plug
	}

	//通过cadvisor获取当前的机器信息
	machineInfo, err := klet.GetCachedMachineInfo()
	if err != nil {
		return nil, err
	}

	//提供通过/proc中cgroup获取容器名的功能
	procFs := procfs.NewProcFS()
	//这是一组包含多个时间段的对象
	imageBackOff := flowcontrol.NewBackOff(backOffPeriod, MaxContainerBackOff)

	//Pod的liveness容器探测结果管理器
	klet.livenessManager = proberesults.NewManager()

	//这里保存这Pod相关的状态信息
	klet.podCache = kubecontainer.NewCache()
	//管理Pod ,提供一个Kube client用于管理mirror pod
	klet.podManager = kubepod.NewBasicPodManager(kubepod.NewBasicMirrorClient(klet.kubeClient))

	//???
	if kubeCfg.RemoteRuntimeEndpoint != "" {
		// kubeCfg.RemoteImageEndpoint is same as kubeCfg.RemoteRuntimeEndpoint if not explicitly specified
		if kubeCfg.RemoteImageEndpoint == "" {
			kubeCfg.RemoteImageEndpoint = kubeCfg.RemoteRuntimeEndpoint
		}
	}

	// TODO: These need to become arguments to a standalone docker shim.
	binDir := kubeCfg.CNIBinDir
	if binDir == "" {
		binDir = kubeCfg.NetworkPluginDir
	}
	//和Cni插件有关
	pluginSettings := dockershim.NetworkPluginSettings{
		HairpinMode:       klet.hairpinMode,
		NonMasqueradeCIDR: klet.nonMasqueradeCIDR,
		PluginName:        kubeCfg.NetworkPluginName,
		PluginConfDir:     kubeCfg.CNIConfDir,
		PluginBinDir:      binDir,
		MTU:               int(kubeCfg.NetworkPluginMTU),
	}

	// Remote runtime shim just cannot talk back to kubelet, so it doesn't
	// support bandwidth shaping or hostports till #35457. To enable legacy
	// features, replace with networkHost.
	var nl *noOpLegacyHost
	pluginSettings.LegacyRuntimeHost = nl

	//??容器运行时接口,这个CRI怎么配置?k8s还没有默认使用CRI
	if kubeCfg.EnableCRI {
		// kubelet defers to the runtime shim to setup networking. Setting
		// this to nil will prevent it from trying to invoke the plugin.
		// It's easier to always probe and initialize plugins till cri
		// becomes the default.
		klet.networkPlugin = nil

		//运行时容器,沙盒管理器
		var runtimeService internalapi.RuntimeService
		//运行时镜像管理器,kubelet的镜像下载通过该接口来实现
		var imageService internalapi.ImageManagerService
		var err error

		switch kubeCfg.ContainerRuntime {
		case "docker":
			streamingConfig := getStreamingConfig(kubeCfg, kubeDeps)
			// Use the new CRI shim for docker.
			ds, err := dockershim.NewDockerService(klet.dockerClient, kubeCfg.SeccompProfileRoot, kubeCfg.PodInfraContainerImage,
				streamingConfig, &pluginSettings, kubeCfg.RuntimeCgroups, kubeCfg.CgroupDriver)
			if err != nil {
				return nil, err
			}
			// TODO: Once we switch to grpc completely, we should move this
			// call to the grpc server start.
			if err := ds.Start(); err != nil {
				return nil, err
			}

			klet.criHandler = ds
			rs := ds.(internalapi.RuntimeService)
			is := ds.(internalapi.ImageManagerService)
			// This is an internal knob to switch between grpc and non-grpc
			// integration.
			// TODO: Remove this knob once we switch to using GRPC completely.
			overGRPC := true
			if overGRPC {
				const (
					// The unix socket for kubelet <-> dockershim communication.
					ep = "/var/run/dockershim.sock"
				)
				kubeCfg.RemoteRuntimeEndpoint = ep
				kubeCfg.RemoteImageEndpoint = ep

				server := dockerremote.NewDockerServer(ep, ds)
				glog.V(2).Infof("Starting the GRPC server for the docker CRI shim.")
				err := server.Start()
				if err != nil {
					return nil, err
				}
				rs, is, err = getRuntimeAndImageServices(kubeCfg)
				if err != nil {
					return nil, err
				}
			}
			// TODO: Move the instrumented interface wrapping into kuberuntime.
			runtimeService = kuberuntime.NewInstrumentedRuntimeService(rs)
			imageService = is
		case "remote":
			runtimeService, imageService, err = getRuntimeAndImageServices(kubeCfg)
			if err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("unsupported CRI runtime: %q", kubeCfg.ContainerRuntime)
		}
		runtime, err := kuberuntime.NewKubeGenericRuntimeManager(
			kubecontainer.FilterEventRecorder(kubeDeps.Recorder),
			klet.livenessManager,
			containerRefManager,
			machineInfo,
			klet.podManager,
			kubeDeps.OSInterface,
			klet.networkPlugin,
			klet,
			klet.httpClient,
			imageBackOff,
			kubeCfg.SerializeImagePulls,
			float32(kubeCfg.RegistryPullQPS),
			int(kubeCfg.RegistryBurst),
			klet.cpuCFSQuota,
			runtimeService,
			kuberuntime.NewInstrumentedImageManagerService(imageService),
		)
		if err != nil {
			return nil, err
		}
		klet.containerRuntime = runtime
		klet.runner = runtime
	} else {
		switch kubeCfg.ContainerRuntime {
		case "docker":
			runtime := dockertools.NewDockerManager(
				kubeDeps.DockerClient,
				kubecontainer.FilterEventRecorder(kubeDeps.Recorder),
				klet.livenessManager,
				containerRefManager,
				klet.podManager,
				machineInfo,
				kubeCfg.PodInfraContainerImage,
				float32(kubeCfg.RegistryPullQPS),
				int(kubeCfg.RegistryBurst),
				ContainerLogsDir,
				kubeDeps.OSInterface,
				klet.networkPlugin,
				klet,
				klet.httpClient,
				dockerExecHandler,
				kubeDeps.OOMAdjuster,
				procFs,
				klet.cpuCFSQuota,
				imageBackOff,
				kubeCfg.SerializeImagePulls,
				kubeCfg.EnableCustomMetrics,
				// If using "kubenet", the Kubernetes network plugin that wraps
				// CNI's bridge plugin, it knows how to set the hairpin veth flag
				// so we tell the container runtime to back away from setting it.
				// If the kubelet is started with any other plugin we can't be
				// sure it handles the hairpin case so we instruct the docker
				// runtime to set the flag instead.
				klet.hairpinMode == componentconfig.HairpinVeth && kubeCfg.NetworkPluginName != "kubenet",
				kubeCfg.SeccompProfileRoot,
				kubeDeps.ContainerRuntimeOptions...,
			)
			klet.containerRuntime = runtime
			klet.runner = kubecontainer.DirectStreamingRunner(runtime)
			//			unner(runtime)
		case "rkt":
			// TODO: Include hairpin mode settings in rkt?
			conf := &rkt.Config{
				Path:            kubeCfg.RktPath,
				Stage1Image:     kubeCfg.RktStage1Image,
				InsecureOptions: "image,ondisk",
			}
			runtime, err := rkt.New(
				kubeCfg.RktAPIEndpoint,
				conf,
				klet,
				kubeDeps.Recorder,
				containerRefManager,
				klet.podManager,
				klet.livenessManager,
				klet.httpClient,
				klet.networkPlugin,
				klet.hairpinMode == componentconfig.HairpinVeth,
				utilexec.New(),
				kubecontainer.RealOS{},
				imageBackOff,
				kubeCfg.SerializeImagePulls,
				float32(kubeCfg.RegistryPullQPS),
				int(kubeCfg.RegistryBurst),
				kubeCfg.RuntimeRequestTimeout.Duration,
			)
			if err != nil {
				return nil, err
			}
			klet.containerRuntime = runtime
			klet.runner = kubecontainer.DirectStreamingRunner(runtime)
		default:
			return nil, fmt.Errorf("unsupported container runtime %q specified", kubeCfg.ContainerRuntime)
		}
	}

	// TODO: Factor out "StatsProvider" from Kubelet so we don't have a cyclic dependency
	//资源分析器?这个分析器的作用?
	klet.resourceAnalyzer = stats.NewResourceAnalyzer(klet, kubeCfg.VolumeStatsAggPeriod.Duration, klet.containerRuntime)

	//pod生命周期时间产生器
	klet.pleg = pleg.NewGenericPLEG(klet.containerRuntime, plegChannelCapacity, plegRelistPeriod, klet.podCache, clock.RealClock{})
	//创建运行状态管理器
	klet.runtimeState = newRuntimeState(maxWaitForContainerRuntime) //默认为30秒
	//更新运行kubelet的cidr地址
	klet.updatePodCIDR(kubeCfg.PodCIDR)

	// setup containerGC
	containerGC, err := kubecontainer.NewContainerGC(klet.containerRuntime, containerGCPolicy)
	if err != nil {
		return nil, err
	}
	//设置容器的回收
	klet.containerGC = containerGC
	klet.containerDeletor = newPodContainerDeletor(klet.containerRuntime, integer.IntMax(containerGCPolicy.MaxPerPodContainer, minDeadContainerInPod))

	// setup imageManager
	//用于回收镜像的管理器
	imageManager, err := images.NewImageGCManager(klet.containerRuntime, kubeDeps.CAdvisorInterface, kubeDeps.Recorder, nodeRef, imageGCPolicy)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize image manager: %v", err)
	}
	klet.imageManager = imageManager

	//用于管理Pod的状态?用于同步api server中pod的状态
	klet.statusManager = status.NewManager(kubeClient, klet.podManager)

	//liveness, readiess
	klet.probeManager = prober.NewManager(
		klet.statusManager,
		klet.livenessManager,
		klet.runner,
		containerRefManager,
		kubeDeps.Recorder)

	klet.volumePluginMgr, err =
		NewInitializedVolumePluginMgr(klet, kubeDeps.VolumePlugins)
	if err != nil {
		return nil, err
	}

	// If the experimentalMounterPathFlag is set, we do not want to
	// check node capabilities since the mount path is not the default
	if len(kubeCfg.ExperimentalMounterPath) != 0 {
		kubeCfg.ExperimentalCheckNodeCapabilitiesBeforeMount = false
	}
	// setup volumeManager
	klet.volumeManager, err = volumemanager.NewVolumeManager(
		kubeCfg.EnableControllerAttachDetach,
		nodeName,
		klet.podManager,
		klet.kubeClient,
		klet.volumePluginMgr,
		klet.containerRuntime,
		kubeDeps.Mounter,
		klet.getPodsDir(),
		kubeDeps.Recorder,
		kubeCfg.ExperimentalCheckNodeCapabilitiesBeforeMount)

	//设置运行时容器的缓存
	runtimeCache, err := kubecontainer.NewRuntimeCache(klet.containerRuntime)
	if err != nil {
		return nil, err
	}
	//pod_worker
	klet.runtimeCache = runtimeCache
	//用于缓存Pod sync中启动容器失败的错误和信息
	klet.reasonCache = NewReasonCache()
	klet.workQueue = queue.NewBasicWorkQueue(klet.clock)
	//syncPod,podWorker真正进行更新Pod的动作
	klet.podWorkers = newPodWorkers(klet.syncPod, kubeDeps.Recorder, klet.workQueue, klet.resyncInterval, backOffPeriod, klet.podCache)

	klet.backOff = flowcontrol.NewBackOff(backOffPeriod, MaxContainerBackOff)
	//将po加入工作队列,并处理未传递的更新请求;如果没有未处理的,则更新worker为空闲状态
	klet.podKillingCh = make(chan *kubecontainer.PodPair, podKillingChannelCapacity)
	klet.setNodeStatusFuncs = klet.defaultNodeStatusFuncs()

	// setup eviction manager
	//安装驱逐管理器
	evictionManager, evictionAdmitHandler, err := eviction.NewManager(klet.resourceAnalyzer, evictionConfig, killPodNow(klet.podWorkers, kubeDeps.Recorder), klet.imageManager, kubeDeps.Recorder, nodeRef, klet.clock)

	if err != nil {
		return nil, fmt.Errorf("failed to initialize eviction manager: %v", err)
	}
	//驱逐管理器
	klet.evictionManager = evictionManager
	klet.admitHandlers.AddPodAdmitHandler(evictionAdmitHandler)

	// add sysctl admission
	runtimeSupport, err := sysctl.NewRuntimeAdmitHandler(klet.containerRuntime)
	if err != nil {
		return nil, err
	}
	safeWhitelist, err := sysctl.NewWhitelist(sysctl.SafeSysctlWhitelist(), v1.SysctlsPodAnnotationKey)
	if err != nil {
		return nil, err
	}
	// Safe, whitelisted sysctls can always be used as unsafe sysctls in the spec
	// Hence, we concatenate those two lists.
	safeAndUnsafeSysctls := append(sysctl.SafeSysctlWhitelist(), kubeCfg.AllowedUnsafeSysctls...)
	unsafeWhitelist, err := sysctl.NewWhitelist(safeAndUnsafeSysctls, v1.UnsafeSysctlsPodAnnotationKey)
	if err != nil {
		return nil, err
	}
	klet.admitHandlers.AddPodAdmitHandler(runtimeSupport)
	klet.admitHandlers.AddPodAdmitHandler(safeWhitelist)
	klet.admitHandlers.AddPodAdmitHandler(unsafeWhitelist)

	// enable active deadline handler
	//注册pod 死亡期限控制器,设定deadline的容器在deadline到达后,将会移除其中的容器
	activeDeadlineHandler, err := newActiveDeadlineHandler(klet.statusManager, kubeDeps.Recorder, klet.clock)
	if err != nil {
		return nil, err
	}
	//将activeDeadlineHandler添加到PodSyncDeadlineHandler控制器集中
	klet.AddPodSyncLoopHandler(activeDeadlineHandler)
	//将activeDeadlineHandler添加到PodSyncHandler控制器集中
	klet.AddPodSyncHandler(activeDeadlineHandler)

	klet.admitHandlers.AddPodAdmitHandler(lifecycle.NewPredicateAdmitHandler(klet.getNodeAnyWay))
	// apply functional Option's
	for _, opt := range kubeDeps.Options {
		opt(klet)
	}

	klet.appArmorValidator = apparmor.NewValidator(kubeCfg.ContainerRuntime)
	klet.softAdmitHandlers.AddPodAdmitHandler(lifecycle.NewAppArmorAdmitHandler(klet.appArmorValidator))

	// Finally, put the most recent version of the config on the Kubelet, so
	// people can see how it was configured.
	klet.kubeletConfiguration = *kubeCfg
	return klet, nil
}

//获取指定标签的服务??
type serviceLister interface {
	List(labels.Selector) ([]*v1.Service, error)
}

type nodeLister interface {
	List() (machines v1.NodeList, err error)
}

// Kubelet is the main kubelet implementation.
type Kubelet struct {
	kubeletConfiguration componentconfig.KubeletConfiguration

	hostname      string         //主机名通常是uname -n,但可以通过kubelet的启动参数--host-override来修改
	nodeName      types.NodeName //kubelet提供给apiserver的节点名,通常值为hostname,见types.NodeName的定义
	dockerClient  dockertools.DockerInterface
	runtimeCache  kubecontainer.RuntimeCache //kubelet pod的缓存
	kubeClient    clientset.Interface        //用来和ApiServer去通信
	iptClient     utilipt.Interface
	rootDirectory string //这个根目录默认路径是什么?/var/lib/kubelet? 对,由kubelet的启动参数--root-dir来配置,默认就是/var/lib/kubelet

	// podWorkers handle syncing Pods in response to events.
	podWorkers PodWorkers //每一个Pod都会关联一个Pod worker

	// resyncInterval is the interval between periodic full reconciliations of
	// pods on this node.
	resyncInterval time.Duration

	// sourcesReady records the sources seen by the kubelet, it is thread-safe.
	sourcesReady config.SourcesReady

	// podManager is a facade that abstracts away the various sources of pods
	// this Kubelet services.
	podManager kubepod.Manager

	// Needed to observe and respond to situations that could impact node stability
	evictionManager eviction.Manager

	// Needed to report events for containers belonging to deleted/modified pods.
	// Tracks references for reporting events
	containerRefManager *kubecontainer.RefManager

	// Optional, defaults to /logs/ from /var/log
	logServer http.Handler
	// Optional, defaults to simple Docker implementation
	runner kubecontainer.ContainerCommandRunner
	// Optional, client for http requests, defaults to empty client
	httpClient kubetypes.HttpGetter

	// cAdvisor used for container information.
	cadvisor cadvisor.Interface //默认暴露端口是4194

	// Set to true to have the node register itself with the apiserver.
	registerNode bool //用来决定是否自动注册到api server, 由启动参数--register-node来决定;如果指定了--api-servers启动参数,默认设置了--register-node
	// Set to true to have the node register itself as schedulable.
	registerSchedulable bool //一个节点是否注册为可调度
	// for internal book keeping; access only from within registerWithApiserver
	registrationCompleted bool // 用来标记kubelet是否向api server注册.见kubelet_node_status.go的registerWithApiServer

	// Set to true if the kubelet is in standalone mode (i.e. setup without an apiserver)
	standaloneMode bool

	// If non-empty, use this for container DNS search.
	clusterDomain string

	// If non-nil, use this for container DNS server.
	clusterDNS net.IP

	// masterServiceNamespace is the namespace that the master service is exposed in.
	masterServiceNamespace string //Kube-system ?不是,该值由kubelet启动参数--master-service-namespace设置,默认为"default".只在kubelet_pod.go中的getServiceEnvVarMap使用
	// serviceLister knows how to list services
	//提供了一个利用k8s client访问api server中的service资源
	serviceLister serviceLister
	// nodeLister knows how to list nodes
	//提供了一个利用k8s client访问api server中的node资源
	nodeLister nodeLister
	// nodeInfo knows how to get information about the node for this kubelet.
	//保存的是api.Node的信息
	nodeInfo predicates.NodeInfo

	// a list of node labels to register
	nodeLabels map[string]string

	// Last timestamp when runtime responded on ping.
	// Mutex is used to protect this value.
	runtimeState *runtimeState //见kubelet_network.go,在没有配置network plugin,由其记录cidr等
	//记录运行时kubelet的内部错误,网络错误(见kubelet.updateRuntimeUp)

	// Volume plugins.
	volumePluginMgr *volume.VolumePluginMgr

	// Network plugin.
	networkPlugin network.NetworkPlugin

	// Handles container probing.
	probeManager prober.Manager
	// Manages container health check results.
	livenessManager proberesults.Manager //用来探测Pod是否存活?不是,其中缓存着各个容器的探测状态

	// How long to keep idle streaming command execution/port forwarding
	// connections open before terminating them
	streamingConnectionIdleTimeout time.Duration

	// The EventRecorder to use
	recorder record.EventRecorder

	// Policy for handling garbage collection of dead containers.
	containerGC kubecontainer.ContainerGC

	// Manager for image garbage collection.
	imageManager images.ImageGCManager

	// Diskspace manager.
	diskSpaceManager diskSpaceManager

	// Cached MachineInfo returned by cadvisor.
	machineInfo *cadvisorapi.MachineInfo

	// Syncs pods statuses with apiserver; also used as a cache of statuses.
	statusManager status.Manager //缓存kubelet上Pod的状态,再更新apiserver上Pod的状态.注意还有一个pkg/kubelet/pod_worker.go中也提供Pod的状态更新
	//还不清楚,这两者的区别.

	// VolumeManager runs a set of asynchronous loops that figure out which
	// volumes need to be attached/mounted/unmounted/detached based on the pods
	// scheduled on this node and makes it so.
	volumeManager volumemanager.VolumeManager

	// Cloud provider interface.
	cloud                   cloudprovider.Interface
	autoDetectCloudProvider bool

	// Reference to this node.
	nodeRef *v1.ObjectReference //向apiserver传递node的信息??

	// Container runtime.
	// 根据底层容器管理器的不同,分别为cri,rkt,docker,所有实际的操作都是由底层管理器实现
	containerRuntime kubecontainer.Runtime

	// reasonCache caches the failure reason of the last creation of all containers, which is
	// used for generating ContainerStatus.
	reasonCache *ReasonCache

	// nodeStatusUpdateFrequency specifies how often kubelet posts node status to master.
	// Note: be cautious when changing the constant, it must work with nodeMonitorGracePeriod
	// in nodecontroller. There are several constraints:
	// 1. nodeMonitorGracePeriod must be N times more than nodeStatusUpdateFrequency, where
	//    N means number of retries allowed for kubelet to post node status. It is pointless
	//    to make nodeMonitorGracePeriod be less than nodeStatusUpdateFrequency, since there
	//    will only be fresh values from Kubelet at an interval of nodeStatusUpdateFrequency.
	//    The constant must be less than podEvictionTimeout.
	// 2. nodeStatusUpdateFrequency needs to be large enough for kubelet to generate node
	//    status. Kubelet may fail to update node status reliably if the value is too small,
	//    as it takes time to gather all necessary node information.
	nodeStatusUpdateFrequency time.Duration // kubelet的启动参数--node-status-update-frequency设置
	//每隔该时间,将会吧kubelet node注册到api server中

	// Generates pod events.
	pleg pleg.PodLifecycleEventGenerator //pod生命周期时间产生器

	// Store kubecontainer.PodStatus for all pods.
	podCache kubecontainer.Cache //缓存kubelet中所有Pod的状态.podworker将从podCache中获取pod的状态

	// os is a facade for various syscalls that need to be mocked during testing.
	os kubecontainer.OSInterface

	// Watcher of out of memory events.
	oomWatcher OOMWatcher

	// Monitor resource usage
	resourceAnalyzer stats.ResourceAnalyzer

	// Whether or not we should have the QOS cgroup hierarchy for resource management
	cgroupsPerQOS bool

	// If non-empty, pass this to the container runtime as the root cgroup.
	cgroupRoot string

	// Mounter to use for volumes.
	mounter mount.Interface

	// Writer interface to use for volumes.
	writer kubeio.Writer

	// Manager of non-Runtime containers.
	containerManager cm.ContainerManager
	nodeConfig       cm.NodeConfig

	// Traffic to IPs outside this range will use IP masquerade.
	nonMasqueradeCIDR string

	// Maximum Number of Pods which can be run by this Kubelet
	maxPods int

	// Number of NVIDIA GPUs on this node
	nvidiaGPUs int

	// Monitor Kubelet's sync loop
	syncLoopMonitor atomic.Value

	// Container restart Backoff
	backOff *flowcontrol.Backoff //重试

	// Channel for sending pods to kill.
	podKillingCh chan *kubecontainer.PodPair // 见kubelet.podKiller

	// The configuration file used as the base to generate the container's
	// DNS resolver configuration file. This can be used in conjunction with
	// clusterDomain and clusterDNS.
	resolverConfig string //容器的/etc/resolv.conf文件相关??由Kubelet的--resolve-conf参数进行配置

	// Optionally shape the bandwidth of a pod
	// TODO: remove when kubenet plugin is ready
	shaper bandwidth.BandwidthShaper //网络带宽整型器?

	// True if container cpu limits should be enforced via cgroup CFS quota
	cpuCFSQuota bool

	// Information about the ports which are opened by daemons on Node running this Kubelet server.
	daemonEndpoints *v1.NodeDaemonEndpoints //kubelet监听的端口,默认是10250,由kubelet的启动参数--port来控制

	// A queue used to trigger pod workers.
	workQueue queue.WorkQueue ///????和pkg/kubelet/pod_worker.go有关
	//每次对Pod进行更新请求时,都会将该Pod加入该队列中.如果已存在,就更新出栈时间
	//见kubelet.getPodsToSync(),该队列包含需要更新的pods

	// oneTimeInitializer is used to initialize modules that are dependent on the runtime to be up.
	oneTimeInitializer sync.Once

	// If non-nil, use this IP address for the node
	nodeIP net.IP //节点ip地址

	// clock is an interface that provides time related functionality in a way that makes it
	// easy to test the code.
	clock clock.Clock

	// outOfDiskTransitionFrequency specifies the amount of time the kubelet has to be actually
	// not out of disk before it can transition the node condition status from out-of-disk to
	// not-out-of-disk. This prevents a pod that causes out-of-disk condition from repeatedly
	// getting rescheduled onto the node.
	outOfDiskTransitionFrequency time.Duration

	// reservation specifies resources which are reserved for non-pod usage, including kubernetes and
	// non-kubernetes system processes.
	reservation kubetypes.Reservation

	// support gathering custom metrics.
	enableCustomMetrics bool

	// How the Kubelet should setup hairpin NAT. Can take the values: "promiscuous-bridge"
	// (make cbr0 promiscuous), "hairpin-veth" (set the hairpin flag on veth interfaces)
	// or "none" (do nothing).
	hairpinMode componentconfig.HairpinMode

	// The node has babysitter process monitoring docker and kubelet
	babysitDaemons bool

	// handlers called during the tryUpdateNodeStatus cycle
	setNodeStatusFuncs []func(*v1.Node) error //一系列更新节点状态的控制器.默认为kubelet_node_status.go中kubelet.defaultNodeStatusFuncs()返回的一系列控制器.

	// TODO: think about moving this to be centralized in PodWorkers in follow-on.
	// the list of handlers to call during pod admission.
	admitHandlers lifecycle.PodAdmitHandlers //用来决定一个pod是否允许进入

	// softAdmithandlers are applied to the pod after it is admitted by the Kubelet, but before it is
	// run. A pod rejected by a softAdmitHandler will be left in a Pending state indefinitely. If a
	// rejected pod should not be recreated, or the scheduler is not aware of the rejection rule, the
	// admission rule should be applied by a softAdmitHandler.
	softAdmitHandlers lifecycle.PodAdmitHandlers //用于决定是否一个pod是否允许运行
	//在syncPod()中的canRunPod()中调用时,用来决定一个pod能够运行

	// the list of handlers to call during pod sync loop.
	lifecycle.PodSyncLoopHandlers //

	// the list of handlers to call during pod sync.
	lifecycle.PodSyncHandlers //用于决定是否驱逐一个pod

	// the number of allowed pods per core
	podsPerCore int

	// enableControllerAttachDetach indicates the Attach/Detach controller
	// should manage attachment/detachment of volumes scheduled to this node,
	// and disable kubelet from executing any attach/detach operations
	enableControllerAttachDetach bool //??卷的关联解关联器?

	// trigger deleting containers in a pod
	containerDeletor *podContainerDeletor

	// config iptables util rules
	makeIPTablesUtilChains bool

	// The bit of the fwmark space to mark packets for SNAT.
	iptablesMasqueradeBit int

	// The bit of the fwmark space to mark packets for dropping.
	iptablesDropBit int

	// The AppArmor validator for checking whether AppArmor is supported.
	appArmorValidator apparmor.Validator

	// The handler serving CRI streaming calls (exec/attach/port-forward).
	criHandler http.Handler

	// experimentalHostUserNamespaceDefaulting sets userns=true when users request host namespaces (pid, ipc, net),
	// are using non-namespaced capabilities (mknod, sys_time, sys_module), the pod contains a privileged container,
	// or using host path volumes.
	// This should only be enabled when the container runtime is performing user remapping AND if the
	// experimental behavior is desired.
	experimentalHostUserNamespaceDefaulting bool
}

// setupDataDirs creates:
// 1.  the root directory
// 2.  the pods directory
// 3.  the plugins directory
//创建kubelet存放数据的目录
func (kl *Kubelet) setupDataDirs() error {
	kl.rootDirectory = path.Clean(kl.rootDirectory)
	if err := os.MkdirAll(kl.getRootDir(), 0750); err != nil {
		return fmt.Errorf("error creating root directory: %v", err)
	}
	if err := os.MkdirAll(kl.getPodsDir(), 0750); err != nil {
		return fmt.Errorf("error creating pods directory: %v", err)
	}
	if err := os.MkdirAll(kl.getPluginsDir(), 0750); err != nil {
		return fmt.Errorf("error creating plugins directory: %v", err)
	}
	return nil
}

// Starts garbage collection threads.
//容器垃圾回收和镜像垃圾回收
func (kl *Kubelet) StartGarbageCollection() {
	loggedContainerGCFailure := false
	//周期性执行垃圾回收,直到接收到wait.NeverStop信号, 什么情况下会接收到这个信号
	//不会接收到该信好,相当于死循环
	go wait.Until(func() {
		if err := kl.containerGC.GarbageCollect(kl.sourcesReady.AllReady()); err != nil {
			glog.Errorf("Container garbage collection failed: %v", err)
			kl.recorder.Eventf(kl.nodeRef, v1.EventTypeWarning, events.ContainerGCFailed, err.Error())
			loggedContainerGCFailure = true
		} else {
			var vLevel glog.Level = 4
			if loggedContainerGCFailure {
				vLevel = 1
				loggedContainerGCFailure = false
			}

			glog.V(vLevel).Infof("Container garbage collection succeeded")
		}
	}, ContainerGCPeriod, wait.NeverStop)

	loggedImageGCFailure := false
	//周期性执行镜像回收策略
	go wait.Until(func() {
		if err := kl.imageManager.GarbageCollect(); err != nil {
			glog.Errorf("Image garbage collection failed: %v", err)
			kl.recorder.Eventf(kl.nodeRef, v1.EventTypeWarning, events.ImageGCFailed, err.Error())
			loggedImageGCFailure = true
		} else {
			var vLevel glog.Level = 4
			if loggedImageGCFailure {
				vLevel = 1
				loggedImageGCFailure = false
			}

			glog.V(vLevel).Infof("Image garbage collection succeeded")
		}
	}, ImageGCPeriod, wait.NeverStop)
}

// initializeModules will initialize internal modules that do not require the container runtime to be up.
// Note that the modules here must not depend on modules that are not initialized here.
//内部模块?
func (kl *Kubelet) initializeModules() error {
	// Step 1: Promethues metrics.
	metrics.Register(kl.runtimeCache) //注册相关的指标?

	// Step 2: Setup filesystem directories.
	//默认的根目录在/var/lib/kubelet?这些目录的作用?
	//用来存放pod/plugins等数据
	if err := kl.setupDataDirs(); err != nil {
		return err
	}

	// Step 3: If the container logs directory does not exist, create it.
	//创建容器的日志目录
	if _, err := os.Stat(ContainerLogsDir); err != nil {
		if err := kl.os.MkdirAll(ContainerLogsDir, 0755); err != nil {
			glog.Errorf("Failed to create directory %q: %v", ContainerLogsDir, err)
		}
	}

	// Step 4: Start the image manager.
	//负责回收镜像的镜像管理器
	kl.imageManager.Start()

	// Step 5: Start container manager.
	//如果不是单节点模式(没有apiserver),则通过kube client获取节点的信息
	node, err := kl.getNodeAnyWay()
	if err != nil {
		return fmt.Errorf("Kubelet failed to get node info: %v", err)
	}

	//容器管理器,这和系统有关/cgroup
	//??kubelet会启动docker daeomon?
	if err := kl.containerManager.Start(node); err != nil {
		return fmt.Errorf("Failed to start ContainerManager %v", err)
	}

	// Step 6: Start out of memory watcher.
	//oomWatcher监控的是系统中的进程?
	if err := kl.oomWatcher.Start(kl.nodeRef); err != nil {
		return fmt.Errorf("Failed to start OOM watcher %v", err)
	}

	// Step 7: Start resource analyzer
	//这个资源分析器的作用是什么?
	kl.resourceAnalyzer.Start()

	return nil
}

// initializeRuntimeDependentModules will initialize internal modules that require the container runtime to be up.
//周期性运行的updateRuntimeUp()调用该函数进行驱逐管理
func (kl *Kubelet) initializeRuntimeDependentModules() {
	//启动cadvisor进行容器统计,默认端口是4194
	if err := kl.cadvisor.Start(); err != nil {
		// Fail kubelet and rely on the babysitter to retry starting kubelet.
		// TODO(random-liu): Add backoff logic in the babysitter
		glog.Fatalf("Failed to start cAdvisor %v", err)
	}
	// eviction manager must start after cadvisor because it needs to know if the container runtime has a dedicated imagefs
	//驱逐管理器只会对没有处于终止状态的Pod驱逐决策, 不对StaticPod进行驱逐决策
	kl.evictionManager.Start(kl, kl.getActivePods, evictionMonitoringPeriod)
}

// Run starts the kubelet reacting to config updates
func (kl *Kubelet) Run(updates <-chan kubetypes.PodUpdate) {
	//将/var/log/下的文件当做文件服务器进行提供? 如何访问这个文件服务器?
	if kl.logServer == nil {
		kl.logServer = http.StripPrefix("/logs/", http.FileServer(http.Dir("/var/log/")))
	}
	if kl.kubeClient == nil {
		glog.Warning("No api server defined - no node status update will be sent.")
	}
	//启动镜像管理器,容器管理器,节点管理器,资源分析器等模块
	if err := kl.initializeModules(); err != nil {
		kl.recorder.Eventf(kl.nodeRef, v1.EventTypeWarning, events.KubeletSetupFailed, err.Error())
		glog.Error(err)
		kl.runtimeState.setInitError(err)
	}

	// Start volume manager
	go kl.volumeManager.Run(kl.sourcesReady, wait.NeverStop)

	if kl.kubeClient != nil {
		// Start syncing node status immediately, this may set up things the runtime needs to run.
		//每个指定时间注册kubelet到apiserver并更新节点状态
		go wait.Until(kl.syncNodeStatus, kl.nodeStatusUpdateFrequency, wait.NeverStop)
	}
	//每30秒同步网络状态
	go wait.Until(kl.syncNetworkStatus, 30*time.Second, wait.NeverStop)
	//每5秒获取kubelet的运行时状态(包括容器管理器状态,网络状况)
	go wait.Until(kl.updateRuntimeUp, 5*time.Second, wait.NeverStop)

	// Start loop to sync iptables util rules
	if kl.makeIPTablesUtilChains {
		go wait.Until(kl.syncNetworkUtil, 1*time.Minute, wait.NeverStop)
	}

	// Start a goroutine responsible for killing pods (that are not properly
	// handled by pod workers).
	//清除pod
	go wait.Until(kl.podKiller, 1*time.Second, wait.NeverStop)

	// Start component sync loops.
	kl.statusManager.Start()
	kl.probeManager.Start()

	// Start the pod lifecycle event generator.
	kl.pleg.Start()
	kl.syncLoop(updates, kl)
}

// GetKubeClient returns the Kubernetes client.
// TODO: This is currently only required by network plugins. Replace
// with more specific methods.
func (kl *Kubelet) GetKubeClient() clientset.Interface {
	return kl.kubeClient
}

// GetClusterDNS returns a list of the DNS servers and a list of the DNS search
// domains of the cluster.
func (kl *Kubelet) GetClusterDNS(pod *v1.Pod) ([]string, []string, error) {
	var hostDNS, hostSearch []string
	// Get host DNS settings
	if kl.resolverConfig != "" {
		f, err := os.Open(kl.resolverConfig)
		if err != nil {
			return nil, nil, err
		}
		defer f.Close()

		hostDNS, hostSearch, err = kl.parseResolvConf(f)
		if err != nil {
			return nil, nil, err
		}
	}
	useClusterFirstPolicy := pod.Spec.DNSPolicy == v1.DNSClusterFirst
	if useClusterFirstPolicy && kl.clusterDNS == nil {
		// clusterDNS is not known.
		// pod with ClusterDNSFirst Policy cannot be created
		kl.recorder.Eventf(pod, v1.EventTypeWarning, "MissingClusterDNS", "kubelet does not have ClusterDNS IP configured and cannot create Pod using %q policy. Falling back to DNSDefault policy.", pod.Spec.DNSPolicy)
		log := fmt.Sprintf("kubelet does not have ClusterDNS IP configured and cannot create Pod using %q policy. pod: %q. Falling back to DNSDefault policy.", pod.Spec.DNSPolicy, format.Pod(pod))
		kl.recorder.Eventf(kl.nodeRef, v1.EventTypeWarning, "MissingClusterDNS", log)

		// fallback to DNSDefault
		useClusterFirstPolicy = false
	}

	if !useClusterFirstPolicy {
		// When the kubelet --resolv-conf flag is set to the empty string, use
		// DNS settings that override the docker default (which is to use
		// /etc/resolv.conf) and effectively disable DNS lookups. According to
		// the bind documentation, the behavior of the DNS client library when
		// "nameservers" are not specified is to "use the nameserver on the
		// local machine". A nameserver setting of localhost is equivalent to
		// this documented behavior.
		if kl.resolverConfig == "" {
			hostDNS = []string{"127.0.0.1"}
			hostSearch = []string{"."}
		}
		return hostDNS, hostSearch, nil
	}

	// for a pod with DNSClusterFirst policy, the cluster DNS server is the only nameserver configured for
	// the pod. The cluster DNS server itself will forward queries to other nameservers that is configured to use,
	// in case the cluster DNS server cannot resolve the DNS query itself
	dns := []string{kl.clusterDNS.String()}

	var dnsSearch []string
	if kl.clusterDomain != "" {
		nsSvcDomain := fmt.Sprintf("%s.svc.%s", pod.Namespace, kl.clusterDomain)
		svcDomain := fmt.Sprintf("svc.%s", kl.clusterDomain)
		dnsSearch = append([]string{nsSvcDomain, svcDomain, kl.clusterDomain}, hostSearch...)
	} else {
		dnsSearch = hostSearch
	}
	return dns, dnsSearch, nil
}

// syncPod is the transaction script for the sync of a single pod.
//
// Arguments:
//
// o - the SyncPodOptions for this invocation
//
// The workflow is:
// * If the pod is being created, record pod worker start latency
// * Call generateAPIPodStatus to prepare an v1.PodStatus for the pod
// * If the pod is being seen as running for the first time, record pod
//   start latency
// * Update the status of the pod in the status manager
// * Kill the pod if it should not be running
// * Create a mirror pod if the pod is a static pod, and does not
//   already have a mirror pod
// * Create the data directories for the pod if they do not exist
// * Wait for volumes to attach/mount
// * Fetch the pull secrets for the pod
// * Call the container runtime's SyncPod callback
// * Update the traffic shaping for the pod's ingress and egress limits
//
// If any step of this workflow errors, the error is returned, and is repeated
// on the next syncPod call.
//????这里传递的参数已经是api pod?那么更新类型为create的意义是什么?
//有可能apipod是由apiserver传递过来,请求创建的pod
func (kl *Kubelet) syncPod(o syncPodOptions) error {
	// pull out the required options
	pod := o.pod
	mirrorPod := o.mirrorPod
	podStatus := o.podStatus
	updateType := o.updateType

	// if we want to kill a pod, do it now!
	//更新类型是杀死一个Pod
	if updateType == kubetypes.SyncPodKill {
		killPodOptions := o.killPodOptions
		if killPodOptions == nil || killPodOptions.PodStatusFunc == nil {
			return fmt.Errorf("kill pod options are required if update type is kill")
		}
		//???
		apiPodStatus := killPodOptions.PodStatusFunc(pod, podStatus)
		//向apiserver更新Pod的状态
		kl.statusManager.SetPodStatus(pod, apiPodStatus)
		// we kill the pod with the specified grace period since this is a termination
		if err := kl.killPod(pod, nil, podStatus, killPodOptions.PodTerminationGracePeriodSecondsOverride); err != nil {
			// there was an error killing the pod, so we return that error directly
			utilruntime.HandleError(err)
			return err
		}
		return nil
	}

	// Latency measurements for the main workflow are relative to the
	// first time the pod was seen by the API server.
	var firstSeenTime time.Time
	//?? api server第一次看到Pod的时间?
	//还是说api server创建了该pod,就给该pod添加一个创建时间
	if firstSeenTimeStr, ok := pod.Annotations[kubetypes.ConfigFirstSeenAnnotationKey]; ok {
		firstSeenTime = kubetypes.ConvertToTimestamp(firstSeenTimeStr).Get()
	}

	// Record pod worker start latency if being created
	// TODO: make pod workers record their own latencies
	if updateType == kubetypes.SyncPodCreate {
		//统计创建时间的指标
		if !firstSeenTime.IsZero() {
			// This is the first time we are syncing the pod. Record the latency
			// since kubelet first saw the pod if firstSeenTime is set.
			metrics.PodWorkerStartLatency.Observe(metrics.SinceInMicroseconds(firstSeenTime))
		} else {
			glog.V(3).Infof("First seen time not recorded for pod %q", pod.UID)
		}
	}

	// Generate final API pod status with pod and status manager status
	//根据kubelet pod status 转换成api pod pod status
	apiPodStatus := kl.generateAPIPodStatus(pod, podStatus)
	// The pod IP may be changed in generateAPIPodStatus if the pod is using host network. (See #24576)
	// TODO(random-liu): After writing pod spec into container labels, check whether pod is using host network, and
	// set pod IP to hostIP directly in runtime.GetPodStatus
	podStatus.IP = apiPodStatus.PodIP

	// Record the time it takes for the pod to become running.
	//获取api pod哦哦
	existingStatus, ok := kl.statusManager.GetPodStatus(pod.UID)
	if !ok || existingStatus.Phase == v1.PodPending && apiPodStatus.Phase == v1.PodRunning &&
		!firstSeenTime.IsZero() {
		metrics.PodStartLatency.Observe(metrics.SinceInMicroseconds(firstSeenTime))
	}

	//决策kubelet是否能运行指定的pod,如果决议失败;更新pod的状态
	runnable := kl.canRunPod(pod)
	//不允许
	if !runnable.Admit {
		// Pod is not runnable; update the Pod and Container statuses to why.
		apiPodStatus.Reason = runnable.Reason
		apiPodStatus.Message = runnable.Message
		// Waiting containers are not creating.
		const waitingReason = "Blocked"
		for _, cs := range apiPodStatus.InitContainerStatuses {
			if cs.State.Waiting != nil {
				cs.State.Waiting.Reason = waitingReason
			}
		}
		for _, cs := range apiPodStatus.ContainerStatuses {
			if cs.State.Waiting != nil {
				cs.State.Waiting.Reason = waitingReason
			}
		}
	}

	// Update status in the status manager
	//更新状态?
	kl.statusManager.SetPodStatus(pod, apiPodStatus)

	// Kill pod if it should not be running
	//如果pod不被允许运行,或者pod已经被要求删除,或者pod处于运行失败状态
	if !runnable.Admit || pod.DeletionTimestamp != nil || apiPodStatus.Phase == v1.PodFailed {
		var syncErr error
		if err := kl.killPod(pod, nil, podStatus, nil); err != nil {
			syncErr = fmt.Errorf("error killing pod: %v", err)
			utilruntime.HandleError(syncErr)
		} else {
			if !runnable.Admit {
				// There was no error killing the pod, but the pod cannot be run.
				// Return an error to signal that the sync loop should back off.
				syncErr = fmt.Errorf("pod cannot be run: %s", runnable.Message)
			}
		}
		return syncErr
	}

	// If the network plugin is not ready, only start the pod if it uses the host network
	//网络出现了错误,由kubelet.updateRuntimeUp()方法负责更新这个错误
	if rs := kl.runtimeState.networkErrors(); len(rs) != 0 && !kubecontainer.IsHostNetworkPod(pod) {
		return fmt.Errorf("network is not ready: %v", rs)
	}

	// Create Cgroups for the pod and apply resource parameters
	// to them if cgroup-per-qos flag is enabled.
	pcm := kl.containerManager.NewPodContainerManager()
	// If pod has already been terminated then we need not create
	// or update the pod's cgroup
	if !kl.podIsTerminated(pod) {
		// When the kubelet is restarted with the cgroup-per-qos
		// flag enabled, all the pod's running containers
		// should be killed intermittently and brought back up
		// under the qos cgroup hierarchy.
		// Check if this is the pod's first sync
		firstSync := true
		for _, containerStatus := range apiPodStatus.ContainerStatuses {
			if containerStatus.State.Running != nil {
				firstSync = false
				break
			}
		}
		// Don't kill containers in pod if pod's cgroups already
		// exists or the pod is running for the first time
		podKilled := false
		if !pcm.Exists(pod) && !firstSync {
			kl.killPod(pod, nil, podStatus, nil)
			podKilled = true
		}
		// Create and Update pod's Cgroups
		// Don't create cgroups for run once pod if it was killed above
		// The current policy is not to restart the run once pods when
		// the kubelet is restarted with the new flag as run once pods are
		// expected to run only once and if the kubelet is restarted then
		// they are not expected to run again.
		// We don't create and apply updates to cgroup if its a run once pod and was killed above
		if !(podKilled && pod.Spec.RestartPolicy == v1.RestartPolicyNever) {
			if err := pcm.EnsureExists(pod); err != nil {
				return fmt.Errorf("failed to ensure that the pod: %v cgroups exist and are correctly applied: %v", pod.UID, err)
			}
		}
	}

	// Create Mirror Pod for Static Pod if it doesn't already exist
	if kubepod.IsStaticPod(pod) {
		podFullName := kubecontainer.GetPodFullName(pod)
		deleted := false
		if mirrorPod != nil {
			if mirrorPod.DeletionTimestamp != nil || !kl.podManager.IsMirrorPodOf(mirrorPod, pod) {
				// The mirror pod is semantically different from the static pod. Remove
				// it. The mirror pod will get recreated later.
				glog.Warningf("Deleting mirror pod %q because it is outdated", format.Pod(mirrorPod))
				if err := kl.podManager.DeleteMirrorPod(podFullName); err != nil {
					glog.Errorf("Failed deleting mirror pod %q: %v", format.Pod(mirrorPod), err)
				} else {
					deleted = true
				}
			}
		}
		if mirrorPod == nil || deleted {
			glog.V(3).Infof("Creating a mirror pod for static pod %q", format.Pod(pod))
			if err := kl.podManager.CreateMirrorPod(pod); err != nil {
				glog.Errorf("Failed creating a mirror pod for %q: %v", format.Pod(pod), err)
			}
		}
	}

	// Make data directories for the pod
	if err := kl.makePodDataDirs(pod); err != nil {
		glog.Errorf("Unable to make pod data directories for pod %q: %v", format.Pod(pod), err)
		return err
	}

	// Wait for volumes to attach/mount
	if err := kl.volumeManager.WaitForAttachAndMount(pod); err != nil {
		kl.recorder.Eventf(pod, v1.EventTypeWarning, events.FailedMountVolume, "Unable to mount volumes for pod %q: %v", format.Pod(pod), err)
		glog.Errorf("Unable to mount volumes for pod %q: %v; skipping pod", format.Pod(pod), err)
		return err
	}

	// Fetch the pull secrets for the pod
	//获取指定pod中镜像拉取secret
	pullSecrets, err := kl.getPullSecretsForPod(pod)
	if err != nil {
		glog.Errorf("Unable to get pull secrets for pod %q: %v", format.Pod(pod), err)
		return err
	}

	// Call the container runtime's SyncPod callback
	//调用容器管理器去同步Pod??
	result := kl.containerRuntime.SyncPod(pod, apiPodStatus, podStatus, pullSecrets, kl.backOff)
	//将Pod的同步结果中启动启动容器失败的部分添加到reason cache中
	kl.reasonCache.Update(pod.UID, result)
	if err = result.Error(); err != nil {
		return err
	}

	// early successful exit if pod is not bandwidth-constrained
	if !kl.shapingEnabled() {
		return nil
	}

	// Update the traffic shaping for the pod's ingress and egress limits
	ingress, egress, err := bandwidth.ExtractPodBandwidthResources(pod.Annotations)
	if err != nil {
		return err
	}
	if egress != nil || ingress != nil {
		if kubecontainer.IsHostNetworkPod(pod) {
			kl.recorder.Event(pod, v1.EventTypeWarning, events.HostNetworkNotSupported, "Bandwidth shaping is not currently supported on the host network")
		} else if kl.shaper != nil {
			if len(apiPodStatus.PodIP) > 0 {
				err = kl.shaper.ReconcileCIDR(fmt.Sprintf("%s/32", apiPodStatus.PodIP), egress, ingress)
			}
		} else {
			kl.recorder.Event(pod, v1.EventTypeWarning, events.UndefinedShaper, "Pod requests bandwidth shaping, but the shaper is undefined")
		}
	}

	return nil
}

// Get pods which should be resynchronized. Currently, the following pod should be resynchronized:
//   * pod whose work is ready.
//   * internal modules that request sync of a pod.
//获取Kubelet上需要同步的Pod,kubelet在syncLoopInteration()中将通过一个定时器周期性的同步Pod
//需要同步的Pod包括:1)在kubelet上的workQueue上的所有Pod,2)经过podSyncLoopHandlers检测需要同步的pod
func (kl *Kubelet) getPodsToSync() []*v1.Pod {
	//获取kubelet上所有的Pod
	allPods := kl.podManager.GetPods()
	//kubelet工作队列等待的pod
	podUIDs := kl.workQueue.GetWork()
	podUIDSet := sets.NewString()
	for _, podUID := range podUIDs {
		podUIDSet.Insert(string(podUID))
	}
	var podsToSync []*v1.Pod
	//获得pod需要被syncloopHandler同步的pod以及workqueue中等待的pods
	for _, pod := range allPods {
		if podUIDSet.Has(string(pod.UID)) {
			// The work of the pod is ready
			podsToSync = append(podsToSync, pod)
			continue
		}
		for _, podSyncLoopHandler := range kl.PodSyncLoopHandlers {
			if podSyncLoopHandler.ShouldSync(pod) {
				podsToSync = append(podsToSync, pod)
				break
			}
		}
	}
	return podsToSync
}

// deletePod deletes the pod from the internal state of the kubelet by:
// 1.  stopping the associated pod worker asynchronously
// 2.  signaling to kill the pod by sending on the podKillingCh channel
//
// deletePod returns an error if not all sources are ready or the pod is not
// found in the runtime cache.
func (kl *Kubelet) deletePod(pod *v1.Pod) error {
	if pod == nil {
		return fmt.Errorf("deletePod does not allow nil pod")
	}
	if !kl.sourcesReady.AllReady() {
		// If the sources aren't ready, skip deletion, as we may accidentally delete pods
		// for sources that haven't reported yet.
		return fmt.Errorf("skipping delete because sources aren't ready yet")
	}
	//关闭指定Pod的pod worker goroutine
	kl.podWorkers.ForgetWorker(pod.UID)

	// Runtime cache may not have been updated to with the pod, but it's okay
	// because the periodic cleanup routine will attempt to delete again later.
	runningPods, err := kl.runtimeCache.GetPods()
	if err != nil {
		return fmt.Errorf("error listing containers: %v", err)
	}
	runningPod := kubecontainer.Pods(runningPods).FindPod("", pod.UID)
	if runningPod.IsEmpty() {
		return fmt.Errorf("pod not found")
	}
	podPair := kubecontainer.PodPair{APIPod: pod, RunningPod: &runningPod}

	kl.podKillingCh <- &podPair
	// TODO: delete the mirror pod here?

	// We leave the volume/directory cleanup to the periodic cleanup routine.
	return nil
}

// isOutOfDisk detects if pods can't fit due to lack of disk space.
//检测Kubelet的剩余空间是否满足设定的策略阈值
func (kl *Kubelet) isOutOfDisk() bool {
	// Check disk space once globally and reject or accept all new pods.
	//检测docker镜像所在文件系统可用空间是否低于指定的策略阈值
	withinBounds, err := kl.diskSpaceManager.IsRuntimeDiskSpaceAvailable()
	// Assume enough space in case of errors.
	if err != nil {
		glog.Errorf("Failed to check if disk space is available for the runtime: %v", err)
	} else if !withinBounds {
		return true
	}

	//检测根文件系统可用空间是否低于指定的策略阈值
	withinBounds, err = kl.diskSpaceManager.IsRootDiskSpaceAvailable()
	// Assume enough space in case of errors.
	if err != nil {
		glog.Errorf("Failed to check if disk space is available on the root partition: %v", err)
	} else if !withinBounds {
		return true
	}
	return false
}

// rejectPod records an event about the pod with the given reason and message,
// and updates the pod to the failed phase in the status manage.
//更新Pod的状态为失败,并更新状态到apiserver
func (kl *Kubelet) rejectPod(pod *v1.Pod, reason, message string) {
	kl.recorder.Eventf(pod, v1.EventTypeWarning, reason, message)
	//更新Pod的状态为PoddFailed,并记录原因
	kl.statusManager.SetPodStatus(pod, v1.PodStatus{
		Phase:   v1.PodFailed,
		Reason:  reason,
		Message: "Pod " + message})
}

// canAdmitPod determines if a pod can be admitted, and gives a reason if it
// cannot. "pod" is new pod, while "pods" are all admitted pods
// The function returns a boolean value indicating whether the pod
// can be admitted, a brief single-word reason and a message explaining why
// the pod cannot be admitted.
//见HandlePodAdditions(),pods是kubelet中没有terminated的pods
func (kl *Kubelet) canAdmitPod(pods []*v1.Pod, pod *v1.Pod) (bool, string, string) {
	// the kubelet will invoke each pod admit handler in sequence
	// if any handler rejects, the pod is rejected.
	// TODO: move out of disk check into a pod admitter
	// TODO: out of resource eviction should have a pod admitter call-out
	//遍历kubelet所有的admitHandlers,决议一个pod是否允许进入,不允许则返回原因
	attrs := &lifecycle.PodAdmitAttributes{Pod: pod, OtherPods: pods}
	// 遍历所有的podAdmitHandler,决议是否允许指定的Pod进入
	for _, podAdmitHandler := range kl.admitHandlers {
		if result := podAdmitHandler.Admit(attrs); !result.Admit {
			return false, result.Reason, result.Message
		}
	}
	// TODO: When disk space scheduling is implemented (#11976), remove the out-of-disk check here and
	// add the disk space predicate to predicates.GeneralPredicates.
	//检测kubelet是否处于disk资源低于阈值
	if kl.isOutOfDisk() {
		glog.Warningf("Failed to admit pod %v - %s", format.Pod(pod), "predicate fails due to OutOfDisk")
		return false, "OutOfDisk", "cannot be started due to lack of disk space."
	}

	return true, "", ""
}

//检测kubelet能够运行指定的Pod
func (kl *Kubelet) canRunPod(pod *v1.Pod) lifecycle.PodAdmitResult {
	attrs := &lifecycle.PodAdmitAttributes{Pod: pod}
	// Get "OtherPods". Rejected pods are failed, so only include admitted pods that are alive.
	//获取kubelet中没有terminated的Pod
	attrs.OtherPods = kl.filterOutTerminatedPods(kl.podManager.GetPods())

	//决定一个Pod是否允许运行
	for _, handler := range kl.softAdmitHandlers {
		if result := handler.Admit(attrs); !result.Admit {
			return result
		}
	}

	// TODO: Refactor as a soft admit handler.
	//检测linux capability/ipc/privileged等是否运行
	if err := canRunPod(pod); err != nil {
		return lifecycle.PodAdmitResult{
			Admit:   false,
			Reason:  "Forbidden",
			Message: err.Error(),
		}
	}

	return lifecycle.PodAdmitResult{Admit: true}
}

// syncLoop is the main loop for processing changes. It watches for changes from
// three channels (file, apiserver, and http) and creates a union of them. For
// any new change seen, will run a sync against desired state and running state. If
// no changes are seen to the configuration, will synchronize the last known desired
// state every sync-frequency seconds. Never returns.
///这个SYncHandler就是kubelet本身
//updatesChan是Pod配置的来源(apiserver,file,http),
//当有Pod添加移除,更新时,将会向该Chan发送pod事件
func (kl *Kubelet) syncLoop(updates <-chan kubetypes.PodUpdate, handler SyncHandler) {
	glog.Info("Starting kubelet main sync loop.")
	// The resyncTicker wakes up kubelet to checks if there are any pod workers
	// that need to be sync'd. A one-second period is sufficient because the
	// sync interval is defaulted to 10s.
	syncTicker := time.NewTicker(time.Second)
	defer syncTicker.Stop()
	housekeepingTicker := time.NewTicker(housekeepingPeriod)
	defer housekeepingTicker.Stop()
	//监听Pod Lifecycle event,接收
	plegCh := kl.pleg.Watch()
	//如果kubelet没有出现运行时错误,则进行Pod的同步
	for {
		//获取Kubelet运行时所有错误(网络错误和内部错误)
		if rs := kl.runtimeState.runtimeErrors(); len(rs) != 0 {
			glog.Infof("skipping pod synchronization - %v", rs)
			time.Sleep(5 * time.Second)
			continue
		}
		//
		if !kl.syncLoopIteration(updates, handler, syncTicker.C, housekeepingTicker.C, plegCh) {
			break
		}
	}
}

// syncLoopIteration reads from various channels and dispatches pods to the
// given handler.
//
// Arguments:
// 1.  configCh:       a channel to read config events from
// 2.  handler:        the SyncHandler to dispatch pods to
// 3.  syncCh:         a channel to read periodic sync events from
// 4.  houseKeepingCh: a channel to read housekeeping events from
// 5.  plegCh:         a channel to read PLEG updates from
//
// Events are also read from the kubelet liveness manager's update channel.
//
// The workflow is to read from one of the channels, handle that event, and
// update the timestamp in the sync loop monitor.
//
// Here is an appropriate place to note that despite the syntactical
// similarity to the switch statement, the case statements in a select are
// evaluated in a pseudorandom order if there are multiple channels ready to
// read from when the select is evaluated.  In other words, case statements
// are evaluated in random order, and you can not assume that the case
// statements evaluate in order if multiple channels have events.
//
// With that in mind, in truly no particular order, the different channels
// are handled as follows:
//
// * configCh: dispatch the pods for the config change to the appropriate
//             handler callback for the event type
// * plegCh: update the runtime cache; sync pod
// * syncCh: sync all pods waiting for sync
// * houseKeepingCh: trigger cleanup of pods
// * liveness manager: sync pods that have failed or in which one or more
//                     containers have failed liveness checks
//syncCh,housekeepingCh都是time ticker channel,每隔指定之间(10秒)将会发送信号
//configCh是pod的配置来源,包含apiserver,file,http几个来源
func (kl *Kubelet) syncLoopIteration(configCh <-chan kubetypes.PodUpdate, handler SyncHandler,
	syncCh <-chan time.Time, housekeepingCh <-chan time.Time, plegCh <-chan *pleg.PodLifecycleEvent) bool {
	kl.syncLoopMonitor.Store(kl.clock.Now())
	select {
	//接收到了pod 源的事件(file,http,apiserver)
	//file,http是一定周期去轮询,而apiserver是使用了list-watch
	case u, open := <-configCh:
		// Update from a config source; dispatch it to the right handler
		// callback.
		if !open {
			glog.Errorf("Update channel is closed. Exiting the sync loop.")
			return false
		}

		switch u.Op {
		case kubetypes.ADD:
			glog.V(2).Infof("SyncLoop (ADD, %q): %q", u.Source, format.Pods(u.Pods))
			// After restarting, kubelet will get all existing pods through
			// ADD as if they are new pods. These pods will then go through the
			// admission process and *may* be rejected. This can be resolved
			// once we have checkpointing.
			handler.HandlePodAdditions(u.Pods)
		case kubetypes.UPDATE:
			glog.V(2).Infof("SyncLoop (UPDATE, %q): %q", u.Source, format.PodsWithDeletiontimestamps(u.Pods))
			handler.HandlePodUpdates(u.Pods)
		case kubetypes.REMOVE:
			glog.V(2).Infof("SyncLoop (REMOVE, %q): %q", u.Source, format.Pods(u.Pods))
			handler.HandlePodRemoves(u.Pods)
		case kubetypes.RECONCILE:
			glog.V(4).Infof("SyncLoop (RECONCILE, %q): %q", u.Source, format.Pods(u.Pods))
			handler.HandlePodReconcile(u.Pods)
		case kubetypes.DELETE:
			glog.V(2).Infof("SyncLoop (DELETE, %q): %q", u.Source, format.Pods(u.Pods))
			// DELETE is treated as a UPDATE because of graceful deletion.
			handler.HandlePodUpdates(u.Pods)
			//这个kubetypes.SET是用来创建pod源的,不应该出现在这里
		case kubetypes.SET: //?? apiserver源设置的动作?见pkg/kubelet/config/apiserver.go
			// TODO: Do we want to support this?
			glog.Errorf("Kubelet does not support snapshot update")
		}

		// Mark the source ready after receiving at least one update from the
		// source. Once all the sources are marked ready, various cleanup
		// routines will start reclaiming resources. It is important that this
		// takes place only after kubelet calls the update handler to process
		// the update to ensure the internal pod cache is up-to-date.
		//记录该pod来源,已经记录过也无所谓
		kl.sourcesReady.AddSource(u.Source)

		// 获取pleg事件
	case e := <-plegCh:
		//如果不是容器被移除?(因为容器被移除,pod会重建??)
		if isSyncPodWorthy(e) {
			// PLEG event for a pod; sync it.
			if pod, ok := kl.podManager.GetPodByUID(e.ID); ok {
				glog.V(2).Infof("SyncLoop (PLEG): %q, event: %#v", format.Pod(pod), e)
				//调用的实际上是kubelet.HandlePodSyncs
				handler.HandlePodSyncs([]*v1.Pod{pod})
			} else {
				// If the pod no longer exists, ignore the event.
				glog.V(4).Infof("SyncLoop (PLEG): ignore irrelevant event: %#v", e)
			}
		}

		//容器处于exited状态
		if e.Type == pleg.ContainerDied {
			if containerID, ok := e.Data.(string); ok {
				//找到指定Pod中已退出的容器,如果pod被驱逐了,则直接删除该容器,否则添加该容器到待删除列表.
				kl.cleanUpContainersInPod(e.ID, containerID)
			}
		}
		//获取需要同步的pods,进行同步
	case <-syncCh:
		// Sync pods waiting for sync
		//获取需要同步的pod,进行同步
		podsToSync := kl.getPodsToSync()
		if len(podsToSync) == 0 {
			break
		}
		glog.V(4).Infof("SyncLoop (SYNC): %d pods; %s", len(podsToSync), format.Pods(podsToSync))
		kl.HandlePodSyncs(podsToSync)

		//如果liveness检测失败了,同步指定的Pod
	case update := <-kl.livenessManager.Updates():
		if update.Result == proberesults.Failure {
			// The liveness manager detected a failure; sync the pod.

			// We should not use the pod from livenessManager, because it is never updated after
			// initialization.
			pod, ok := kl.podManager.GetPodByUID(update.PodUID)
			if !ok {
				// If the pod no longer exists, ignore the update.
				glog.V(4).Infof("SyncLoop (container unhealthy): ignore irrelevant update: %#v", update)
				break
			}
			glog.V(1).Infof("SyncLoop (container unhealthy): %q", format.Pod(pod))
			handler.HandlePodSyncs([]*v1.Pod{pod})
		}
	case <-housekeepingCh:
		if !kl.sourcesReady.AllReady() {
			// If the sources aren't ready or volume manager has not yet synced the states,
			// skip housekeeping, as we may accidentally delete pods from unready sources.
			glog.V(4).Infof("SyncLoop (housekeeping, skipped): sources aren't ready yet.")
		} else {
			glog.V(4).Infof("SyncLoop (housekeeping)")
			if err := handler.HandlePodCleanups(); err != nil {
				glog.Errorf("Failed cleaning pods: %v", err)
			}
		}
	}
	kl.syncLoopMonitor.Store(kl.clock.Now())
	return true
}

// dispatchWork starts the asynchronous sync of the pod in a pod worker.
// If the pod is terminated, dispatchWork
//调度Pod工作?
//被handleMirrorPod(),HandlePodAddtion(),HandlePodUpdate(),HandlerPodSync()
func (kl *Kubelet) dispatchWork(pod *v1.Pod, syncType kubetypes.SyncPodType, mirrorPod *v1.Pod, start time.Time) {
	if kl.podIsTerminated(pod) {
		if pod.DeletionTimestamp != nil {
			// If the pod is in a terminated state, there is no pod worker to
			// handle the work item. Check if the DeletionTimestamp has been
			// set, and force a status update to trigger a pod deletion request
			// to the apiserver.
			//更新Pod中容器的状态为Terminated
			kl.statusManager.TerminatePod(pod)
		}
		return
	}
	// Run the sync in an async worker.
	kl.podWorkers.UpdatePod(&UpdatePodOptions{
		Pod:        pod,
		MirrorPod:  mirrorPod,
		UpdateType: syncType,
		OnCompleteFunc: func(err error) {
			if err != nil {
				metrics.PodWorkerLatency.WithLabelValues(syncType.String()).Observe(metrics.SinceInMicroseconds(start))
			}
		},
	})
	// Note the number of containers for new pods.
	if syncType == kubetypes.SyncPodCreate {
		metrics.ContainersPerPodCount.Observe(float64(len(pod.Spec.Containers)))
	}
}

// TODO: handle mirror pods in a separate component (issue #17251)
func (kl *Kubelet) handleMirrorPod(mirrorPod *v1.Pod, start time.Time) {
	// Mirror pod ADD/UPDATE/DELETE operations are considered an UPDATE to the
	// corresponding static pod. Send update to the pod worker if the static
	// pod exists.
	if pod, ok := kl.podManager.GetPodByMirrorPod(mirrorPod); ok {
		kl.dispatchWork(pod, kubetypes.SyncPodUpdate, mirrorPod, start)
	}
}

// HandlePodAdditions is the callback in SyncHandler for pods being added from
// a config source.
func (kl *Kubelet) HandlePodAdditions(pods []*v1.Pod) {
	start := kl.clock.Now()

	// Pass critical pods through admission check first.
	var criticalPods []*v1.Pod
	var nonCriticalPods []*v1.Pod
	//对Pod进行重要和非重要划分
	for _, p := range pods {
		if kubetypes.IsCriticalPod(p) {
			criticalPods = append(criticalPods, p)
		} else {
			nonCriticalPods = append(nonCriticalPods, p)
		}
	}
	//按照创建时间排序
	sort.Sort(sliceutils.PodsByCreationTime(criticalPods))
	sort.Sort(sliceutils.PodsByCreationTime(nonCriticalPods))

	//遍历所有的Pod,优先处理先创建的重要Pod,再处理先创建的非重要Pod
	for _, pod := range append(criticalPods, nonCriticalPods...) {
		//获取Kubelet上所有的Pod
		existingPods := kl.podManager.GetPods()
		// Always add the pod to the pod manager. Kubelet relies on the pod
		// manager as the source of truth for the desired state. If a pod does
		// not exist in the pod manager, it means that it has been deleted in
		// the apiserver and no action (other than cleanup) is required.
		//添加Pod的信息到kubelet的Pod manager
		kl.podManager.AddPod(pod)

		if kubepod.IsMirrorPod(pod) {
			kl.handleMirrorPod(pod, start)
			continue
		}

		//pod是否仍处于激活状态
		if !kl.podIsTerminated(pod) {
			// Only go through the admission process if the pod is not
			// terminated.

			// We failed pods that we rejected, so activePods include all admitted
			// pods that are alive.
			//找出仍然存活的pod
			activePods := kl.filterOutTerminatedPods(existingPods)

			// Check if we can admit the pod; if not, reject it.
			//决策一个Pod是否允许接收
			if ok, reason, message := kl.canAdmitPod(activePods, pod); !ok {
				//更新Pod的状态为PodFailed以及相应的拒绝信息
				kl.rejectPod(pod, reason, message)
				continue
			}
		}
		mirrorPod, _ := kl.podManager.GetMirrorPodByPod(pod)
		kl.dispatchWork(pod, kubetypes.SyncPodCreate, mirrorPod, start)
		kl.probeManager.AddPod(pod)
	}
}

// HandlePodUpdates is the callback in the SyncHandler interface for pods
// being updated from a config source.
func (kl *Kubelet) HandlePodUpdates(pods []*v1.Pod) {
	start := kl.clock.Now()
	for _, pod := range pods {
		kl.podManager.UpdatePod(pod)
		if kubepod.IsMirrorPod(pod) {
			kl.handleMirrorPod(pod, start)
			continue
		}
		// TODO: Evaluate if we need to validate and reject updates.

		mirrorPod, _ := kl.podManager.GetMirrorPodByPod(pod)
		kl.dispatchWork(pod, kubetypes.SyncPodUpdate, mirrorPod, start)
	}
}

// HandlePodRemoves is the callback in the SyncHandler interface for pods
// being removed from a config source.
func (kl *Kubelet) HandlePodRemoves(pods []*v1.Pod) {
	start := kl.clock.Now()
	for _, pod := range pods {
		kl.podManager.DeletePod(pod)
		if kubepod.IsMirrorPod(pod) {
			kl.handleMirrorPod(pod, start)
			continue
		}
		// Deletion is allowed to fail because the periodic cleanup routine
		// will trigger deletion again.
		if err := kl.deletePod(pod); err != nil {
			glog.V(2).Infof("Failed to delete pod %q, err: %v", format.Pod(pod), err)
		}
		kl.probeManager.RemovePod(pod)
	}
}

// HandlePodReconcile is the callback in the SyncHandler interface for pods
// that should be reconciled.
//对所有Pod进行更新,如果Pod被驱逐了,则移除该Pod中所有Exited的容器.
func (kl *Kubelet) HandlePodReconcile(pods []*v1.Pod) {
	for _, pod := range pods {
		// Update the pod in pod manager, status manager will do periodically reconcile according
		// to the pod manager.
		kl.podManager.UpdatePod(pod)

		// After an evicted pod is synced, all dead containers in the pod can be removed.
		//如果Pod处于驱逐状态,则删除Pod中所有已退出的容器
		if eviction.PodIsEvicted(pod.Status) {
			if podStatus, err := kl.podCache.Get(pod.UID); err == nil {
				kl.containerDeletor.deleteContainersInPod("", podStatus, true)
			}
		}
	}
}

// HandlePodSyncs is the callback in the syncHandler interface for pods
// that should be dispatched to pod workers for sync.
func (kl *Kubelet) HandlePodSyncs(pods []*v1.Pod) {
	start := kl.clock.Now()
	for _, pod := range pods {
		mirrorPod, _ := kl.podManager.GetMirrorPodByPod(pod)
		kl.dispatchWork(pod, kubetypes.SyncPodSync, mirrorPod, start)
	}
}

// LatestLoopEntryTime returns the last time in the sync loop monitor.
func (kl *Kubelet) LatestLoopEntryTime() time.Time {
	val := kl.syncLoopMonitor.Load()
	if val == nil {
		return time.Time{}
	}
	return val.(time.Time)
}

// PLEGHealthCheck returns whether the PLEG is healthy.
func (kl *Kubelet) PLEGHealthCheck() (bool, error) {
	return kl.pleg.Healthy()
}

// updateRuntimeUp calls the container runtime status callback, initializing
// the runtime dependent modules when the container runtime first comes up,
// and returns an error if the status check fails.  If the status check is OK,
// update the container runtime uptime in the kubelet runtimeState.
//负责更新kubelet的运行时的状态包括网络错误,容器管理器的状态等
//获取容器管理器的状态,更新网络状态
//调用驱逐管理器对未处于终止状态的Pod进行驱逐决议
func (kl *Kubelet) updateRuntimeUp() {
	//容器管理器的状态
	s, err := kl.containerRuntime.Status()
	if err != nil {
		glog.Errorf("Container runtime sanity check failed: %v", err)
		return
	}
	// Only check specific conditions when runtime integration type is cri,
	// because the old integration doesn't populate any runtime condition.
	if kl.kubeletConfiguration.EnableCRI {
		if s == nil {
			glog.Errorf("Container runtime status is nil")
			return
		}
		// Periodically log the whole runtime status for debugging.
		// TODO(random-liu): Consider to send node event when optional
		// condition is unmet.
		glog.V(4).Infof("Container runtime status: %v", s)
		networkReady := s.GetRuntimeCondition(kubecontainer.NetworkReady)
		//根据容器的网络状态,设置
		if networkReady == nil || !networkReady.Status {
			glog.Errorf("Container runtime network not ready: %v", networkReady)
			kl.runtimeState.setNetworkState(fmt.Errorf("runtime network not ready: %v", networkReady))
		} else {
			// Set nil if the containe runtime network is ready.
			kl.runtimeState.setNetworkState(nil)
		}
		// TODO(random-liu): Add runtime error in runtimeState, and update it
		// when runtime is not ready, so that the information in RuntimeReady
		// condition will be propagated to NodeReady condition.
		runtimeReady := s.GetRuntimeCondition(kubecontainer.RuntimeReady)
		// If RuntimeReady is not set or is false, report an error.
		if runtimeReady == nil || !runtimeReady.Status {
			glog.Errorf("Container runtime not ready: %v", runtimeReady)
			return
		}
	}
	//进行驱逐决议
	kl.oneTimeInitializer.Do(kl.initializeRuntimeDependentModules)
	//更新同步时间
	kl.runtimeState.setRuntimeSync(kl.clock.Now())
}

// updateCloudProviderFromMachineInfo updates the node's provider ID field
// from the given cadvisor machine info.
func (kl *Kubelet) updateCloudProviderFromMachineInfo(node *v1.Node, info *cadvisorapi.MachineInfo) {
	if info.CloudProvider != cadvisorapi.UnknownProvider &&
		info.CloudProvider != cadvisorapi.Baremetal {
		// The cloud providers from pkg/cloudprovider/providers/* that update ProviderID
		// will use the format of cloudprovider://project/availability_zone/instance_name
		// here we only have the cloudprovider and the instance name so we leave project
		// and availability zone empty for compatibility.
		node.Spec.ProviderID = strings.ToLower(string(info.CloudProvider)) +
			":////" + string(info.InstanceID)
	}
}

// GetConfiguration returns the KubeletConfiguration used to configure the kubelet.
func (kl *Kubelet) GetConfiguration() componentconfig.KubeletConfiguration {
	return kl.kubeletConfiguration
}

// BirthCry sends an event that the kubelet has started up.
//记录kubelet的事件
func (kl *Kubelet) BirthCry() {
	// Make an event that kubelet restarted.
	kl.recorder.Eventf(kl.nodeRef, v1.EventTypeNormal, events.StartingKubelet, "Starting kubelet.")
}

// StreamingConnectionIdleTimeout returns the timeout for streaming connections to the HTTP server.
func (kl *Kubelet) StreamingConnectionIdleTimeout() time.Duration {
	return kl.streamingConnectionIdleTimeout
}

// ResyncInterval returns the interval used for periodic syncs.
func (kl *Kubelet) ResyncInterval() time.Duration {
	return kl.resyncInterval
}

// ListenAndServe runs the kubelet HTTP server.
func (kl *Kubelet) ListenAndServe(address net.IP, port uint, tlsOptions *server.TLSOptions, auth server.AuthInterface, enableDebuggingHandlers bool) {
	server.ListenAndServeKubeletServer(kl, kl.resourceAnalyzer, address, port, tlsOptions, auth, enableDebuggingHandlers, kl.containerRuntime, kl.criHandler)
}

// ListenAndServeReadOnly runs the kubelet HTTP server in read-only mode.
func (kl *Kubelet) ListenAndServeReadOnly(address net.IP, port uint) {
	server.ListenAndServeKubeletReadOnlyServer(kl, kl.resourceAnalyzer, address, port, kl.containerRuntime)
}

// Delete the eligible dead container instances in a pod. Depending on the configuration, the latest dead containers may be kept around.
//找到指定Pod中已退出的容器,如果pod被驱逐了,则直接删除该容器,否则添加该容器到待删除列表.
func (kl *Kubelet) cleanUpContainersInPod(podId types.UID, exitedContainerID string) {
	//获取Pod的状态,如果pod被驱逐,
	if podStatus, err := kl.podCache.Get(podId); err == nil {
		removeAll := false
		if syncedPod, ok := kl.podManager.GetPodByUID(podId); ok {
			// When an evicted pod has already synced, all containers can be removed.
			//如果pod是由于被驱逐导致移除的
			removeAll = eviction.PodIsEvicted(syncedPod.Status)
		}
		//
		kl.containerDeletor.deleteContainersInPod(exitedContainerID, podStatus, removeAll)
	}
}

// isSyncPodWorthy filters out events that are not worthy of pod syncing
//Pod的事件为容器移除
func isSyncPodWorthy(event *pleg.PodLifecycleEvent) bool {
	// ContatnerRemoved doesn't affect pod state
	return event.Type != pleg.ContainerRemoved
}

// parseResourceList parses the given configuration map into an API
// ResourceList or returns an error.
//从配置中解析资源列表, 目前仅支持Memory和Cpu
func parseResourceList(m utilconfig.ConfigurationMap) (v1.ResourceList, error) {
	rl := make(v1.ResourceList)
	for k, v := range m {
		switch v1.ResourceName(k) {
		// Only CPU and memory resources are supported.
		case v1.ResourceCPU, v1.ResourceMemory:
			q, err := resource.ParseQuantity(v)
			if err != nil {
				return nil, err
			}
			if q.Sign() == -1 {
				return nil, fmt.Errorf("resource quantity for %q cannot be negative: %v", k, v)
			}
			rl[v1.ResourceName(k)] = q
		default:
			return nil, fmt.Errorf("cannot reserve %q resource", k)
		}
	}
	return rl, nil
}

// ParseReservation parses the given kubelet- and system- reservations
// configuration maps into an internal Reservation instance or returns an
// error.
func ParseReservation(kubeReserved, systemReserved utilconfig.ConfigurationMap) (*kubetypes.Reservation, error) {
	reservation := new(kubetypes.Reservation) //用于记录k8s组件和非K8s组件的资源的数量
	//解析配置中需要保留的资源, 目前只支持CPU和Memory,kube 组件保存的资源?是k8s集群的组件(所有组件?),还是包括运行的Pod
	if rl, err := parseResourceList(kubeReserved); err != nil {
		return nil, err
	} else {
		reservation.Kubernetes = rl
	}
	//如果使用的资源达到了上限,将会执行什么样的动作?
	if rl, err := parseResourceList(systemReserved); err != nil {
		return nil, err
	} else {
		reservation.System = rl
	}
	return reservation, nil
}

// Gets the streaming server configuration to use with in-process CRI shims.
func getStreamingConfig(kubeCfg *componentconfig.KubeletConfiguration, kubeDeps *KubeletDeps) *streaming.Config {
	config := &streaming.Config{
		// Use a relative redirect (no scheme or host).
		BaseURL: &url.URL{
			Path: "/cri/", //这个url的作用?
		},
		StreamIdleTimeout:     kubeCfg.StreamingConnectionIdleTimeout.Duration,
		StreamCreationTimeout: streaming.DefaultConfig.StreamCreationTimeout,
		SupportedProtocols:    streaming.DefaultConfig.SupportedProtocols,
	}
	if kubeDeps.TLSOptions != nil {
		config.TLSConfig = kubeDeps.TLSOptions.Config
	}
	return config
}
