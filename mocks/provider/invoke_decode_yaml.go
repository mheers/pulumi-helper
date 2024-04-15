// Copyright 2016-2019, Pulumi Corporation.
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
	"io"
	"strings"

	"github.com/pulumi/pulumi-kubernetes/provider/v4/pkg/clients"
	"github.com/pulumi/pulumi-kubernetes/provider/v4/pkg/kinds"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
)

// decodeYaml parses a YAML string, and then returns a slice of untyped structs that can be marshalled into
// Pulumi RPC calls. If a default namespace is specified, set that on the relevant decoded objects.
func decodeYaml(text, defaultNamespace string, clientSet *clients.DynamicClientSet) ([]any, error) {
	var resources []unstructured.Unstructured

	dec := yaml.NewYAMLOrJSONDecoder(io.NopCloser(strings.NewReader(text)), 128)
	for {
		var value map[string]any
		if err := dec.Decode(&value); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		resource := unstructured.Unstructured{Object: value}

		// Sometimes manifests include empty resources, so skip these.
		if len(resource.GetKind()) == 0 || len(resource.GetAPIVersion()) == 0 {
			continue
		}

		if len(defaultNamespace) > 0 {
			namespaced, err := IsNamespacedKind(resource.GroupVersionKind(), nil)
			if err != nil {
				if clients.IsNoNamespaceInfoErr(err) {
					// Assume resource is namespaced.
					namespaced = true
				} else {
					return nil, err
				}
			}

			// Set namespace if resource Kind is namespaced and namespace is not already set.
			if namespaced && len(resource.GetNamespace()) == 0 {
				resource.SetNamespace(defaultNamespace)
			}
		}
		resources = append(resources, resource)
	}

	result := make([]any, 0, len(resources))
	for _, resource := range resources {
		result = append(result, resource.Object)
	}

	return result, nil
}

// IsNamespacedKind checks if a given GVK is namespace-scoped. Known GVKs are compared against a static lookup table.
// For GVKs not in the table, look at the given objects for a matching CRD.
// Finally, attempt to look up the GVK from the API server. If the GVK cannot be found, a
// NoNamespaceInfoErr is returned.
func IsNamespacedKind(gvk schema.GroupVersionKind, disco discovery.DiscoveryInterface, objs ...unstructured.Unstructured) (bool, error) {
	if gvk.Group == "core" { // nolint:goconst
		gvk.Group = ""
	}

	if kinds.KnownGroupVersions.Has(gvk.GroupVersion().String()) {
		kind := strings.TrimSuffix(gvk.Kind, "Patch") // Check using the underlying kind for Patch resources
		if known, namespaced := kinds.Kind(kind).Namespaced(); known {
			return namespaced, nil
		}
	}

	// check the provided objects for a matching CRD.
	crd := clients.FindCRD(objs, gvk.GroupKind())
	if crd != nil {
		crdScope, _, err := unstructured.NestedString(crd.Object, "spec", "scope")
		return crdScope == "Namespaced", err
	}

	// If the Kind is not known, attempt to look up from the API server. This applies to Kinds defined using a CRD.
	// If the API server is not reachable, return an error.
	if disco == nil {
		return false, &clients.NoNamespaceInfoErr{}
	}
	resourceList, err := disco.ServerResourcesForGroupVersion(gvk.GroupVersion().String())
	if err != nil {
		return false, &clients.NoNamespaceInfoErr{}
	}

	for _, resource := range resourceList.APIResources {
		if resource.Kind == gvk.Kind {
			return resource.Namespaced, nil
		}
	}

	return false, &clients.NoNamespaceInfoErr{}
}
