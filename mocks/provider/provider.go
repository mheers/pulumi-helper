// Copyright 2016-2023, Pulumi Corporation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang/protobuf/ptypes/empty"
	pbempty "github.com/golang/protobuf/ptypes/empty"
	structpb "github.com/golang/protobuf/ptypes/struct"
	pkgerrors "github.com/pkg/errors"
	"github.com/pulumi/pulumi-kubernetes/provider/v4/pkg/await"
	"github.com/pulumi/pulumi-kubernetes/provider/v4/pkg/clients"
	"github.com/pulumi/pulumi-kubernetes/provider/v4/pkg/cluster"
	"github.com/pulumi/pulumi-kubernetes/provider/v4/pkg/gen"
	"github.com/pulumi/pulumi-kubernetes/provider/v4/pkg/kinds"
	"github.com/pulumi/pulumi-kubernetes/provider/v4/pkg/logging"
	"github.com/pulumi/pulumi-kubernetes/provider/v4/pkg/metadata"
	"github.com/pulumi/pulumi-kubernetes/provider/v4/pkg/openapi"
	"github.com/pulumi/pulumi/pkg/v3/resource/deploy/providers"
	"github.com/pulumi/pulumi/pkg/v3/resource/provider"
	"github.com/pulumi/pulumi/sdk/v3/go/common/diag"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/plugin"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/contract"
	logger "github.com/pulumi/pulumi/sdk/v3/go/common/util/logging"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/rpcutil/rpcerror"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/helmpath"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientapi "k8s.io/client-go/tools/clientcmd/api"
	k8sopenapi "k8s.io/kubectl/pkg/util/openapi"
	"sigs.k8s.io/yaml"
)

// --------------------------------------------------------------------------

// Kubernetes resource provider.
//
// Implements functionality for the Pulumi Kubernetes Resource Provider. This code is responsible
// for producing sensible responses for the gRPC server to send back to a client when it requests
// something to do with the Kubernetes resources it's meant to manage.

// --------------------------------------------------------------------------

const (
	streamInvokeList     = "kubernetes:kubernetes:list"
	streamInvokeWatch    = "kubernetes:kubernetes:watch"
	streamInvokePodLogs  = "kubernetes:kubernetes:podLogs"
	invokeDecodeYaml     = "kubernetes:yaml:decode"
	invokeHelmTemplate   = "kubernetes:helm:template"
	invokeKustomize      = "kubernetes:kustomize:directory"
	lastAppliedConfigKey = "kubectl.kubernetes.io/last-applied-configuration"
	initialAPIVersionKey = "__initialApiVersion"
	fieldManagerKey      = "__fieldManager"
	secretKind           = "Secret"
)

type cancellationContext struct {
	context context.Context
	cancel  context.CancelFunc
}

func makeCancellationContext() *cancellationContext {
	ctx, cancel := context.WithCancel(context.Background())
	return &cancellationContext{
		context: ctx,
		cancel:  cancel,
	}
}

type kubeOpts struct {
	rejectUnknownResources bool
}

type KubeProvider struct {
	pulumirpc.UnimplementedResourceProviderServer

	host             *provider.HostClient
	canceler         *cancellationContext
	name             string
	version          string
	pulumiSchema     []byte
	providerPackage  string
	opts             kubeOpts
	defaultNamespace string

	deleteUnreachable           bool
	enableConfigMapMutable      bool
	enableSecrets               bool
	suppressDeprecationWarnings bool
	suppressHelmHookWarnings    bool
	serverSideApplyMode         bool

	helmDriver               string
	helmPluginsPath          string
	helmRegistryConfigPath   string
	helmRepositoryConfigPath string
	helmRepositoryCache      string
	helmReleaseProvider      customResourceProvider

	yamlRenderMode bool
	yamlDirectory  string

	clusterUnreachable       bool   // Kubernetes cluster is unreachable.
	clusterUnreachableReason string // Detailed error message if cluster is unreachable.

	config     *rest.Config // Cluster config, e.g., through $KUBECONFIG file.
	kubeconfig clientcmd.ClientConfig
	clientSet  *clients.DynamicClientSet
	logClient  *clients.LogClient
	k8sVersion cluster.ServerVersion

	resources      k8sopenapi.Resources
	resourcesMutex sync.RWMutex
}

var _ pulumirpc.ResourceProviderServer = (*KubeProvider)(nil)

func MakeKubeProvider(
	host *provider.HostClient, name, version string, pulumiSchema []byte,
) (pulumirpc.ResourceProviderServer, error) {
	return &KubeProvider{
		host:                        host,
		canceler:                    makeCancellationContext(),
		name:                        name,
		version:                     version,
		pulumiSchema:                pulumiSchema,
		providerPackage:             name,
		enableSecrets:               false,
		suppressDeprecationWarnings: false,
		deleteUnreachable:           false,
	}, nil
}

func (k *KubeProvider) defaultKubeVersion() *chartutil.KubeVersion {

	version := k.version // e.g. v1.25
	major := strings.Split(version, ".")[0]
	major = strings.ReplaceAll(major, "v", "")
	minor := strings.Split(version, ".")[1]

	defaultKubeVersion := &chartutil.KubeVersion{
		Version: k.version,
		Major:   major,
		Minor:   minor,
	}

	return defaultKubeVersion
}

func (k *KubeProvider) HelmTemplate(opts HelmChartOpts) (string, error) {
	return helmTemplate(opts, k.clientSet, k.defaultKubeVersion())
}

func (k *KubeProvider) DecodeYaml(text string, defaultNamespace string) ([]interface{}, error) {
	return decodeYaml(text, defaultNamespace, k.clientSet)
}

func (k *KubeProvider) getResources() (k8sopenapi.Resources, error) {
	k.resourcesMutex.RLock()
	rs := k.resources
	k.resourcesMutex.RUnlock()

	if rs != nil {
		return rs, nil
	}

	k.resourcesMutex.Lock()
	defer k.resourcesMutex.Unlock()

	rs, err := openapi.GetResourceSchemasForClient(k.clientSet.DiscoveryClientCached)
	if err != nil {
		return nil, err
	}
	k.resources = rs
	return k.resources, nil
}

// Call dynamically executes a method in the provider associated with a component resource.
func (k *KubeProvider) Call(ctx context.Context, req *pulumirpc.CallRequest) (*pulumirpc.CallResponse, error) {
	return nil, status.Error(codes.Unimplemented, "Call is not yet implemented")
}

// GetMapping fetches the mapping for this resource provider, if any. A provider should return an empty
// response (not an error) if it doesn't have a mapping for the given key.
func (k *KubeProvider) GetMapping(ctx context.Context, req *pulumirpc.GetMappingRequest) (*pulumirpc.GetMappingResponse, error) {
	return nil, status.Error(codes.Unimplemented, "GetMapping is not yet implemented")
}

// Construct creates a new instance of the provided component resource and returns its state.
func (k *KubeProvider) Construct(ctx context.Context, req *pulumirpc.ConstructRequest) (*pulumirpc.ConstructResponse, error) {
	return nil, status.Error(codes.Unimplemented, "Construct is not yet implemented")
}

// GetSchema returns the JSON-encoded schema for this provider's package.
func (k *KubeProvider) GetSchema(ctx context.Context, req *pulumirpc.GetSchemaRequest) (*pulumirpc.GetSchemaResponse, error) {
	if v := req.GetVersion(); v != 0 {
		return nil, fmt.Errorf("unsupported schema version %d", v)
	}

	return &pulumirpc.GetSchemaResponse{Schema: string(k.pulumiSchema)}, nil
}

// CheckConfig validates the configuration for this provider.
func (k *KubeProvider) CheckConfig(ctx context.Context, req *pulumirpc.CheckRequest) (*pulumirpc.CheckResponse, error) {
	urn := resource.URN(req.GetUrn())
	label := fmt.Sprintf("%s.CheckConfig(%s)", k.label(), urn)
	logger.V(9).Infof("%s executing", label)

	news, err := plugin.UnmarshalProperties(req.GetNews(), plugin.MarshalOptions{
		Label:        fmt.Sprintf("%s.news", label),
		KeepUnknowns: true,
		SkipNulls:    true,
	})
	if err != nil {
		return nil, pkgerrors.Wrapf(err, "CheckConfig failed because of malformed resource inputs")
	}

	truthyValue := func(argName resource.PropertyKey, props resource.PropertyMap) bool {
		if arg := props[argName]; arg.HasValue() {
			switch {
			case arg.IsString() && len(arg.StringValue()) > 0:
				return true
			case arg.IsBool() && arg.BoolValue():
				return true
			default:
				return false
			}
		}
		return false
	}

	strictMode := false
	if pConfig, ok := k.loadPulumiConfig(); ok {
		if v, ok := pConfig["strictMode"]; ok {
			if v, ok := v.(string); ok {
				strictMode = v == "true"
			}
		}
	}
	if v := news["strictMode"]; v.HasValue() && v.IsString() {
		strictMode = v.StringValue() == "true"
	}

	if strictMode && providers.IsProviderType(urn.Type()) {
		var failures []*pulumirpc.CheckFailure

		if providers.IsDefaultProvider(urn) {
			failures = append(failures, &pulumirpc.CheckFailure{
				Reason: fmt.Sprintf("strict mode prohibits default provider"),
			})
		}
		if v := news["kubeconfig"]; !v.HasValue() || v.StringValue() == "" {
			failures = append(failures, &pulumirpc.CheckFailure{
				Property: "kubeconfig",
				Reason:   fmt.Sprintf(`strict mode requires Provider "kubeconfig" argument`),
			})
		}
		if v := news["context"]; !v.HasValue() || v.StringValue() == "" {
			failures = append(failures, &pulumirpc.CheckFailure{
				Property: "context",
				Reason:   fmt.Sprintf(`strict mode requires Provider "context" argument`),
			})
		}

		if len(failures) > 0 {
			return &pulumirpc.CheckResponse{Inputs: req.GetNews(), Failures: failures}, nil
		}
	}

	renderYamlEnabled := truthyValue("renderYamlToDirectory", news)

	errTemplate := `%q arg is not compatible with "renderYamlToDirectory" arg`
	if renderYamlEnabled {
		var failures []*pulumirpc.CheckFailure

		if truthyValue("cluster", news) {
			failures = append(failures, &pulumirpc.CheckFailure{
				Property: "cluster",
				Reason:   fmt.Sprintf(errTemplate, "cluster"),
			})
		}
		if truthyValue("context", news) {
			failures = append(failures, &pulumirpc.CheckFailure{
				Property: "context",
				Reason:   fmt.Sprintf(errTemplate, "context"),
			})
		}
		if truthyValue("kubeconfig", news) {
			failures = append(failures, &pulumirpc.CheckFailure{
				Property: "kubeconfig",
				Reason:   fmt.Sprintf(errTemplate, "kubeconfig"),
			})
		}

		if len(failures) > 0 {
			return &pulumirpc.CheckResponse{Inputs: req.GetNews(), Failures: failures}, nil
		}
	}

	return &pulumirpc.CheckResponse{Inputs: req.GetNews()}, nil
}

