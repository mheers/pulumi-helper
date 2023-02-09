// Copyright 2016-2022, Pulumi Corporation.
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
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"

	jsonpatch "github.com/evanphx/json-patch"
	"github.com/golang/protobuf/ptypes/empty"
	pbempty "github.com/golang/protobuf/ptypes/empty"
	structpb "github.com/golang/protobuf/ptypes/struct"
	pkgerrors "github.com/pkg/errors"
	"github.com/pulumi/pulumi-kubernetes/provider/v3/pkg/await"
	"github.com/pulumi/pulumi-kubernetes/provider/v3/pkg/clients"
	"github.com/pulumi/pulumi-kubernetes/provider/v3/pkg/cluster"
	"github.com/pulumi/pulumi-kubernetes/provider/v3/pkg/gen"
	"github.com/pulumi/pulumi-kubernetes/provider/v3/pkg/kinds"
	"github.com/pulumi/pulumi-kubernetes/provider/v3/pkg/metadata"
	"github.com/pulumi/pulumi-kubernetes/provider/v3/pkg/openapi"
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
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	k8sresource "k8s.io/cli-runtime/pkg/resource"
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
	lastAppliedConfigKey = "kubectl.kubernetes.io/last-applied-configuration"
	initialAPIVersionKey = "__initialApiVersion"
	fieldManagerKey      = "__fieldManager"
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
	enableDryRun                bool
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

	yamlRenderMode bool
	yamlDirectory  string

	clusterUnreachable       bool   // Kubernetes cluster is unreachable.
	clusterUnreachableReason string // Detailed error message if cluster is unreachable.

	config         *rest.Config // Cluster config, e.g., through $KUBECONFIG file.
	kubeconfig     clientcmd.ClientConfig
	clientSet      *clients.DynamicClientSet
	dryRunVerifier *k8sresource.QueryParamVerifier
	logClient      *clients.LogClient
	k8sVersion     cluster.ServerVersion

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

