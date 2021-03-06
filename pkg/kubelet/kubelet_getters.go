/*
Copyright 2016 The Kubernetes Authors.

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
	"io/ioutil"
	"net"
	"path"

	"github.com/golang/glog"
	"k8s.io/kubernetes/cmd/kubelet/app/options"
	"k8s.io/kubernetes/pkg/api/v1"
	"k8s.io/kubernetes/pkg/kubelet/cm"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/types"
	"k8s.io/kubernetes/pkg/util"
	nodeutil "k8s.io/kubernetes/pkg/util/node"
	volumeutil "k8s.io/kubernetes/pkg/volume/util"
)

// getRootDir returns the full path to the directory under which kubelet can
// store data.  These functions are useful to pass interfaces to other modules
// that may need to know where to write data without getting a whole kubelet
// instance.
//获取kubelet存放数据的根目录,由kubelet启动参数决定,默认为/var/lib/kubelet
func (kl *Kubelet) getRootDir() string {
	return kl.rootDirectory
}

// getPodsDir returns the full path to the directory under which pod
// directories are created.
//获取kubelet存放的Pod数据的目录
func (kl *Kubelet) getPodsDir() string {
	return path.Join(kl.getRootDir(), options.DefaultKubeletPodsDirName)
}

// getPluginsDir returns the full path to the directory under which plugin
// directories are created.  Plugins can use these directories for data that
// they need to persist.  Plugins should create subdirectories under this named
// after their own names.
//获取kubelet存放的plugins数据的目录.怎么安装插件, 有些哪些插件可以安装
//https://kubernetes.io/docs/concepts/cluster-administration/network-plugins/
func (kl *Kubelet) getPluginsDir() string {
	return path.Join(kl.getRootDir(), options.DefaultKubeletPluginsDirName)
}

// getPluginDir returns a data directory name for a given plugin name.
// Plugins can use these directories to store data that they need to persist.
// For per-pod plugin data, see getPodPluginDir.
//获取kubelet指定插件的目录
func (kl *Kubelet) getPluginDir(pluginName string) string {
	return path.Join(kl.getPluginsDir(), pluginName)
}

// GetPodDir returns the full path to the per-pod data directory for the
// specified pod. This directory may not exist if the pod does not exist.
//获取指定pod的目录
func (kl *Kubelet) GetPodDir(podUID types.UID) string {
	return kl.getPodDir(podUID)
}

// getPodDir returns the full path to the per-pod directory for the pod with
// the given UID.
func (kl *Kubelet) getPodDir(podUID types.UID) string {
	// Backwards compat.  The "old" stuff should be removed before 1.0
	// release.  The thinking here is this:
	//     !old && !new = use new
	//     !old && new  = use new
	//     old && !new  = use old
	//     old && new   = use new (but warn)
	oldPath := path.Join(kl.getRootDir(), string(podUID))
	oldExists := dirExists(oldPath)
	newPath := path.Join(kl.getPodsDir(), string(podUID))
	newExists := dirExists(newPath)
	if oldExists && !newExists {
		return oldPath
	}
	if oldExists {
		glog.Warningf("Data dir for pod %q exists in both old and new form, using new", podUID)
	}
	return newPath
}

// getPodVolumesDir returns the full path to the per-pod data directory under
// which volumes are created for the specified pod.  This directory may not
// exist if the pod does not exist.
func (kl *Kubelet) getPodVolumesDir(podUID types.UID) string {
	return path.Join(kl.getPodDir(podUID), options.DefaultKubeletVolumesDirName)
}

// getPodVolumeDir returns the full path to the directory which represents the
// named volume under the named plugin for specified pod.  This directory may not
// exist if the pod does not exist.
func (kl *Kubelet) getPodVolumeDir(podUID types.UID, pluginName string, volumeName string) string {
	return path.Join(kl.getPodVolumesDir(podUID), pluginName, volumeName)
}

// getPodPluginsDir returns the full path to the per-pod data directory under
// which plugins may store data for the specified pod.  This directory may not
// exist if the pod does not exist.
func (kl *Kubelet) getPodPluginsDir(podUID types.UID) string {
	return path.Join(kl.getPodDir(podUID), options.DefaultKubeletPluginsDirName)
}

// getPodPluginDir returns a data directory name for a given plugin name for a
// given pod UID.  Plugins can use these directories to store data that they
// need to persist.  For non-per-pod plugin data, see getPluginDir.
func (kl *Kubelet) getPodPluginDir(podUID types.UID, pluginName string) string {
	return path.Join(kl.getPodPluginsDir(podUID), pluginName)
}

// getPodContainerDir returns the full path to the per-pod data directory under
// which container data is held for the specified pod.  This directory may not
// exist if the pod or container does not exist.
func (kl *Kubelet) getPodContainerDir(podUID types.UID, ctrName string) string {
	// Backwards compat.  The "old" stuff should be removed before 1.0
	// release.  The thinking here is this:
	//     !old && !new = use new
	//     !old && new  = use new
	//     old && !new  = use old
	//     old && new   = use new (but warn)
	oldPath := path.Join(kl.getPodDir(podUID), ctrName)
	oldExists := dirExists(oldPath)
	newPath := path.Join(kl.getPodDir(podUID), options.DefaultKubeletContainersDirName, ctrName)
	newExists := dirExists(newPath)
	if oldExists && !newExists {
		return oldPath
	}
	if oldExists {
		glog.Warningf("Data dir for pod %q, container %q exists in both old and new form, using new", podUID, ctrName)
	}
	return newPath
}

// GetPods returns all pods bound to the kubelet and their spec, and the mirror
// pods.
//获取kubelet上绑定的正常pods
func (kl *Kubelet) GetPods() []*v1.Pod {
	return kl.podManager.GetPods()
}

// GetRunningPods returns all pods running on kubelet from looking at the
// container runtime cache. This function converts kubecontainer.Pod to
// v1.Pod, so only the fields that exist in both kubecontainer.Pod and
// v1.Pod are considered meaningful.
//获取kubelet运行时缓存中记录的所有Pod, 转换成api pod,并返回
func (kl *Kubelet) GetRunningPods() ([]*v1.Pod, error) {
	pods, err := kl.runtimeCache.GetPods()
	if err != nil {
		return nil, err
	}

	apiPods := make([]*v1.Pod, 0, len(pods))
	for _, pod := range pods {
		apiPods = append(apiPods, pod.ToAPIPod())
	}
	return apiPods, nil
}

// GetPodByFullName gets the pod with the given 'full' name, which
// incorporates the namespace as well as whether the pod was found.
//由上面来看, podFullName中包含了namespace, podFullName = pod + "_" + namespace
func (kl *Kubelet) GetPodByFullName(podFullName string) (*v1.Pod, bool) {
	return kl.podManager.GetPodByFullName(podFullName)
}

// GetPodByName provides the first pod that matches namespace and name, as well
// as whether the pod was found.
func (kl *Kubelet) GetPodByName(namespace, name string) (*v1.Pod, bool) {
	return kl.podManager.GetPodByName(namespace, name)
}

// GetHostname Returns the hostname as the kubelet sees it.
func (kl *Kubelet) GetHostname() string {
	return kl.hostname
}

// GetRuntime returns the current Runtime implementation in use by the kubelet. This func
// is exported to simplify integration with third party kubelet extensions (e.g. kubernetes-mesos).
func (kl *Kubelet) GetRuntime() kubecontainer.Runtime {
	return kl.containerRuntime
}

// GetNode returns the node info for the configured node name of this Kubelet.
//??kubelet的节点名和主机名的区别,主机名通过uname -n获取,可以通过--host-override来修改,nodename是kubelet提供给apiserver节点的名字,通常等于hostname
func (kl *Kubelet) GetNode() (*v1.Node, error) {
	if kl.standaloneMode {
		return kl.initialNode()
	}
	return kl.nodeInfo.GetNodeInfo(string(kl.nodeName))
}

// getNodeAnyWay() must return a *v1.Node which is required by RunGeneralPredicates().
// The *v1.Node is obtained as follows:
// Return kubelet's nodeInfo for this node, except on error or if in standalone mode,
// in which case return a manufactured nodeInfo representing a node with no pods,
// zero capacity, and the default labels.
//
func (kl *Kubelet) getNodeAnyWay() (*v1.Node, error) {
	// 如果kubelet是standalone模式,则获取相应节点的信息
	if !kl.standaloneMode {
		if n, err := kl.nodeInfo.GetNodeInfo(string(kl.nodeName)); err == nil {
			return n, nil
		}
	}
	//否则根据kubelet创建一个api node
	return kl.initialNode()
}

// GetNodeConfig returns the container manager node config.
func (kl *Kubelet) GetNodeConfig() cm.NodeConfig {
	return kl.containerManager.GetNodeConfig()
}

// Returns host IP or nil in case of error.
func (kl *Kubelet) GetHostIP() (net.IP, error) {
	node, err := kl.GetNode()
	if err != nil {
		return nil, fmt.Errorf("cannot get node: %v", err)
	}
	return nodeutil.GetNodeHostIP(node)
}

// getHostIPAnyway attempts to return the host IP from kubelet's nodeInfo, or
// the initialNode.
func (kl *Kubelet) getHostIPAnyWay() (net.IP, error) {
	node, err := kl.getNodeAnyWay()
	if err != nil {
		return nil, err
	}
	return nodeutil.GetNodeHostIP(node)
}

// GetExtraSupplementalGroupsForPod returns a list of the extra
// supplemental groups for the Pod. These extra supplemental groups come
// from annotations on persistent volumes that the pod depends on.
func (kl *Kubelet) GetExtraSupplementalGroupsForPod(pod *v1.Pod) []int64 {
	return kl.volumeManager.GetExtraSupplementalGroupsForPod(pod)
}

// getPodVolumePathListFromDisk returns a list of the volume paths by reading the
// volume directories for the given pod from the disk.
func (kl *Kubelet) getPodVolumePathListFromDisk(podUID types.UID) ([]string, error) {
	volumes := []string{}
	podVolDir := kl.getPodVolumesDir(podUID)

	if pathExists, pathErr := volumeutil.PathExists(podVolDir); pathErr != nil {
		return volumes, fmt.Errorf("Error checking if path %q exists: %v", podVolDir, pathErr)
	} else if !pathExists {
		glog.Warningf("Warning: path %q does not exist: %q", podVolDir)
		return volumes, nil
	}

	volumePluginDirs, err := ioutil.ReadDir(podVolDir)
	if err != nil {
		glog.Errorf("Could not read directory %s: %v", podVolDir, err)
		return volumes, err
	}
	for _, volumePluginDir := range volumePluginDirs {
		volumePluginName := volumePluginDir.Name()
		volumePluginPath := path.Join(podVolDir, volumePluginName)
		volumeDirs, err := util.ReadDirNoStat(volumePluginPath)
		if err != nil {
			return volumes, fmt.Errorf("Could not read directory %s: %v", volumePluginPath, err)
		}
		for _, volumeDir := range volumeDirs {
			volumes = append(volumes, path.Join(volumePluginPath, volumeDir))
		}
	}
	return volumes, nil
}
