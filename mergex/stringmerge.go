package helpers

import (
	"sync"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumix"
)

type StringMerge struct {
	Values []string
	Key    pulumix.Output[*string]
}

type StringMergeArray []StringMerge

func StringMergeToStringMergeArray(s ...StringMerge) StringMergeArray {
	return s
}

func (sma StringMergeArray) Merge() StringMergeArray {
	result := StringMergeArray{}

	sm := pulumi.MapArray{}
	for _, x := range sma {
		sm = append(sm, pulumi.Map{
			"key":    x.Key,
			"values": pulumi.ToStringArray(x.Values),
		})
	}

	handledKeys := map[string]bool{}

	wg := sync.WaitGroup{}
	wg.Add(1)
	sm.ToMapArrayOutput().ApplyT(func(m interface{}) interface{} {
		mi := m.([]map[string]interface{})
		for _, mx := range mi {
			kk := mx["key"].(*string)
			if kk == nil {
				continue
			}
			key := *kk
			if _, handled := handledKeys[key]; handled {
				continue
			}
			handledKeys[key] = true

			values := values(sm, key)

			result = append(result, StringMerge{
				Key:    pulumix.MustConvertTyped[*string](pulumi.String(key).ToStringPtrOutput()),
				Values: values,
			})
		}
		wg.Done()
		return mi
	})
	wg.Wait()
	return result
}

func values(sm pulumi.MapArray, key string) []string {
	values := []string{}
	wg := sync.WaitGroup{}
	wg.Add(1)
	sm.ToMapArrayOutput().ApplyT(func(m interface{}) interface{} {
		mi := m.([]map[string]interface{})
		for _, mx := range mi {
			if mx["key"] == nil || mx["values"] == nil {
				continue
			}
			currentKey := *mx["key"].(*string)
			currentHostNames := mx["values"].([]string)
			if currentKey == key {
				values = append(values, currentHostNames...)
			}
		}
		wg.Done()
		return mi
	})
	wg.Wait()
	return values
}
