package k8sfakeutils

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	k8s_api_meta "k8s.io/apimachinery/pkg/api/meta"
	k8s_api_metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8s_api_fields "k8s.io/apimachinery/pkg/fields"
	k8s_api_runtime "k8s.io/apimachinery/pkg/runtime"
	k8s_api_runtime_schema "k8s.io/apimachinery/pkg/runtime/schema"
	k8s_api_selection "k8s.io/apimachinery/pkg/selection"
	k8s_api_watch "k8s.io/apimachinery/pkg/watch"
	k8s_go_testing "k8s.io/client-go/testing"
	k8s_go_tools_cache "k8s.io/client-go/tools/cache"
	k8s_ctr_cache "sigs.k8s.io/controller-runtime/pkg/cache"
	k8s_ctr_client "sigs.k8s.io/controller-runtime/pkg/client"
	k8s_ctr_client_apiutil "sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	k8s_ctr_client_fake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	k8s_ctr_client_interceptor "sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// K8s Library architecture Refresher:
//   - Client - provides Listers and Watches (operate directly on API)
//   - Cache - provides Informers and Indexers (super set of Listers and Watches, that operate out of cache, with periodic syncs)

// FakeClientCache is a fake client & cache combined interface.
//   - It is used to provide a fake client and a fake cache.
//   - It is used to test controllers by replacing the real client and cache with a fake one.
type FakeClientCache struct {
	k8s_ctr_client.Client

	logger *zap.Logger
	store  k8s_go_testing.ObjectTracker
	scheme *k8s_api_runtime.Scheme
	mapper k8s_api_meta.RESTMapper
	trace  bool

	stopSigRaise func()
	stopSig      <-chan struct{}

	stopAwaitablesM sync.Mutex
	stopAwaitables  []<-chan struct{}

	// Field indexers for supporting MatchingFields in List operations
	indexersM sync.RWMutex
	indexers  map[indexKey]k8s_ctr_client.IndexerFunc
}

type indexKey struct {
	gvk   k8s_api_runtime_schema.GroupVersionKind
	field string
}

// FakeClientCacheParams is the set of parameters for the FakeClientCache.
type FakeClientCacheParams struct {
	Logger       *zap.Logger
	Store        k8s_go_testing.ObjectTracker
	Scheme       *k8s_api_runtime.Scheme
	Mapper       k8s_api_meta.RESTMapper
	Interceptors *k8s_ctr_client_interceptor.Funcs
	Trace        bool
}

var _ k8s_ctr_cache.Cache = (*FakeClientCache)(nil)

// NewFakeClientCache creates a new fake client & cache combined interface.
// Note: Technically mapper is not needed, but to ensure consistency between
// the fake client and the fake cache and fake controller manager, we pass it in here.
func NewFakeClientCache(
	ctx context.Context,
	params FakeClientCacheParams,
) *FakeClientCache {
	ctx, ctxCancel := context.WithCancel(ctx)

	clientBuilder := k8s_ctr_client_fake.
		NewClientBuilder().
		WithScheme(params.Scheme).
		WithObjectTracker(params.Store).
		WithRESTMapper(params.Mapper)

	if params.Interceptors != nil {
		clientBuilder.WithInterceptorFuncs(*params.Interceptors)
	}
	client := clientBuilder.Build()

	return &FakeClientCache{
		Client:       client,
		logger:       params.Logger,
		store:        params.Store,
		scheme:       params.Scheme,
		mapper:       params.Mapper,
		trace:        params.Trace,
		stopSigRaise: ctxCancel,
		stopSig:      ctx.Done(),
		indexers:     make(map[indexKey]k8s_ctr_client.IndexerFunc),
	}
}

// addStopAwaitable is a helper methodo for dependent informers to add themselves to the stop list.
// The [ch] here is like a future that can be awaited on till the stop signal is raised.
// It is assumed that all dependent informers will be using [stopSig] as means of cancellation.
func (c *FakeClientCache) addStopAwaitable(ch <-chan struct{}) bool {
	c.stopAwaitablesM.Lock()
	defer c.stopAwaitablesM.Unlock()

	select {
	case <-c.stopSig:
		return false
	default:
		c.stopAwaitables = append(c.stopAwaitables, ch)
		return true
	}
}

