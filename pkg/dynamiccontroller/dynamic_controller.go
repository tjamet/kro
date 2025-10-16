// Copyright 2025 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package dynamiccontroller provides a flexible and efficient solution for
// managing multiple GroupVersionResources (GVRs) in a Kubernetes environment.
// It implements a single controller capable of dynamically handling various
// resource types concurrently, adapting to runtime changes without system restarts.
//
// Key features and design considerations:
//
//  1. Multi GVR management: It handles multiple resource types concurrently,
//     creating and managing separate workflows for each.
//
//  2. Dynamic informer management: Creates and deletes informers on the fly
//     for new resource types, allowing real time adaptation to changes in the
//     cluster.
//
//  3. Minimal disruption: Operations on one resource type do not affect
//     the performance or functionality of others.
//
//  4. Minimalism: Unlike controller-runtime, this implementation
//     is tailored specifically for kro's needs, avoiding unnecessary
//     dependencies and overhead.
//
//  5. Future Extensibility: It allows for future enhancements such as
//     sharding and CEL cost aware leader election, which are not readily
//     achievable with k8s.io/controller-runtime.
//
// Why not use k8s.io/controller-runtime:
//
//  1. Static nature: controller-runtime is optimized for statically defined
//     controllers, however kro requires runtime creation and management
//     of controllers for various GVRs.
//
//  2. Overhead reduction: by not including unused features like leader election
//     and certain metrics, this implementation remains minimalistic and efficient.
//
//  3. Customization: this design allows for deep customization and
//     optimization specific to kro's unique requirements for managing
//     multiple GVRs dynamically.
//
// This implementation aims to provide a reusable, efficient, and flexible
// solution for dynamic multi-GVR controller management in Kubernetes environments.
//
// NOTE(a-hilaly): Potentially we might open source this package for broader use cases.
package dynamiccontroller

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/time/rate"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/requeue"
)

// Config holds the configuration for DynamicController
type Config struct {
	// Workers specifies the number of workers processing items from the queue
	Workers int
	// ResyncPeriod defines the interval at which the controller will re list
	// the resources, even if there haven't been any changes.
	ResyncPeriod time.Duration
	// QueueMaxRetries is the maximum number of retries for an item in the queue
	// will be retried before being dropped.
	//
	// NOTE(a-hilaly): I'm not very sure how useful is this, i'm trying to avoid
	// situations where reconcile errors exhaust the queue.
	QueueMaxRetries int
	// MinRetryDelay is the minimum delay before retrying an item in the queue
	MinRetryDelay time.Duration
	// MaxRetryDelay is the maximum delay before retrying an item in the queue
	MaxRetryDelay time.Duration
	// RateLimit is the maximum number of events processed per second
	RateLimit int
	// BurstLimit is the maximum number of events in a burst
	BurstLimit int
}

// DynamicController (DC) is a single controller capable of managing multiple different
// kubernetes resources (GVRs) in parallel. It can safely start watching new
// resources and stop watching others at runtime - hence the term "dynamic". This
// flexibility allows us to accept and manage various resources in a Kubernetes
// cluster without requiring restarts or pod redeployments.
//
// It is mainly inspired by native Kubernetes controllers but designed for more
// flexible and lightweight operation. DC serves as the core component of kro's
// dynamic resource management system. Its primary purpose is to create and manage
// "micro" controllers for custom resources defined by users at runtime (via the
// ResourceGraphDefinition CRs).
type DynamicController struct {
	config Config

	// kubeClient is the dynamic client used to create the informers
	kubeClient dynamic.Interface
	// informers is a safe map of GVR to informers. Each informer is responsible
	// for watching a specific GVR.
	informers sync.Map

	// handlers is a safe map of GVR to workflow operators. Each
	// handler is responsible for managing a specific GVR.
	handlers sync.Map

	// access is the mutex to access the resolvers map
	access sync.Mutex
	// reconcileRequestMappers is a map of managed GVR to reconciler GVR to resolver function
	// the managed GVR is stored first to allow more efficient lookup in the informer
	reconcileRequestMappers map[schema.GroupVersionResource]map[schema.GroupVersionResource]func(obj *unstructured.Unstructured) (types.NamespacedName, bool)

	// queue is the workqueue used to process items
	queue workqueue.TypedRateLimitingInterface[ObjectIdentifiers]

	log logr.Logger
}

