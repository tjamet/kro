package dynamiccontroller

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
)

type informerWrapper struct {
	factory               dynamicinformer.DynamicSharedInformerFactory
	informer              cache.SharedInformer
	handlersRegistrations typedSyncMap[schema.GroupVersionResource, cache.ResourceEventHandlerRegistration]
	shutdown              func()
}

func (i *informerWrapper) SetEventHandler(gvr schema.GroupVersionResource, handler cache.ResourceEventHandlerFuncs) error {
	registration, err := i.informer.AddEventHandler(handler)
	if err != nil {
		return err
	}
	oldRegistration, loaded := i.handlersRegistrations.Swap(gvr, registration)
	if loaded {
		err := i.informer.RemoveEventHandler(oldRegistration)
		if err != nil {
			return err
		}
	}
	return nil
}

func (i *informerWrapper) RemoveEventHandler(gvr schema.GroupVersionResource) error {
	registration, loaded := i.handlersRegistrations.LoadAndDelete(gvr)
	if !loaded {
		return nil
	}
	return i.informer.RemoveEventHandler(registration)
}

func (i *informerWrapper) ShouldShutdown() bool {
	return i.handlersRegistrations.Len() == 0
}

func (i *informerWrapper) Shutdown() bool {
	if !i.ShouldShutdown() {
		return false
	}
	if i.shutdown != nil {
		i.shutdown()
		i.shutdown = nil
	}
	if i.factory != nil {
		// Wait for the informer to shut down
		i.factory.Shutdown()
		i.factory = nil
	}
	return true
}

type InformerEventHandlerFactory struct {
	gvr           schema.GroupVersionResource
	eventHandlers map[schema.GroupVersionResource]func(log logr.Logger, enqueue func(ObjectIdentifiers)) cache.ResourceEventHandlerFuncs
	errors        []error
}

func For(gvr schema.GroupVersionResource) *InformerEventHandlerFactory {
	return &InformerEventHandlerFactory{
		gvr: gvr,
		eventHandlers: map[schema.GroupVersionResource]func(log logr.Logger, enqueue func(ObjectIdentifiers)) cache.ResourceEventHandlerFuncs{
			gvr: func(log logr.Logger, enqueue func(ObjectIdentifiers)) cache.ResourceEventHandlerFuncs {

				enqueueObject := func(obj interface{}, eventType string) {
					namespacedKey, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
					if err != nil {
						log.Error(err, "Failed to get key for object", "eventType", eventType)
						return
					}

					u, ok := obj.(*unstructured.Unstructured)
					if !ok {
						err := fmt.Errorf("object is not an Unstructured")
						log.Error(err, "Failed to cast object to Unstructured", "eventType", eventType, "namespacedKey", namespacedKey)
						return
					}

					gvk := u.GroupVersionKind()
					gvr := metadata.GVKtoGVR(gvk)
					objectIdentifiers := ObjectIdentifiers{
						NamespacedName: parseNamespacedName(namespacedKey),
						GVR:            gvr,
					}
					enqueue(objectIdentifiers)
				}

				return cache.ResourceEventHandlerFuncs{
					AddFunc: func(obj interface{}) { enqueueObject(obj, "add") },
					// We don't expect anything else to change the main object status.
					// We can avoid unnecessary and possibly infinite reconciliations
					// by only reconciling when the generation (i.e. spec) changes.
					UpdateFunc: func(old, new interface{}) {
						newObj, ok := new.(*unstructured.Unstructured)
						if !ok {
							log.Error(nil, "failed to cast new object to unstructured")
							return
						}
						oldObj, ok := old.(*unstructured.Unstructured)
						if !ok {
							log.Error(nil, "failed to cast old object to unstructured")
							return
						}

						if newObj.GetGeneration() == oldObj.GetGeneration() {
							log.V(2).Info("Skipping update due to unchanged generation",
								"name", newObj.GetName(),
								"namespace", newObj.GetNamespace(),
								"generation", newObj.GetGeneration())
							return
						}

						enqueueObject(new, "update")
					},
					DeleteFunc: func(obj interface{}) { enqueueObject(obj, "delete") },
				}
			},
		},
	}
}

