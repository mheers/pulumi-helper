package helpers

import (
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/assert"
)

func TestStringMergeArrayMerge(t *testing.T) {
	alias1 := StringMerge{
		Key:    pulumi.String("192.168.0.1").ToStringPtrOutput(),
		Values: []string{"hostname1"},
	}

	alias2 := StringMerge{
		Key:    pulumi.String("192.168.0.1").ToStringPtrOutput(),
		Values: []string{"hostname2"},
	}
	alias3 := StringMerge{
		Key:    pulumi.String("192.168.0.2").ToStringPtrOutput(),
		Values: []string{"hostname3"},
	}

	sma := StringMergeToStringMergeArray(alias1, alias2, alias3)
	got := sma.Merge()

	assert.True(t, len(got) == 2)
	assert.True(t, len(got[0].Values) == 2)
	assert.True(t, got[0].Values[0] != got[0].Values[1])
}
