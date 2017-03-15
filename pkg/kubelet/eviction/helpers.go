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

package eviction

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/golang/glog"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/resource"
	"k8s.io/kubernetes/pkg/api/v1"
	statsapi "k8s.io/kubernetes/pkg/kubelet/api/v1alpha1/stats"
	"k8s.io/kubernetes/pkg/kubelet/qos"
	"k8s.io/kubernetes/pkg/kubelet/server/stats"
	"k8s.io/kubernetes/pkg/quota/evaluator/core"
	"k8s.io/kubernetes/pkg/util/sets"
)

const (
	unsupportedEvictionSignal = "unsupported eviction signal %v"
	// the reason reported back in status.
	reason = "Evicted"
	// the message associated with the reason.
	message = "The node was low on resource: %v."
	// disk, in bytes.  internal to this module, used to account for local disk usage.
	resourceDisk v1.ResourceName = "disk"
	// inodes, number. internal to this module, used to account for local disk inode consumption.
	resourceInodes v1.ResourceName = "inodes"
	// imagefs, in bytes.  internal to this module, used to account for local image filesystem usage.
	//什么是imagefs?用来存储镜像和容器的可写层
	resourceImageFs v1.ResourceName = "imagefs"
	// imagefs inodes, number.  internal to this module, used to account for local image filesystem inodes.
	resourceImageFsInodes v1.ResourceName = "imagefsInodes"
	// nodefs, in bytes.  internal to this module, used to account for local node root filesystem usage.
	resourceNodeFs v1.ResourceName = "nodefs"
	// nodefs inodes, number.  internal to this module, used to account for local node root filesystem inodes.
	resourceNodeFsInodes v1.ResourceName = "nodefsInodes"
)

var (
	// signalToNodeCondition maps a signal to the node condition to report if threshold is met.
	signalToNodeCondition map[Signal]v1.NodeConditionType
	// signalToResource maps a Signal to its associated Resource.
	signalToResource map[Signal]v1.ResourceName
	// resourceToSignal maps a Resource to its associated Signal
	resourceToSignal map[v1.ResourceName]Signal
)

func init() {
	// map eviction signals to node conditions
	//映射驱逐信号和节点健康状态
	signalToNodeCondition = map[Signal]v1.NodeConditionType{}
	signalToNodeCondition[SignalMemoryAvailable] = v1.NodeMemoryPressure
	signalToNodeCondition[SignalImageFsAvailable] = v1.NodeDiskPressure
	signalToNodeCondition[SignalNodeFsAvailable] = v1.NodeDiskPressure
	signalToNodeCondition[SignalImageFsInodesFree] = v1.NodeDiskPressure
	signalToNodeCondition[SignalNodeFsInodesFree] = v1.NodeDiskPressure

	// map signals to resources (and vice-versa)
	//映射驱逐信号和资源
	signalToResource = map[Signal]v1.ResourceName{}
	signalToResource[SignalMemoryAvailable] = v1.ResourceMemory
	signalToResource[SignalImageFsAvailable] = resourceImageFs
	signalToResource[SignalImageFsInodesFree] = resourceImageFsInodes
	signalToResource[SignalNodeFsAvailable] = resourceNodeFs
	signalToResource[SignalNodeFsInodesFree] = resourceNodeFsInodes
	resourceToSignal = map[v1.ResourceName]Signal{}
	for key, value := range signalToResource {
		resourceToSignal[value] = key
	}
}

// validSignal returns true if the signal is supported.
//检测是否是有效的信号
func validSignal(signal Signal) bool {
	_, found := signalToResource[signal]
	return found
}

// ParseThresholdConfig parses the flags for thresholds.
//解析回收阈值配置字符串成阈值对象
func ParseThresholdConfig(evictionHard, evictionSoft, evictionSoftGracePeriod, evictionMinimumReclaim string) ([]Threshold, error) {
	results := []Threshold{}

	//将阈值字符串解析成阈值描述符
	hardThresholds, err := parseThresholdStatements(evictionHard)
	if err != nil {
		return nil, err
	}
	results = append(results, hardThresholds...)

	softThresholds, err := parseThresholdStatements(evictionSoft)
	if err != nil {
		return nil, err
	}
	gracePeriods, err := parseGracePeriods(evictionSoftGracePeriod)
	if err != nil {
		return nil, err
	}
	minReclaims, err := parseMinimumReclaims(evictionMinimumReclaim)
	if err != nil {
		return nil, err
	}
	//检测
	for i := range softThresholds {
		signal := softThresholds[i].Signal
		period, found := gracePeriods[signal]
		if !found {
			return nil, fmt.Errorf("grace period must be specified for the soft eviction threshold %v", signal)
		}
		softThresholds[i].GracePeriod = period
	}
	results = append(results, softThresholds...)
	for i := range results {
		for signal, minReclaim := range minReclaims {
			if results[i].Signal == signal {
				results[i].MinReclaim = &minReclaim
				break
			}
		}
	}
	return results, nil
}