// Owns finds the owner of a given resource based on the OwnerReferences metadata
func (b *InformerEventHandlerFactory) Owns(ownedGVK schema.GroupVersionKind) *InformerEventHandlerFactory {
	ownedGVR := metadata.GVKtoGVR(ownedGVK)
	if _, ok := b.eventHandlers[ownedGVR]; ok {
		b.errors = append(b.errors, fmt.Errorf("event handler already registered for GVK: %s", ownedGVK))
	}
	b.eventHandlers[ownedGVR] = func(log logr.Logger, enqueue func(ObjectIdentifiers)) cache.ResourceEventHandlerFuncs {
		log.V(1).Info("Registering Owns handler for GVK", "ownedGVK", ownedGVK)

		enqueueObject := func(obj interface{}, eventType string) {
			namespacedKey, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err != nil {
				log.Error(err, "Failed to get key for object", "eventType", eventType)
				return
			}
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				err := fmt.Errorf("object is not an Unstructured")
				log.Error(err, "Failed to cast object to Unstructured", "eventType", eventType, "namespacedKey", namespacedKey)
				return
			}

			ownerReferences := u.GetOwnerReferences()
			for _, ownerReference := range ownerReferences {
				gv, err := schema.ParseGroupVersion(ownerReference.APIVersion)
				if err != nil {
					log.Error(err, "Failed to parse group version", "ownerReference", ownerReference)
					continue
				}
				gvk := schema.GroupVersionKind{
					Group:   gv.Group,
					Version: gv.Version,
					Kind:    ownerReference.Kind,
				}
				gvr := metadata.GVKtoGVR(gvk)
				if gvr == b.gvr {
					enqueue(ObjectIdentifiers{
						NamespacedName: types.NamespacedName{
							Namespace: u.GetNamespace(),
							Name:      ownerReference.Name,
						},
						GVR: b.gvr,
					})
				} else {
					log.V(1).Info("Object is not owned by the given GVK", "namespacedName", namespacedKey, "ownedGVK", ownedGVK)
				}
			}
		}

		return cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) { enqueueObject(obj, "add") },
			// For owner references, we always enqueue the new object.
			// Even if the object generation didn't change,
			// any change in the status may be relevant for the reconciler.
			UpdateFunc: func(old, new interface{}) { enqueueObject(new, "update") },
			DeleteFunc: func(obj interface{}) { enqueueObject(obj, "delete") },
		}
	}
	return b
}

// KroOwns finds the owner of a given resource based on the KRO labels
func (b *InformerEventHandlerFactory) KroOwns(ownedGVK schema.GroupVersionKind) *InformerEventHandlerFactory {
	ownedGVR := metadata.GVKtoGVR(ownedGVK)
	if _, ok := b.eventHandlers[ownedGVR]; ok {
		b.errors = append(b.errors, fmt.Errorf("event handler already registered for GVK: %s", ownedGVR))
	}

	b.eventHandlers[ownedGVR] = func(log logr.Logger, enqueue func(ObjectIdentifiers)) cache.ResourceEventHandlerFuncs {

		enqueueObject := func(obj interface{}, eventType string) {
			namespacedKey, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err != nil {
				log.Error(err, "Failed to get key for object", "eventType", eventType)
				return
			}
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				err := fmt.Errorf("object is not an Unstructured")
				log.Error(err, "Failed to cast object to Unstructured", "eventType", eventType, "namespacedKey", namespacedKey)
				return
			}
			namespace, name, ok := metadata.GetKROOwner(u)
			if !ok {
				log.V(1).Info("Object is not KRO owned", "namespacedName", namespacedKey)
				return
			}
			enqueue(ObjectIdentifiers{
				NamespacedName: types.NamespacedName{
					Namespace: namespace,
					Name:      name,
				},
				GVR: b.gvr,
			})
			log.V(1).Info("Enqueueing object", "namespacedName", namespacedKey, "ownedGVR", ownedGVR)
		}

		return cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) { enqueueObject(obj, "add") },
			// For owner references, we always enqueue the new object.
			// Even if the object generation didn't change,
			// any change in the status may be relevant for the reconciler.
			UpdateFunc: func(old, new interface{}) { enqueueObject(new, "update") },
			DeleteFunc: func(obj interface{}) { enqueueObject(obj, "delete") },
		}
	}
	return b
}

