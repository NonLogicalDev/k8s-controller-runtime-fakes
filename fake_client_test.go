package k8sfakeutils_test

import (
	"context"
	"testing"
	"time"

	"github.com/NonLogicalDev/k8s-controller-runtime-fakes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	k8s_api_corev1 "k8s.io/api/core/v1"
	k8s_api_metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8s_api_unstructured "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8s_api_fields "k8s.io/apimachinery/pkg/fields"
	k8s_api_runtime_schema "k8s.io/apimachinery/pkg/runtime/schema"
	k8s_go_tools_cache "k8s.io/client-go/tools/cache"
	k8s_ctl_client "sigs.k8s.io/controller-runtime/pkg/client"
)

func TestFakeClientCache_BasicCRUD(t *testing.T) {
	ctx := context.Background()
	logger := zaptest.NewLogger(t)
	scheme := k8sfakeutils.NewCombinedScheme(k8s_api_corev1.AddToScheme)
	store := k8sfakeutils.NewDefaultStore(scheme)
	mapper := k8sfakeutils.NewDefaultRESTMapper(scheme)

	client := k8sfakeutils.NewFakeClientCache(ctx, k8sfakeutils.FakeClientCacheParams{
		Logger: logger,
		Store:  store,
		Scheme: scheme,
		Mapper: mapper,
		Trace:  true,
	})

	// Start the informer (this is a no-op for the fake client cache)
	require.NoError(t, client.Start(ctx))

	pod := &k8s_api_corev1.Pod{
		ObjectMeta: k8s_api_metav1.ObjectMeta{
			Namespace: "default",
			Name:      "test-pod",
		},
	}

	// Create the object
	err := client.Create(ctx, pod)
	require.NoError(t, err, "should create pod without error")

	// Get the object
	got := &k8s_api_corev1.Pod{}
	err = client.Get(ctx, k8s_ctl_client.ObjectKeyFromObject(pod), got)
	assert.NoError(t, err, "should get pod without error")
	assert.Equal(t, pod.Name, got.Name)
	assert.Equal(t, pod.Namespace, got.Namespace)

	// IndexFields
	client.IndexField(ctx, pod, "spec.nodeName", func(o k8s_ctl_client.Object) []string {
		return []string{o.GetName()}
	})
}

func TestFakeClientCache_InformerReceivesEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	logger := zaptest.NewLogger(t)
	scheme := k8sfakeutils.NewCombinedScheme(k8s_api_corev1.AddToScheme)
	store := k8sfakeutils.NewDefaultStore(scheme)
	mapper := k8sfakeutils.NewDefaultRESTMapper(scheme)

	client := k8sfakeutils.NewFakeClientCache(ctx, k8sfakeutils.FakeClientCacheParams{
		Logger: logger,
		Store:  store,
		Scheme: scheme,
		Mapper: mapper,
		Trace:  true,
	})

	defer client.Stop(ctx)

	informer, err := client.GetInformer(ctx, &k8s_api_corev1.Pod{})
	require.NoError(t, err, "should get informer without error")

	type event struct {
		kind string
		obj  string
	}
	eventCh := make(chan event)

	handler := &testEventHandler{
		onAdd: func(obj interface{}) {
			pod, ok := obj.(*k8s_api_corev1.Pod)
			require.True(t, ok, "object should be a Pod")
			eventCh <- event{
				kind: "add",
				obj:  pod.Name,
			}
		},
		onUpdate: func(oldObj, newObj interface{}) {
			pod, ok := newObj.(*k8s_api_corev1.Pod)
			require.True(t, ok, "object should be a Pod")
			eventCh <- event{
				kind: "update",
				obj:  pod.Name,
			}
		},
		onDelete: func(obj interface{}) {
			pod, ok := obj.(*k8s_api_corev1.Pod)
			require.True(t, ok, "object should be a Pod")
			eventCh <- event{
				kind: "delete",
				obj:  pod.Name,
			}
		},
	}
	handlerNoop := &testEventHandler{}

	// No-Ops
	informer.AddEventHandler(handler)
	informer.AddEventHandlerWithResyncPeriod(handlerNoop, 0)
	informer.AddIndexers(k8s_go_tools_cache.Indexers{})
	assert.True(t, informer.HasSynced(), "informer should be synced")

	// Create a pod and expect an event
	pod := &k8s_api_corev1.Pod{
		ObjectMeta: k8s_api_metav1.ObjectMeta{
			Namespace: "default",
			Name:      "event-pod",
		},
	}

	// Create the pod and expect an event.
	err = client.Create(ctx, pod)
	require.NoError(t, err, "should create pod without error")

	select {
	case name := <-eventCh:
		assert.Equal(t, "event-pod", name.obj, "informer should receive add event for pod")
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for informer event")
	}

	// List pods and expect the pod to be in the list.
	pods := &k8s_api_corev1.PodList{}
	err = client.List(ctx, pods)
	require.NoError(t, err, "should list pods without error")
	assert.Len(t, pods.Items, 1, "should have one pod in the list")
	assert.Equal(t, "event-pod", pods.Items[0].Name, "pod should be in the list")

	// Update the pod and expect an event.
	podUpdated := &k8s_api_corev1.Pod{
		ObjectMeta: k8s_api_metav1.ObjectMeta{
			Namespace: "default",
			Name:      "event-pod",
			Annotations: map[string]string{
				"test": "test",
			},
		},
	}

	err = client.Update(ctx, podUpdated)
	require.NoError(t, err, "should update pod without error")

	select {
	case name := <-eventCh:
		assert.Equal(t, "event-pod", name.obj, "informer should receive update event for pod")
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for informer event")
	}

	// Delete the pod and expect an event.
	err = client.Delete(ctx, podUpdated)
	require.NoError(t, err, "should delete pod without error")

	select {
	case name := <-eventCh:
		assert.Equal(t, "event-pod", name.obj, "informer should receive delete event for pod")
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for informer event")
	}

	// List pods and expect none.
	pods = &k8s_api_corev1.PodList{}
	err = client.List(ctx, pods)
	require.NoError(t, err, "should list pods without error")
	assert.Len(t, pods.Items, 0, "should have no pods in the list")

	// Get the store of the fake client cache.
	clientStore := client.GetStore()
	assert.NotNil(t, clientStore, "store should not be nil")

	// Get the pod from the store.
	storePod, storeErr := clientStore.List(k8s_api_runtime_schema.GroupVersionResource{
		Version:  "v1",
		Resource: "pods",
	}, k8s_api_runtime_schema.GroupVersionKind{
		Version: "v1",
		Kind:    "Pod",
	}, "default")
	storePodList := storePod.(*k8s_api_corev1.PodList)
	assert.NoError(t, storeErr, "should get pod without error")
	assert.Len(t, storePodList.Items, 0, "should have one pod in the store")
}