// parseThresholdStatements parses the input statements into a list of Threshold objects.
func parseThresholdStatements(expr string) ([]Threshold, error) {
	if len(expr) == 0 {
		return nil, nil
	}
	results := []Threshold{}
	//通过","表达式进行切割
	statements := strings.Split(expr, ",")
	signalsFound := sets.NewString()
	for _, statement := range statements {
		result, err := parseThresholdStatement(statement)
		if err != nil {
			return nil, err
		}
		if signalsFound.Has(string(result.Signal)) {
			return nil, fmt.Errorf("found duplicate eviction threshold for signal %v", result.Signal)
		}
		signalsFound.Insert(string(result.Signal))
		results = append(results, result)
	}
	return results, nil
}

// parseThresholdStatement parses a threshold statement.
//解析阈值字符串成阈值对象
func parseThresholdStatement(statement string) (Threshold, error) {
	tokens2Operator := map[string]ThresholdOperator{
		"<": OpLessThan,
	}
	var (
		operator ThresholdOperator
		parts    []string
	)
	for token := range tokens2Operator {
		parts = strings.Split(statement, token)
		// if we got a token, we know this was the operator...
		if len(parts) > 1 {
			operator = tokens2Operator[token]
			break
		}
	}
	if len(operator) == 0 || len(parts) != 2 {
		return Threshold{}, fmt.Errorf("invalid eviction threshold syntax %v, expected <signal><operator><value>", statement)
	}
	signal := Signal(parts[0])
	if !validSignal(signal) {
		return Threshold{}, fmt.Errorf(unsupportedEvictionSignal, signal)
	}

	//阈值
	quantityValue := parts[1]
	//值是否为百分比
	if strings.HasSuffix(quantityValue, "%") {
		percentage, err := parsePercentage(quantityValue)
		if err != nil {
			return Threshold{}, err
		}
		if percentage <= 0 {
			return Threshold{}, fmt.Errorf("eviction percentage threshold %v must be positive: %s", signal, quantityValue)
		}
		return Threshold{
			Signal:   signal,
			Operator: operator,
			Value: ThresholdValue{
				Percentage: percentage,
			},
		}, nil
	}
	quantity, err := resource.ParseQuantity(quantityValue)
	if err != nil {
		return Threshold{}, err
	}
	if quantity.Sign() < 0 || quantity.IsZero() {
		return Threshold{}, fmt.Errorf("eviction threshold %v must be positive: %s", signal, &quantity)
	}
	return Threshold{
		Signal:   signal,
		Operator: operator,
		Value: ThresholdValue{
			Quantity: &quantity,
		},
	}, nil
}

// parsePercentage parses a string representing a percentage value
func parsePercentage(input string) (float32, error) {
	value, err := strconv.ParseFloat(strings.TrimRight(input, "%"), 32)
	if err != nil {
		return 0, err
	}
	return float32(value) / 100, nil
}

// parseGracePeriods parses the grace period statements
//获得宽限期时间
func parseGracePeriods(expr string) (map[Signal]time.Duration, error) {
	if len(expr) == 0 {
		return nil, nil
	}
	results := map[Signal]time.Duration{}
	statements := strings.Split(expr, ",")
	for _, statement := range statements {
		parts := strings.Split(statement, "=")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid eviction grace period syntax %v, expected <signal>=<duration>", statement)
		}
		signal := Signal(parts[0])
		if !validSignal(signal) {
			return nil, fmt.Errorf(unsupportedEvictionSignal, signal)
		}

		gracePeriod, err := time.ParseDuration(parts[1])
		if err != nil {
			return nil, err
		}
		if gracePeriod < 0 {
			return nil, fmt.Errorf("invalid eviction grace period specified: %v, must be a positive value", parts[1])
		}

		// check against duplicate statements
		if _, found := results[signal]; found {
			return nil, fmt.Errorf("duplicate eviction grace period specified for %v", signal)
		}
		results[signal] = gracePeriod
	}
	return results, nil
}

// parseMinimumReclaims parses the minimum reclaim statements
//解析最小回收字符串表达式,转换成相应的数值表
func parseMinimumReclaims(expr string) (map[Signal]ThresholdValue, error) {
	if len(expr) == 0 {
		return nil, nil
	}
	results := map[Signal]ThresholdValue{}
	statements := strings.Split(expr, ",")
	for _, statement := range statements {
		parts := strings.Split(statement, "=")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid eviction minimum reclaim syntax: %v, expected <signal>=<value>", statement)
		}
		signal := Signal(parts[0])
		if !validSignal(signal) {
			return nil, fmt.Errorf(unsupportedEvictionSignal, signal)
		}

		quantityValue := parts[1]
		if strings.HasSuffix(quantityValue, "%") {
			percentage, err := parsePercentage(quantityValue)
			if err != nil {
				return nil, err
			}
			if percentage <= 0 {
				return nil, fmt.Errorf("eviction percentage minimum reclaim %v must be positive: %s", signal, quantityValue)
			}
			// check against duplicate statements
			if _, found := results[signal]; found {
				return nil, fmt.Errorf("duplicate eviction minimum reclaim specified for %v", signal)
			}
			results[signal] = ThresholdValue{
				Percentage: percentage,
			}
			continue
		}
		// check against duplicate statements
		if _, found := results[signal]; found {
			return nil, fmt.Errorf("duplicate eviction minimum reclaim specified for %v", signal)
		}
		quantity, err := resource.ParseQuantity(parts[1])
		if quantity.Sign() < 0 {
			return nil, fmt.Errorf("negative eviction minimum reclaim specified for %v", signal)
		}
		if err != nil {
			return nil, err
		}
		results[signal] = ThresholdValue{
			Quantity: &quantity,
		}
	}
	return results, nil
}

