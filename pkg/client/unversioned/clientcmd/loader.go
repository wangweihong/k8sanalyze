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
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"reflect"
	goruntime "runtime"
	"strings"

	"github.com/golang/glog"
	"github.com/imdario/mergo"

	"k8s.io/kubernetes/pkg/client/restclient"
	clientcmdapi "k8s.io/kubernetes/pkg/client/unversioned/clientcmd/api"
	clientcmdlatest "k8s.io/kubernetes/pkg/client/unversioned/clientcmd/api/latest"
	"k8s.io/kubernetes/pkg/runtime"
	"k8s.io/kubernetes/pkg/runtime/schema"
	utilerrors "k8s.io/kubernetes/pkg/util/errors"
	"k8s.io/kubernetes/pkg/util/homedir"
)

const (
	RecommendedConfigPathFlag   = "kubeconfig"
	RecommendedConfigPathEnvVar = "KUBECONFIG"
	RecommendedHomeDir          = ".kube"
	RecommendedFileName         = "config"
	RecommendedSchemaName       = "schema"
)

var RecommendedHomeFile = path.Join(homedir.HomeDir(), RecommendedHomeDir, RecommendedFileName)     //$HOME/.kube/config
var RecommendedSchemaFile = path.Join(homedir.HomeDir(), RecommendedHomeDir, RecommendedSchemaName) //$HOME/.kube/schema

// currentMigrationRules returns a map that holds the history of recommended home directories used in previous versions.
// Any future changes to RecommendedHomeFile and related are expected to add a migration rule here, in order to make
// sure existing config files are migrated to their new locations properly.
//老版本默认的配置文件在$HOME/.kube/.kubeconfig文件中
//将会将老版本的配置文件$HOME/.kube/.kubeconfig迁移到$HOME/.kube/config中
func currentMigrationRules() map[string]string {
	oldRecommendedHomeFile := path.Join(os.Getenv("HOME"), "/.kube/.kubeconfig")
	oldRecommendedWindowsHomeFile := path.Join(os.Getenv("HOME"), RecommendedHomeDir, RecommendedFileName)

	migrationRules := map[string]string{}
	migrationRules[RecommendedHomeFile] = oldRecommendedHomeFile
	if goruntime.GOOS == "windows" {
		migrationRules[RecommendedHomeFile] = oldRecommendedWindowsHomeFile
	}
	return migrationRules
}

//被ClientConfigLoadingRules实现
//配置加载规则
type ClientConfigLoader interface {
	ConfigAccess
	// IsDefaultConfig returns true if the returned config matches the defaults.
	IsDefaultConfig(*restclient.Config) bool
	// Load returns the latest config
	Load() (*clientcmdapi.Config, error)
}

type KubeconfigGetter func() (*clientcmdapi.Config, error)

//
type ClientConfigGetter struct {
	kubeconfigGetter KubeconfigGetter //返回命令行工具配置
}

// ClientConfigGetter implements the ClientConfigLoader interface.
var _ ClientConfigLoader = &ClientConfigGetter{}

func (g *ClientConfigGetter) Load() (*clientcmdapi.Config, error) {
	return g.kubeconfigGetter()
}

func (g *ClientConfigGetter) GetLoadingPrecedence() []string {
	return nil
}
func (g *ClientConfigGetter) GetStartingConfig() (*clientcmdapi.Config, error) {
	return g.kubeconfigGetter()
}
func (g *ClientConfigGetter) GetDefaultFilename() string {
	return ""
}
func (g *ClientConfigGetter) IsExplicitFile() bool {
	return false
}
func (g *ClientConfigGetter) GetExplicitFile() string {
	return ""
}
func (g *ClientConfigGetter) IsDefaultConfig(config *restclient.Config) bool {
	return false
}