// Configure configures the resource provider with "globals" that control its behavior.
func (k *KubeProvider) Configure(_ context.Context, req *pulumirpc.ConfigureRequest) (*pulumirpc.ConfigureResponse, error) {
	const trueStr = "true"

	vars := req.GetVariables()

	//
	// Set simple configuration settings.
	//

	k.opts = kubeOpts{
		rejectUnknownResources: vars["kubernetes:config:rejectUnknownResources"] == trueStr,
	}
	k.enableSecrets = req.GetAcceptSecrets()

	//
	// Configure client-go using provided or ambient kubeconfig file.
	//
	if defaultNamespace := vars["kubernetes:config:namespace"]; defaultNamespace != "" {
		k.defaultNamespace = defaultNamespace
	}

	// Compute config overrides.
	overrides := &clientcmd.ConfigOverrides{
		Context: clientapi.Context{
			Cluster:   vars["kubernetes:config:cluster"],
			Namespace: k.defaultNamespace,
		},
		CurrentContext: vars["kubernetes:config:context"],
	}

	deleteUnreachable := func() bool {
		// If the provider flag is set, use that value to determine behavior. This will override the ENV var.
		if enabled, exists := vars["kubernetes:config:deleteUnreachable"]; exists {
			return enabled == trueStr
		}
		// If the provider flag is not set, fall back to the ENV var.
		if enabled, exists := os.LookupEnv("PULUMI_K8S_DELETE_UNREACHABLE"); exists {
			return enabled == trueStr
		}
		// Default to false.
		return false
	}
	if deleteUnreachable() {
		k.deleteUnreachable = true
	}

	enableServerSideApply := func() bool {
		// If the provider flag is set, use that value to determine behavior. This will override the ENV var.
		if enabled, exists := vars["kubernetes:config:enableServerSideApply"]; exists {
			return enabled == trueStr
		}
		// If the provider flag is not set, fall back to the ENV var.
		if enabled, exists := os.LookupEnv("PULUMI_K8S_ENABLE_SERVER_SIDE_APPLY"); exists {
			return enabled == trueStr
		}
		// Default to true.
		return true
	}
	if enableServerSideApply() {
		k.serverSideApplyMode = true
	}

	enableConfigMapMutable := func() bool {
		// If the provider flag is set, use that value to determine behavior. This will override the ENV var.
		if enabled, exists := vars["kubernetes:config:enableConfigMapMutable"]; exists {
			return enabled == trueStr
		}
		// If the provider flag is not set, fall back to the ENV var.
		if enabled, exists := os.LookupEnv("PULUMI_K8S_ENABLE_CONFIGMAP_MUTABLE"); exists {
			return enabled == trueStr
		}
		// Default to false.
		return false
	}
	if enableConfigMapMutable() {
		k.enableConfigMapMutable = true
	}

	suppressDeprecationWarnings := func() bool {
		// If the provider flag is set, use that value to determine behavior. This will override the ENV var.
		if enabled, exists := vars["kubernetes:config:suppressDeprecationWarnings"]; exists {
			return enabled == trueStr
		}
		// If the provider flag is not set, fall back to the ENV var.
		if enabled, exists := os.LookupEnv("PULUMI_K8S_SUPPRESS_DEPRECATION_WARNINGS"); exists {
			return enabled == trueStr
		}
		// Default to false.
		return false
	}
	if suppressDeprecationWarnings() {
		k.suppressDeprecationWarnings = true
	}

	suppressHelmHookWarnings := func() bool {
		// If the provider flag is set, use that value to determine behavior. This will override the ENV var.
		if enabled, exists := vars["kubernetes:config:suppressHelmHookWarnings"]; exists {
			return enabled == trueStr
		}
		// If the provider flag is not set, fall back to the ENV var.
		if enabled, exists := os.LookupEnv("PULUMI_K8S_SUPPRESS_HELM_HOOK_WARNINGS"); exists {
			return enabled == trueStr
		}
		// Default to false.
		return false
	}
	if suppressHelmHookWarnings() {
		k.suppressHelmHookWarnings = true
	}

	renderYamlToDirectory := func() string {
		// Read the config from the Provider.
		if directory, exists := vars["kubernetes:config:renderYamlToDirectory"]; exists && directory != "" {
			return directory
		}
		return ""
	}
	k.yamlDirectory = renderYamlToDirectory()
	k.yamlRenderMode = len(k.yamlDirectory) > 0

	var helmReleaseSettings HelmReleaseSettings
	if obj, ok := vars["kubernetes:config:helmReleaseSettings"]; ok {
		err := json.Unmarshal([]byte(obj), &helmReleaseSettings)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal helmReleaseSettings option: %w", err)
		}
	}

	// TODO: Once https://github.com/pulumi/pulumi/issues/8132 is fixed, we can drop the env var handling logic.

	helmDriver := func() string {
		if helmReleaseSettings.Driver != nil {
			return *helmReleaseSettings.Driver
		}

		// If the provider flag is not set, fall back to the ENV var.
		if driver, exists := os.LookupEnv("PULUMI_K8S_HELM_DRIVER"); exists {
			return driver
		}
		return "secret"
	}
	k.helmDriver = helmDriver() // TODO: Make sure this is in provider state

	helmPluginsPath := func() string {
		if helmReleaseSettings.PluginsPath != nil {
			return *helmReleaseSettings.PluginsPath
		}

		// If the provider flag is not set, fall back to the ENV var.
		if pluginsPath, exists := os.LookupEnv("PULUMI_K8S_HELM_PLUGINS_PATH"); exists {
			return pluginsPath
		}
		return helmpath.DataPath("plugins")
	}
	k.helmPluginsPath = helmPluginsPath()

	helmRegistryConfigPath := func() string {
		if helmReleaseSettings.RegistryConfigPath != nil {
			return *helmReleaseSettings.RegistryConfigPath
		}

		// If the provider flag is not set, fall back to the ENV var.
		if registryPath, exists := os.LookupEnv("PULUMI_K8S_HELM_REGISTRY_CONFIG_PATH"); exists {
			return registryPath
		}
		return helmpath.ConfigPath("registry.json")
	}
	k.helmRegistryConfigPath = helmRegistryConfigPath()

	helmRepositoryConfigPath := func() string {
		if helmReleaseSettings.RepositoryConfigPath != nil {
			return *helmReleaseSettings.RepositoryConfigPath
		}

		if repositoryConfigPath, exists := os.LookupEnv("PULUMI_K8S_HELM_REPOSITORY_CONFIG_PATH"); exists {
			return repositoryConfigPath
		}
		return helmpath.ConfigPath("repositories.yaml")
	}
	k.helmRepositoryConfigPath = helmRepositoryConfigPath()

	helmRepositoryCache := func() string {
		if helmReleaseSettings.RepositoryCache != nil {
			return *helmReleaseSettings.RepositoryCache
		}

		if repositoryCache, exists := os.LookupEnv("PULUMI_K8S_HELM_REPOSITORY_CACHE"); exists {
			return repositoryCache
		}
		return helmpath.CachePath("repository")
	}
	k.helmRepositoryCache = helmRepositoryCache()

	// Rather than erroring out on an invalid k8s config, mark the cluster as unreachable and conditionally bail out on
	// operations that require a valid cluster. This will allow us to perform invoke operations using the default
	// provider.
	unreachableCluster := func(err error) {
		k.clusterUnreachable = true
		k.clusterUnreachableReason = fmt.Sprintf(
			"failed to parse kubeconfig data in `kubernetes:config:kubeconfig`- %v", err)
	}

	var kubeconfig clientcmd.ClientConfig
	var apiConfig *clientapi.Config
	homeDir := func() string {
		// Ignore errors. The filepath will be checked later, so we can handle failures there.
		usr, _ := user.Current()
		return usr.HomeDir
	}
	// Note: the Python SDK was setting the kubeconfig value to "" by default, so explicitly check for empty string.
	if pathOrContents, ok := vars["kubernetes:config:kubeconfig"]; ok && pathOrContents != "" {
		var contents string

		// Handle the '~' character if it is set in the config string. Normally, this would be expanded by the shell
		// into the user's home directory, but we have to do that manually if it is set in a config value.
		if pathOrContents == "~" {
			// In case of "~", which won't be caught by the "else if"
			pathOrContents = homeDir()
		} else if strings.HasPrefix(pathOrContents, "~/") {
			pathOrContents = filepath.Join(homeDir(), pathOrContents[2:])
		}

		// If the variable is a valid filepath, load the file and parse the contents as a k8s config.
		_, err := os.Stat(pathOrContents)
		if err == nil {
			b, err := os.ReadFile(pathOrContents)
			if err != nil {
				unreachableCluster(err)
			} else {
				contents = string(b)
			}
		} else { // Assume the contents are a k8s config.
			contents = pathOrContents
		}

		// Load the contents of the k8s config.
		apiConfig, err = clientcmd.Load([]byte(contents))
		if err != nil {
			unreachableCluster(err)
		} else {
			kubeconfig = clientcmd.NewDefaultClientConfig(*apiConfig, overrides)
			configurationNamespace, _, err := kubeconfig.Namespace()
			if err == nil {
				k.defaultNamespace = configurationNamespace
			}
		}
	} else {
		// Use client-go to resolve the final configuration values for the client. Typically, these
		// values would reside in the $KUBECONFIG file, but can also be altered in several
		// places, including in env variables, client-go default values, and (if we allowed it) CLI
		// flags.
		loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
		loadingRules.DefaultClientConfig = &clientcmd.DefaultClientConfig
		kubeconfig = clientcmd.NewInteractiveDeferredLoadingClientConfig(loadingRules, overrides, os.Stdin)
		configurationNamespace, _, err := kubeconfig.Namespace()
		if err == nil {
			k.defaultNamespace = configurationNamespace
		}
	}

	var kubeClientSettings KubeClientSettings
	if obj, ok := vars["kubernetes:config:kubeClientSettings"]; ok {
		err := json.Unmarshal([]byte(obj), &kubeClientSettings)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal kubeClientSettings option: %w", err)
		}
	}

	// TODO: Once https://github.com/pulumi/pulumi/issues/8132 is fixed, we can drop the env var handling logic.
	if burst := os.Getenv("PULUMI_K8S_CLIENT_BURST"); burst != "" && kubeClientSettings.Burst == nil {
		asInt, err := strconv.Atoi(burst)
		if err != nil {
			return nil, fmt.Errorf("invalid value specified for PULUMI_K8S_CLIENT_BURST: %w", err)
		}
		kubeClientSettings.Burst = &asInt
	} else {
		v := 120 // Increased from default value of 10
		kubeClientSettings.Burst = &v
	}

	if qps := os.Getenv("PULUMI_K8S_CLIENT_QPS"); qps != "" && kubeClientSettings.QPS == nil {
		asFloat, err := strconv.ParseFloat(qps, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid value specified for PULUMI_K8S_CLIENT_QPS: %w", err)
		}
		kubeClientSettings.QPS = &asFloat
	} else {
		v := 50.0 // Increased from default value of 5.0
		kubeClientSettings.QPS = &v
	}

	if timeout := os.Getenv("PULUMI_K8S_CLIENT_TIMEOUT"); timeout != "" && kubeClientSettings.Timeout == nil {
		asInt, err := strconv.Atoi(timeout)
		if err != nil {
			return nil, fmt.Errorf("invalid value specified for PULUMI_K8S_CLIENT_TIMEOUT: %w", err)
		}
		kubeClientSettings.Timeout = &asInt
	}

	// Attempt to load the configuration from the provided kubeconfig. If this fails, mark the cluster as unreachable.
	if !k.clusterUnreachable {
		config, err := kubeconfig.ClientConfig()
		if err != nil {
			k.clusterUnreachable = true
			k.clusterUnreachableReason = fmt.Sprintf("unable to load Kubernetes client configuration from kubeconfig file. Make sure you have: \n\n"+
				" \t • set up the provider as per https://www.pulumi.com/registry/packages/kubernetes/installation-configuration/ \n\n %v", err)
		} else {
			if kubeClientSettings.Burst != nil {
				config.Burst = *kubeClientSettings.Burst
				logger.V(9).Infof("kube client burst set to %v", config.Burst)
			}
			if kubeClientSettings.QPS != nil {
				config.QPS = float32(*kubeClientSettings.QPS)
				logger.V(9).Infof("kube client QPS set to %v", config.QPS)
			}
			if kubeClientSettings.Timeout != nil {
				config.Timeout = time.Duration(*kubeClientSettings.Timeout) * time.Second
				logger.V(9).Infof("kube client timeout set to %v", config.Timeout)
			}
			warningConfig := rest.CopyConfig(config)
			warningConfig.WarningHandler = rest.NoWarnings{}
			k.config = warningConfig
			k.kubeconfig = kubeconfig
		}
	}

	// These operations require a reachable cluster.
	if !k.clusterUnreachable {
		cs, err := clients.NewDynamicClientSet(k.config)
		if err != nil {
			return nil, err
		}
		k.clientSet = cs
		lc, err := clients.NewLogClient(k.canceler.context, k.config)
		if err != nil {
			return nil, err
		}
		k.logClient = lc

		k.k8sVersion = cluster.TryGetServerVersion(cs.DiscoveryClientCached)

		if k.k8sVersion.Compare(cluster.ServerVersion{Major: 1, Minor: 13}) < 0 {
			return nil, fmt.Errorf("minimum supported cluster version is v1.13. found v%s", k.k8sVersion)
		}

		if _, err = k.getResources(); err != nil {
			k.clusterUnreachable = true
			k.clusterUnreachableReason = fmt.Sprintf(
				"unable to load schema information from the API server: %v", err)
		}
	}

	var err error

	k.helmReleaseProvider, err = newHelmReleaseProvider(
		k.host,
		apiConfig,
		overrides,
		k.config,
		k.clientSet,
		k.helmDriver,
		k.defaultNamespace,
		k.enableSecrets,
		k.helmPluginsPath,
		k.helmRegistryConfigPath,
		k.helmRepositoryConfigPath,
		k.helmRepositoryCache,
		k.clusterUnreachable,
		k.clusterUnreachableReason)
	if err != nil {
		return nil, err
	}

	return &pulumirpc.ConfigureResponse{
		AcceptSecrets:   true,
		SupportsPreview: true,
	}, nil
}