// diskUsage converts used bytes into a resource quantity.
func diskUsage(fsStats *statsapi.FsStats) *resource.Quantity {
	if fsStats == nil || fsStats.UsedBytes == nil {
		return &resource.Quantity{Format: resource.BinarySI}
	}
	usage := int64(*fsStats.UsedBytes)
	return resource.NewQuantity(usage, resource.BinarySI)
}

// inodeUsage converts inodes consumed into a resource quantity.
func inodeUsage(fsStats *statsapi.FsStats) *resource.Quantity {
	if fsStats == nil || fsStats.InodesUsed == nil {
		return &resource.Quantity{Format: resource.BinarySI}
	}
	usage := int64(*fsStats.InodesUsed)
	return resource.NewQuantity(usage, resource.BinarySI)
}

// memoryUsage converts working set into a resource quantity.
func memoryUsage(memStats *statsapi.MemoryStats) *resource.Quantity {
	if memStats == nil || memStats.WorkingSetBytes == nil {
		return &resource.Quantity{Format: resource.BinarySI}
	}
	usage := int64(*memStats.WorkingSetBytes)
	return resource.NewQuantity(usage, resource.BinarySI)
}

// localVolumeNames returns the set of volumes for the pod that are local
// TODO: sumamry API should report what volumes consume local storage rather than hard-code here.
func localVolumeNames(pod *v1.Pod) []string {
	result := []string{}
	for _, volume := range pod.Spec.Volumes {
		if volume.HostPath != nil ||
			(volume.EmptyDir != nil && volume.EmptyDir.Medium != v1.StorageMediumMemory) ||
			volume.ConfigMap != nil ||
			volume.GitRepo != nil {
			result = append(result, volume.Name)
		}
	}
	return result
}

// podDiskUsage aggregates pod disk usage and inode consumption for the specified stats to measure.
//根据要计算的文件系统资源统计类型,获取一个Pod的所有容器的相应资源统计
func podDiskUsage(podStats statsapi.PodStats, pod *v1.Pod, statsToMeasure []fsStatsType) (v1.ResourceList, error) {
	disk := resource.Quantity{Format: resource.BinarySI}
	inodes := resource.Quantity{Format: resource.BinarySI}
	//遍历统计中的所有容器
	for _, container := range podStats.Containers {
		// 检测是否要计算Pod容器根文件系统的容量
		if hasFsStatsType(statsToMeasure, fsStatsRoot) {
			//添加容器的
			disk.Add(*diskUsage(container.Rootfs))
			inodes.Add(*inodeUsage(container.Rootfs))
		}
		// 检测是否要计算Pod中容器日志所占用的空间
		if hasFsStatsType(statsToMeasure, fsStatsLogs) {
			disk.Add(*diskUsage(container.Logs))
			inodes.Add(*inodeUsage(container.Logs))
		}
	}
	//是否统计Pod中Volume所占用的空间
	if hasFsStatsType(statsToMeasure, fsStatsLocalVolumeSource) {
		volumeNames := localVolumeNames(pod)
		for _, volumeName := range volumeNames {
			for _, volumeStats := range podStats.VolumeStats {
				if volumeStats.Name == volumeName {
					disk.Add(*diskUsage(&volumeStats.FsStats))
					inodes.Add(*inodeUsage(&volumeStats.FsStats))
					break
				}
			}
		}
	}
	return v1.ResourceList{
		resourceDisk:   disk,
		resourceInodes: inodes,
	}, nil
}

// podMemoryUsage aggregates pod memory usage.
func podMemoryUsage(podStats statsapi.PodStats) (v1.ResourceList, error) {
	disk := resource.Quantity{Format: resource.BinarySI}
	memory := resource.Quantity{Format: resource.BinarySI}
	for _, container := range podStats.Containers {
		// disk usage (if known)
		for _, fsStats := range []*statsapi.FsStats{container.Rootfs, container.Logs} {
			disk.Add(*diskUsage(fsStats))
		}
		// memory usage (if known)
		memory.Add(*memoryUsage(container.Memory))
	}
	return v1.ResourceList{
		v1.ResourceMemory: memory,
		resourceDisk:      disk,
	}, nil
}