type Handler func(ctx context.Context, req ctrl.Request) error

type ReconciledInstanceFinder func(obj *unstructured.Unstructured) (types.NamespacedName, bool)

type ReconciledInstanceFinderBuilder interface {
	// WatchedResource returns the resource that needs to be watched
	WatchedResource() schema.GroupVersionResource
	NewFinder(reconciledResource schema.GroupVersionResource) ReconciledInstanceFinder
}

type informerWrapper struct {
	informer dynamicinformer.DynamicSharedInformerFactory
	shutdown func()
}

type selfInstanceFinder struct {
	log              logr.Logger
	instanceResource schema.GroupVersionResource
}

func (r *selfInstanceFinder) WatchedResource() schema.GroupVersionResource {
	return r.instanceResource
}
func (r *selfInstanceFinder) NewFinder(reconciledResource schema.GroupVersionResource) ReconciledInstanceFinder {
	return func(obj *unstructured.Unstructured) (types.NamespacedName, bool) {
		namespacedKey, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
		if err != nil {
			r.log.Error(err, "Failed to get key for object")
			return types.NamespacedName{}, false
		}
		return parseNamespacedName(namespacedKey), true
	}
}

type ownerInstanceFinder struct {
	watchedResource schema.GroupVersionResource
}

func (r *ownerInstanceFinder) WatchedResource() schema.GroupVersionResource {
	return r.watchedResource
}

func (r *ownerInstanceFinder) NewFinder(reconciledResource schema.GroupVersionResource) ReconciledInstanceFinder {
	return func(obj *unstructured.Unstructured) (types.NamespacedName, bool) {
		ownerReferences := obj.GetOwnerReferences()
		for _, ownerReference := range ownerReferences {
			gv, err := schema.ParseGroupVersion(ownerReference.APIVersion)
			if err != nil {
				continue
			}
			ownerGVK := schema.GroupVersionKind{
				Group:   gv.Group,
				Version: gv.Version,
				Kind:    ownerReference.Kind,
			}
			ownerGVR := metadata.GVKtoGVR(ownerGVK)
			if ownerGVR == reconciledResource {
				return types.NamespacedName{
					Namespace: obj.GetNamespace(),
					Name:      ownerReference.Name,
				}, true
			}
		}
		return types.NamespacedName{}, false
	}
}

func Owns(gvk schema.GroupVersionKind) ReconciledInstanceFinderBuilder {
	gvr := metadata.GVKtoGVR(gvk)
	return &ownerInstanceFinder{
		watchedResource: gvr,
	}
}

// NewDynamicController creates a new DynamicController instance.
func NewDynamicController(
	log logr.Logger,
	config Config,
	kubeClient dynamic.Interface) *DynamicController {
	logger := log.WithName("dynamic-controller")

	dc := &DynamicController{
		config:     config,
		kubeClient: kubeClient,
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(workqueue.NewTypedMaxOfRateLimiter(
			workqueue.NewTypedItemExponentialFailureRateLimiter[ObjectIdentifiers](config.MinRetryDelay, config.MaxRetryDelay),
			&workqueue.TypedBucketRateLimiter[ObjectIdentifiers]{Limiter: rate.NewLimiter(rate.Limit(config.RateLimit), config.BurstLimit)},
		), workqueue.TypedRateLimitingQueueConfig[ObjectIdentifiers]{Name: "dynamic-controller-queue"}),
		log: logger,
		// pass version and pod id from env
	}

	return dc
}

// AllInformerHaveSynced checks if all registered informers have synced, returns
// true if they have.
func (dc *DynamicController) AllInformerHaveSynced() bool {
	var allSynced bool
	var informerCount int

	// Unfortunately we can't know the number of informers in advance, so we need to
	// iterate over all of them to check if they have synced.

	dc.informers.Range(func(key, value interface{}) bool {
		informerCount++
		// possibly panic if the value is not a SharedIndexInformer
		informer, ok := value.(cache.SharedIndexInformer)
		if !ok {
			dc.log.Error(nil, "Failed to cast informer", "key", key)
			allSynced = false
			return false
		}
		if !informer.HasSynced() {
			allSynced = false
			return false
		}
		return true
	})

	if informerCount == 0 {
		return true
	}
	return allSynced
}