func (b *InformerEventHandlerFactory) BuildInformerHandlers(log logr.Logger, enqueue func(ObjectIdentifiers)) (map[schema.GroupVersionResource]cache.ResourceEventHandlerFuncs, error) {
	handlers := make(map[schema.GroupVersionResource]cache.ResourceEventHandlerFuncs)
	for gvr, factory := range b.eventHandlers {
		handlers[gvr] = factory(log, enqueue)
	}
	return handlers, nil
}

// Register registers a new GVK to the informers map safely.
func (dc *DynamicController) startGVKInformer(ctx context.Context, gvr schema.GroupVersionResource) (*informerWrapper, error) {
	dc.log.V(1).Info("Starting GVK informer", "gvr", gvr)

	informerContext := context.Background()
	cancelableContext, cancel := context.WithCancel(informerContext)

	informerWrapper, loaded := dc.informers.LoadOrStore(gvr, &informerWrapper{
		shutdown: cancel,
	})
	if loaded {
		dc.log.V(2).Info("Informer already registered, triggering reconciliation", "gvr", gvr)
		// trigger reconciliation of the corresponding gvr's
		objs, err := dc.kubeClient.Resource(gvr).Namespace("").List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to list objects for GVR %s: %w", gvr, err)
		}
		for _, obj := range objs.Items {
			dc.enqueueObject(&obj, "update")
		}
		return informerWrapper, nil
	}
	// Create a new informer without starting it.
	// If there was no informer we will start it later
	// otherwise it will be garbage collected
	informerFactory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		dc.kubeClient,
		dc.config.ResyncPeriod,
		// Maybe we can make this configurable in the future. Thinking that
		// we might want to filter out some resources, by namespace or labels
		"",
		nil,
	)

	informerWrapper.factory = informerFactory
	informer := informerWrapper.factory.ForResource(gvr).Informer()
	informerWrapper.informer = informer

	if err := informer.SetWatchErrorHandler(func(r *cache.Reflector, err error) {
		dc.log.Error(err, "Watch error", "gvr", gvr)
	}); err != nil {
		dc.log.Error(err, "Failed to set watch error handler", "gvr", gvr)
		return nil, fmt.Errorf("failed to set watch error handler for GVR %s: %w", gvr, err)
	}

	// Start the informer
	go func() {
		dc.log.V(1).Info("Starting informer", "gvr", gvr)
		// time.Sleep(5 * time.Millisecond)
		informer.Run(cancelableContext.Done())
	}()

	dc.log.V(1).Info("Waiting for cache sync", "gvr", gvr)
	startTime := time.Now()
	// Wait for cache sync with a timeout
	synced := cache.WaitForCacheSync(cancelableContext.Done(), informer.HasSynced)
	syncDuration := time.Since(startTime)
	informerSyncDuration.WithLabelValues(gvr.String()).Observe(syncDuration.Seconds())

	if !synced {
		cancel()
		return nil, fmt.Errorf("failed to sync informer cache for GVR %s", gvr)
	}

	informerCount.Inc()
	dc.log.V(1).Info("Successfully started GVK informer", "gvr", gvr)
	return informerWrapper, nil
}

// Deregister safely removes a GVK from the controller and cleans up associated resources.
func (dc *DynamicController) stopGVKInformer(ctx context.Context, gvr schema.GroupVersionResource) error {
	dc.log.Info("Stopping GVK informer", "gvr", gvr)
	// Retrieve the informer
	wrapper, ok := dc.informers.LoadAndDelete(gvr)
	if !ok {
		dc.log.V(1).Info("GVK informer not registered, nothing to stop", "gvr", gvr)
		return nil
	}

	// Stop the informer
	dc.log.V(1).Info("Stopping informer", "gvr", gvr)

	// Cancel the context to stop the informer
	wrapper.Shutdown()

	informerCount.Dec()
	// Clean up any pending items in the queue for this GVR
	// NOTE(a-hilaly): This is a bit heavy.. maybe we can find a better way to do this.
	// Thinking that we might want to have a queue per GVR.
	// dc.cleanupQueue(gvr)
	// time.Sleep(1 * time.Second)
	// isStopped := wrapper.informer.ForResource(gvr).Informer().IsStopped()
	dc.log.V(1).Info("Successfully stopped GVK informer", "gvr", gvr)
	return nil
}