type testEventHandler struct {
	onAdd    func(obj interface{})
	onUpdate func(oldObj, newObj interface{})
	onDelete func(obj interface{})
}

func (h *testEventHandler) OnAdd(obj interface{}, isInInitialList bool) {
	if h.onAdd != nil {
		h.onAdd(obj)
	}
}

func (h *testEventHandler) OnUpdate(oldObj, newObj interface{}) {
	if h.onUpdate != nil {
		h.onUpdate(oldObj, newObj)
	}
}

func (h *testEventHandler) OnDelete(obj interface{}) {
	if h.onDelete != nil {
		h.onDelete(obj)
	}
}

func TestFakeClientCache_FieldIndexing(t *testing.T) {
	ctx := context.Background()
	logger := zaptest.NewLogger(t)
	scheme := k8sfakeutils.NewCombinedScheme(k8s_api_corev1.AddToScheme)
	store := k8sfakeutils.NewDefaultStore(scheme)
	mapper := k8sfakeutils.NewDefaultRESTMapper(scheme)

	client := k8sfakeutils.NewFakeClientCache(ctx, k8sfakeutils.FakeClientCacheParams{
		Logger: logger,
		Store:  store,
		Scheme: scheme,
		Mapper: mapper,
		Trace:  true,
	})

	// Register field index for spec.nodeName
	err := client.IndexField(ctx, &k8s_api_corev1.Pod{}, "spec.nodeName", func(o k8s_ctl_client.Object) []string {
		pod := o.(*k8s_api_corev1.Pod)
		return []string{pod.Spec.NodeName}
	})
	require.NoError(t, err, "should register field index without error")

	// Create pods on different nodes
	pod1 := &k8s_api_corev1.Pod{
		ObjectMeta: k8s_api_metav1.ObjectMeta{
			Namespace: "default",
			Name:      "pod-1",
		},
		Spec: k8s_api_corev1.PodSpec{
			NodeName: "node-a",
		},
	}
	pod2 := &k8s_api_corev1.Pod{
		ObjectMeta: k8s_api_metav1.ObjectMeta{
			Namespace: "default",
			Name:      "pod-2",
		},
		Spec: k8s_api_corev1.PodSpec{
			NodeName: "node-b",
		},
	}
	pod3 := &k8s_api_corev1.Pod{
		ObjectMeta: k8s_api_metav1.ObjectMeta{
			Namespace: "default",
			Name:      "pod-3",
		},
		Spec: k8s_api_corev1.PodSpec{
			NodeName: "node-a",
		},
	}

	require.NoError(t, client.Create(ctx, pod1))
	require.NoError(t, client.Create(ctx, pod2))
	require.NoError(t, client.Create(ctx, pod3))

	// List all pods
	allPods := &k8s_api_corev1.PodList{}
	err = client.List(ctx, allPods)
	require.NoError(t, err)
	assert.Len(t, allPods.Items, 3, "should have 3 pods total")

	// List pods on node-a using field selector
	nodeAPods := &k8s_api_corev1.PodList{}
	err = client.List(ctx, nodeAPods, k8s_ctl_client.MatchingFields{"spec.nodeName": "node-a"})
	require.NoError(t, err, "should list pods with field selector without error")
	assert.Len(t, nodeAPods.Items, 2, "should have 2 pods on node-a")

	podNames := []string{nodeAPods.Items[0].Name, nodeAPods.Items[1].Name}
	assert.Contains(t, podNames, "pod-1")
	assert.Contains(t, podNames, "pod-3")

	// List pods on node-b using field selector
	nodeBPods := &k8s_api_corev1.PodList{}
	err = client.List(ctx, nodeBPods, k8s_ctl_client.MatchingFields{"spec.nodeName": "node-b"})
	require.NoError(t, err, "should list pods with field selector without error")
	assert.Len(t, nodeBPods.Items, 1, "should have 1 pod on node-b")
	assert.Equal(t, "pod-2", nodeBPods.Items[0].Name)

	// Test missing indexer - try to list with a field selector for an unindexed field
	missingIndexerPods := &k8s_api_corev1.PodList{}
	err = client.List(ctx, missingIndexerPods, k8s_ctl_client.MatchingFields{"spec.schedulerName": "default-scheduler"})
	assert.Error(t, err, "should return error when field selector is used without registered indexer")
	assert.Contains(t, err.Error(), "no index with name spec.schedulerName has been registered", "error message should indicate missing indexer")

	// Test bad field selector - multiple requirements
	t.Run("limitation: bad field selector multiple requirements", func(t *testing.T) {
		badSelector := k8s_api_fields.SelectorFromSet(k8s_api_fields.Set{
			"spec.nodeName":      "node-a",
			"spec.schedulerName": "default-scheduler",
		})
		badPods := &k8s_api_corev1.PodList{}
		err := client.List(ctx, badPods, &k8s_ctl_client.ListOptions{FieldSelector: badSelector})
		assert.Error(t, err, "should return error when field selector has multiple requirements")
		assert.Contains(t, err.Error(), "is not in one of the two supported forms", "error message should indicate unsupported field selector format")
	})

	// Test bad field selector - unsupported operator (NotEquals)
	t.Run("limitation: bad field selector unsupported operator", func(t *testing.T) {
		// ParseSelector creates a selector from a string, using != operator which is not supported
		badSelector, err := k8s_api_fields.ParseSelector("spec.nodeName!=node-a")
		require.NoError(t, err, "should parse selector string")
		badPods := &k8s_api_corev1.PodList{}
		err = client.List(ctx, badPods, &k8s_ctl_client.ListOptions{FieldSelector: badSelector})
		assert.Error(t, err, "should return error when field selector uses unsupported operator")
		assert.Contains(t, err.Error(), "is not in one of the two supported forms", "error message should indicate unsupported field selector format")
	})

	// Test filterList with non-list object - passing a Pod directly wrapped as UnstructuredList
	// t.Run("filterList with non-list object", func(t *testing.T) {
	// 	// Register field index - return empty list if not a Pod
	// 	err := client.IndexField(ctx, &k8s_api_corev1.Pod{}, "spec.nodeName", func(o k8s_ctl_client.Object) []string {
	// 		if pod, ok := o.(*k8s_api_corev1.Pod); ok {
	// 			return []string{pod.Spec.NodeName}
	// 		}
	// 		return []string{}
	// 	})
	// 	require.NoError(t, err, "should register field index without error")

	// 	// Create an UnstructuredList without Items field - simulates passing a Pod (non-list) to List
	// 	// This will cause ExtractList to fail because it expects Items to be a slice
	// 	badList := &k8s_api_unstructured.UnstructuredList{}
	// 	badList.SetGroupVersionKind(k8s_api_runtime_schema.GroupVersionKind{
	// 		Group:   "",
	// 		Version: "v1",
	// 		Kind:    "PodList",
	// 	})
	// 	// Set Object without "items" field - ExtractList expects Items to be a slice
	// 	badList.Object = map[string]interface{}{
	// 		"kind":       "PodList",
	// 		"apiVersion": "v1",
	// 		"metadata":   map[string]interface{}{},
	// 		// Intentionally missing "items" field to cause ExtractList to fail
	// 	}

	// 	badSelector := k8s_api_fields.SelectorFromSet(k8s_api_fields.Set{
	// 		"spec.nodeName": "node-a",
	// 	})
	// 	err = client.List(ctx, badList, &k8s_ctl_client.ListOptions{FieldSelector: badSelector})
	// 	// if assert.Error(t, err, "should return error when ExtractList fails on non-list object") {
	// 	// 	assert.Contains(t, err.Error(), "expected pointer, but got invalid kind", "error message should indicate ExtractList failure")
	// 	// }
	// })
}