// GetStore provides a way to get the store of the fake client cache.
func (c *FakeClientCache) GetStore() k8s_go_testing.ObjectTracker {
	return c.store
}

// List overrides the embedded client's List to support field indexing.
func (c *FakeClientCache) List(ctx context.Context, list k8s_ctr_client.ObjectList, opts ...k8s_ctr_client.ListOption) error {
	// Get the GVK for filtering
	gvk, err := k8s_ctr_client_apiutil.GVKForObject(list, c.scheme)
	if err != nil {
		return fmt.Errorf("failed to get GVK for list: %w", err)
	}

	// Extract list options to check for field selectors
	listOpts := &k8s_ctr_client.ListOptions{}
	for _, opt := range opts {
		opt.ApplyToList(listOpts)
	}

	// Check if we have field selectors and validate early
	hasFieldSelector := listOpts.FieldSelector != nil && len(listOpts.FieldSelector.Requirements()) > 0
	var fieldKey, fieldVal string
	if hasFieldSelector {
		// Validate that the field selector is in the supported form (k=v or k==v)
		var err error
		fieldKey, fieldVal, err = requiresExactMatch(listOpts.FieldSelector)
		if err != nil {
			return err
		}

		// If we have field selectors, strip them out since the fake client doesn't support field indexing
		// Clear the field selector and call List with the modified options
		listOpts.FieldSelector = nil
	}
	// Call the underlying client's List (with or without field selectors)
	if err := c.Client.List(ctx, list, listOpts); err != nil {
		return err
	}
	// If no field selectors, return as-is
	if !hasFieldSelector {
		return nil
	}

	return c.filterList(list, gvk, fieldKey, fieldVal)
}

// filterList filters the list items based on the field selector using the registered indexer.
func (c *FakeClientCache) filterList(list k8s_ctr_client.ObjectList, gvk k8s_api_runtime_schema.GroupVersionKind, fieldKey, fieldVal string) error {
	// Remove the "List" suffix to get the item GVK
	itemGVK := gvk
	itemGVK.Kind = strings.TrimSuffix(gvk.Kind, "List")
	if c.trace {
		c.logger.Info("List with field selector", zap.String("listGVK", gvk.String()), zap.String("itemGVK", itemGVK.String()), zap.String("field", fieldKey), zap.String("value", fieldVal))
	}

	// Extract items from the list
	items := mustExtractList(list)

	// Get the indexer for this field
	key := indexKey{gvk: itemGVK, field: fieldKey}
	c.indexersM.RLock()
	indexer, hasIndexer := c.indexers[key]
	c.indexersM.RUnlock()

	if !hasIndexer {
		return fmt.Errorf(
			"List on GroupVersionKind %v specifies selector on field %s, but no index with name %s has been registered for GroupVersionKind %v",
			itemGVK, fieldKey, fieldKey, itemGVK,
		)
	}

	// Filter items based on field selector
	filtered := make([]k8s_api_runtime.Object, 0, len(items))
	for _, item := range items {
		obj := mustCast[k8s_ctr_client.Object](item)

		// Check if any of the indexed values match the required value
		if objMatchesFieldSelector(obj, indexer, fieldVal) {
			filtered = append(filtered, item)
		}
	}

	// Set the filtered items back to the list
	return k8s_api_meta.SetList(list, filtered)
}

// Stop provides a way to stop the fake client cache and all dependent informers.
func (c *FakeClientCache) Stop(ctx context.Context) error {
	c.stopAwaitablesM.Lock()
	defer c.stopAwaitablesM.Unlock()
	c.stopSigRaise()
	for _, ch := range c.stopAwaitables {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		}
	}
	return nil
}

// GetInformer implements [k8s_ctr_cache.Cache].
func (c *FakeClientCache) GetInformer(ctx context.Context, obj k8s_ctr_client.Object, opts ...k8s_ctr_cache.InformerGetOption) (k8s_ctr_cache.Informer, error) {
	gvk, err := k8s_ctr_client_apiutil.GVKForObject(obj, c.scheme)
	if err != nil {
		return nil, err
	}

	return c.GetInformerForKind(ctx, gvk)
}