// ClientConfigLoadingRules is an ExplicitPath and string slice of specific locations that are used for merging together a Config
// Callers can put the chain together however they want, but we'd recommend:
// EnvVarPathFiles if set (a list of files if set) OR the HomeDirectoryPath
// ExplicitPath is special, because if a user specifically requests a certain file be used and error is reported if thie file is not present
//客户端配置加载规则
//见默认的加载规则clientcmd.NewDefaultClientConfigLoadingRules()
//但有些客户端会根据标志--kubeconfig或者环境变量等,更改加载规则的内容
type ClientConfigLoadingRules struct {
	ExplicitPath string   //??通过参数-kubeconfig指定的配置文件的路径?应该是,指定了显式路径,就不会再管HOME/.kube/config以及$KUBECONFIG指定的配置文件(precedence的值),见Load()方法
	Precedence   []string //优先项.其内容为$KUBECONFIG的值或者$HOME/.kube/config,见 NewDefaultClientConfigLoadingRules

	// MigrationRules is a map of destination files to source files.  If a destination file is not present, then the source file is checked.
	// If the source file is present, then it is copied to the destination file BEFORE any further loading happens.
	MigrationRules map[string]string //key为destination,value是source,将source路径的文件拷贝到destination路径.见Migrate()
	//见currentMigrationRules(),默认的迁移规则是将老版本的$HOME/.kube/kubeconfig文件迁移到$HOME/.kube/config文件

	// DoNotResolvePaths indicates whether or not to resolve paths with respect to the originating files.  This is phrased as a negative so
	// that a default object that doesn't set this will usually get the behavior it wants.
	DoNotResolvePaths bool //当配置文件中配置项引用了想读路径的文件,是否解析该路径为绝对路径

	// DefaultClientConfig is an optional field indicating what rules to use to calculate a default configuration.
	// This should match the overrides passed in to ClientConfig loader.
	DefaultClientConfig ClientConfig // api client config interface
}

// ClientConfigLoadingRules implements the ClientConfigLoader interface.
var _ ClientConfigLoader = &ClientConfigLoadingRules{}

// NewDefaultClientConfigLoadingRules returns a ClientConfigLoadingRules object with default fields filled in.  You are not required to
// use this constructor
//获取默认的配置文件的加载规则
//1.优先从KUBECONFIG环境变量中获取配置文件
//2.如果环境变量为空,则从$HOME/.kube/config文件中读取
//3.默认迁移规则是$HOME/.kube/.kubeconfig到$HOME/.kube/config
func NewDefaultClientConfigLoadingRules() *ClientConfigLoadingRules {
	chain := []string{}

	//获得KUBECONFIG环境变量获取配置文件路径,如果为空,则使用默认$HOME/.kube/config配置文件
	envVarFiles := os.Getenv(RecommendedConfigPathEnvVar)
	if len(envVarFiles) != 0 {
		chain = append(chain, filepath.SplitList(envVarFiles)...)

	} else {
		chain = append(chain, RecommendedHomeFile)
	}

	return &ClientConfigLoadingRules{
		Precedence:     chain,
		MigrationRules: currentMigrationRules(), //默认迁移规则是将$HOME/.kube/.kubeconfig迁移到$HOME/.kube/config中
	}
}