// Invoke dynamically executes a built-in function in the provider.
func (k *KubeProvider) Invoke(ctx context.Context,
	req *pulumirpc.InvokeRequest) (*pulumirpc.InvokeResponse, error) {

	// Important: Some invoke logic is intended to run during preview, and the Kubernetes provider
	// inputs may not have resolved yet. Any invoke logic that depends on an active cluster must check
	// k.clusterUnreachable and handle that condition appropriately.

	tok := req.GetTok()
	label := fmt.Sprintf("%s.Invoke(%s)", k.label(), tok)
	args, err := plugin.UnmarshalProperties(
		req.GetArgs(), plugin.MarshalOptions{Label: label, KeepUnknowns: true})
	if err != nil {
		return nil, pkgerrors.Wrapf(err, "failed to unmarshal %v args during an Invoke call", tok)
	}

	switch tok {
	case invokeDecodeYaml:
		var text, defaultNamespace string
		if textArg := args["text"]; textArg.HasValue() && textArg.IsString() {
			text = textArg.StringValue()
		} else {
			return nil, pkgerrors.New("missing required field 'text' of type string")
		}
		if defaultNsArg := args["defaultNamespace"]; defaultNsArg.HasValue() && defaultNsArg.IsString() {
			defaultNamespace = defaultNsArg.StringValue()
		}

		result, err := decodeYaml(text, defaultNamespace, k.clientSet)
		if err != nil {
			return nil, err
		}

		objProps, err := plugin.MarshalProperties(
			resource.NewPropertyMapFromMap(map[string]any{"result": result}),
			plugin.MarshalOptions{
				Label: label, KeepUnknowns: true, SkipNulls: true,
			})
		if err != nil {
			return nil, err
		}

		return &pulumirpc.InvokeResponse{Return: objProps}, nil
	case invokeHelmTemplate:
		var jsonOpts string
		if jsonOptsArgs := args["jsonOpts"]; jsonOptsArgs.HasValue() && jsonOptsArgs.IsString() {
			jsonOpts = jsonOptsArgs.StringValue()
		} else {
			return nil, pkgerrors.New("missing required field 'jsonOpts' of type string")
		}

		var opts HelmChartOpts
		err = json.Unmarshal([]byte(jsonOpts), &opts)
		if err != nil {
			return nil, pkgerrors.Wrap(err, "failed to unmarshal 'jsonOpts'")
		}

		text, err := helmTemplate(opts, k.clientSet, k.defaultKubeVersion())
		if err != nil {
			return nil, pkgerrors.Wrap(err, "failed to generate YAML for specified Helm chart")
		}

		// Decode the generated YAML here to avoid an extra invoke in the client.
		result, err := decodeYaml(text, opts.Namespace, k.clientSet)
		if err != nil {
			return nil, pkgerrors.Wrap(err, "failed to decode YAML for specified Helm chart")
		}

		objProps, err := plugin.MarshalProperties(
			resource.NewPropertyMapFromMap(map[string]any{"result": result}),
			plugin.MarshalOptions{
				Label: label, KeepUnknowns: true, SkipNulls: true,
			})
		if err != nil {
			return nil, err
		}

		return &pulumirpc.InvokeResponse{Return: objProps}, nil

	default:
		return nil, fmt.Errorf("unknown Invoke type %q", tok)
	}
}

