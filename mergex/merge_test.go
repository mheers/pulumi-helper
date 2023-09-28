package helpers

import (
	"testing"

	"github.com/aws/smithy-go/ptr"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumix"
	"github.com/stretchr/testify/assert"
)

func TestMergeArrayMerge(t *testing.T) {
	alias1 := Merge[*string, string]{
		Key:    pulumix.Val[*string](ptr.String("192.168.0.1")),
		Values: []string{"hostname1"},
	}

	alias2 := Merge[*string, string]{
		Key:    pulumix.Val[*string](ptr.String("192.168.0.1")),
		Values: []string{"hostname2"},
	}
	alias3 := Merge[*string, string]{
		Key:    pulumix.Val[*string](ptr.String("192.168.0.2")),
		Values: []string{"hostname3"},
	}

	sma := MergeToMergeArray(alias1, alias2, alias3)
	got := sma.Merge()

	assert.True(t, len(got) == 2)
	assert.True(t, len(got[0].Values) == 2)
	assert.True(t, got[0].Values[0] != got[0].Values[1])
}

// // Test with a derivate example
type hostAliasX struct {
	HostNames []string
	IP        pulumix.Output[*string]
}

func mergeHostAliasesX(aliases []hostAliasX) []hostAliasX {
	aliasMerges := make([]Merge[*string, string], 0)
	for _, alias := range aliases {
		sm := Merge[*string, string]{
			Key:    alias.IP,
			Values: alias.HostNames,
		}
		aliasMerges = append(aliasMerges, sm)
	}
	sma := MergeToMergeArray(aliasMerges...)
	merged := sma.Merge()

	result := make([]hostAliasX, 0)
	for _, m := range merged {
		result = append(result, hostAliasX{
			HostNames: m.Values,
			IP:        m.Key,
		})
	}
	return result
}

func TestMergeHostAliasesX(t *testing.T) {
	alias1 := hostAliasX{
		IP:        pulumix.MustConvertTyped[*string](pulumi.String("192.168.0.1").ToStringPtrOutput()),
		HostNames: []string{"alias1"},
	}

	alias2 := hostAliasX{
		IP:        pulumix.MustConvertTyped[*string](pulumi.String("192.168.0.1").ToStringPtrOutput()),
		HostNames: []string{"alias2"},
	}

	aliases := []hostAliasX{alias1, alias2}

	got := mergeHostAliasesX(aliases)
	assert.True(t, len(got) == 1)
	assert.True(t, len(got[0].HostNames) == 2)
}