// Load starts by running the MigrationRules and then
// takes the loading rules and returns a Config object based on following rules.
//   if the ExplicitPath, return the unmerged explicit file
//   Otherwise, return a merged config based on the Precedence slice
// A missing ExplicitPath file produces an error. Empty filenames or other missing files are ignored.
// Read errors or files with non-deserializable content produce errors.
// The first file to set a particular map key wins and map key's value is never changed.
// BUT, if you set a struct value that is NOT contained inside of map, the value WILL be changed.
// This results in some odd looking logic to merge in one direction, merge in the other, and then merge the two.
// It also means that if two files specify a "red-user", only values from the first file's red-user are used.  Even
// non-conflicting entries from the second file's "red-user" are discarded.
// Relative paths inside of the .kubeconfig files are resolved against the .kubeconfig file's parent folder
// and only absolute file paths are returned.
//加载合并配置文件生成kubectl配置
func (rules *ClientConfigLoadingRules) Load() (*clientcmdapi.Config, error) {
	//根据迁移规则,拷贝迁移规则中的源文件到目的文件中
	if err := rules.Migrate(); err != nil {
		return nil, err
	}

	errlist := []error{}

	kubeConfigFiles := []string{}

	// Make sure a file we were explicitly told to use exists
	//强制指定了一个kubeconfig文件,--kubeconfig参数
	if len(rules.ExplicitPath) > 0 {
		if _, err := os.Stat(rules.ExplicitPath); os.IsNotExist(err) {
			return nil, err
		}
		kubeConfigFiles = append(kubeConfigFiles, rules.ExplicitPath)

	} else {
		kubeConfigFiles = append(kubeConfigFiles, rules.Precedence...)
	}

	kubeconfigs := []*clientcmdapi.Config{}
	// read and cache the config files so that we only look at them once
	//遍历所有的配置文件
	for _, filename := range kubeConfigFiles {
		if len(filename) == 0 {
			// no work to do
			continue
		}

		//从配置文件中加载Kubectl 配置
		config, err := LoadFromFile(filename)
		if os.IsNotExist(err) {
			// skip missing files
			continue
		}
		if err != nil {
			errlist = append(errlist, fmt.Errorf("Error loading config file \"%s\": %v", filename, err))
			continue
		}
		//添加到配置项中
		kubeconfigs = append(kubeconfigs, config)
	}

	// first merge all of our maps
	//创建新的kubectl配置
	mapConfig := clientcmdapi.NewConfig()

	//整合所有的kubectl配置,到Mapconfig
	//同样的字段中的值,会被后续的配置所覆盖
	//kubeconfigs[1].a=1, 而kubeconfigs[2].a=2,mergo.Merge(kubeconfigs[1],kubeconfigs[2]),则kubeconfigs[1].a的值就等于2
	for _, kubeconfig := range kubeconfigs {
		mergo.Merge(mapConfig, kubeconfig)
	}

	// merge all of the struct values in the reverse order so that priority is given correctly
	// errors are not added to the list the second time
	nonMapConfig := clientcmdapi.NewConfig()
	for i := len(kubeconfigs) - 1; i >= 0; i-- {
		kubeconfig := kubeconfigs[i]
		//合并所有配置到nonMapConfig,
		mergo.Merge(nonMapConfig, kubeconfig)
	}

	// since values are overwritten, but maps values are not, we can merge the non-map config on top of the map config and
	// get the values we expect.
	//创建新的kubectl配置
	config := clientcmdapi.NewConfig()
	//合并m
	mergo.Merge(config, mapConfig)
	mergo.Merge(config, nonMapConfig)

	//确认是否解析路径
	if rules.ResolvePaths() {
		//解析配置中相对路径的引用文件为绝对路径
		if err := ResolveLocalPaths(config); err != nil {
			errlist = append(errlist, err)
		}
	}
	return config, utilerrors.NewAggregate(errlist)
}

// Migrate uses the MigrationRules map.  If a destination file is not present, then the source file is checked.
// If the source file is present, then it is copied to the destination file BEFORE any further loading happens.
//根据迁移规则,拷贝迁移规则中的源文件到目的文件中
func (rules *ClientConfigLoadingRules) Migrate() error {
	if rules.MigrationRules == nil {
		return nil
	}

	for destination, source := range rules.MigrationRules {
		if _, err := os.Stat(destination); err == nil {
			// if the destination already exists, do nothing
			continue
		} else if os.IsPermission(err) {
			// if we can't access the file, skip it
			continue
		} else if !os.IsNotExist(err) {
			// if we had an error other than non-existence, fail
			return err
		}

		if sourceInfo, err := os.Stat(source); err != nil {
			if os.IsNotExist(err) || os.IsPermission(err) {
				// if the source file doesn't exist or we can't access it, there's no work to do.
				continue
			}

			// if we had an error other than non-existence, fail
			return err
		} else if sourceInfo.IsDir() {
			return fmt.Errorf("cannot migrate %v to %v because it is a directory", source, destination)
		}

		in, err := os.Open(source)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(destination)
		if err != nil {
			return err
		}
		defer out.Close()

		if _, err = io.Copy(out, in); err != nil {
			return err
		}
	}

	return nil
}