// StreamInvoke dynamically executes a built-in function in the provider. The result is streamed
// back as a series of messages.
func (k *KubeProvider) StreamInvoke(
	req *pulumirpc.InvokeRequest, server pulumirpc.ResourceProvider_StreamInvokeServer) error {

	// Important: Some invoke logic is intended to run during preview, and the Kubernetes provider
	// inputs may not have resolved yet. Any invoke logic that depends on an active cluster must check
	// k.clusterUnreachable and handle that condition appropriately.

	// Unmarshal arguments.
	tok := req.GetTok()
	label := fmt.Sprintf("%s.StreamInvoke(%s)", k.label(), tok)
	args, err := plugin.UnmarshalProperties(
		req.GetArgs(), plugin.MarshalOptions{Label: label, KeepUnknowns: true})
	if err != nil {
		return pkgerrors.Wrapf(err, "failed to unmarshal %v args during an StreamInvoke call", tok)
	}

	switch tok {
	case streamInvokeList:
		//
		// Request a list of all resources of some type, in some number of namespaces.
		//
		// DESIGN NOTES: `list` must be a `StreamInvoke` instead of an `Invoke` to avoid the gRPC
		// message size limit. Unlike `watch`, which will continue until the user cancels the
		// request, `list` is guaranteed to terminate after all the resources are listed. The role
		// of the SDK implementations of `list` is thus to wait for the stream to terminate,
		// aggregate the resources into a list, and return to the user.
		//
		// We send the resources asynchronously. This requires an "event loop" (below), which
		// continuously attempts to send the resource, checking for cancellation on each send. This
		// allows for the theoretical possibility that the gRPC client cancels the `list` operation
		// prior to completion. The SDKs implementing `list` will very probably never expose a
		// `cancel` handler in the way that `watch` does; `watch` requires it because a watcher is
		// expected to never terminate, and users of the various SDKs need a way to tell the
		// provider to stop streaming and reclaim the resources associated with the stream.
		//
		// Still, we implement this cancellation also for `list`, primarily for completeness. We'd
		// like to avoid an unpleasant and non-actionable error that would appear on a `Send` on a
		// client that is no longer accepting requests. This also helps to guard against the
		// possibility that some dark corner of gRPC signals cancellation by accident, e.g., during
		// shutdown.
		//

		if k.clusterUnreachable {
			return fmt.Errorf("configured Kubernetes cluster is unreachable: %s", k.clusterUnreachableReason)
		}

		namespace := ""
		if args["namespace"].HasValue() {
			namespace = args["namespace"].StringValue()
		}
		if !args["group"].HasValue() || !args["version"].HasValue() || !args["kind"].HasValue() {
			return fmt.Errorf(
				"list requires a group, version, and kind that uniquely specify the resource type")
		}
		cl, err := k.clientSet.ResourceClient(schema.GroupVersionKind{
			Group:   args["group"].StringValue(),
			Version: args["version"].StringValue(),
			Kind:    args["kind"].StringValue(),
		}, namespace)
		if err != nil {
			return err
		}

		list, err := cl.List(k.canceler.context, metav1.ListOptions{})
		if err != nil {
			return err
		}

		//
		// List resources. Send them one-by-one, asynchronously, to the client requesting them.
		//

		objects := make(chan map[string]any)
		defer close(objects)
		done := make(chan struct{})
		defer close(done)
		go func() {
			for _, o := range list.Items {
				objects <- o.Object
			}
			done <- struct{}{}
		}()

		for {
			select {
			case <-k.canceler.context.Done():
				//
				// `KubeProvider#Cancel` was called. Terminate the `StreamInvoke` RPC, free all
				// resources, and exit without error.
				//

				return nil
			case <-done:
				//
				// Success. Return.
				//

				return nil
			case o := <-objects:
				//
				// Publish resource from the list back to user.
				//

				resp, err := plugin.MarshalProperties(
					resource.NewPropertyMapFromMap(o),
					plugin.MarshalOptions{})
				if err != nil {
					return err
				}

				err = server.Send(&pulumirpc.InvokeResponse{Return: resp})
				if err != nil {
					return err
				}
			case <-server.Context().Done():
				//
				// gRPC stream was cancelled from the client that issued the `StreamInvoke` request
				// to us. In this case, we terminate the `StreamInvoke` RPC, free all resources, and
				// exit without error.
				//
				// This is required for `watch`, but is implemented in `list` for completeness.
				// Users calling `watch` from one of the SDKs need to be able to cancel a `watch`
				// and signal to the provider that it's ok to reclaim the resources associated with
				// a `watch`. In `list` it's to prevent the user from getting weird errors if a
				// client somehow cancels the streaming request and they subsequently send a message
				// anyway.
				//

				return nil
			}
		}
	case streamInvokeWatch:
		//
		// Set up resource watcher.
		//

		if k.clusterUnreachable {
			return fmt.Errorf("configured Kubernetes cluster is unreachable: %s", k.clusterUnreachableReason)
		}

		namespace := ""
		if args["namespace"].HasValue() {
			namespace = args["namespace"].StringValue()
		}
		if !args["group"].HasValue() || !args["version"].HasValue() || !args["kind"].HasValue() {
			return fmt.Errorf(
				"watch requires a group, version, and kind that uniquely specify the resource type")
		}
		cl, err := k.clientSet.ResourceClient(schema.GroupVersionKind{
			Group:   args["group"].StringValue(),
			Version: args["version"].StringValue(),
			Kind:    args["kind"].StringValue(),
		}, namespace)
		if err != nil {
			return err
		}

		watch, err := cl.Watch(k.canceler.context, metav1.ListOptions{})
		if err != nil {
			return err
		}

		//
		// Watch for resource updates, and stream them back to the caller.
		//

		for {
			select {
			case <-k.canceler.context.Done():
				//
				// `KubeProvider#Cancel` was called. Terminate the `StreamInvoke` RPC, free all
				// resources, and exit without error.
				//

				watch.Stop()
				return nil
			case event := <-watch.ResultChan():
				//
				// Kubernetes resource was updated. Publish resource update back to user.
				//

				resp, err := plugin.MarshalProperties(
					resource.NewPropertyMapFromMap(
						map[string]any{
							"type":   event.Type,
							"object": event.Object.(*unstructured.Unstructured).Object,
						}),
					plugin.MarshalOptions{})
				if err != nil {
					return err
				}

				err = server.Send(&pulumirpc.InvokeResponse{Return: resp})
				if err != nil {
					return err
				}
			case <-server.Context().Done():
				//
				// gRPC stream was cancelled from the client that issued the `StreamInvoke` request
				// to us. In this case, we terminate the `StreamInvoke` RPC, free all resources, and
				// exit without error.
				//
				// Usually, this happens in the language provider, e.g., in the call to `cancel`
				// below.
				//
				//     const deployments = await streamInvoke("kubernetes:kubernetes:watch", {
				//         group: "apps", version: "v1", kind: "Deployment",
				//     });
				//     deployments.cancel();
				//

				watch.Stop()
				return nil
			}
		}
	case streamInvokePodLogs:
		//
		// Set up log stream for Pod.
		//

		if k.clusterUnreachable {
			return fmt.Errorf("configured Kubernetes cluster is unreachable: %s", k.clusterUnreachableReason)
		}

		namespace := "default"
		if args["namespace"].HasValue() {
			namespace = args["namespace"].StringValue()
		}

		if !args["name"].HasValue() {
			return fmt.Errorf(
				"could not retrieve pod logs because the pod name was not present")
		}
		name := args["name"].StringValue()

		podLogs, err := k.logClient.Logs(namespace, name)
		if err != nil {
			return err
		}
		defer contract.IgnoreClose(podLogs)

		//
		// Enumerate logs by line. Send back to the user.
		//
		// TODO: We send the logs back one-by-one, but we should probably batch them instead.
		//

		logLines := make(chan string)
		defer close(logLines)
		done := make(chan error)
		defer close(done)

		go func() {
			podLogLines := bufio.NewScanner(podLogs)
			for podLogLines.Scan() {
				logLines <- podLogLines.Text()
			}

			if err := podLogLines.Err(); err != nil {
				done <- err
			} else {
				done <- nil
			}
		}()

		for {
			select {
			case <-k.canceler.context.Done():
				//
				// `KubeProvider#Cancel` was called. Terminate the `StreamInvoke` RPC, free all
				// resources, and exit without error.
				//

				return nil
			case err := <-done:
				//
				// Complete. Return the error if applicable.
				//

				return err
			case line := <-logLines:
				//
				// Publish log line back to user.
				//

				resp, err := plugin.MarshalProperties(
					resource.NewPropertyMapFromMap(
						map[string]any{"lines": []string{line}}),
					plugin.MarshalOptions{})
				if err != nil {
					return err
				}

				err = server.Send(&pulumirpc.InvokeResponse{Return: resp})
				if err != nil {
					return err
				}
			case <-server.Context().Done():
				//
				// gRPC stream was cancelled from the client that issued the `StreamInvoke` request
				// to us. In this case, we terminate the `StreamInvoke` RPC, free all resources, and
				// exit without error.
				//
				// Usually, this happens in the language provider, e.g., in the call to `cancel`
				// below.
				//
				//     const podLogLines = await streamInvoke("kubernetes:kubernetes:podLogs", {
				//         namespace: "default", name: "nginx-f94d8bc55-xftvs",
				//     });
				//     podLogLines.cancel();
				//

				return nil
			}
		}
	default:
		return fmt.Errorf("unknown Invoke type '%s'", tok)
	}
}

// Attach sends the engine address to an already running plugin.
func (k *KubeProvider) Attach(_ context.Context, req *pulumirpc.PluginAttach) (*empty.Empty, error) {
	host, err := provider.NewHostClient(req.GetAddress())
	if err != nil {
		return nil, err
	}
	k.host = host
	return &empty.Empty{}, nil
}

