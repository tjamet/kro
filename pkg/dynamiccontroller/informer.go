package dynamiccontroller

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
)

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

	informerWrapper, loaded := dc.informers.LoadOrStore(gvr, &informerWrapper{
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
	wrapper, ok := dc.informers.LoadAndDelete(gvr)
	if !ok {
		dc.log.V(1).Info("GVK informer not registered, nothing to stop", "gvr", gvr)
		return nil
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