// GetLoadingPrecedence implements ConfigAccess
func (rules *ClientConfigLoadingRules) GetLoadingPrecedence() []string {
	return rules.Precedence
}

// GetStartingConfig implements ConfigAccess
func (rules *ClientConfigLoadingRules) GetStartingConfig() (*clientcmdapi.Config, error) {
	clientConfig := NewNonInteractiveDeferredLoadingClientConfig(rules, &ConfigOverrides{})
	rawConfig, err := clientConfig.RawConfig()
	if os.IsNotExist(err) {
		return clientcmdapi.NewConfig(), nil
	}
	if err != nil {
		return nil, err
	}

	return &rawConfig, nil
}

// GetDefaultFilename implements ConfigAccess
func (rules *ClientConfigLoadingRules) GetDefaultFilename() string {
	// Explicit file if we have one.
	if rules.IsExplicitFile() {
		return rules.GetExplicitFile()
	}
	// Otherwise, first existing file from precedence.
	for _, filename := range rules.GetLoadingPrecedence() {
		if _, err := os.Stat(filename); err == nil {
			return filename
		}
	}
	// If none exists, use the first from precedence.
	if len(rules.Precedence) > 0 {
		return rules.Precedence[0]
	}
	return ""
}

// IsExplicitFile implements ConfigAccess
func (rules *ClientConfigLoadingRules) IsExplicitFile() bool {
	return len(rules.ExplicitPath) > 0
}

// GetExplicitFile implements ConfigAccess
func (rules *ClientConfigLoadingRules) GetExplicitFile() string {
	return rules.ExplicitPath
}

// IsDefaultConfig returns true if the provided configuration matches the default
func (rules *ClientConfigLoadingRules) IsDefaultConfig(config *restclient.Config) bool {
	if rules.DefaultClientConfig == nil {
		return false
	}
	defaultConfig, err := rules.DefaultClientConfig.ClientConfig()
	if err != nil {
		return false
	}
	return reflect.DeepEqual(config, defaultConfig)
}

// LoadFromFile takes a filename and deserializes the contents into Config object
//解析kubeconfig文件,并加载成kubectl config
func LoadFromFile(filename string) (*clientcmdapi.Config, error) {
	//读取配置文件
	kubeconfigBytes, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	//加载配置文件并且进行解析
	config, err := Load(kubeconfigBytes)
	if err != nil {
		return nil, err
	}
	glog.V(6).Infoln("Config loaded from file", filename)

	// set LocationOfOrigin on every Cluster, User, and Context
	for key, obj := range config.AuthInfos {
		obj.LocationOfOrigin = filename
		config.AuthInfos[key] = obj
	}
	for key, obj := range config.Clusters {
		obj.LocationOfOrigin = filename
		config.Clusters[key] = obj
	}
	for key, obj := range config.Contexts {
		obj.LocationOfOrigin = filename
		config.Contexts[key] = obj
	}

	if config.AuthInfos == nil {
		config.AuthInfos = map[string]*clientcmdapi.AuthInfo{}
	}
	if config.Clusters == nil {
		config.Clusters = map[string]*clientcmdapi.Cluster{}
	}
	if config.Contexts == nil {
		config.Contexts = map[string]*clientcmdapi.Context{}
	}

	return config, nil
}