// WaitForInformerSync waits for all informers to sync or timeout
func (dc *DynamicController) WaitForInformersSync(stopCh <-chan struct{}) bool {
	dc.log.V(1).Info("Waiting for all informers to sync")
	start := time.Now()
	defer func() {
		dc.log.V(1).Info("Finished waiting for informers to sync", "duration", time.Since(start))
	}()

	return cache.WaitForCacheSync(stopCh, dc.AllInformerHaveSynced)
}

// Run starts the DynamicController.
func (dc *DynamicController) Start(ctx context.Context) error {
	defer utilruntime.HandleCrash()
	defer dc.queue.ShutDown()

	dc.log.Info("Starting dynamic controller")
	defer dc.log.Info("Shutting down dynamic controller")

	// Wait for all informers to sync
	if !dc.WaitForInformersSync(ctx.Done()) {
		return fmt.Errorf("failed to sync informers")
	}

	// Spin up workers.
	//
	// TODO(a-hilaly): Allow for dynamic scaling of workers.
	var wg sync.WaitGroup
	for i := 0; i < dc.config.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wait.UntilWithContext(ctx, dc.worker, time.Second)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		dc.log.Info("Received shutdown signal, shutting down dynamic controller queue")
		dc.queue.ShutDown()
	}()

	wg.Wait()
	dc.log.Info("All workers have stopped")

	// when shutting down, the context given to Start is already closed,
	// and the expectation is that we block until the graceful shutdown is complete.
	return dc.shutdown(context.Background())
}

// worker processes items from the queue.
func (dc *DynamicController) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			dc.log.Info("Dynamic controller worker received shutdown signal, stopping")
			return
		default:
			dc.processNextWorkItem(ctx)
		}
	}
}

// processNextWorkItem processes a single item from the queue.
func (dc *DynamicController) processNextWorkItem(ctx context.Context) bool {
	item, shutdown := dc.queue.Get()
	if shutdown {
		return false
	}
	defer dc.queue.Done(item)

	queueLength.Set(float64(dc.queue.Len()))

	err := dc.syncFunc(ctx, item)
	if err == nil || apierrors.IsNotFound(err) {
		dc.queue.Forget(item)
		return true
	}

	gvrKey := fmt.Sprintf("%s/%s/%s", item.GVR.Group, item.GVR.Version, item.GVR.Resource)

	// Handle requeues
	switch typedErr := err.(type) {
	case *requeue.NoRequeue:
		dc.log.Error(typedErr, "Error syncing item, not requeuing", "item", item)
		requeueTotal.WithLabelValues(gvrKey, "no_requeue").Inc()
		dc.queue.Forget(item)
	case *requeue.RequeueNeeded:
		dc.log.V(1).Info("Requeue needed", "item", item, "error", typedErr)
		requeueTotal.WithLabelValues(gvrKey, "requeue").Inc()
		dc.queue.Add(item) // Add without rate limiting
	case *requeue.RequeueNeededAfter:
		dc.log.V(1).Info("Requeue needed after delay", "item", item, "error", typedErr, "delay", typedErr.Duration())
		requeueTotal.WithLabelValues(gvrKey, "requeue_after").Inc()
		dc.queue.AddAfter(item, typedErr.Duration())
	default:
		// Arriving here means we have an unexpected error, we should requeue the item
		// with rate limiting.
		requeueTotal.WithLabelValues(gvrKey, "rate_limited").Inc()
		if dc.queue.NumRequeues(item) < dc.config.QueueMaxRetries {
			dc.log.Error(err, "Error syncing item, requeuing with rate limit", "item", item)
			dc.queue.AddRateLimited(item)
		} else {
			dc.log.Error(err, "Dropping item from queue after max retries", "item", item)
			dc.queue.Forget(item)
		}
	}

	return true
}