// formatThreshold formats a threshold for logging.
func formatThreshold(threshold Threshold) string {
	return fmt.Sprintf("threshold(signal=%v, operator=%v, value=%v, gracePeriod=%v)", threshold.Signal, formatThresholdValue(threshold.Value), threshold.Operator, threshold.GracePeriod)
}

// formatThresholdValue formats a thresholdValue for logging.
func formatThresholdValue(value ThresholdValue) string {
	if value.Quantity != nil {
		return value.Quantity.String()
	}
	return fmt.Sprintf("%f%%", value.Percentage*float32(100))
}

// cachedStatsFunc returns a statsFunc based on the provided pod stats.
//将pods的资源统计保存在数组中,
func cachedStatsFunc(podStats []statsapi.PodStats) statsFunc {
	uid2PodStats := map[string]statsapi.PodStats{}
	//遍历pod的资源统计
	for i := range podStats {
		uid2PodStats[podStats[i].PodRef.UID] = podStats[i]
	}
	return func(pod *v1.Pod) (statsapi.PodStats, bool) {
		stats, found := uid2PodStats[string(pod.UID)]
		return stats, found
	}
}

// Cmp compares p1 and p2 and returns:
//
//   -1 if p1 <  p2
//    0 if p1 == p2
//   +1 if p1 >  p2
//
type cmpFunc func(p1, p2 *v1.Pod) int

// multiSorter implements the Sort interface, sorting changes within.
type multiSorter struct {
	pods []*v1.Pod
	cmp  []cmpFunc
}

// Sort sorts the argument slice according to the less functions passed to OrderedBy.
func (ms *multiSorter) Sort(pods []*v1.Pod) {
	ms.pods = pods
	sort.Sort(ms)
}

// OrderedBy returns a Sorter that sorts using the cmp functions, in order.
// Call its Sort method to sort the data.
//使用多个排序器进行排序
func orderedBy(cmp ...cmpFunc) *multiSorter {
	return &multiSorter{
		cmp: cmp,
	}
}

// Len is part of sort.Interface.
func (ms *multiSorter) Len() int {
	return len(ms.pods)
}

// Swap is part of sort.Interface.
func (ms *multiSorter) Swap(i, j int) {
	ms.pods[i], ms.pods[j] = ms.pods[j], ms.pods[i]
}

// Less is part of sort.Interface.
func (ms *multiSorter) Less(i, j int) bool {
	p1, p2 := ms.pods[i], ms.pods[j]
	var k int
	//遍历所有的比较器
	for k = 0; k < len(ms.cmp)-1; k++ {
		cmpResult := ms.cmp[k](p1, p2)
		// p1 is less than p2
		if cmpResult < 0 {
			return true
		}
		// p1 is greater than p2
		if cmpResult > 0 {
			return false
		}
		// we don't know yet
	}
	// the last cmp func is the final decider
	return ms.cmp[k](p1, p2) < 0
}

// qosComparator compares pods by QoS (BestEffort < Burstable < Guaranteed)
//根据两个Pod的QOS进行排序
func qosComparator(p1, p2 *v1.Pod) int {
	qosP1 := qos.GetPodQOS(p1) //通过判断pod中的容器有没有设置资源request和limit来确定容器的qos
	qosP2 := qos.GetPodQOS(p2)
	// its a tie
	if qosP1 == qosP2 {
		return 0
	}
	// if p1 is best effort, we know p2 is burstable or guaranteed
	if qosP1 == qos.BestEffort {
		return -1
	}
	// we know p1 and p2 are not besteffort, so if p1 is burstable, p2 must be guaranteed
	if qosP1 == qos.Burstable {
		if qosP2 == qos.Guaranteed {
			return -1
		}
		return 1
	}
	// ok, p1 must be guaranteed.
	return 1
}

// memory compares pods by largest consumer of memory relative to request.
func memory(stats statsFunc) cmpFunc {
	return func(p1, p2 *v1.Pod) int {
		p1Stats, found := stats(p1)
		// if we have no usage stats for p1, we want p2 first
		if !found {
			return -1
		}
		// if we have no usage stats for p2, but p1 has usage, we want p1 first.
		p2Stats, found := stats(p2)
		if !found {
			return 1
		}
		// if we cant get usage for p1 measured, we want p2 first
		p1Usage, err := podMemoryUsage(p1Stats)
		if err != nil {
			return -1
		}
		// if we cant get usage for p2 measured, we want p1 first
		p2Usage, err := podMemoryUsage(p2Stats)
		if err != nil {
			return 1
		}

		// adjust p1, p2 usage relative to the request (if any)
		p1Memory := p1Usage[v1.ResourceMemory]
		p1Spec, err := core.PodUsageFunc(p1)
		if err != nil {
			return -1
		}
		p1Request := p1Spec[api.ResourceRequestsMemory]
		p1Memory.Sub(p1Request)

		p2Memory := p2Usage[v1.ResourceMemory]
		p2Spec, err := core.PodUsageFunc(p2)
		if err != nil {
			return 1
		}
		p2Request := p2Spec[api.ResourceRequestsMemory]
		p2Memory.Sub(p2Request)

		// if p2 is using more than p1, we want p2 first
		return p2Memory.Cmp(p1Memory)
	}
}