func (k *KubeProvider) invalidateResources() {
	k.resourcesMutex.Lock()
	defer k.resourcesMutex.Unlock()

	k.resources = nil
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

	enableDryRun := func() bool {
		// If the provider flag is set, use that value to determine behavior. This will override the ENV var.
		if enabled, exists := vars["kubernetes:config:enableDryRun"]; exists {
			return enabled == trueStr
		}
		// If the provider flag is not set, fall back to the ENV var.
		if enabled, exists := os.LookupEnv("PULUMI_K8S_ENABLE_DRY_RUN"); exists {
			return enabled == trueStr
		}
		// Default to false.
		return false
	}
	if enableDryRun() {
		k.enableDryRun = true
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
		// Default to false.
		return false
	}
	if enableServerSideApply() {
		k.enableDryRun = true
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

	// TODO: Once https://github.com/pulumi/pulumi/issues/8132 is fixed, we can drop the env var handling logic.

	helmDriver := func() string {
		// If the provider flag is not set, fall back to the ENV var.
		if driver, exists := os.LookupEnv("PULUMI_K8S_HELM_DRIVER"); exists {
			return driver
		}
		return "secret"
	}
	k.helmDriver = helmDriver() // TODO: Make sure this is in provider state

	helmPluginsPath := func() string {
		// If the provider flag is not set, fall back to the ENV var.
		if pluginsPath, exists := os.LookupEnv("PULUMI_K8S_HELM_PLUGINS_PATH"); exists {
			return pluginsPath
		}
		return helmpath.DataPath("plugins")
	}
	k.helmPluginsPath = helmPluginsPath()

	helmRegistryConfigPath := func() string {
		// If the provider flag is not set, fall back to the ENV var.
		if registryPath, exists := os.LookupEnv("PULUMI_K8S_HELM_REGISTRY_CONFIG_PATH"); exists {
			return registryPath
		}
		return helmpath.ConfigPath("registry.json")
	}
	k.helmRegistryConfigPath = helmRegistryConfigPath()

	helmRepositoryConfigPath := func() string {
		if repositoryConfigPath, exists := os.LookupEnv("PULUMI_K8S_HELM_REPOSITORY_CONFIG_PATH"); exists {
			return repositoryConfigPath
		}
		return helmpath.ConfigPath("repositories.yaml")
	}
	k.helmRepositoryConfigPath = helmRepositoryConfigPath()

	helmRepositoryCache := func() string {
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
			b, err := ioutil.ReadFile(pathOrContents)
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
	}

	if qps := os.Getenv("PULUMI_K8S_CLIENT_QPS"); qps != "" && kubeClientSettings.QPS == nil {
		asFloat, err := strconv.ParseFloat(qps, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid value specified for PULUMI_K8S_CLIENT_QPS: %w", err)
		}
		kubeClientSettings.QPS = &asFloat
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
		k.dryRunVerifier = k8sresource.NewQueryParamVerifier(
			cs.GenericClient, cs.DiscoveryClientCached, k8sresource.QueryParamDryRun)
		lc, err := clients.NewLogClient(k.canceler.context, k.config)
		if err != nil {
			return nil, err
		}
		k.logClient = lc

		k.k8sVersion = cluster.TryGetServerVersion(cs.DiscoveryClientCached)

		if _, err = k.getResources(); err != nil {
			k.clusterUnreachable = true
			k.clusterUnreachableReason = fmt.Sprintf(
				"unable to load schema information from the API server: %v", err)
		}
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
			resource.NewPropertyMapFromMap(map[string]interface{}{"result": result}),
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
			resource.NewPropertyMapFromMap(map[string]interface{}{"result": result}),
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

		objects := make(chan map[string]interface{})
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
				// `kubeProvider#Cancel` was called. Terminate the `StreamInvoke` RPC, free all
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
				// `kubeProvider#Cancel` was called. Terminate the `StreamInvoke` RPC, free all
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
						map[string]interface{}{
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
		defer podLogs.Close()

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
				// `kubeProvider#Cancel` was called. Terminate the `StreamInvoke` RPC, free all
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
						map[string]interface{}{"lines": []string{line}}),
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

	if !k.serverSideApplyMode && isPatchURN(urn) {
		return nil, fmt.Errorf("patch resources require Server-side Apply mode, which is enabled using the " +
			"`enableServerSideApply` Provider config")
	}

	// Utilities for determining whether a resource's GVK exists.
	gvkExists := func(gvk schema.GroupVersionKind) bool {
		knownGVKs := sets.NewString()
		if knownGVKs.Has(gvk.String()) {
			return true
		}
		gv := gvk.GroupVersion()
		rls, err := k.clientSet.DiscoveryClientCached.ServerResourcesForGroupVersion(gv.String())
		if err != nil {
			if !errors.IsNotFound(err) {
				logger.V(3).Infof("ServerResourcesForGroupVersion(%q) returned unexpected error %v", gv, err)
			}
			return false
		}
		for _, rl := range rls.APIResources {
			knownGVKs.Insert(gv.WithKind(rl.Kind).String())
		}
		return knownGVKs.Has(gvk.String())
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

	if k.serverSideApplyMode && isPatchURN(urn) {
		if len(newInputs.GetName()) == 0 {
			return nil, fmt.Errorf("patch resources require the resource `.metadata.name` to be set")
		}
	}

	var failures []*pulumirpc.CheckFailure

	k.helmHookWarning(ctx, newInputs, urn)

	annotatedInputs, err := legacyInitialAPIVersion(oldInputs, newInputs)
	if err != nil {
		return nil, pkgerrors.Wrapf(
			err, "Failed to create resource %s/%s because of an error generating the %s value in "+
				"`.metadata.annotations`",
			newInputs.GetNamespace(), newInputs.GetName(), metadata.AnnotationInitialAPIVersion)
	}
	newInputs = annotatedInputs

	// Adopt name from old object if appropriate.
	//
	// If the user HAS NOT assigned a name in the new inputs, we autoname it and mark the object as
	// autonamed in `.metadata.annotations`. This makes it easier for `Diff` to decide whether this
	// needs to be `DeleteBeforeReplace`'d. If the resource is marked `DeleteBeforeReplace`, then
	// `Create` will allocate it a new name later.
	if len(oldInputs.Object) > 0 {
		// NOTE: If old inputs exist, they have a name, either provided by the user or filled in with a
		// previous run of `Check`.
		contract.Assert(oldInputs.GetName() != "")
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
			resourceNotFound := errors.IsNotFound(err) ||
				strings.Contains(err.Error(), "is not supported by the server")
			k8sAPIUnreachable := strings.Contains(err.Error(), "connection refused")
			if resourceNotFound && gvkExists(gvk) {
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

// isPatchURN returns true if the URN is for a Patch resource.
func isPatchURN(urn resource.URN) bool {
	return strings.HasSuffix(urn.Type().String(), "Patch")
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

func (k *KubeProvider) gvkFromUnstructured(input *unstructured.Unstructured) schema.GroupVersionKind {
	var group, version, kind string

	kind = input.GetKind()
	gv := strings.Split(input.GetAPIVersion(), "/")
	if len(gv) == 1 {
		version = input.GetAPIVersion()
	} else {
		group, version = gv[0], gv[1]
	}
	if group == "core" {
		group = ""
	}

	return schema.GroupVersionKind{
		Group:   group,
		Version: version,
		Kind:    kind,
	}
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

func (k *KubeProvider) readLiveObject(obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	rc, err := k.clientSet.ResourceClientForObject(obj)
	if err != nil {
		return nil, err
	}

	// Get the "live" version of the last submitted object. This is necessary because the server may
	// have populated some fields automatically, updated status fields, and so on.
	return rc.Get(k.canceler.context, obj.GetName(), metav1.GetOptions{})
}

func (k *KubeProvider) serverSidePatch(oldInputs, newInputs *unstructured.Unstructured, fieldManager string,
) ([]byte, map[string]interface{}, error) {

	client, err := k.clientSet.ResourceClient(oldInputs.GroupVersionKind(), oldInputs.GetNamespace())
	if err != nil {
		return nil, nil, err
	}

	liveObject, err := client.Get(k.canceler.context, oldInputs.GetName(), metav1.GetOptions{})
	if err != nil {
		return nil, nil, err
	}

	// If the new resource does not exist, we need to dry-run a Create rather than a Patch.
	var newObject *unstructured.Unstructured
	_, err = client.Get(k.canceler.context, newInputs.GetName(), metav1.GetOptions{})
	switch {
	case errors.IsNotFound(err):
		newObject, err = client.Create(k.canceler.context, newInputs, metav1.CreateOptions{
			DryRun: []string{metav1.DryRunAll},
		})
	case newInputs.GetNamespace() != oldInputs.GetNamespace():
		client, err := k.clientSet.ResourceClient(newInputs.GroupVersionKind(), newInputs.GetNamespace())
		if err != nil {
			return nil, nil, err
		}
		newObject, err = client.Create(k.canceler.context, newInputs, metav1.CreateOptions{
			DryRun: []string{metav1.DryRunAll},
		})
		if err != nil {
			return nil, nil, err
		}
	case err == nil:
		if k.serverSideApplyMode {
			objYAML, err := yaml.Marshal(newInputs.Object)
			if err != nil {
				return nil, nil, err
			}

			// Always force on diff to avoid erroneous conflict errors for resource replacements
			force := true
			if v := metadata.GetAnnotationValue(newInputs, metadata.AnnotationPatchFieldManager); len(v) > 0 {
				fieldManager = v
			}

			newObject, err = client.Patch(
				k.canceler.context, newInputs.GetName(), types.ApplyPatchType, objYAML, metav1.PatchOptions{
					DryRun:          []string{metav1.DryRunAll},
					FieldManager:    fieldManager,
					FieldValidation: metav1.FieldValidationWarn,
					Force:           &force,
				})
			if err != nil {
				return nil, nil, err
			}
		} else {
			liveInputs := parseLiveInputs(liveObject, oldInputs)

			resources, err := k.getResources()
			if err != nil {
				return nil, nil, err
			}

			patch, patchType, _, err := openapi.PatchForResourceUpdate(resources, liveInputs, newInputs, liveObject)
			if err != nil {
				return nil, nil, err
			}

			newObject, err = client.Patch(k.canceler.context, newInputs.GetName(), patchType, patch, metav1.PatchOptions{
				DryRun: []string{metav1.DryRunAll},
			})
			if err != nil {
				return nil, nil, err
			}
		}

	default:
		return nil, nil, err
	}
	if err != nil {
		return nil, nil, err
	}

	if k.serverSideApplyMode {
		objMetadata := newObject.Object["metadata"].(map[string]interface{})
		if len(objMetadata) == 1 {
			delete(newObject.Object, "metadata")
		} else {
			delete(newObject.Object["metadata"].(map[string]interface{}), "managedFields")
		}
		objMetadata = liveObject.Object["metadata"].(map[string]interface{})
		if len(objMetadata) == 1 {
			delete(liveObject.Object, "metadata")
		} else {
			delete(liveObject.Object["metadata"].(map[string]interface{}), "managedFields")
		}
	}

	liveJSON, err := liveObject.MarshalJSON()
	if err != nil {
		return nil, nil, err
	}
	newJSON, err := newObject.MarshalJSON()
	if err != nil {
		return nil, nil, err
	}

	patch, err := jsonpatch.CreateMergePatch(liveJSON, newJSON)
	if err != nil {
		return nil, nil, err
	}

	return patch, liveObject.Object, nil
}

// inputPatch calculates a patch on the client-side by comparing old inputs to the current inputs.
func (k *KubeProvider) inputPatch(
	oldInputs, newInputs *unstructured.Unstructured,
) ([]byte, error) {
	oldInputsJSON, err := oldInputs.MarshalJSON()
	if err != nil {
		return nil, err
	}
	newInputsJSON, err := newInputs.MarshalJSON()
	if err != nil {
		return nil, err
	}

	return jsonpatch.CreateMergePatch(oldInputsJSON, newInputsJSON)
}

func (k *KubeProvider) supportsDryRun(gvk schema.GroupVersionKind) bool {
	// Check to see if the configuration has explicitly disabled server-side dry run.
	if !k.enableDryRun {
		logger.V(9).Infof("dry run is disabled")
		return false
	}
	// Ensure that the cluster is reachable and supports the server-side diff feature.
	if k.clusterUnreachable || !openapi.SupportsDryRun(k.dryRunVerifier, gvk) {
		logger.V(9).Infof("server cannot dry run %v", gvk)
		return false
	}
	return true
}

func (k *KubeProvider) isDryRunDisabledError(err error) bool {
	se, isStatusError := err.(*errors.StatusError)
	if !isStatusError {
		return false
	}

	return se.Status().Code == http.StatusBadRequest &&
		(se.Status().Message == "the dryRun alpha feature is disabled" ||
			se.Status().Message == "the dryRun beta feature is disabled" ||
			strings.Contains(se.Status().Message, "does not support dry run"))
}

// tryServerSidePatch attempts to compute a server-side patch. Returns true iff the operation succeeded.
// The following are expected ways in which the server-side apply can not work;
// we return no error, but a `false` value indicating that there's no result either.
// The caller will fall back to using the client-side diff. In the particular case of an
// update to an immutable field, it is already known from the schema when
// replacement will be necessary.
func (k *KubeProvider) tryServerSidePatch(
	oldInputs, newInputs *unstructured.Unstructured,
	gvk schema.GroupVersionKind,
	fieldManager string,
) (ssPatch []byte, ssPatchBase map[string]interface{}, ok bool, err error) {
	// If the resource's GVK changed, so compute patch using inputs.
	if oldInputs.GroupVersionKind().String() != gvk.String() {
		return nil, nil, false, nil
	}
	// If we can't dry-run the new GVK, computed the patch using inputs.
	if !k.supportsDryRun(gvk) {
		return nil, nil, false, nil
	}
	// TODO: Skipping server-side diff for resources with computed values is a hack. We will want to address this
	// more granularly so that previews are as accurate as possible, but this is an easy workaround for a critical
	// bug.
	if hasComputedValue(newInputs) || hasComputedValue(oldInputs) {
		return nil, nil, false, nil
	}

	ssPatch, ssPatchBase, err = k.serverSidePatch(oldInputs, newInputs, fieldManager)
	if err == nil {
		// The server-side patch succeeded.
		return ssPatch, ssPatchBase, true, nil
	}

	if k.isDryRunDisabledError(err) {
		return nil, nil, false, err
	}
	if se, isStatusError := err.(*errors.StatusError); isStatusError {
		if se.Status().Code == http.StatusUnprocessableEntity &&
			strings.Contains(se.ErrStatus.Message, "field is immutable") {
			// This error occurs if the resource field is immutable.
			// Ignore this error since this case is handled by the replacement logic.
			return nil, nil, false, nil
		}
	}
	if err.Error() == "name is required" {
		// If the name was removed in this update, then the resource will be replaced with an auto-named resource.
		// Ignore this error since this case is handled by the replacement logic.
		return nil, nil, false, nil
	}

	return nil, nil, false, err
}

func (k *KubeProvider) withLastAppliedConfig(config *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if k.serverSideApplyMode || k.supportsDryRun(config.GroupVersionKind()) {
		// Skip last-applied-config annotation if the resource supports server-side apply.
		return config, nil
	}

	// CRDs are updated using a separate mechanism, so skip the last-applied-configuration annotation, and delete it
	// if it was present from a previous update.
	if clients.IsCRD(config) {
		// Deep copy the config before returning.
		config = config.DeepCopy()

		annotations := getAnnotations(config)
		delete(annotations, lastAppliedConfigKey)
		config.SetAnnotations(annotations)
		return config, nil
	}

	// Serialize the inputs and add the last-applied-configuration annotation.
	marshaled, err := config.MarshalJSON()
	if err != nil {
		return nil, err
	}

	// Deep copy the config before returning.
	config = config.DeepCopy()

	annotations := getAnnotations(config)
	annotations[lastAppliedConfigKey] = string(marshaled)
	config.SetAnnotations(annotations)
	return config, nil
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
	contract.AssertNoError(err)

	return fieldManager
}

func mapReplStripSecrets(v resource.PropertyValue) (interface{}, bool) {
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

func getAnnotations(config *unstructured.Unstructured) map[string]string {
	annotations := config.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}

	return annotations
}

// legacyInitialAPIVersion maintains backward compatibility with behavior introduced in the 1.2.0 release. This
// information is now stored in the checkpoint file and the annotation is no longer used by the provider.
func legacyInitialAPIVersion(oldConfig, newConfig *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	oldAnnotations := getAnnotations(oldConfig)
	newAnnotations := getAnnotations(newConfig)

	apiVersion, exists := oldAnnotations[metadata.AnnotationInitialAPIVersion]
	if exists {
		// Keep the annotation if it was already created previously to minimize further disruption
		// to existing resources.
		newAnnotations[metadata.AnnotationInitialAPIVersion] = apiVersion
	}

	if len(newConfig.GetAnnotations()) > 0 {
		newConfig.SetAnnotations(newAnnotations)
	}

	return newConfig, nil
}

// initialAPIVersion retrieves the initialAPIVersion property from the checkpoint file and falls back to using
// the `pulumi.com/initialAPIVersion` annotation if that property is not present.
func initialAPIVersion(state resource.PropertyMap, oldConfig *unstructured.Unstructured) (string, error) {
	if v, ok := state[initialAPIVersionKey]; ok {
		return v.StringValue(), nil
	}

	oldAnnotations := getAnnotations(oldConfig)
	apiVersion, exists := oldAnnotations[metadata.AnnotationInitialAPIVersion]
	if exists {
		return apiVersion, nil
	}

	return oldConfig.GetAPIVersion(), nil
}

func checkpointObject(inputs, live *unstructured.Unstructured, fromInputs resource.PropertyMap,
	initialAPIVersion, fieldManager string) resource.PropertyMap {
	object := resource.NewPropertyMapFromMap(live.Object)
	inputsPM := resource.NewPropertyMapFromMap(inputs.Object)

	annotateSecrets(object, fromInputs)
	annotateSecrets(inputsPM, fromInputs)

	// For secrets, if `stringData` is present in the inputs, the API server will have filled in `data` based on it. By
	// base64 encoding the secrets. We should mark any of the values which were secrets in the `stringData` object
	// as secrets in the `data` field as well.
	if live.GetAPIVersion() == "v1" && live.GetKind() == "Secret" {
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

	// Ensure that the annotation we add for lastAppliedConfig is treated as a secret if any of the inputs were secret
	// (the value of this annotation is a string-ified JSON so marking the entire thing as a secret is really the best
	// that we can do).
	if fromInputs.ContainsSecrets() {
		if _, has := object["metadata"]; has && object["metadata"].IsObject() {
			metadata := object["metadata"].ObjectValue()
			if _, has := metadata["annotations"]; has && metadata["annotations"].IsObject() {
				annotations := metadata["annotations"].ObjectValue()
				if lastAppliedConfig, has := annotations[lastAppliedConfigKey]; has && !lastAppliedConfig.IsSecret() {
					annotations[lastAppliedConfigKey] = resource.MakeSecret(lastAppliedConfig)
				}
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
			delete(liveMap.(map[string]interface{}), "__inputs")
		} else {
			inputs = map[string]interface{}{}
		}
	}

	oldInputs = &unstructured.Unstructured{Object: inputs.(map[string]interface{})}
	live = &unstructured.Unstructured{Object: liveMap.(map[string]interface{})}
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

// parseLastAppliedConfig attempts to find and parse an annotation that records the last applied configuration for the
// given live object state.
func parseLastAppliedConfig(live *unstructured.Unstructured) *unstructured.Unstructured {
	// If `kubectl.kubernetes.io/last-applied-configuration` metadata annotation is present, parse it into a real object
	// and use it as the current set of live inputs. Otherwise, return nil.
	if live == nil {
		return nil
	}

	annotations := live.GetAnnotations()
	if annotations == nil {
		return nil
	}
	lastAppliedConfig, ok := annotations[lastAppliedConfigKey]
	if !ok {
		return nil
	}

	liveInputs := &unstructured.Unstructured{}
	if err := liveInputs.UnmarshalJSON([]byte(lastAppliedConfig)); err != nil {
		return nil
	}
	return liveInputs
}

// parseLiveInputs attempts to parse the provider inputs that produced the given live object out of the object's state.
// This is used by Read.
func parseLiveInputs(live, oldInputs *unstructured.Unstructured) *unstructured.Unstructured {
	// First try to find and parse a `kubectl.kubernetes.io/last-applied-configuration` metadata anotation. If that
	// succeeds, we are done.
	if inputs := parseLastAppliedConfig(live); inputs != nil {
		return inputs
	}

	// If no such annotation was present--or if parsing failed--either retain the old inputs if they exist, or
	// attempt to propagate the live object's GVK, any Pulumi-generated autoname and its annotation, and return
	// the result.
	if oldInputs != nil && len(oldInputs.Object) > 0 {
		return oldInputs
	}

	inputs := &unstructured.Unstructured{Object: map[string]interface{}{}}
	inputs.SetGroupVersionKind(live.GroupVersionKind())
	metadata.AdoptOldAutonameIfUnnamed(inputs, live)

	return inputs
}

// convertPatchToDiff converts the given JSON merge patch to a Pulumi detailed diff.
func convertPatchToDiff(
	patch, oldLiveState, newInputs, oldInputs map[string]interface{}, forceNewFields ...string,
) (map[string]*pulumirpc.PropertyDiff, error) {

	contract.Require(len(patch) != 0, "len(patch) != 0")
	contract.Require(oldLiveState != nil, "oldLiveState != nil")

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
func makePatchSlice(path []interface{}, v interface{}) interface{} {
	if len(path) == 0 {
		return v
	}
	switch p := path[0].(type) {
	case string:
		return map[string]interface{}{
			p: makePatchSlice(path[1:], v),
		}
	case int:
		return []interface{}{makePatchSlice(path[1:], v)}
	default:
		contract.Failf("unexpected element type in path: %T", p)
		return nil
	}
}

// equalNumbers returns true if both a and b are number values (int64 or float64). Note that if a this will fail if
// either value is not representable as a float64.
func equalNumbers(a, b interface{}) bool {
	aKind, bKind := reflect.TypeOf(a).Kind(), reflect.TypeOf(b).Kind()
	if aKind == bKind {
		return reflect.DeepEqual(a, b)
	}

	toFloat := func(v interface{}) (float64, bool) {
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
	path []interface{}, v, old, newInput, oldInput interface{}, inArray bool,
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
		case map[string]interface{}:
			if oldMap, ok := old.(map[string]interface{}); ok {
				newInputMap, _ := newInput.(map[string]interface{})
				oldInputMap, _ := oldInput.(map[string]interface{})
				return pc.addPatchMapToDiff(path, v, oldMap, newInputMap, oldInputMap, inArray)
			}
			diffKind = pulumirpc.PropertyDiff_UPDATE
		case []interface{}:
			if oldArray, ok := old.([]interface{}); ok {
				newInputArray, _ := newInput.([]interface{})
				oldInputArray, _ := oldInput.([]interface{})
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
	matches, err := openapi.PatchPropertiesChanged(makePatchSlice(path, v).(map[string]interface{}), pc.forceNew)
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
	path []interface{}, m, old, newInput, oldInput map[string]interface{}, inArray bool,
) error {

	if newInput == nil {
		newInput = map[string]interface{}{}
	}
	if oldInput == nil {
		oldInput = map[string]interface{}{}
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
	path []interface{}, a, old, newInput, oldInput []interface{}, inArray bool,
) error {

	at := func(arr []interface{}, i int) interface{} {
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
	if outs == nil || ins == nil {
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
	err = ioutil.WriteFile(path, yamlBytes, 0600)
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