// Check validates that the given property bag is valid for a resource of the given type and returns
// the inputs that should be passed to successive calls to Diff, Create, or Update for this
// resource. As a rule, the provider inputs returned by a call to Check should preserve the original
// representation of the properties as present in the program inputs. Though this rule is not
// required for correctness, violations thereof can negatively impact the end-user experience, as
// the provider inputs are using for detecting and rendering diffs.
func (k *KubeProvider) Check(ctx context.Context, req *pulumirpc.CheckRequest) (*pulumirpc.CheckResponse, error) {
	//
	// Behavior as of v0.12.x: We take two inputs:
	//
	// 1. req.News, the new resource inputs, i.e., the property bag coming from a custom resource like
	//    k8s.core.v1.Service
	// 2. req.Olds, the last version submitted from a custom resource.
	//
	// `req.Olds` are ignored (and are sometimes nil). `req.News` are validated, and `.metadata.name`
	// is given to it if it's not already provided.
	//

	urn := resource.URN(req.GetUrn())
	if isHelmRelease(urn) {
		return k.helmReleaseProvider.Check(ctx, req)
	}

	if isListURN(urn) {
		// TODO: It might be possible to automatically expand List resources into a list of the underlying resources.
		//       Until then, return a descriptive error message. https://github.com/pulumi/pulumi-kubernetes/issues/2494
		return nil, fmt.Errorf("list resources exist for compatibility with YAML manifests and Helm charts, " +
			"and cannot be created directly. Use the underlying resource type instead")
	}

	if !k.serverSideApplyMode && isPatchURN(urn) {
		return nil, fmt.Errorf("patch resources require Server-side Apply mode, which is enabled using the " +
			"`enableServerSideApply` Provider config")
	}

	label := fmt.Sprintf("%s.Check(%s)", k.label(), urn)
	logger.V(9).Infof("%s executing", label)

	// Obtain old resource inputs. This is the old version of the resource(s) supplied by the user as
	// an update.
	oldResInputs := req.GetOlds()
	olds, err := plugin.UnmarshalProperties(oldResInputs, plugin.MarshalOptions{
		Label: fmt.Sprintf("%s.olds", label), KeepUnknowns: true, SkipNulls: true, KeepSecrets: true,
	})
	if err != nil {
		return nil, err
	}

	// Obtain new resource inputs. This is the new version of the resource(s) supplied by the user as
	// an update.
	newResInputs := req.GetNews()
	news, err := plugin.UnmarshalProperties(newResInputs, plugin.MarshalOptions{
		Label:        fmt.Sprintf("%s.news", label),
		KeepUnknowns: true,
		SkipNulls:    true,
		KeepSecrets:  true,
	})
	if err != nil {
		return nil, pkgerrors.Wrapf(err, "check failed because malformed resource inputs: %+v", err)
	}

	oldInputs := propMapToUnstructured(olds)
	newInputs := propMapToUnstructured(news)

	newInputs, err = normalize(newInputs)
	if err != nil {
		return nil, err
	}

	if k.serverSideApplyMode && isPatchURN(urn) {
		if len(newInputs.GetName()) == 0 {
			return nil, fmt.Errorf("patch resources require the resource `.metadata.name` to be set")
		}
	}

	var failures []*pulumirpc.CheckFailure

	k.helmHookWarning(ctx, newInputs, urn)

	// Adopt name from old object if appropriate.
	//
	// If the user HAS NOT assigned a name in the new inputs, we autoname it and mark the object as
	// autonamed in `.metadata.annotations`. This makes it easier for `Diff` to decide whether this
	// needs to be `DeleteBeforeReplace`'d. If the resource is marked `DeleteBeforeReplace`, then
	// `Create` will allocate it a new name later.
	if len(oldInputs.Object) > 0 {
		// NOTE: If old inputs exist, they have a name, either provided by the user or filled in with a
		// previous run of `Check`.
		contract.Assertf(oldInputs.GetName() != "", "expected object name to be nonempty: %v", oldInputs)
		metadata.AdoptOldAutonameIfUnnamed(newInputs, oldInputs)

		// If the resource has existing state, we only set the "managed-by: pulumi" label if it is already present. This
		// avoids causing diffs for cases where the resource is being imported, or was created using SSA. The goal in
		// both cases is to leave the resource unchanged. The label is added if already present, or omitted if not.
		if metadata.HasManagedByLabel(oldInputs) {
			_, err = metadata.TrySetManagedByLabel(newInputs)
			if err != nil {
				return nil, pkgerrors.Wrapf(err,
					"Failed to create object because of a problem setting managed-by labels")
			}
		}
	} else {
		metadata.AssignNameIfAutonamable(req.RandomSeed, newInputs, news, urn)

		// Set a "managed-by: pulumi" label on resources created with Client-Side Apply. Do not set this label for SSA
		// resources since the fieldManagers field contains granular information about the managers.
		if !k.serverSideApplyMode {
			_, err = metadata.TrySetManagedByLabel(newInputs)
			if err != nil {
				return nil, pkgerrors.Wrapf(err,
					"Failed to create object because of a problem setting managed-by labels")
			}
		}
	}

	gvk, err := k.gvkFromURN(urn)
	if err != nil {
		return nil, err
	}

	// Skip the API version check if the cluster is unreachable.
	if !k.clusterUnreachable {
		if removed, version := kinds.RemovedAPIVersion(gvk, k.k8sVersion); removed {
			_ = k.host.Log(ctx, diag.Warning, urn, (&kinds.RemovedAPIError{GVK: gvk, Version: version}).Error())
		} else if !k.suppressDeprecationWarnings && kinds.DeprecatedAPIVersion(gvk, &k.k8sVersion) {
			_ = k.host.Log(ctx, diag.Warning, urn, gen.APIVersionComment(gvk))
		}
	}

	// If a default namespace is set on the provider for this resource, check if the resource has Namespaced
	// or Global scope. For namespaced resources, set the namespace to the default value if unset.
	if k.defaultNamespace != "" && len(newInputs.GetNamespace()) == 0 {
		namespacedKind, err := clients.IsNamespacedKind(gvk, k.clientSet)
		if err != nil {
			if clients.IsNoNamespaceInfoErr(err) {
				// This is probably a CustomResource without a registered CustomResourceDefinition.
				// Since we can't tell for sure at this point, assume it is namespaced, and correct if
				// required during the Create step.
				namespacedKind = true
			} else {
				return nil, err
			}
		}

		if namespacedKind {
			newInputs.SetNamespace(k.defaultNamespace)
		}
	}

	// HACK: Do not validate against OpenAPI spec if there is a computed value. The OpenAPI spec
	// does not know how to deal with the placeholder values for computed values.
	if !hasComputedValue(newInputs) && !k.clusterUnreachable {
		resources, err := k.getResources()
		if err != nil {
			return nil, pkgerrors.Wrapf(err, "Failed to fetch OpenAPI schema from the API server")
		}

		// Validate the object according to the OpenAPI schema for its GVK.
		err = openapi.ValidateAgainstSchema(resources, newInputs)
		if err != nil {
			resourceNotFound := apierrors.IsNotFound(err) ||
				strings.Contains(err.Error(), "is not supported by the server")
			k8sAPIUnreachable := strings.Contains(err.Error(), "connection refused")
			if resourceNotFound && k.gvkExists(newInputs) {
				failures = append(failures, &pulumirpc.CheckFailure{
					Reason: fmt.Sprintf(" Found API Group, but it did not contain a schema for %q", gvk),
				})
			} else if k8sAPIUnreachable {
				k8sURL := ""
				if err, ok := err.(*url.Error); ok {
					k8sURL = fmt.Sprintf("at %q", err.URL)
				}
				failures = append(failures, &pulumirpc.CheckFailure{
					Reason: fmt.Sprintf(" Kubernetes API server %s is unreachable. It's "+
						"possible that the URL or authentication information in your "+
						"kubeconfig is incorrect: %v", k8sURL, err),
				})
			} else if k.opts.rejectUnknownResources {
				// If the schema doesn't exist, it could still be a CRD (which may not have a
				// schema). Thus, if we are directed to check resources even if they have unknown
				// types, we fail here.
				return nil, pkgerrors.Wrapf(err, "unable to fetch schema for resource type %s/%s",
					newInputs.GetAPIVersion(), newInputs.GetKind())
			}
		}
	}

	checkedInputs := resource.NewPropertyMapFromMap(newInputs.Object)
	annotateSecrets(checkedInputs, news)

	autonamedInputs, err := plugin.MarshalProperties(checkedInputs, plugin.MarshalOptions{
		Label:        fmt.Sprintf("%s.autonamedInputs", label),
		KeepUnknowns: true,
		SkipNulls:    true,
		KeepSecrets:  k.enableSecrets,
	})
	if err != nil {
		return nil, err
	}

	if k.yamlRenderMode {
		if checkedInputs.ContainsSecrets() {
			_ = k.host.Log(ctx, diag.Warning, urn, "rendered YAML will contain a secret value in plaintext")
		}
	}

	// Return new, possibly-autonamed inputs.
	return &pulumirpc.CheckResponse{Inputs: autonamedInputs, Failures: failures}, nil
}

// helmHookWarning logs a warning if a Chart contains unsupported hooks. The warning can be disabled by setting
// the suppressHelmHookWarnings provider flag or related ENV var.
func (k *KubeProvider) helmHookWarning(ctx context.Context, newInputs *unstructured.Unstructured, urn resource.URN) {
	hasHelmHook := false
	for key, value := range newInputs.GetAnnotations() {
		// If annotations with a reserved internal prefix exist, ignore them.
		if metadata.IsInternalAnnotation(key) {
			_ = k.host.Log(ctx, diag.Warning, urn,
				fmt.Sprintf("ignoring user-specified value for internal annotation %q", key))
		}

		// If the Helm hook annotation is found, set the hasHelmHook flag.
		if has := metadata.IsHelmHookAnnotation(key); has {
			// Test hooks are handled, so ignore this one.
			if match, _ := regexp.MatchString(`test|test-success|test-failure`, value); !match {
				hasHelmHook = hasHelmHook || has
			}
		}
	}
	if hasHelmHook && !k.suppressHelmHookWarnings {
		_ = k.host.Log(ctx, diag.Warning, urn,
			"This resource contains Helm hooks that are not currently supported by Pulumi. The resource will "+
				"be created, but any hooks will not be executed. Hooks support is tracked at "+
				"https://github.com/pulumi/pulumi-kubernetes/issues/555 -- This warning can be disabled by setting "+
				"the PULUMI_K8S_SUPPRESS_HELM_HOOK_WARNINGS environment variable")
	}
}