// disk compares pods by largest consumer of disk relative to request for the specified disk resource.
//通过统计函数,获取po的资源统计,并从中获取"root"或者"log"文件系统的容量使用统计.并
func disk(stats statsFunc, fsStatsToMeasure []fsStatsType, diskResource v1.ResourceName) cmpFunc {
	return func(p1, p2 *v1.Pod) int {
		p1Stats, found := stats(p1)
		// if we have no usage stats for p1, we want p2 first
		if !found {
			return -1
		}
		// if we have no usage stats for p2, but p1 has usage, we want p1 first.
		p2Stats, found := stats(p2)
		if !found {
			return 1
		}
		// if we cant get usage for p1 measured, we want p2 first
		//获取文件系统指定资源类型的统计
		p1Usage, err := podDiskUsage(p1Stats, p1, fsStatsToMeasure)
		if err != nil {
			return -1
		}
		// if we cant get usage for p2 measured, we want p1 first
		p2Usage, err := podDiskUsage(p2Stats, p2, fsStatsToMeasure)
		if err != nil {
			return 1
		}

		// disk is best effort, so we don't measure relative to a request.
		// TODO: add disk as a guaranteed resource
		p1Disk := p1Usage[diskResource]
		p2Disk := p2Usage[diskResource]
		// if p2 is using more than p1, we want p2 first
		return p2Disk.Cmp(p1Disk)
	}
}

// rankMemoryPressure orders the input pods for eviction in response to memory pressure.
//根据Qos已经以及所使用的内存统计进行排序
func rankMemoryPressure(pods []*v1.Pod, stats statsFunc) {
	orderedBy(qosComparator, memory(stats)).Sort(pods)
}

// rankDiskPressureFunc returns a rankFunc that measures the specified fs stats.
//根据Qos已经以及所使用的硬盘统计进行排序
func rankDiskPressureFunc(fsStatsToMeasure []fsStatsType, diskResource v1.ResourceName) rankFunc {
	return func(pods []*v1.Pod, stats statsFunc) {
		orderedBy(qosComparator, disk(stats, fsStatsToMeasure, diskResource)).Sort(pods)
	}
}

// byEvictionPriority implements sort.Interface for []v1.ResourceName.
type byEvictionPriority []v1.ResourceName

func (a byEvictionPriority) Len() int      { return len(a) }
func (a byEvictionPriority) Swap(i, j int) { a[i], a[j] = a[j], a[i] }

// Less ranks memory before all other resources.
func (a byEvictionPriority) Less(i, j int) bool {
	return a[i] == v1.ResourceMemory
}

// makeSignalObservations derives observations using the specified summary provider.
//通过资源统计接口,获取总/可用内存,文件系统容量,文件节点,imagefs容量的统计结果,以及Pod统计的获取函数
func makeSignalObservations(summaryProvider stats.SummaryProvider) (signalObservations, statsFunc, error) {
	summary, err := summaryProvider.Get()
	if err != nil {
		return nil, nil, err
	}

	// build the function to work against for pod stats
	//将资源收集的Pod统计信息,缓存到内存中..返回一个通过PodId返回相应Pod统计的函数
	statsFunc := cachedStatsFunc(summary.Pods)
	// build an evaluation context for current eviction signals
	//用来记录驱逐信号对应的资源统计
	result := signalObservations{}

	//当前的可用内存和总内存
	if memory := summary.Node.Memory; memory != nil && memory.AvailableBytes != nil && memory.WorkingSetBytes != nil {
		result[SignalMemoryAvailable] = signalObservation{
			available: resource.NewQuantity(int64(*memory.AvailableBytes), resource.BinarySI),
			capacity:  resource.NewQuantity(int64(*memory.AvailableBytes+*memory.WorkingSetBytes), resource.BinarySI),
			time:      memory.Time,
		}
	}
	//文件的可用容量和总容量
	if nodeFs := summary.Node.Fs; nodeFs != nil {
		if nodeFs.AvailableBytes != nil && nodeFs.CapacityBytes != nil {
			result[SignalNodeFsAvailable] = signalObservation{
				available: resource.NewQuantity(int64(*nodeFs.AvailableBytes), resource.BinarySI),
				capacity:  resource.NewQuantity(int64(*nodeFs.CapacityBytes), resource.BinarySI),
				// TODO: add timestamp to stat (see memory stat)
			}
		}
		//总文件节点和空闲文件节点数
		if nodeFs.InodesFree != nil && nodeFs.Inodes != nil {
			result[SignalNodeFsInodesFree] = signalObservation{
				available: resource.NewQuantity(int64(*nodeFs.InodesFree), resource.BinarySI),
				capacity:  resource.NewQuantity(int64(*nodeFs.Inodes), resource.BinarySI),
				// TODO: add timestamp to stat (see memory stat)
			}
		}
	}
	//imagefs的总容量和可用容量
	if summary.Node.Runtime != nil {
		if imageFs := summary.Node.Runtime.ImageFs; imageFs != nil {
			if imageFs.AvailableBytes != nil && imageFs.CapacityBytes != nil {
				result[SignalImageFsAvailable] = signalObservation{
					available: resource.NewQuantity(int64(*imageFs.AvailableBytes), resource.BinarySI),
					capacity:  resource.NewQuantity(int64(*imageFs.CapacityBytes), resource.BinarySI),
					// TODO: add timestamp to stat (see memory stat)
				}
				if imageFs.InodesFree != nil && imageFs.Inodes != nil {
					result[SignalImageFsInodesFree] = signalObservation{
						available: resource.NewQuantity(int64(*imageFs.InodesFree), resource.BinarySI),
						capacity:  resource.NewQuantity(int64(*imageFs.Inodes), resource.BinarySI),
						// TODO: add timestamp to stat (see memory stat)
					}
				}
			}
		}
	}
	return result, statsFunc, nil
}