// GetInformerForKind implements [k8s_ctr_cache.Cache].
func (c *FakeClientCache) GetInformerForKind(ctx context.Context, gvk k8s_api_runtime_schema.GroupVersionKind, opts ...k8s_ctr_cache.InformerGetOption) (k8s_ctr_cache.Informer, error) {
	gvrResourcePlural, _ := k8s_api_meta.UnsafeGuessKindToResource(gvk)
	return &fakeClientCacheInformer{
		store:  c.store,
		gvr:    gvrResourcePlural,
		logger: c.logger,
		parent: c,
		scheme: c.scheme,
	}, nil
}

// RemoveInformer implements [k8s_ctr_cache.Cache].
// The fake does not retain informer instances; there is nothing to tear down.
func (c *FakeClientCache) RemoveInformer(ctx context.Context, obj k8s_ctr_client.Object) error {
	return nil
}

// IndexField implements [k8s_ctr_cache.Cache].
// Registers a field indexer for the given object type and field name.
func (c *FakeClientCache) IndexField(ctx context.Context, obj k8s_ctr_client.Object, field string, extractValue k8s_ctr_client.IndexerFunc) error {
	gvk, err := k8s_ctr_client_apiutil.GVKForObject(obj, c.scheme)
	if err != nil {
		return fmt.Errorf("failed to get GVK for object: %w", err)
	}

	c.indexersM.Lock()
	defer c.indexersM.Unlock()

	key := indexKey{gvk: gvk, field: field}
	c.indexers[key] = extractValue

	if c.trace {
		c.logger.Info("Registered field indexer", zap.String("gvk", gvk.String()), zap.String("field", field), zap.Any("key", key))
	}

	return nil
}

// Start implements [k8s_ctr_cache.Cache].
func (c *FakeClientCache) Start(ctx context.Context) error {
	// No-Op
	return nil
}

// WaitForCacheSync implements [k8s_ctr_cache.Cache].
func (c *FakeClientCache) WaitForCacheSync(ctx context.Context) bool {
	// No-Op
	return true
}

// fakeClientCacheInformer implements [k8s_ctr_cache.Informer].
// Is tied to the fake client.
type fakeClientCacheInformer struct {
	logger *zap.Logger
	parent *FakeClientCache
	scheme *k8s_api_runtime.Scheme

	gvr   k8s_api_runtime_schema.GroupVersionResource
	store k8s_go_testing.ObjectTracker
}

var _ k8s_ctr_cache.Informer = (*fakeClientCacheInformer)(nil)

// fakeResourceEventHandlerRegistration implements [k8s_go_tools_cache.ResourceEventHandlerRegistration].
// controller-runtime's Kind source waits on registration.HasSynced(); a nil registration panics.
type fakeResourceEventHandlerRegistration struct{}

func (fakeResourceEventHandlerRegistration) HasSynced() bool { return true }

func (i *fakeClientCacheInformer) implEventHandler(handler k8s_go_tools_cache.ResourceEventHandler) {
	if i.parent != nil && i.parent.trace {
		i.logger.Info("Adding event handler", zap.String("gvr", i.gvr.String()))
	}

	watch, err := i.store.Watch(i.gvr, k8s_api_metav1.NamespaceAll)
	if err != nil {
		return
	}

	stoppedSig := make(chan struct{})
	i.parent.addStopAwaitable(stoppedSig)
	go func() {
		select {
		case <-i.parent.stopSig:
			watch.Stop()
		}
	}()

	go func() {
		defer close(stoppedSig)
		objCache := make(map[string]k8s_api_runtime.Object)

		for event := range watch.ResultChan() {
			i.logger.Info("Received event", zap.String("gvr", i.gvr.String()), zap.Any("event", event))
			objAccessor := mustGetAccessor(event.Object)
			gvr, err := GVRFromObject(event.Object, i.scheme)
			if err != nil {
				i.logger.Error("Failed to get GVR", zap.Error(err))
				continue
			}
			key := strings.Join([]string{
				gvr.GroupVersion().String(),
				gvr.Resource,
				objAccessor.GetNamespace(),
				objAccessor.GetName(),
			}, "/")
			switch event.Type {
			case k8s_api_watch.Added:
				handler.OnAdd(event.Object, false)
				objCache[key] = event.Object
				i.logger.Info("Added event", zap.String("key", key))
			case k8s_api_watch.Modified:
				handler.OnUpdate(objCache[key], event.Object)
				objCache[key] = event.Object
				i.logger.Info("Updated event", zap.String("key", key))
			case k8s_api_watch.Deleted:
				handler.OnDelete(event.Object)
				delete(objCache, key)
				i.logger.Info("Deleted event", zap.String("key", key))
			}
		}
	}()
}

