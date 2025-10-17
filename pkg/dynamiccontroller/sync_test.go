package dynamiccontroller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTypedAsyncMapWithPlainValues(t *testing.T) {
	m := typedSyncMap[string, int]{}

	m.Store("foo", 1)
	m.Store("bar", 2)
	m.Store("baz", 3)

	assert.Equal(t, 3, m.Len())

	keys := 0
	m.Range(func(key string, value int) bool {
		keys++
		switch key {
		case "foo":
			assert.Equal(t, 1, value)
		case "bar":
			assert.Equal(t, 2, value)
		case "baz":
			assert.Equal(t, 3, value)
		}
		return true
	})
	assert.Equal(t, 3, keys)

	value, loaded := m.LoadAndDelete("foo")
	assert.Equal(t, 1, value)
	assert.True(t, loaded)

	_, loaded = m.Load("foo")
	assert.False(t, loaded)

	_, loaded = m.Swap("foo", 4)
	assert.False(t, loaded)

	old, loaded := m.Swap("foo", 5)
	assert.True(t, loaded)
	assert.Equal(t, 4, old)

	m.Delete("bar")
	assert.Equal(t, 2, m.Len())

	_, loaded = m.Load("bar")
	assert.False(t, loaded)

	value, loaded = m.LoadOrStore("buz", 6)
	assert.Equal(t, 6, value)
	assert.False(t, loaded)

	value, loaded = m.LoadOrStore("buz", 7)
	assert.Equal(t, 6, value)
	assert.True(t, loaded)
}

func TestTypedAsyncMapWithPointers(t *testing.T) {
	m := typedSyncMap[string, *int]{}

	one := 1
	two := 2
	three := 3

	m.Store("foo", &one)

	keys := 0
	m.Range(func(key string, value *int) bool {
		keys++
		require.NotNil(t, value)
		assert.Equal(t, 1, *value)
		return true
	})
	assert.Equal(t, 1, keys)

	value, loaded := m.LoadAndDelete("foo")
	require.NotNil(t, value)
	assert.Equal(t, 1, *value)
	assert.True(t, loaded)

	_, loaded = m.Load("foo")
	assert.False(t, loaded)

	_, loaded = m.Swap("foo", &two)
	assert.False(t, loaded)

	old, loaded := m.Swap("foo", &three)
	assert.True(t, loaded)
	require.NotNil(t, old)
	assert.Equal(t, two, *old)

	value, loaded = m.LoadOrStore("buz", &one)
	require.NotNil(t, value)
	assert.Equal(t, 1, *value)
	assert.False(t, loaded)

	value, loaded = m.LoadOrStore("buz", &two)
	require.NotNil(t, value)
	assert.Equal(t, 1, *value)
	assert.True(t, loaded)
}