// syncFunc reconciles a single item.
func (dc *DynamicController) syncFunc(ctx context.Context, oi ObjectIdentifiers) error {
	gvrKey := fmt.Sprintf("%s/%s/%s", oi.GVR.Group, oi.GVR.Version, oi.GVR.Resource)
	dc.log.V(1).Info("Syncing object", "gvr", gvrKey, "namespacedKey", oi.NamespacedName)

	startTime := time.Now()
	defer func() {
		duration := time.Since(startTime)
		reconcileDuration.WithLabelValues(gvrKey).Observe(duration.Seconds())
		reconcileTotal.WithLabelValues(gvrKey).Inc()
		dc.log.V(1).Info("Finished syncing object",
			"gvr", gvrKey,
			"namespacedKey", oi.NamespacedName,
			"duration", duration)
	}()

	genericHandler, ok := dc.handlers.Load(oi.GVR)
	if !ok {
		// NOTE(a-hilaly): this might mean that the GVR is not registered, or the workflow operator
		// is not found. We should probably handle this in a better way.
		return fmt.Errorf("no handler found for GVR: %s", gvrKey)
	}

	// this is worth a panic if it fails...
	handlerFunc, ok := genericHandler.(Handler)
	if !ok {
		return fmt.Errorf("invalid handler type for GVR: %s", gvrKey)
	}

	err := handlerFunc(ctx, ctrl.Request{NamespacedName: oi.NamespacedName})
	if err != nil {
		handlerErrorsTotal.WithLabelValues(gvrKey).Inc()
	}
	return err
}

// shutdown performs a graceful shutdown of the controller.
func (dc *DynamicController) shutdown(ctx context.Context) error {
	dc.log.Info("Starting graceful shutdown")

	var wg sync.WaitGroup
	dc.informers.Range(func(key, value interface{}) bool {
		k := key.(schema.GroupVersionResource)
		dc.log.V(1).Info("Shutting down informer", "gvr", k.String())
		wg.Add(1)
		go func(informer *informerWrapper) {
			defer wg.Done()
			informer.informer.Shutdown()
		}(value.(*informerWrapper))
		return true
	})

	// Wait for all informers to shut down or timeout
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		dc.log.Info("All informers shut down successfully")
	case <-ctx.Done():
		dc.log.Error(ctx.Err(), "Timeout waiting for informers to shut down")
		return ctx.Err()
	}

	return nil
}

// ObjectIdentifiers is a struct that holds the namespaced key and the GVR of the object.
//
// Since we are handling all the resources using the same handlerFunc, we need to know
// what GVR we're dealing with - so that we can use the appropriate workflow operator.
type ObjectIdentifiers struct {
	// NamespacedName is the namespaced key of the object.
	NamespacedName types.NamespacedName
	GVR            schema.GroupVersionResource
}

// updateFunc is the update event handler for the GVR informers
func (dc *DynamicController) updateFunc(old, new interface{}) {
	newObj, ok := new.(*unstructured.Unstructured)
	if !ok {
		dc.log.Error(nil, "failed to cast new object to unstructured")
		return
	}
	oldObj, ok := old.(*unstructured.Unstructured)
	if !ok {
		dc.log.Error(nil, "failed to cast old object to unstructured")
		return
	}

	if newObj.GetGeneration() == oldObj.GetGeneration() {
		dc.log.V(2).Info("Skipping update due to unchanged generation",
			"name", newObj.GetName(),
			"namespace", newObj.GetNamespace(),
			"generation", newObj.GetGeneration())
		return
	}

	dc.enqueueObject(new, "update")
}

// enqueueObject adds an object to the workqueue
func (dc *DynamicController) enqueueObject(obj interface{}, eventType string) {
	namespacedKey, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		dc.log.Error(err, "Failed to get key for object", "eventType", eventType)
		return
	}

	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		err := fmt.Errorf("object is not an Unstructured")
		dc.log.Error(err, "Failed to cast object to Unstructured", "eventType", eventType, "namespacedKey", namespacedKey)
		return
	}

	gvk := u.GroupVersionKind()
	gvr := metadata.GVKtoGVR(gvk)

	for _, identifier := range dc.loadHandlerObjectIdentifiers(gvr) {
		objectIdentifiers, ok := identifier(u)
		if ok {
			dc.queue.Add(objectIdentifiers)

			dc.log.V(1).Info("Enqueueing object",
				"objectIdentifiers", objectIdentifiers,
				"eventType", eventType)
		}
	}
}