// AddEventHandler implements [k8s_ctr_cache.Informer].
func (i *fakeClientCacheInformer) AddEventHandler(handler k8s_go_tools_cache.ResourceEventHandler) (k8s_go_tools_cache.ResourceEventHandlerRegistration, error) {
	if i.parent != nil && i.parent.trace {
		i.logger.Info("AddEventHandler", zap.String("gvr", i.gvr.String()), zap.String("handler", fmt.Sprintf("%T", handler)))
	}
	i.implEventHandler(handler)
	return fakeResourceEventHandlerRegistration{}, nil
}

// AddEventHandlerWithResyncPeriod implements [k8s_ctr_cache.Informer].
func (i *fakeClientCacheInformer) AddEventHandlerWithResyncPeriod(handler k8s_go_tools_cache.ResourceEventHandler, resyncPeriod time.Duration) (k8s_go_tools_cache.ResourceEventHandlerRegistration, error) {
	if i.parent != nil && i.parent.trace {
		i.logger.Info("AddEventHandlerWithResyncPeriod", zap.String("gvr", i.gvr.String()), zap.String("handler", fmt.Sprintf("%T", handler)), zap.Duration("resyncPeriod", resyncPeriod))
	}
	i.implEventHandler(handler)
	return fakeResourceEventHandlerRegistration{}, nil
}

// AddEventHandlerWithOptions implements [k8s_ctr_cache.Informer].
func (i *fakeClientCacheInformer) AddEventHandlerWithOptions(handler k8s_go_tools_cache.ResourceEventHandler, options k8s_go_tools_cache.HandlerOptions) (k8s_go_tools_cache.ResourceEventHandlerRegistration, error) {
	resync := time.Duration(0)
	if options.ResyncPeriod != nil {
		resync = *options.ResyncPeriod
	}
	return i.AddEventHandlerWithResyncPeriod(handler, resync)
}

// IsStopped implements [k8s_ctr_cache.Informer].
func (i *fakeClientCacheInformer) IsStopped() bool {
	if i.parent == nil {
		return true
	}
	select {
	case <-i.parent.stopSig:
		return true
	default:
		return false
	}
}

// AddIndexers implements [k8s_ctr_cache.Informer].
func (i *fakeClientCacheInformer) AddIndexers(indexers k8s_go_tools_cache.Indexers) error {
	// No-Op
	return nil
}

// RemoveEventHandler implements [k8s_ctr_cache.Informer].
func (i *fakeClientCacheInformer) RemoveEventHandler(handle k8s_go_tools_cache.ResourceEventHandlerRegistration) error {
	// No-Op
	return nil
}

// HasSynced implements [k8s_ctr_cache.Informer].
func (i *fakeClientCacheInformer) HasSynced() bool {
	// No-Op
	return true
}

// requiresExactMatch checks if the given field selector is of the form `k=v` or `k==v`.
// Returns an error if the selector is not in one of the supported forms.
func requiresExactMatch(sel k8s_api_fields.Selector) (field, val string, err error) {
	reqs := sel.Requirements()
	if len(reqs) != 1 {
		return "", "", fmt.Errorf("field selector %s is not in one of the two supported forms \"key==val\" or \"key=val\"", sel)
	}
	req := reqs[0]
	if req.Operator != k8s_api_selection.Equals && req.Operator != k8s_api_selection.DoubleEquals {
		return "", "", fmt.Errorf("field selector %s is not in one of the two supported forms \"key==val\" or \"key=val\"", sel)
	}
	return req.Field, req.Value, nil
}

// objMatchesFieldSelector checks if the given object matches the field selector value.
func objMatchesFieldSelector(obj k8s_ctr_client.Object, extractIndex k8s_ctr_client.IndexerFunc, val string) bool {
	for _, extractedVal := range extractIndex(obj) {
		if extractedVal == val {
			return true
		}
	}
	return false
}
