package types

import (
	"sync"
	"testing"

	"github.com/mheers/pulumi-helper/mocks"
	corev1 "github.com/pulumi/pulumi-kubernetes/sdk/v3/go/kubernetes/core/v1"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/require"
)

func TestMergeResources(t *testing.T) {
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		cm1, err := corev1.NewConfigMap(ctx, "cm1", &corev1.ConfigMapArgs{})
		if err != nil {
			return err
		}

		cm2, err := corev1.NewConfigMap(ctx, "cm2", &corev1.ConfigMapArgs{})
		if err != nil {
			return err
		}

		merged := MergeResourceArrayOutputs(
			[]pulumi.ResourceArrayOutput{
				pulumi.NewResourceArrayOutput(
					pulumi.NewResourceOutput(cm1),
				),
				pulumi.NewResourceArrayOutput(
					pulumi.NewResourceOutput(cm2),
				),
			},
		)

		var wg sync.WaitGroup
		wg.Add(1)

		pulumi.All(merged).ApplyT(func(args []interface{}) pulumi.ArrayOutput {
			arr := args[0].([]pulumi.Resource)
			require.Len(t, arr, 2)
			wg.Done()
			return pulumi.ArrayOutput{}
		})

		wg.Wait()

		return nil
	}, pulumi.WithMocks("demo-project", "demo-stack", mocks.Mocks(0)))
	require.NoError(t, err)
}