// Register registers a new GVK to the informers map safely.
func (dc *DynamicController) Register(ctx context.Context, gvr schema.GroupVersionResource, handler Handler, resolvers ...ReconciledInstanceFinderBuilder) error {
	dc.log.V(1).Info("Registering new GVK", "gvr", gvr)
	// Even thought the informer is already registered, we should still
	// update the handler, as it might have changed.
	_, loaded := dc.handlers.Swap(gvr, handler)
	if !loaded {
		// the resource did not have a registered handler before
		// hence there was no handler registered for this kind
		// we should increase the counter
		gvrCount.Inc()
	}
	resolvers = append(resolvers, &selfInstanceFinder{
		log:              dc.log,
		instanceResource: gvr,
	})
	dc.storeHandlerResolvers(gvr, resolvers...)
	for _, resolver := range resolvers {
		err := dc.startGVKInformer(ctx, resolver.WatchedResource())
		if err != nil {
			return err
		}
	}

	dc.log.V(1).Info("Successfully registered GVK", "gvr", gvr)
	return nil
}

// Deregister safely removes a GVK from the controller and cleans up associated resources.
func (dc *DynamicController) Deregister(ctx context.Context, gvr schema.GroupVersionResource) error {
	dc.log.Info("Unregistering GVK", "gvr", gvr)
	// Unregister the handler if any
	_, loaded := dc.handlers.LoadAndDelete(gvr)
	if loaded {
		// the resource actually had a registered handler.
		// it does not any longer, so we should decrement the count.
		gvrCount.Dec()
	}
	// the resolvers map is used to store a reference of all the objects
	// that needs to have informormers.
	// Ensure there is no concurrent edit while stopping unnecessary informers.
	dc.access.Lock()
	defer dc.access.Unlock()
	for informerResource, reconcileRequestMapper := range dc.reconcileRequestMappers {
		if reconcileRequestMapper != nil {
			delete(reconcileRequestMapper, gvr)
		}
		// it there is nothing else that needs to be reconciled for this resource,
		// (i.e. no more RGD or managed object)
		// we should stop the informer.
		if len(reconcileRequestMapper) == 0 {
			dc.log.V(1).Info("Stopping unnecessary informer", "gvr", informerResource)
			err := dc.stopGVKInformer(ctx, informerResource)
			if err != nil {
				dc.log.Error(err, "Failed to stop unnecessary informer", "gvr", informerResource)
				return err
			}
		}
	}
	dc.log.V(1).Info("Successfully unregistered GVK", "gvr", gvr)
	return nil
}

func (dc *DynamicController) storeHandlerResolvers(reconciledResource schema.GroupVersionResource, reconcileRequestMappers ...ReconciledInstanceFinderBuilder) {
	dc.access.Lock()
	defer dc.access.Unlock()
	// replace all existing handlers with the new ones
	for _, resolvers := range dc.reconcileRequestMappers {
		delete(resolvers, reconciledResource)
	}
	if dc.reconcileRequestMappers == nil {
		dc.reconcileRequestMappers = make(map[schema.GroupVersionResource]map[schema.GroupVersionResource]func(obj *unstructured.Unstructured) (types.NamespacedName, bool))
	}
	for _, reconcilerResolver := range reconcileRequestMappers {
		_, ok := dc.reconcileRequestMappers[reconcilerResolver.WatchedResource()]
		if !ok {
			resolvers := make(map[schema.GroupVersionResource]func(obj *unstructured.Unstructured) (types.NamespacedName, bool))
			dc.reconcileRequestMappers[reconcilerResolver.WatchedResource()] = resolvers
		}
		dc.reconcileRequestMappers[reconcilerResolver.WatchedResource()][reconciledResource] = reconcilerResolver.NewFinder(reconciledResource)
	}
}

type identifier func(obj *unstructured.Unstructured) (ObjectIdentifiers, bool)