// thresholdsMet returns the set of thresholds that were met independent of grace period
//比较驱逐信号值和相应资源统计,返回资源可用量低于阈值的驱逐信号
func thresholdsMet(thresholds []Threshold, observations signalObservations, enforceMinReclaim bool) []Threshold {
	results := []Threshold{}
	//遍历所有的阈值,如出现在获取的资源统计中,
	for i := range thresholds {
		threshold := thresholds[i]
		//阈值信号对应的资源统计
		observed, found := observations[threshold.Signal]
		if !found {
			glog.Warningf("eviction manager: no observation found for eviction signal %v", threshold.Signal)
			continue
		}
		// determine if we have met the specified threshold
		thresholdMet := false
		//获取阈值,如果是数值返回相应数值,如果是百分比,返回资源统计的容量*百分比,来获取相应的阈值
		quantity := getThresholdQuantity(threshold.Value, observed.capacity)
		// if enforceMinReclaim is specified, we compare relative to value - minreclaim
		//如果指定了强制最小回收,最小回收数量,则将最小回收量添加到阈值上
		if enforceMinReclaim && threshold.MinReclaim != nil {
			quantity.Add(*getThresholdQuantity(*threshold.MinReclaim, observed.capacity))
		}
		//如果阈值高于当前资源的可用量
		thresholdResult := quantity.Cmp(*observed.available)
		switch threshold.Operator {
		case OpLessThan:
			//标记已经遇到了阈值
			thresholdMet = thresholdResult > 0
		}
		if thresholdMet {
			results = append(results, threshold)
		}
	}
	return results
}

//当前资源观察比上一次资源观察时间晚的资源的阈值
func thresholdsUpdatedStats(thresholds []Threshold, observations, lastObservations signalObservations) []Threshold {
	results := []Threshold{}
	for i := range thresholds {
		threshold := thresholds[i]
		observed, found := observations[threshold.Signal]
		if !found {
			glog.Warningf("eviction manager: no observation found for eviction signal %v", threshold.Signal)
			continue
		}
		last, found := lastObservations[threshold.Signal]
		//当前观察的资源统计时间晚于上次资源统计时间
		if !found || observed.time.IsZero() || observed.time.After(last.time.Time) {
			results = append(results, threshold)
		}
	}
	return results
}

// getThresholdQuantity returns the expected quantity value for a thresholdValue
//如果阈值是数值,则返回数值,否则返回资源容量*阈值中的百分比
func getThresholdQuantity(value ThresholdValue, capacity *resource.Quantity) *resource.Quantity {
	if value.Quantity != nil {
		return value.Quantity.Copy()
	}
	return resource.NewQuantity(int64(float64(capacity.Value())*float64(value.Percentage)), resource.BinarySI)
}

// thresholdsFirstObservedAt merges the input set of thresholds with the previous observation to determine when active set of thresholds were initially met.
// 返回阈值信号表中的观察时间,如果是第一次观察到,则设置观察时间为now; 如果上次已经观察到,返回上一次观察时间
func thresholdsFirstObservedAt(thresholds []Threshold, lastObservedAt thresholdsObservedAt, now time.Time) thresholdsObservedAt {
	results := thresholdsObservedAt{}
	for i := range thresholds {
		observedAt, found := lastObservedAt[thresholds[i]]
		if !found {
			observedAt = now
		}
		results[thresholds[i]] = observedAt
	}
	return results
}

// thresholdsMetGracePeriod returns the set of thresholds that have satisfied associated grace period
// 返回观察的阈值信号表中,信号观察时间距离当前时间大于阈值宽限期的阈值信号
func thresholdsMetGracePeriod(observedAt thresholdsObservedAt, now time.Time) []Threshold {
	results := []Threshold{}
	for threshold, at := range observedAt {
		duration := now.Sub(at)
		if duration < threshold.GracePeriod {
			glog.V(2).Infof("eviction manager: eviction criteria not yet met for %v, duration: %v", formatThreshold(threshold), duration)
			continue
		}
		results = append(results, threshold)
	}
	return results
}

