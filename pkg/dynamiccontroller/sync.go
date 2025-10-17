package dynamiccontroller

import "sync"

type typedSyncMap[K comparable, V any] sync.Map

func (m *typedSyncMap[K, V]) syncMap() *sync.Map {
	return (*sync.Map)(m)
}

func (m *typedSyncMap[K, V]) LoadAndDelete(key K) (V, bool) {
	v, ok := m.syncMap().LoadAndDelete(key)
	if !ok {
		return *new(V), false
	}
	return v.(V), true
}

func (m *typedSyncMap[K, V]) Store(key K, value V) {
	m.syncMap().Store(key, value)
}

func (m *typedSyncMap[K, V]) Delete(key K) {
	m.syncMap().Delete(key)
}

func (m *typedSyncMap[K, V]) Range(f func(key K, value V) bool) {
	m.syncMap().Range(func(key any, value any) bool {
		return f(key.(K), value.(V))
	})
}

func (m *typedSyncMap[K, V]) Load(key K) (V, bool) {
	v, ok := m.syncMap().Load(key)
	if !ok {
		return *new(V), false
	}
	return v.(V), true
}

func (m *typedSyncMap[K, V]) Swap(key K, value V) (V, bool) {
	v, ok := m.syncMap().Swap(key, value)
	if !ok {
		return *new(V), false
	}
	return v.(V), true
}

func (m *typedSyncMap[K, V]) LoadOrStore(key K, value V) (V, bool) {
	v, ok := m.syncMap().LoadOrStore(key, value)
	return v.(V), ok
}

func (m *typedSyncMap[K, V]) Len() int {
	var count int
	m.syncMap().Range(func(key any, value any) bool {
		count++
		return true
	})
	return count
}