// Delete tears down an existing resource with the given ID.  If it fails, the resource is assumed
// to still exist.
func (k *KubeProvider) Delete(ctx context.Context, req *pulumirpc.DeleteRequest) (*pbempty.Empty, error) {
	urn := resource.URN(req.GetUrn())
	label := fmt.Sprintf("%s.Delete(%s)", k.label(), urn)
	logger.V(9).Infof("%s executing", label)

	// TODO(hausdorff): Propagate other options, like grace period through flags.

	// Obtain new properties, create a Kubernetes `unstructured.Unstructured`.
	oldState, err := plugin.UnmarshalProperties(req.GetProperties(), plugin.MarshalOptions{
		Label: fmt.Sprintf("%s.olds", label), KeepUnknowns: true, SkipNulls: true, KeepSecrets: true,
	})
	if err != nil {
		return nil, err
	}
	oldInputs, _ := parseCheckpointObject(oldState)

	if isHelmRelease(urn) {
		if k.clusterUnreachable {
			return nil, fmt.Errorf("can't delete Helm Release with unreachable cluster. Reason: %q", k.clusterUnreachableReason)
		}
		return k.helmReleaseProvider.Delete(ctx, req)
	}

	_, current := parseCheckpointObject(oldState)
	_, name := parseFqName(req.GetId())

	if k.yamlRenderMode {
		file := renderPathForResource(current, k.yamlDirectory)
		err := os.Remove(file)
		if err != nil {
			// Most of the time, errors will be because the file was already deleted. In this case,
			// the operation succeeds. It's also possible that deletion fails due to file permission if
			// the user changed the directory out-of-band, so log the error to help debug this scenario.
			logger.V(3).Infof("Failed to delete YAML file: %q - %v", file, err)
		}

		_ = k.host.LogStatus(ctx, diag.Info, urn, fmt.Sprintf("deleted %s", file))

		return &pbempty.Empty{}, nil
	}

	if k.clusterUnreachable {
		_ = k.host.Log(ctx, diag.Warning, urn, fmt.Sprintf(
			"configured Kubernetes cluster is unreachable: %s", k.clusterUnreachableReason))
		if k.deleteUnreachable {
			_ = k.host.Log(ctx, diag.Info, urn, fmt.Sprintf(
				"configured Kubernetes cluster is unreachable and the `deleteUnreachable` option is enabled. "+
					"Deleting the unreachable resource from Pulumi state"))
			return &pbempty.Empty{}, nil
		}

		return nil, fmt.Errorf("configured Kubernetes cluster is unreachable. If the cluster was deleted, " +
			"you can remove this resource from Pulumi state by rerunning the operation with the " +
			"PULUMI_K8S_DELETE_UNREACHABLE environment variable set to \"true\"")
	}

	initialAPIVersion := initialAPIVersion(oldState, &unstructured.Unstructured{})
	fieldManager := k.fieldManagerName(nil, oldState, oldInputs)
	resources, err := k.getResources()
	if err != nil {
		return nil, pkgerrors.Wrapf(err, "Failed to fetch OpenAPI schema from the API server")
	}

	config := await.DeleteConfig{
		ProviderConfig: await.ProviderConfig{
			Context:           k.canceler.context, // TODO: should this just be ctx from the args?
			Host:              k.host,
			URN:               urn,
			InitialAPIVersion: initialAPIVersion,
			FieldManager:      fieldManager,
			ClientSet:         k.clientSet,
			DedupLogger:       logging.NewLogger(k.canceler.context, k.host, urn),
			Resources:         resources,
			ServerSideApply:   k.serverSideApplyMode,
		},
		Inputs:  current,
		Name:    name,
		Timeout: req.Timeout,
	}

	awaitErr := await.Deletion(config)
	if awaitErr != nil {
		if meta.IsNoMatchError(awaitErr) {
			// If it's a "no match" error, this is probably a CustomResource with no corresponding
			// CustomResourceDefinition. This usually happens if the CRD was deleted, and it's safe
			// to consider the CR to be deleted as well in this case.
			return &pbempty.Empty{}, nil
		}
		if isPatchURN(urn) && await.IsDeleteRequiredFieldErr(awaitErr) {
			if cause, ok := apierrors.StatusCause(awaitErr, metav1.CauseTypeFieldValueRequired); ok {
				awaitErr = fmt.Errorf(
					"this Patch resource is currently managing a required field, so it can't be deleted "+
						"directly. Either set the `retainOnDelete` resource option, or transfer ownership of the "+
						"field before deleting: %s", cause.Field)
			}
		}
		partialErr, isPartialErr := awaitErr.(await.PartialError)
		if !isPartialErr {
			// There was an error executing the delete operation. The resource is still present and tracked.
			return nil, awaitErr
		}

		lastKnownState := partialErr.Object()

		inputsAndComputed, err := plugin.MarshalProperties(
			checkpointObject(current, lastKnownState, oldState, initialAPIVersion, fieldManager), plugin.MarshalOptions{
				Label:        fmt.Sprintf("%s.inputsAndComputed", label),
				KeepUnknowns: true,
				SkipNulls:    true,
				KeepSecrets:  k.enableSecrets,
			})
		if err != nil {
			return nil, err
		}

		// Resource delete was issued, but failed to complete. Return live version of object so it can be
		// checkpointed.
		return nil, partialError(fqObjName(lastKnownState), awaitErr, inputsAndComputed, nil)
	}

	return &pbempty.Empty{}, nil
}

// isPatchURN returns true if the URN is for a Patch resource.
func isPatchURN(urn resource.URN) bool {
	return kinds.PatchQualifiedTypes.Has(urn.QualifiedType().String())
}

// isListURN returns true if the URN is for a List resource.
func isListURN(urn resource.URN) bool {
	return kinds.ListQualifiedTypes.Has(urn.QualifiedType().String())
}

// GetPluginInfo returns generic information about this plugin, like its version.
func (k *KubeProvider) GetPluginInfo(context.Context, *pbempty.Empty) (*pulumirpc.PluginInfo, error) {
	return &pulumirpc.PluginInfo{
		Version: k.version,
	}, nil
}

// Cancel signals the provider to gracefully shut down and abort any ongoing resource operations.
// Operations aborted in this way will return an error (e.g., `Update` and `Create` will either a
// creation error or an initialization error). Since Cancel is advisory and non-blocking, it is up
// to the host to decide how long to wait after Cancel is called before (e.g.)
// hard-closing any gRPC connection.
func (k *KubeProvider) Cancel(context.Context, *pbempty.Empty) (*pbempty.Empty, error) {
	k.canceler.cancel()
	return &pbempty.Empty{}, nil
}

// --------------------------------------------------------------------------

// Private helpers.

// --------------------------------------------------------------------------

func (k *KubeProvider) label() string {
	return fmt.Sprintf("Provider[%s]", k.name)
}

func (k *KubeProvider) gvkFromURN(urn resource.URN) (schema.GroupVersionKind, error) {
	if string(urn.Type().Package()) != k.providerPackage {
		return schema.GroupVersionKind{}, fmt.Errorf("unrecognized resource type: %q for this provider",
			urn.Type())
	}

	// Emit GVK.
	kind := string(urn.Type().Name())
	gv := strings.Split(string(urn.Type().Module().Name()), "/")
	if len(gv) != 2 {
		return schema.GroupVersionKind{},
			fmt.Errorf("apiVersion does not have both a group and a version: %q", urn.Type().Module().Name())
	}
	group, version := gv[0], gv[1]
	if group == "core" {
		group = ""
	}

	return schema.GroupVersionKind{
		Group:   group,
		Version: version,
		Kind:    kind,
	}, nil
}

// fieldManagerName returns the name to use for the Server-Side Apply fieldManager. The values are looked up with the
// following precedence:
// 1. Resource annotation (this will likely change to a typed option field in the next major release)
// 2. Value from the Pulumi state
// 3. Randomly generated name
func (k *KubeProvider) fieldManagerName(
	randomSeed []byte, state resource.PropertyMap, inputs *unstructured.Unstructured,
) string {
	// Always use the same fieldManager name for Client-Side Apply mode to avoid conflicts based on the name of the
	// provider executable.
	if !k.serverSideApplyMode {
		return "pulumi-kubernetes"
	}

	if v := metadata.GetAnnotationValue(inputs, metadata.AnnotationPatchFieldManager); len(v) > 0 {
		return v
	}
	if v, ok := state[fieldManagerKey]; ok {
		return v.StringValue()
	}

	prefix := "pulumi-kubernetes-"
	// This function is called from other provider function apart from Check and so doesn't have a randomSeed
	// for those calls, but the field manager name should have already been filled in via Check so this case
	// shouldn't actually get hit.
	fieldManager, err := resource.NewUniqueName(randomSeed, prefix, 0, 0, nil)
	contract.AssertNoErrorf(err, "unexpected error while creating NewUniqueName")

	return fieldManager
}

// gvkExists attempts to load a REST mapping for the given resource and returns true on success. Since this operation
// will fail if the GVK has not been registered with the apiserver, it can be used to indirectly check if the resource
// may be an unregistered CustomResource.
func (k *KubeProvider) gvkExists(obj *unstructured.Unstructured) bool {
	gvk := obj.GroupVersionKind()
	if k.clusterUnreachable {
		logger.V(3).Infof("gvkExists check failed due to unreachable cluster")
		return false
	}
	if _, err := k.clientSet.RESTMapper.RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
		if !meta.IsNoMatchError(err) {
			logger.V(3).Infof("RESTMapping(%q) returned unexpected error %v", gvk, err)
		}
		return false
	}
	return true
}

// loadPulumiConfig loads the PULUMI_CONFIG environment variable set by the engine, unmarshals the JSON string into
// a map, and returns the map and a bool indicating if the operation succeeded.
func (k *KubeProvider) loadPulumiConfig() (map[string]any, bool) {
	configStr, ok := os.LookupEnv("PULUMI_CONFIG")
	// PULUMI_CONFIG is not set on older versions of the engine, so check if the lookup succeeds.
	if !ok || configStr == "" {
		return nil, false
	}

	// PULUMI_CONFIG should be a JSON string that looks something like this:
	// {"enableServerSideApply":"true","kubeClientSettings":"{\"burst\":120,\"qps\":50}","strictMode":"true"}
	// The keys correspond to any project/stack config with a "kubernetes" prefix.
	var pConfig map[string]any
	err := json.Unmarshal([]byte(configStr), &pConfig)
	if err != nil {
		logger.V(3).Infof("failed to load provider config from PULUMI_CONFIG: %v", err)
		return nil, false
	}

	return pConfig, true
}