// nodeConditions returns the set of node conditions associated with a threshold
//根据阈值,生成相应的nodeCondtion
func nodeConditions(thresholds []Threshold) []v1.NodeConditionType {
	results := []v1.NodeConditionType{}
	for _, threshold := range thresholds {
		if nodeCondition, found := signalToNodeCondition[threshold.Signal]; found {
			if !hasNodeCondition(results, nodeCondition) {
				results = append(results, nodeCondition)
			}
		}
	}
	return results
}

// nodeConditionsLastObservedAt merges the input with the previous observation to determine when a condition was most recently met.
//设置节点状况表的观察时间为now,合并上一次节点状况表中不存在当前表的节点状况
func nodeConditionsLastObservedAt(nodeConditions []v1.NodeConditionType, lastObservedAt nodeConditionsObservedAt, now time.Time) nodeConditionsObservedAt {
	results := nodeConditionsObservedAt{}
	// the input conditions were observed "now"
	for i := range nodeConditions {
		results[nodeConditions[i]] = now
	}
	// the conditions that were not observed now are merged in with their old time
	for key, value := range lastObservedAt {
		_, found := results[key]
		if !found {
			results[key] = value
		}
	}
	return results
}

// nodeConditionsObservedSince returns the set of conditions that have been observed within the specified period
//返回所有被观察时间距当前时间 小于指定周期的节点条件
func nodeConditionsObservedSince(observedAt nodeConditionsObservedAt, period time.Duration, now time.Time) []v1.NodeConditionType {
	results := []v1.NodeConditionType{}
	//遍历所有观察到的节点条件,
	for nodeCondition, at := range observedAt {
		duration := now.Sub(at)
		if duration < period {
			results = append(results, nodeCondition)
		}
	}
	return results
}

// hasFsStatsType returns true if the fsStat is in the input list
func hasFsStatsType(inputs []fsStatsType, item fsStatsType) bool {
	for _, input := range inputs {
		if input == item {
			return true
		}
	}
	return false
}

// hasNodeCondition returns true if the node condition is in the input list
func hasNodeCondition(inputs []v1.NodeConditionType, item v1.NodeConditionType) bool {
	for _, input := range inputs {
		if input == item {
			return true
		}
	}
	return false
}

// mergeThresholds will merge both threshold lists eliminating duplicates.
func mergeThresholds(inputsA []Threshold, inputsB []Threshold) []Threshold {
	results := inputsA
	for _, threshold := range inputsB {
		if !hasThreshold(results, threshold) {
			results = append(results, threshold)
		}
	}
	return results
}

// hasThreshold returns true if the threshold is in the input list
func hasThreshold(inputs []Threshold, item Threshold) bool {
	for _, input := range inputs {
		if input.GracePeriod == item.GracePeriod && input.Operator == item.Operator && input.Signal == item.Signal && compareThresholdValue(input.Value, item.Value) {
			return true
		}
	}
	return false
}

// compareThresholdValue returns true if the two thresholdValue objects are logically the same
func compareThresholdValue(a ThresholdValue, b ThresholdValue) bool {
	if a.Quantity != nil {
		if b.Quantity == nil {
			return false
		}
		return a.Quantity.Cmp(*b.Quantity) == 0
	}
	if b.Quantity != nil {
		return false
	}
	return a.Percentage == b.Percentage
}

// getStarvedResources returns the set of resources that are starved based on thresholds met.
//根据阈值信号,找到对应触发阈值信号的资源
func getStarvedResources(thresholds []Threshold) []v1.ResourceName {
	results := []v1.ResourceName{}
	for _, threshold := range thresholds {
		if starvedResource, found := signalToResource[threshold.Signal]; found {
			results = append(results, starvedResource)
		}
	}
	return results
}

// isSoftEviction returns true if the thresholds met for the starved resource are only soft thresholds
//找到饥饿资源对应的阈值信号,确认其是软信号,还是硬信号
func isSoftEvictionThresholds(thresholds []Threshold, starvedResource v1.ResourceName) bool {
	for _, threshold := range thresholds {
		if resourceToCheck := signalToResource[threshold.Signal]; resourceToCheck != starvedResource {
			continue
		}
		if isHardEvictionThreshold(threshold) {
			return false
		}
	}
	return true
}

// isSoftEviction returns true if the thresholds met for the starved resource are only soft thresholds
//没有设置优雅回收时间的阈值,是硬回收阈值.(这在Kubelet启动时配置)
func isHardEvictionThreshold(threshold Threshold) bool {
	return threshold.GracePeriod == time.Duration(0)
}