func (dc *DynamicController) loadHandlerObjectIdentifiers(managedResource schema.GroupVersionResource) []identifier {
	dc.access.Lock()
	defer dc.access.Unlock()
	resolvers := make([]identifier, len(dc.reconcileRequestMappers[managedResource]))
	// make a copy of the resolver map to reduce the lock time
	// and avoid long locks shall enqueing the object take a while.
	i := 0
	for reconciledResource, mapper := range dc.reconcileRequestMappers[managedResource] {
		resolvers[i] = func(obj *unstructured.Unstructured) (ObjectIdentifiers, bool) {
			namespacedName, ok := mapper(obj)
			return ObjectIdentifiers{
				NamespacedName: namespacedName,
				GVR:            reconciledResource,
			}, ok
		}
		i++
	}
	return resolvers
}

// Register registers a new GVK to the informers map safely.
func (dc *DynamicController) startGVKInformer(ctx context.Context, gvr schema.GroupVersionResource) error {
	dc.log.V(1).Info("Starting GVK informer", "gvr", gvr)

	// Create a new informer without starting it.
	// If there was no informer we will start it later
	// otherwise it will be garbage collected
	gvkInformer := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		dc.kubeClient,
		dc.config.ResyncPeriod,
		// Maybe we can make this configurable in the future. Thinking that
		// we might want to filter out some resources, by namespace or labels
		"",
		nil,
	)
	informerContext := context.Background()
	cancelableContext, cancel := context.WithCancel(informerContext)

	genericInformerWrapper, loaded := dc.informers.LoadOrStore(gvr, &informerWrapper{
		informer: gvkInformer,
		shutdown: cancel,
	})
	if loaded {
		// trigger reconciliation of the corresponding gvr's
		objs, err := dc.kubeClient.Resource(gvr).Namespace("").List(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("failed to list objects for GVR %s: %w", gvr, err)
		}
		for _, obj := range objs.Items {
			dc.enqueueObject(&obj, "update")
		}
		return nil
	}

	informerWrapper, ok := genericInformerWrapper.(*informerWrapper)
	if !ok {
		return fmt.Errorf("invalid informer wrapper type for GVR: %s", gvr)
	}

	informer := informerWrapper.informer.ForResource(gvr).Informer()

	// Set up event handlers
	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { dc.enqueueObject(obj, "add") },
		UpdateFunc: dc.updateFunc,
		DeleteFunc: func(obj interface{}) { dc.enqueueObject(obj, "delete") },
	})
	if err != nil {
		dc.log.Error(err, "Failed to add event handler", "gvr", gvr)
		return fmt.Errorf("failed to add event handler for GVR %s: %w", gvr, err)
	}
	if err := informer.SetWatchErrorHandler(func(r *cache.Reflector, err error) {
		dc.log.Error(err, "Watch error", "gvr", gvr)
	}); err != nil {
		dc.log.Error(err, "Failed to set watch error handler", "gvr", gvr)
		return fmt.Errorf("failed to set watch error handler for GVR %s: %w", gvr, err)
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
		return fmt.Errorf("failed to sync informer cache for GVR %s: %w", gvr, err)
	}

	informerCount.Inc()
	dc.log.V(1).Info("Successfully started GVK informer", "gvr", gvr)
	return nil
}

// Deregister safely removes a GVK from the controller and cleans up associated resources.
func (dc *DynamicController) stopGVKInformer(ctx context.Context, gvr schema.GroupVersionResource) error {
	dc.log.Info("Stopping GVK informer", "gvr", gvr)

	// Retrieve the informer
	informerObj, ok := dc.informers.LoadAndDelete(gvr)
	if !ok {
		dc.log.V(1).Info("GVK informer not registered, nothing to stop", "gvr", gvr)
		return nil
	}

	wrapper, ok := informerObj.(*informerWrapper)
	if !ok {
		return fmt.Errorf("invalid informer type for GVR: %s", gvr)
	}

	// Stop the informer
	dc.log.V(1).Info("Stopping informer", "gvr", gvr)

	// Cancel the context to stop the informer
	wrapper.shutdown()
	// Wait for the informer to shut down
	wrapper.informer.Shutdown()

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

func parseNamespacedName(namespacedName string) types.NamespacedName {
	nn := types.NamespacedName{}
	if parts := strings.Split(namespacedName, string(types.Separator)); len(parts) == 1 {
		nn.Name = parts[0]
	} else {
		nn.Namespace = parts[0]
		nn.Name = parts[1]
	}
	return nn
}