// removeLastAppliedConfigurationAnnotation is used by the Update method to remove an existing
// last-applied-configuration annotation from a resource. This annotation was set automatically by the provider, so it
// does not show up in the resource inputs. If the value is present in the live state, copy that value into the old
// inputs so that a negative diff will be generated for it.
func removeLastAppliedConfigurationAnnotation(oldLive, oldInputs *unstructured.Unstructured) {
	oldLiveValue, existsInOldLive, _ := unstructured.NestedString(oldLive.Object,
		"metadata", "annotations", lastAppliedConfigKey)
	_, existsInOldInputs, _ := unstructured.NestedString(oldInputs.Object,
		"metadata", "annotations", lastAppliedConfigKey)

	if existsInOldLive && !existsInOldInputs {
		contract.IgnoreError(unstructured.SetNestedField(
			oldInputs.Object, oldLiveValue, "metadata", "annotations", lastAppliedConfigKey))
	}
}

// pruneLiveState prunes a live resource object to match the shape of the input object that created the resource.
func pruneLiveState(live, oldInputs *unstructured.Unstructured) *unstructured.Unstructured {
	oldLivePruned := &unstructured.Unstructured{
		Object: pruneMap(live.Object, oldInputs.Object),
	}

	return oldLivePruned
}

// shouldNormalize returns false for CustomResources, and true otherwise.
func shouldNormalize(uns *unstructured.Unstructured) bool {
	return kinds.KnownGroupVersions.Has(uns.GetAPIVersion())
}

// normalize converts an Unstructured resource into a normalized form so that semantically equivalent representations
// are set to the same output shape. This is important to avoid generating diffs for inputs that will produce the same
// result on the cluster.
func normalize(uns *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if shouldNormalize(uns) {
		normalized, err := clients.Normalize(uns)
		if err != nil {
			return nil, err
		}
		uns = pruneLiveState(normalized, uns)
	}
	return uns, nil
}

func mapReplStripSecrets(v resource.PropertyValue) (any, bool) {
	if v.IsSecret() {
		return v.SecretValue().Element.MapRepl(nil, mapReplStripSecrets), true
	}

	return nil, false
}

// mapReplUnderscoreToDash is needed to work around cases where SDKs don't allow dashes in variable names, and so the
// parameter is renamed with an underscore during schema generation. This function normalizes those keys to the format
// expected by the cluster.
func mapReplUnderscoreToDash(v string) (string, bool) {
	switch v {
	case "x_kubernetes_embedded_resource":
		return "x-kubernetes-embedded-resource", true
	case "x_kubernetes_int_or_string":
		return "x-kubernetes-int-or-string", true
	case "x_kubernetes_list_map_keys":
		return "x-kubernetes-list-map-keys", true
	case "x_kubernetes_list_type":
		return "x-kubernetes-list-type", true
	case "x_kubernetes_map_type":
		return "x-kubernetes-map-type", true
	case "x_kubernetes_preserve_unknown_fields":
		return "x-kubernetes-preserve-unknown-fields", true
	case "x_kubernetes_validations":
		return "x-kubernetes-validations", true
	}
	return "", false
}

func propMapToUnstructured(pm resource.PropertyMap) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: pm.MapRepl(mapReplUnderscoreToDash, mapReplStripSecrets)}
}

// initialAPIVersion retrieves the initialAPIVersion property from the checkpoint file and falls back to using
// the version from the resource metadata if that property is not present.
func initialAPIVersion(state resource.PropertyMap, oldInputs *unstructured.Unstructured) string {
	if v, ok := state[initialAPIVersionKey]; ok {
		return v.StringValue()
	}

	return oldInputs.GetAPIVersion()
}

func checkpointObject(inputs, live *unstructured.Unstructured, fromInputs resource.PropertyMap,
	initialAPIVersion, fieldManager string) resource.PropertyMap {

	object := resource.NewPropertyMapFromMap(live.Object)
	inputsPM := resource.NewPropertyMapFromMap(inputs.Object)

	annotateSecrets(object, fromInputs)
	annotateSecrets(inputsPM, fromInputs)

	isSecretKind := live.GetKind() == secretKind

	// For secrets, if `stringData` is present in the inputs, the API server will have filled in `data` based on it. By
	// base64 encoding the secrets. We should mark any of the values which were secrets in the `stringData` object
	// as secrets in the `data` field as well.
	if live.GetAPIVersion() == "v1" && isSecretKind {
		stringData, hasStringData := fromInputs["stringData"]
		data, hasData := object["data"]

		if hasStringData && hasData {
			if stringData.IsSecret() && !data.IsSecret() {
				object["data"] = resource.MakeSecret(data)
			}

			if stringData.IsObject() && data.IsObject() {
				annotateSecrets(data.ObjectValue(), stringData.ObjectValue())
			}
		}
	}

	object["__inputs"] = resource.NewObjectProperty(inputsPM)
	object[initialAPIVersionKey] = resource.NewStringProperty(initialAPIVersion)
	object[fieldManagerKey] = resource.NewStringProperty(fieldManager)

	return object
}

func parseCheckpointObject(obj resource.PropertyMap) (oldInputs, live *unstructured.Unstructured) {
	// Since we are converting everything to unstructured's, we need to strip out any secretness that
	// may nested deep within the object.
	pm := obj.MapRepl(nil, mapReplStripSecrets)

	//
	// NOTE: Inputs are now stored in `__inputs` to allow output properties to work. The inputs and
	// live properties used to be stored next to each other, in an object that looked like {live:
	// (...), inputs: (...)}, but this broke this resolution. See[1] for more information.
	//
	// [1]: https://github.com/pulumi/pulumi-kubernetes/issues/137
	//
	inputs, hasInputs := pm["inputs"]
	liveMap, hasLive := pm["live"]

	if !hasInputs || !hasLive {
		liveMap = pm

		inputs, hasInputs = pm["__inputs"]
		if hasInputs {
			delete(liveMap.(map[string]any), "__inputs")
		} else {
			inputs = map[string]any{}
		}
	}

	oldInputs = &unstructured.Unstructured{Object: inputs.(map[string]any)}
	live = &unstructured.Unstructured{Object: liveMap.(map[string]any)}
	return
}

// partialError creates an error for resources that did not complete an operation in progress.
// The last known state of the object is included in the error so that it can be checkpointed.
func partialError(id string, err error, state *structpb.Struct, inputs *structpb.Struct) error {
	reasons := []string{err.Error()}
	err = pkgerrors.Cause(err)
	if aggregate, isAggregate := err.(await.AggregatedError); isAggregate {
		reasons = append(reasons, aggregate.SubErrors()...)
	}
	detail := pulumirpc.ErrorResourceInitFailed{
		Id:         id,
		Properties: state,
		Reasons:    reasons,
		Inputs:     inputs,
	}
	return rpcerror.WithDetails(rpcerror.New(codes.Unknown, err.Error()), &detail)
}

// canonicalNamespace will provides the canonical name for a namespace. Specifically, if the
// namespace is "", the empty string, we report this as its canonical name, "default".
func canonicalNamespace(ns string) string {
	if ns == "" {
		return "default"
	}
	return ns
}

// deleteResponse causes the resource to be deleted from the state.
var deleteResponse = &pulumirpc.ReadResponse{Id: "", Properties: nil}

// convertPatchToDiff converts the given JSON merge patch to a Pulumi detailed diff.
func convertPatchToDiff(
	patch, oldLiveState, newInputs, oldInputs map[string]any, forceNewFields ...string,
) (map[string]*pulumirpc.PropertyDiff, error) {

	contract.Requiref(len(patch) != 0, "patch", "expected len() != 0")
	contract.Requiref(oldLiveState != nil, "oldLiveState", "expected != nil")

	pc := &patchConverter{
		forceNew: forceNewFields,
		diff:     map[string]*pulumirpc.PropertyDiff{},
	}
	err := pc.addPatchMapToDiff(nil, patch, oldLiveState, newInputs, oldInputs, false)
	return pc.diff, err
}

// makePatchSlice recursively processes the given path to create a slice of a POJO value that is appropriately shaped
// for querying using a JSON path. We use this in addPatchValueToDiff when deciding whether or not a particular
// property causes a replacement.
func makePatchSlice(path []any, v any) any {
	if len(path) == 0 {
		return v
	}
	switch p := path[0].(type) {
	case string:
		return map[string]any{
			p: makePatchSlice(path[1:], v),
		}
	case int:
		return []any{makePatchSlice(path[1:], v)}
	default:
		contract.Failf("unexpected element type in path: %T", p)
		return nil
	}
}

// equalNumbers returns true if both a and b are number values (int64 or float64). Note that if a this will fail if
// either value is not representable as a float64.
func equalNumbers(a, b any) bool {
	aKind, bKind := reflect.TypeOf(a).Kind(), reflect.TypeOf(b).Kind()
	if aKind == bKind {
		return reflect.DeepEqual(a, b)
	}

	toFloat := func(v any) (float64, bool) {
		switch field := v.(type) {
		case int64:
			return float64(field), true
		case float64:
			return field, true
		default:
			return 0, false
		}
	}

	aVal, aOk := toFloat(a)
	bVal, bOk := toFloat(b)
	return aOk && bOk && aVal == bVal
}

// patchConverter carries context for convertPatchToDiff.
type patchConverter struct {
	forceNew []string
	diff     map[string]*pulumirpc.PropertyDiff
}