// Load takes a byte slice and deserializes the contents into Config object.
// Encapsulates deserialization without assuming the source is a file.
//解析配置内容为kubectl配置
func Load(data []byte) (*clientcmdapi.Config, error) {
	config := clientcmdapi.NewConfig()
	// if there's no data in a file, return the default object instead of failing (DecodeInto reject empty input)
	if len(data) == 0 {
		return config, nil
	}
	//利用Codec机型解码
	//这个Codec是静态初始化的,可以直接使用
	decoded, _, err := clientcmdlatest.Codec.Decode(data, &schema.GroupVersionKind{Version: clientcmdlatest.Version, Kind: "Config"}, config)
	if err != nil {
		return nil, err
	}
	return decoded.(*clientcmdapi.Config), nil
}

// WriteToFile serializes the config to yaml and writes it out to a file.  If not present, it creates the file with the mode 0600.  If it is present
// it stomps the contents
//将kubectl配置写到指定的文件中
func WriteToFile(config clientcmdapi.Config, filename string) error {
	content, err := Write(config)
	if err != nil {
		return err
	}
	dir := filepath.Dir(filename)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if err = os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}

	if err := ioutil.WriteFile(filename, content, 0600); err != nil {
		return err
	}
	return nil
}

func lockFile(filename string) error {
	// TODO: find a way to do this with actual file locks. Will
	// probably need seperate solution for windows and linux.

	// Make sure the dir exists before we try to create a lock file.
	dir := filepath.Dir(filename)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if err = os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(lockName(filename), os.O_CREATE|os.O_EXCL, 0)
	if err != nil {
		return err
	}
	f.Close()
	return nil
}

func unlockFile(filename string) error {
	return os.Remove(lockName(filename))
}

func lockName(filename string) string {
	return filename + ".lock"
}

// Write serializes the config to yaml.
// Encapsulates serialization without assuming the destination is a file.
//将kubectl config编码成[]byte,写到文件文件中就是
func Write(config clientcmdapi.Config) ([]byte, error) {
	return runtime.Encode(clientcmdlatest.Codec, &config)
}

func (rules ClientConfigLoadingRules) ResolvePaths() bool {
	return !rules.DoNotResolvePaths
}

// ResolveLocalPaths resolves all relative paths in the config object with respect to the stanza's LocationOfOrigin
// this cannot be done directly inside of LoadFromFile because doing so there would make it impossible to load a file without
// modification of its contents.
//解析配置中相对路径的引用文件为绝对路径
func ResolveLocalPaths(config *clientcmdapi.Config) error {
	for _, cluster := range config.Clusters {
		if len(cluster.LocationOfOrigin) == 0 {
			continue
		}
		//获取指定目录的绝对路径
		base, err := filepath.Abs(filepath.Dir(cluster.LocationOfOrigin))
		if err != nil {
			return fmt.Errorf("Could not determine the absolute path of config file %s: %v", cluster.LocationOfOrigin, err)
		}

		//解析集群引用的证书文件的绝对路径
		if err := ResolvePaths(GetClusterFileReferences(cluster), base); err != nil {
			return err
		}
	}
	for _, authInfo := range config.AuthInfos {
		if len(authInfo.LocationOfOrigin) == 0 {
			continue
		}

		//解析授权引用的证书文件的绝对路径
		base, err := filepath.Abs(filepath.Dir(authInfo.LocationOfOrigin))
		if err != nil {
			return fmt.Errorf("Could not determine the absolute path of config file %s: %v", authInfo.LocationOfOrigin, err)
		}

		if err := ResolvePaths(GetAuthInfoFileReferences(authInfo), base); err != nil {
			return err
		}
	}

	return nil
}

// RelativizeClusterLocalPaths first absolutizes the paths by calling ResolveLocalPaths.  This assumes that any NEW path is already
// absolute, but any existing path will be resolved relative to LocationOfOrigin
func RelativizeClusterLocalPaths(cluster *clientcmdapi.Cluster) error {
	if len(cluster.LocationOfOrigin) == 0 {
		return fmt.Errorf("no location of origin for %s", cluster.Server)
	}
	base, err := filepath.Abs(filepath.Dir(cluster.LocationOfOrigin))
	if err != nil {
		return fmt.Errorf("could not determine the absolute path of config file %s: %v", cluster.LocationOfOrigin, err)
	}

	if err := ResolvePaths(GetClusterFileReferences(cluster), base); err != nil {
		return err
	}
	if err := RelativizePathWithNoBacksteps(GetClusterFileReferences(cluster), base); err != nil {
		return err
	}

	return nil
}