func TestFakeClientCache_UnstructuredMissingGVK(t *testing.T) {
	ctx := context.Background()
	logger := zaptest.NewLogger(t)
	scheme := k8sfakeutils.NewCombinedScheme(k8s_api_corev1.AddToScheme)
	store := k8sfakeutils.NewDefaultStore(scheme)
	mapper := k8sfakeutils.NewDefaultRESTMapper(scheme)

	client := k8sfakeutils.NewFakeClientCache(ctx, k8sfakeutils.FakeClientCacheParams{
		Logger: logger,
		Store:  store,
		Scheme: scheme,
		Mapper: mapper,
		Trace:  true,
	})

	// Test Unstructured object with missing kind
	t.Run("missing kind", func(t *testing.T) {
		unstructuredObj := &k8s_api_unstructured.Unstructured{}
		// Set version but not kind
		unstructuredObj.SetGroupVersionKind(k8s_api_runtime_schema.GroupVersionKind{
			Group:   "",
			Version: "v1",
			Kind:    "", // Missing kind
		})

		// Test IndexField with missing kind
		err := client.IndexField(ctx, unstructuredObj, "spec.nodeName", func(o k8s_ctl_client.Object) []string {
			return []string{}
		})
		assert.Error(t, err, "should return error when GVK lookup fails due to missing kind")
		assert.Contains(t, err.Error(), "failed to get GVK for object", "error message should indicate GVK lookup failure")

		// Test GetInformer with missing kind
		_, err = client.GetInformer(ctx, unstructuredObj)
		assert.Error(t, err, "should return error when GVK lookup fails due to missing kind")
	})

	// Test Unstructured object with missing version
	t.Run("missing version", func(t *testing.T) {
		unstructuredObj := &k8s_api_unstructured.Unstructured{}
		// Set kind but not version
		unstructuredObj.SetGroupVersionKind(k8s_api_runtime_schema.GroupVersionKind{
			Group:   "",
			Version: "", // Missing version
			Kind:    "Pod",
		})

		// Test IndexField with missing version
		err := client.IndexField(ctx, unstructuredObj, "spec.nodeName", func(o k8s_ctl_client.Object) []string {
			return []string{}
		})
		assert.Error(t, err, "should return error when GVK lookup fails due to missing version")
		assert.Contains(t, err.Error(), "failed to get GVK for object", "error message should indicate GVK lookup failure")

		// Test GetInformer with missing version
		_, err = client.GetInformer(ctx, unstructuredObj)
		assert.Error(t, err, "should return error when GVK lookup fails due to missing version")
	})

	// Test UnstructuredList with missing kind
	t.Run("list missing kind", func(t *testing.T) {
		unstructuredList := &k8s_api_unstructured.UnstructuredList{}
		// Set version but not kind
		unstructuredList.SetGroupVersionKind(k8s_api_runtime_schema.GroupVersionKind{
			Group:   "",
			Version: "v1",
			Kind:    "", // Missing kind
		})

		// Test List with missing kind
		err := client.List(ctx, unstructuredList)
		assert.Error(t, err, "should return error when GVK lookup fails due to missing kind in list")
		assert.Contains(t, err.Error(), "failed to get GVK for list", "error message should indicate GVK lookup failure")
	})

	// Test UnstructuredList with missing version
	t.Run("list missing version", func(t *testing.T) {
		unstructuredList := &k8s_api_unstructured.UnstructuredList{}
		// Set kind but not version
		unstructuredList.SetGroupVersionKind(k8s_api_runtime_schema.GroupVersionKind{
			Group:   "",
			Version: "", // Missing version
			Kind:    "PodList",
		})

		// Test List with missing version
		err := client.List(ctx, unstructuredList)
		assert.Error(t, err, "should return error when GVK lookup fails due to missing version in list")
		assert.Contains(t, err.Error(), "failed to get GVK for list", "error message should indicate GVK lookup failure")
	})
}
