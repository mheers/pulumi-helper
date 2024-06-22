package types

import (
	"sync"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func MergeResourceArrayOutputs(resourceArrayOutputs []pulumi.ResourceArrayOutput) pulumi.ResourceArrayOutput {

	var wg sync.WaitGroup
	wg.Add(len(resourceArrayOutputs))

	return pulumi.All(resourceArrayOutputs).ApplyT(func(vs []any) pulumi.ResourceArrayOutput {
		arr := []pulumi.Resource{}

		for _, v := range resourceArrayOutputs {
			pulumi.All(v).ApplyT(func(vs []any) pulumi.ResourceArrayOutput {
				r := vs[0].([]pulumi.Resource)
				arr = append(arr, r...)
				wg.Done()
				return pulumi.ResourceArrayOutput{}
			})
		}

		wg.Wait()

		return pulumi.ToResourceArray(arr).ToResourceArrayOutput()
	}).(pulumi.ResourceArrayOutput)
}

func ResourceMapToSlice(resourceMap map[string]pulumi.Resource) []pulumi.Resource {
	var resources []pulumi.Resource
	for _, resource := range resourceMap {
		resources = append(resources, resource)
	}
	return resources
}
