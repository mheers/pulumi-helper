package helpers

import (
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumix"
	"github.com/stretchr/testify/assert"
)

func TestStringMergeArrayMerge(t *testing.T) {
	alias1 := StringMerge{
		Key:    pulumix.MustConvertTyped[*string](pulumi.String("192.168.0.1").ToStringPtrOutput()),
		Values: []string{"hostname1"},
	}

	alias2 := StringMerge{
		Key:    pulumix.MustConvertTyped[*string](pulumi.String("192.168.0.1").ToStringPtrOutput()),
		Values: []string{"hostname2"},
	}
	alias3 := StringMerge{
		Key:    pulumix.MustConvertTyped[*string](pulumi.String("192.168.0.2").ToStringPtrOutput()),
		Values: []string{"hostname3"},
	}

	sma := StringMergeToStringMergeArray(alias1, alias2, alias3)
	got := sma.Merge()

	assert.True(t, len(got) == 2)
	assert.True(t, len(got[0].Values) == 2)
	assert.True(t, got[0].Values[0] != got[0].Values[1])
}

// // Test with a derivate example
type hostAlias struct {
	HostNames []string
	IP        pulumix.Output[*string]
}

func mergeHostAliases(aliases []hostAlias) []hostAlias {
	aliasMerges := make([]StringMerge, 0)
	for _, alias := range aliases {
		sm := StringMerge{
			Values: alias.HostNames,
			Key:    alias.IP,
		}
		aliasMerges = append(aliasMerges, sm)
	}
	sma := StringMergeToStringMergeArray(aliasMerges...)
	merged := sma.Merge()

	result := make([]hostAlias, 0)
	for _, m := range merged {
		result = append(result, hostAlias{
			HostNames: m.Values,
			IP:        m.Key,
		})
	}
	return result
}

func TestMergeHostAliases(t *testing.T) {
	alias1 := hostAlias{
		IP:        pulumix.MustConvertTyped[*string](pulumi.String("192.168.0.1").ToStringPtrOutput()),
		HostNames: []string{"alias1"},
	}

	alias2 := hostAlias{
		IP:        pulumix.MustConvertTyped[*string](pulumi.String("192.168.0.1").ToStringPtrOutput()),
		HostNames: []string{"alias2"},
	}

	aliases := []hostAlias{alias1, alias2}

	got := mergeHostAliases(aliases)
	assert.True(t, len(got) == 1)
	assert.True(t, len(got[0].HostNames) == 2)
}