// RelativizeAuthInfoLocalPaths first absolutizes the paths by calling ResolveLocalPaths.  This assumes that any NEW path is already
// absolute, but any existing path will be resolved relative to LocationOfOrigin
func RelativizeAuthInfoLocalPaths(authInfo *clientcmdapi.AuthInfo) error {
	if len(authInfo.LocationOfOrigin) == 0 {
		return fmt.Errorf("no location of origin for %v", authInfo)
	}
	base, err := filepath.Abs(filepath.Dir(authInfo.LocationOfOrigin))
	if err != nil {
		return fmt.Errorf("could not determine the absolute path of config file %s: %v", authInfo.LocationOfOrigin, err)
	}

	if err := ResolvePaths(GetAuthInfoFileReferences(authInfo), base); err != nil {
		return err
	}
	if err := RelativizePathWithNoBacksteps(GetAuthInfoFileReferences(authInfo), base); err != nil {
		return err
	}

	return nil
}

func RelativizeConfigPaths(config *clientcmdapi.Config, base string) error {
	return RelativizePathWithNoBacksteps(GetConfigFileReferences(config), base)
}

//解析指定配置中引用的文件的相对路径为绝对路径
func ResolveConfigPaths(config *clientcmdapi.Config, base string) error {
	return ResolvePaths(GetConfigFileReferences(config), base)
}

//获得所有引用的外部文件
func GetConfigFileReferences(config *clientcmdapi.Config) []*string {
	refs := []*string{}

	for _, cluster := range config.Clusters {
		refs = append(refs, GetClusterFileReferences(cluster)...)
	}
	for _, authInfo := range config.AuthInfos {
		refs = append(refs, GetAuthInfoFileReferences(authInfo)...)
	}

	return refs
}

//获得服务器证书的路径
func GetClusterFileReferences(cluster *clientcmdapi.Cluster) []*string {
	return []*string{&cluster.CertificateAuthority}
}

//获得授权证书的路径
func GetAuthInfoFileReferences(authInfo *clientcmdapi.AuthInfo) []*string {
	return []*string{&authInfo.ClientCertificate, &authInfo.ClientKey, &authInfo.TokenFile}
}

// ResolvePaths updates the given refs to be absolute paths, relative to the given base directory
//refs中非绝对的路径,添加base前缀
func ResolvePaths(refs []*string, base string) error {
	for _, ref := range refs {
		// Don't resolve empty paths
		if len(*ref) > 0 {
			// Don't resolve absolute paths
			if !filepath.IsAbs(*ref) {
				*ref = filepath.Join(base, *ref)
			}
		}
	}
	return nil
}

// RelativizePathWithNoBacksteps updates the given refs to be relative paths, relative to the given base directory as long as they do not require backsteps.
// Any path requiring a backstep is left as-is as long it is absolute.  Any non-absolute path that can't be relativized produces an error
func RelativizePathWithNoBacksteps(refs []*string, base string) error {
	for _, ref := range refs {
		// Don't relativize empty paths
		if len(*ref) > 0 {
			rel, err := MakeRelative(*ref, base)
			if err != nil {
				return err
			}

			// if we have a backstep, don't mess with the path
			if strings.HasPrefix(rel, "../") {
				if filepath.IsAbs(*ref) {
					continue
				}

				return fmt.Errorf("%v requires backsteps and is not absolute", *ref)
			}

			*ref = rel
		}
	}
	return nil
}

//获得指定path相对于base的路径
func MakeRelative(path, base string) (string, error) {
	if len(path) > 0 {
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return path, err
		}
		return rel, nil
	}
	return path, nil
}