// addPatchValueToDiff adds the given patched value to the detailed diff. Either the patched value or the old value
// must not be nil.
//
// The particular difference that is recorded depends on the old and new values:
// - If the patched value is nil, the property is recorded as deleted
// - If the old value is nil, the property is recorded as added
// - If the types of the old and new values differ, the property is recorded as updated
// - If both values are maps, the maps are recursively compared on a per-property basis and added to the diff
// - If both values are arrays, the arrays are recursively compared on a per-element basis and added to the diff
// - If both values are primitives and the values differ, the property is recorded as updated
// - Otherwise, no diff is recorded.
//
// If a difference is present at the given path and the path matches one of the patterns in the database of
// force-new properties, the diff is amended to indicate that the resource needs to be replaced due to the change in
// this property.
func (pc *patchConverter) addPatchValueToDiff(
	path []any, v, old, newInput, oldInput any, inArray bool,
) error {
	contract.Assertf(v != nil || old != nil || oldInput != nil,
		"path: %+v  |  v: %+v  | old: %+v  |  oldInput: %+v",
		path, v, old, oldInput)

	// If there is no new input, then the only possible diff here is a delete. All other diffs must be diffs between
	// old and new properties that are populated by the server. If there is also no old input, then there is no diff
	// whatsoever.
	if newInput == nil && (v != nil || oldInput == nil) {
		return nil
	}

	var diffKind pulumirpc.PropertyDiff_Kind
	inputDiff := false
	if v == nil {
		diffKind, inputDiff = pulumirpc.PropertyDiff_DELETE, true
	} else if old == nil {
		diffKind = pulumirpc.PropertyDiff_ADD
	} else {
		switch v := v.(type) {
		case map[string]any:
			if oldMap, ok := old.(map[string]any); ok {
				newInputMap, _ := newInput.(map[string]any)
				oldInputMap, _ := oldInput.(map[string]any)
				return pc.addPatchMapToDiff(path, v, oldMap, newInputMap, oldInputMap, inArray)
			}
			diffKind = pulumirpc.PropertyDiff_UPDATE
		case []any:
			if oldArray, ok := old.([]any); ok {
				newInputArray, _ := newInput.([]any)
				oldInputArray, _ := oldInput.([]any)
				return pc.addPatchArrayToDiff(path, v, oldArray, newInputArray, oldInputArray, inArray)
			}
			diffKind = pulumirpc.PropertyDiff_UPDATE
		default:
			if reflect.DeepEqual(v, old) || equalNumbers(v, old) {
				// From RFC 7386 (the JSON Merge Patch spec):
				//
				//   If the patch is anything other than an object, the result will always be to replace the entire
				//   target with the entire patch. Also, it is not possible to patch part of a target that is not an
				//   object, such as to replace just some of the values in an array.
				//
				// Because JSON merge patch does not allow array elements to be updated--instead, the array must be
				// replaced in full--the patch we have is an overestimate of the properties that changed. As such, we
				// only record updates for values that have in fact changed.
				return nil
			}
			diffKind = pulumirpc.PropertyDiff_UPDATE
		}
	}

	// Determine if this change causes a replace.
	matches, err := openapi.PatchPropertiesChanged(makePatchSlice(path, v).(map[string]any), pc.forceNew)
	if err != nil {
		return err
	}
	if len(matches) != 0 {
		switch diffKind {
		case pulumirpc.PropertyDiff_ADD:
			diffKind = pulumirpc.PropertyDiff_ADD_REPLACE
		case pulumirpc.PropertyDiff_DELETE:
			diffKind = pulumirpc.PropertyDiff_DELETE_REPLACE
		case pulumirpc.PropertyDiff_UPDATE:
			diffKind = pulumirpc.PropertyDiff_UPDATE_REPLACE
		}
	}

	pathStr := ""
	for _, v := range path {
		switch v := v.(type) {
		case string:
			if strings.ContainsAny(v, `."[]`) {
				pathStr = fmt.Sprintf(`%s["%s"]`, pathStr, strings.ReplaceAll(v, `"`, `\"`))
			} else if pathStr != "" {
				pathStr = fmt.Sprintf("%s.%s", pathStr, v)
			} else {
				pathStr = v
			}
		case int:
			pathStr = fmt.Sprintf("%s[%d]", pathStr, v)
		}
	}

	pc.diff[pathStr] = &pulumirpc.PropertyDiff{Kind: diffKind, InputDiff: inputDiff}
	return nil
}

// addPatchMapToDiff adds the diffs in the given patched map to the detailed diff.
//
// If this map is contained within an array, we do a little bit more work to detect deletes, as they are not recorded
// in the patch in this case (see the note in addPatchValueToDiff for more details).
func (pc *patchConverter) addPatchMapToDiff(
	path []any, m, old, newInput, oldInput map[string]any, inArray bool,
) error {

	if newInput == nil {
		newInput = map[string]any{}
	}
	if oldInput == nil {
		oldInput = map[string]any{}
	}

	for k, v := range m {
		if err := pc.addPatchValueToDiff(append(path, k), v, old[k], newInput[k], oldInput[k], inArray); err != nil {
			return err
		}
	}
	if inArray {
		for k, v := range old {
			if _, ok := m[k]; ok {
				continue
			}
			if err := pc.addPatchValueToDiff(append(path, k), nil, v, newInput[k], oldInput[k], inArray); err != nil {
				return err
			}
		}
	}
	return nil
}

// addPatchArrayToDiff adds the diffs in the given patched array to the detailed diff.
func (pc *patchConverter) addPatchArrayToDiff(
	path []any, a, old, newInput, oldInput []any, inArray bool,
) error {

	at := func(arr []any, i int) any {
		if i < len(arr) {
			return arr[i]
		}
		return nil
	}

	var i int
	for i = 0; i < len(a) && i < len(old); i++ {
		err := pc.addPatchValueToDiff(append(path, i), a[i], old[i], at(newInput, i), at(oldInput, i), true)
		if err != nil {
			return err
		}
	}

	if i < len(a) {
		for ; i < len(a); i++ {
			err := pc.addPatchValueToDiff(append(path, i), a[i], nil, at(newInput, i), at(oldInput, i), true)
			if err != nil {
				return err
			}
		}
	} else {
		for ; i < len(old); i++ {
			err := pc.addPatchValueToDiff(append(path, i), nil, old[i], at(newInput, i), at(oldInput, i), true)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// annotateSecrets copies the "secretness" from the ins to the outs. If there are values with the same keys for the
// outs and the ins, if they are both objects, they are transformed recursively. Otherwise, if the value in the ins
// contains a secret, the entire out value is marked as a secret.  This is very close to how we project secrets
// in the programming model, with one small difference, which is how we treat the case where both are objects. In the
// programming model, we would say the entire output object is a secret. Here, we actually recur in. We do this because
// we don't want a single secret value in a rich structure to taint the entire object. Doing so would mean things like
// the entire value in the deployment would be encrypted instead of a small chunk. It also means the entire property
// would be displayed as `[secret]` in the CLI instead of a small part.
//
// NOTE: This means that for an array, if any value in the input version is a secret, the entire output array is
// marked as a secret. This is actually a very nice result, because often arrays are treated like sets by providers
// and the order may not be preserved across an operation. This means we do end up encrypting the entire array
// but that's better than accidentally leaking a value which just moved to a different location.
func annotateSecrets(outs, ins resource.PropertyMap) {
	if outs == nil {
		return
	}

	// Note on IsString(): k8s "kind" is always a string, but we might be recursing into the structure and there
	// might be another, unrelated "kind" in there.
	if kind, ok := outs["kind"]; ok && kind.IsString() && kind.StringValue() == secretKind {
		if data, hasData := outs["data"]; hasData {
			outs["data"] = resource.MakeSecret(data)
		}
		if stringData, hasStringData := outs["stringData"]; hasStringData {
			outs["stringData"] = resource.MakeSecret(stringData)
		}
		return
	}

	if ins == nil {
		return
	}

	for key, inValue := range ins {
		outValue, has := outs[key]
		if !has {
			continue
		}
		if outValue.IsObject() && inValue.IsObject() {
			annotateSecrets(outValue.ObjectValue(), inValue.ObjectValue())
		} else if !outValue.IsSecret() && inValue.ContainsSecrets() {
			outs[key] = resource.MakeSecret(outValue)
		}
	}
}

// renderYaml marshals an Unstructured resource to YAML and writes it to the specified path on disk or returns an error.
func renderYaml(resource *unstructured.Unstructured, yamlDirectory string) error {
	jsonBytes, err := resource.MarshalJSON()
	if err != nil {
		return pkgerrors.Wrapf(err, "failed to render YAML file: %q", yamlDirectory)
	}
	yamlBytes, err := yaml.JSONToYAML(jsonBytes)
	if err != nil {
		return pkgerrors.Wrapf(err, "failed to render YAML file: %q", yamlDirectory)
	}

	crdDirectory := filepath.Join(yamlDirectory, "0-crd")
	manifestDirectory := filepath.Join(yamlDirectory, "1-manifest")

	if _, err := os.Stat(crdDirectory); os.IsNotExist(err) {
		err = os.MkdirAll(crdDirectory, 0700)
		if err != nil {
			return pkgerrors.Wrapf(err, "failed to create directory for rendered YAML: %q", crdDirectory)
		}
	}
	if _, err := os.Stat(manifestDirectory); os.IsNotExist(err) {
		err = os.MkdirAll(manifestDirectory, 0700)
		if err != nil {
			return pkgerrors.Wrapf(err, "failed to create directory for rendered YAML: %q", manifestDirectory)
		}
	}

	path := renderPathForResource(resource, yamlDirectory)
	err = os.WriteFile(path, yamlBytes, 0600)
	if err != nil {
		return pkgerrors.Wrapf(err, "failed to write YAML file: %q", path)
	}

	return nil
}

// renderPathForResource determines the appropriate YAML render path depending on the resource kind.
func renderPathForResource(resource *unstructured.Unstructured, yamlDirectory string) string {
	crdDirectory := filepath.Join(yamlDirectory, "0-crd")
	manifestDirectory := filepath.Join(yamlDirectory, "1-manifest")

	namespace := "default"
	if "" != resource.GetNamespace() {
		namespace = resource.GetNamespace()
	}

	sanitise := func(name string) string {
		name = strings.NewReplacer("/", "_", ":", "_").Replace(name)
		return name
	}

	fileName := fmt.Sprintf("%s-%s-%s-%s.yaml", sanitise(resource.GetAPIVersion()), strings.ToLower(resource.GetKind()), namespace, resource.GetName())
	filepath.Join(yamlDirectory, fileName)

	var path string
	if kinds.Kind(resource.GetKind()) == kinds.CustomResourceDefinition {
		path = filepath.Join(crdDirectory, fileName)
	} else {
		path = filepath.Join(manifestDirectory, fileName)
	}

	return path
}
