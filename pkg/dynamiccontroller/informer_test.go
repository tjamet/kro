package dynamiccontroller

import (
	"testing"

	"github.com/go-logr/logr/testr"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestInformerEWventFactory_KroOwns(t *testing.T) {
	reconciledGVR := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}
	ownedGVK := schema.GroupVersionKind{Group: "test", Version: "v1", Kind: "Other"}
	ownedGVR := metadata.GVKtoGVR(ownedGVK)

	enqueued := 0
	enqueue := func(identifier ObjectIdentifiers) {
		assert.Equal(t, reconciledGVR, identifier.GVR)
		assert.Equal(t, "test-instance-name", identifier.NamespacedName.Name)
		assert.Equal(t, "test-instance-namespace", identifier.NamespacedName.Namespace)
		enqueued++
	}
	factory := For(reconciledGVR).KroOwns(ownedGVK).KroOwns(ownedGVK)
	logger := testr.New(t)
	handlers, err := factory.BuildInformerHandlers(logger, enqueue)
	assert.NoError(t, err)
	assert.Len(t, handlers, 2)
	require.Contains(t, handlers, ownedGVR)

	labeller := metadata.NewInstanceLabeler(&unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "test.com/v1",
			"kind":       "Test",
			"metadata": map[string]interface{}{
				"name":      "test-instance-name",
				"namespace": "test-instance-namespace",
			},
		},
	})

	notKroOwned := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "test.com/v1",
			"kind":       "Other",
		},
	}

	kroOwned := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "test.com/v1",
			"kind":       "Other",
		},
	}
	metadata.NewKROMetaLabeler().ApplyLabels(notKroOwned)
	metadata.NewKROMetaLabeler().ApplyLabels(kroOwned)
	labeller.ApplyLabels(kroOwned)

	handlers[ownedGVR].AddFunc(notKroOwned)
	handlers[ownedGVR].UpdateFunc(nil, notKroOwned)
	handlers[ownedGVR].DeleteFunc(notKroOwned)
	assert.Equal(t, 0, enqueued)

	handlers[ownedGVR].AddFunc(kroOwned)
	handlers[ownedGVR].UpdateFunc(nil, kroOwned)
	handlers[ownedGVR].DeleteFunc(kroOwned)
	assert.Equal(t, 3, enqueued)
}

func TestInformerEWventFactory_Owns(t *testing.T) {

	reconciledGVR := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}
	ownedGVK := schema.GroupVersionKind{Group: "test", Version: "v1", Kind: "Other"}
	ownedGVR := metadata.GVKtoGVR(ownedGVK)

	enqueued := 0
	enqueue := func(identifier ObjectIdentifiers) {
		assert.Equal(t, reconciledGVR, identifier.GVR)
		assert.Equal(t, "test-instance-name", identifier.NamespacedName.Name)
		assert.Equal(t, "test-instance-namespace", identifier.NamespacedName.Namespace)
		enqueued++
	}

	factory := For(reconciledGVR).Owns(ownedGVK)
	logger := testr.New(t)

	handlers, err := factory.BuildInformerHandlers(logger, enqueue)
	assert.NoError(t, err)
	assert.Len(t, handlers, 2)
	require.Contains(t, handlers, ownedGVR)

	notOwned := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "test/v1",
			"kind":       "Other",
			"metadata": map[string]interface{}{
				"name":      "some-name",
				"namespace": "test-instance-namespace",
			},
		},
	}

	owned := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "test/v1",
			"kind":       "Other",
			"metadata": map[string]interface{}{
				"name":      "some-name",
				"namespace": "test-instance-namespace",
			},
		},
	}
	metadata.NewKROMetaLabeler().ApplyLabels(notOwned)
	metadata.NewKROMetaLabeler().ApplyLabels(owned)
	owned.SetOwnerReferences([]metav1.OwnerReference{
		{
			APIVersion: "test/v1",
			Kind:       "Test",
			Name:       "test-instance-name",
		},
	})

	handlers[ownedGVR].AddFunc(notOwned)
	handlers[ownedGVR].UpdateFunc(nil, notOwned)
	handlers[ownedGVR].DeleteFunc(notOwned)
	assert.Equal(t, 0, enqueued)

	handlers[ownedGVR].AddFunc(owned)
	handlers[ownedGVR].UpdateFunc(nil, owned)
	handlers[ownedGVR].DeleteFunc(owned)
	assert.Equal(t, 3, enqueued)
}

func TestInformerEWventFactory_For(t *testing.T) {

	reconciledGVR := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}

	enqueued := 0
	enqueue := func(identifier ObjectIdentifiers) {
		assert.Equal(t, reconciledGVR, identifier.GVR)
		assert.Equal(t, "test-instance-name", identifier.NamespacedName.Name)
		assert.Equal(t, "test-instance-namespace", identifier.NamespacedName.Namespace)
		enqueued++
	}

	factory := For(reconciledGVR)
	logger := testr.New(t)

	handlers, err := factory.BuildInformerHandlers(logger, enqueue)
	assert.NoError(t, err)
	assert.Len(t, handlers, 1)
	require.Contains(t, handlers, reconciledGVR)

	previous := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "test/v1",
			"kind":       "Test",
			"metadata": map[string]interface{}{
				"name":       "test-instance-name",
				"namespace":  "test-instance-namespace",
				"generation": int64(1),
			},
		},
	}

	latest := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "test/v1",
			"kind":       "Test",
			"metadata": map[string]interface{}{
				"name":       "test-instance-name",
				"namespace":  "test-instance-namespace",
				"generation": int64(2),
			},
		},
	}

	t.Run("When there is no change in the spec generation, it should not enqueue the object", func(t *testing.T) {
		handlers[reconciledGVR].UpdateFunc(latest, latest)
		assert.Equal(t, 0, enqueued)
	})

	t.Run("When the spec generation changes, it should enqueue the object", func(t *testing.T) {
		enqueued = 0
		handlers[reconciledGVR].UpdateFunc(previous, latest)
		assert.Equal(t, 1, enqueued)
	})

	t.Run("When the object is added, it should enqueue the object", func(t *testing.T) {
		enqueued = 0
		handlers[reconciledGVR].AddFunc(latest)
		assert.Equal(t, 1, enqueued)
	})

	t.Run("When the object is deleted, it should enqueue the object", func(t *testing.T) {
		enqueued = 0
		handlers[reconciledGVR].DeleteFunc(latest)
		assert.Equal(t, 1, enqueued)
	})
}
