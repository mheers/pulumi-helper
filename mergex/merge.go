package helpers

import (
	"sync"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumix"
)

type allowedKeyType interface {
	// string | int | float64 | bool | *string | *int | *float64 | *bool | chan string | chan int | chan float64 | chan bool // TODO
	*string | *int | *float64 | *bool
}

type Merge[K allowedKeyType, V any] struct {
	Values []V
	Key    pulumix.Output[K]
}

type MergeArray[K allowedKeyType, V any] []Merge[K, V]

func MergeToMergeArray[K allowedKeyType, V any](s ...Merge[K, V]) MergeArray[K, V] {
	return s
}

func (sma MergeArray[K, V]) Merge() MergeArray[K, V] {
	result := MergeArray[K, V]{}

	sm := pulumix.Array[pulumix.Map[any]]{}
	for _, x := range sma {
		m := pulumix.Map[any]{
			"key":    x.Key.AsAny(),
			"values": pulumix.Val[[]V](x.Values).AsAny(),
		}
		sm = append(sm, pulumix.Val[pulumix.Map[any]](m))
	}

	handledKeys := map[K]bool{}

	wg := sync.WaitGroup{}
	wg.Add(1)
	sm.AsAny().ApplyT(func(m any) any {
		mi := m.([]pulumix.Map[any])
		for _, mx := range mi {
			kk := mx["key"].(K)
			if kk == K(nil) {
				continue
			}
			key := kk
			if _, handled := handledKeys[key]; handled {
				continue
			}
			handledKeys[key] = true

			values := values[K, V](sm, key)

			result = append(result, Merge[K, V]{
				Key:    pulumix.Val[K](key),
				Values: values,
			})
		}
		wg.Done()
		return mi
	})
	wg.Wait()
	return result
}

func values[K allowedKeyType, V any](sm pulumix.Array[pulumix.Map[any]], key K) []V {
	values := []V{}
	wg := sync.WaitGroup{}
	wg.Add(1)
	pulumix.Apply[[]pulumix.Map[any], any](sm, func(mi []pulumix.Map[any]) any {
		for _, mx := range mi {
			if mx["key"] == nil || mx["values"] == nil {
				continue
			}
			currentKey := mx["key"].(K)
			currentValues := mx["values"]
			if currentKey == key {
				pulumix.All(currentValues).ApplyT(func(v any) any {
					values = append(values, v.(V))
					return values
				})
			}
		}
		wg.Done()
		return mi
	})
	wg.Wait()
	return values
}