// buildResourceToRankFunc returns ranking functions associated with resources
//构建每个资源的排序函数
func buildResourceToRankFunc(withImageFs bool) map[v1.ResourceName]rankFunc {
	resourceToRankFunc := map[v1.ResourceName]rankFunc{
		v1.ResourceMemory: rankMemoryPressure,
	}
	// usage of an imagefs is optional
	if withImageFs {
		// with an imagefs, nodefs pod rank func for eviction only includes logs and local volumes
		resourceToRankFunc[resourceNodeFs] = rankDiskPressureFunc([]fsStatsType{fsStatsLogs, fsStatsLocalVolumeSource}, resourceDisk)
		resourceToRankFunc[resourceNodeFsInodes] = rankDiskPressureFunc([]fsStatsType{fsStatsLogs, fsStatsLocalVolumeSource}, resourceInodes)
		// with an imagefs, imagefs pod rank func for eviction only includes rootfs
		resourceToRankFunc[resourceImageFs] = rankDiskPressureFunc([]fsStatsType{fsStatsRoot}, resourceDisk)
		resourceToRankFunc[resourceImageFsInodes] = rankDiskPressureFunc([]fsStatsType{fsStatsRoot}, resourceInodes)
	} else {
		// without an imagefs, nodefs pod rank func for eviction looks at all fs stats.
		// since imagefs and nodefs share a common device, they share common ranking functions.
		resourceToRankFunc[resourceNodeFs] = rankDiskPressureFunc([]fsStatsType{fsStatsRoot, fsStatsLogs, fsStatsLocalVolumeSource}, resourceDisk)
		resourceToRankFunc[resourceNodeFsInodes] = rankDiskPressureFunc([]fsStatsType{fsStatsRoot, fsStatsLogs, fsStatsLocalVolumeSource}, resourceInodes)
		resourceToRankFunc[resourceImageFs] = rankDiskPressureFunc([]fsStatsType{fsStatsRoot, fsStatsLogs, fsStatsLocalVolumeSource}, resourceDisk)
		resourceToRankFunc[resourceImageFsInodes] = rankDiskPressureFunc([]fsStatsType{fsStatsRoot, fsStatsLogs, fsStatsLocalVolumeSource}, resourceInodes)
	}
	return resourceToRankFunc
}

// PodIsEvicted returns true if the reported pod status is due to an eviction.
//根据Pod状态中的原因为"Evicted"来判定一个Pod是否
func PodIsEvicted(podStatus v1.PodStatus) bool {
	return podStatus.Phase == v1.PodFailed && podStatus.Reason == reason
}

// buildResourceToNodeReclaimFuncs returns reclaim functions associated with resources.
func buildResourceToNodeReclaimFuncs(imageGC ImageGC, withImageFs bool) map[v1.ResourceName]nodeReclaimFuncs {
	resourceToReclaimFunc := map[v1.ResourceName]nodeReclaimFuncs{}
	// usage of an imagefs is optional
	if withImageFs {
		// with an imagefs, nodefs pressure should just delete logs
		resourceToReclaimFunc[resourceNodeFs] = nodeReclaimFuncs{deleteLogs()}
		resourceToReclaimFunc[resourceNodeFsInodes] = nodeReclaimFuncs{deleteLogs()}
		// with an imagefs, imagefs pressure should delete unused images
		resourceToReclaimFunc[resourceImageFs] = nodeReclaimFuncs{deleteImages(imageGC, true)}
		resourceToReclaimFunc[resourceImageFsInodes] = nodeReclaimFuncs{deleteImages(imageGC, false)}
	} else {
		// without an imagefs, nodefs pressure should delete logs, and unused images
		// since imagefs and nodefs share a common device, they share common reclaim functions
		resourceToReclaimFunc[resourceNodeFs] = nodeReclaimFuncs{deleteLogs(), deleteImages(imageGC, true)}
		resourceToReclaimFunc[resourceNodeFsInodes] = nodeReclaimFuncs{deleteLogs(), deleteImages(imageGC, false)}
		resourceToReclaimFunc[resourceImageFs] = nodeReclaimFuncs{deleteLogs(), deleteImages(imageGC, true)}
		resourceToReclaimFunc[resourceImageFsInodes] = nodeReclaimFuncs{deleteLogs(), deleteImages(imageGC, false)}
	}
	return resourceToReclaimFunc
}

// deleteLogs will delete logs to free up disk pressure.
func deleteLogs() nodeReclaimFunc {
	return func() (*resource.Quantity, error) {
		// TODO: not yet supported.
		return resource.NewQuantity(int64(0), resource.BinarySI), nil
	}
}

// deleteImages will delete unused images to free up disk pressure.
//调用镜像垃圾回收器,回收无用的镜像
func deleteImages(imageGC ImageGC, reportBytesFreed bool) nodeReclaimFunc {
	return func() (*resource.Quantity, error) {
		glog.Infof("eviction manager: attempting to delete unused images")
		bytesFreed, err := imageGC.DeleteUnusedImages()
		if err != nil {
			return nil, err
		}
		reclaimed := int64(0)
		if reportBytesFreed {
			reclaimed = bytesFreed
		}
		return resource.NewQuantity(reclaimed, resource.BinarySI), nil
	}
}
