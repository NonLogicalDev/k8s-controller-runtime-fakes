# k8s-controller-runtime-fakes

`k8s-controller-runtime-fakes` is a small Go testing toolkit for Kubernetes controllers.

It provides fake, in-memory implementations of the main `controller-runtime` dependencies so you can test reconcile behavior without a real API server, envtest, or external side effects.

## What this package gives you

- `FakeClientCache`: a combined fake `k8s_ctl_client.Client` + `k8s_ctl_cache.Cache`
- `FakeControllerManager`: a fake `k8s_ctl_manager.Manager` that runs controllers in tests
- `FakeClusterCtr`: a convenience wrapper that wires scheme, mapper, store, client/cache, and manager together
- Utility helpers for schemes, object trackers, GVK/GVR resolution, and leak-aware cleanup

## Why use it

- Fast tests: fully in-memory, no cluster bootstrapping
- Deterministic lifecycle: explicit start/stop signaling
- Realistic controller wiring: watches, handlers, predicates, reconcile flows
- Practical assertions: wait for controller startup, then assert reconcile requests from object events

## What about controller-runtime/pkg/client/fake?

The existing [`sigs.k8s.io/controller-runtime/pkg/client/fake`](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/client/fake) package provides a fake `Client` for basic CRUD operations, but doesn't include the additional components needed to test real controller behavior.

### What's missing from the upstream fake

To use all functionality of `controller-runtime`, you need more than just a fake client:

1. **Cache implementation** – controllers rely on informer-backed caches to watch resources
2. **Informer implementations** – needed for event subscriptions and watch handlers
3. **Manager implementation** – coordinates controller lifecycle and dependency injection
4. **Event plumbing** – connecting object changes to controller reconcile requests

Without these, you can't:
- Register controllers that use watches
- Test event handler logic (predicates, mappers, filters)
- Assert that controllers receive reconcile requests from object events
- Control controller startup ordering in tests
- Verify end-to-end controller behavior

### What this package adds

`k8s-controller-runtime-fakes` builds on top of `controller-runtime/pkg/client/fake` and extends it with:

- `FakeClientCache`: combines the fake client with an in-memory cache/informer layer
- `FakeControllerManager`: allows you to register and start actual controllers in tests:
  ```go
  ctr, err := k8s_ctl_controller.New("my-controller", fakeManager, k8s_ctl_controller.Options{
      Reconciler: k8s_ctl_reconcile.Func(myReconciler.Reconcile),
  })
  ```
- **Event subscription support**: all watches, handlers, and predicates work
- **Fine-grained startup control**: `AwaitControllerStarted(ctx, "controller-name")` ensures controllers are ready before sending events
- **Realistic reconcile flows**: create/update/delete objects → events → handlers → reconcile requests
- **Custom object store support**: you can supply your own Kubernetes object store (e.g., from [`k8s.io/client-go/testing`](https://pkg.go.dev/k8s.io/client-go/testing)), making it compatible with the standard Kubernetes fake clientset for scenarios where you need both controller-runtime and client-go fakes sharing the same underlying state

#### Example: Using with k8s.io/client-go fake clientset

If you need to test code that uses both `controller-runtime` clients and `client-go` clients against the same fake cluster state:

```go
import (
    "testing"

    k8s_api_corev1 "k8s.io/api/core/v1"
    k8s_api_metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    k8s_client_fake "k8s.io/client-go/kubernetes/fake"
    k8s_ctl_client "sigs.k8s.io/controller-runtime/pkg/client"

    "github.com/NonLogicalDev/k8s-controller-runtime-fakes"
)

func TestWithSharedFakeStore(t *testing.T) {
    // Create a standard k8s fake clientset
    k8sClientset := k8s_client_fake.NewSimpleClientset()

    // Get the underlying object tracker/store
    tracker := k8sClientset.Tracker()

    // Create k8sfakeutils components using the shared tracker
    scheme := k8sfakeutils.NewCombinedScheme(/* your schemes */)

    // Use NewFakeClientCacheWithStore to provide the custom tracker
    cluster := k8sfakeutils.NewFakeClusterCtr(k8sfakeutils.FakeClusterCtrParams{
        Scheme: scheme,
        Tracker: tracker, // Share the same object store
        // ... other params
    })

    // Now both k8sClientset (client-go) and cluster.Client (controller-runtime)
    // operate on the same in-memory object store

    // Create with client-go
    pod := &k8s_api_corev1.Pod{/* ... */}
    _, err := k8sClientset.CoreV1().Pods("default").Create(ctx, pod, k8s_api_metav1.CreateOptions{})

    // Read with controller-runtime
    var readPod k8s_api_corev1.Pod
    err = cluster.Client.Get(ctx, k8s_ctl_client.ObjectKeyFromObject(pod), &readPod)
    // readPod will contain the same pod created via client-go
}
```

## Test style (recommended)

This package works best with the following style:

- Build test fixtures with helper constructors
- Use table-driven subtests (`t.Run`) for behavior matrices
- Start manager/client in goroutines and synchronize on `AwaitControllerStarted`
- Trigger events via normal CRUD on fake client objects
- Assert reconcile requests received on channels
- Always tear down cleanly (`context` cancellation + `Stop`) and check goroutine leaks

## Quick start

```go
package mycontroller_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	k8s_api_corev1 "k8s.io/api/core/v1"
	k8s_api_metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8s_ctl_client "sigs.k8s.io/controller-runtime/pkg/client"
	k8s_ctl_controller "sigs.k8s.io/controller-runtime/pkg/controller"
	k8s_ctl_reconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	// Replace with your module path.
	"github.com/your-org/k8sfakeutils"
)

func TestController_ReconcilesPodCreate(t *testing.T) {
	k8sfakeutils.TestAssertNoLeakyK8SGoroutines(t, 10*time.Second)

	ctx, ctxCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer ctxCancel()

	scheme := k8sfakeutils.NewCombinedScheme(k8s_api_corev1.AddToScheme)
	cluster := k8sfakeutils.NewDefaultFakeClusterCtr(ctx, k8sfakeutils.DefaultFakeClusterCtrParams{
		Logger: zaptest.NewLogger(t),
		Scheme: scheme,
		Trace:  false,
	})

	reconcileRequestCh := make(chan k8s_ctl_reconcile.Request, 10)
	_, err := k8s_ctl_controller.New("demo-controller", cluster.Manager, k8s_ctl_controller.Options{
		Reconciler: k8s_ctl_reconcile.Func(func(ctx context.Context, req k8s_ctl_reconcile.Request) (k8s_ctl_reconcile.Result, error) {
			reconcileRequestCh <- req
			return k8s_ctl_reconcile.Result{}, nil
		}),
	})
	require.NoError(t, err)

	go func() { _ = cluster.Start(ctx) }()
	require.NoError(t, cluster.Manager.AwaitControllerStarted(ctx, "demo-controller"))

	pod := &k8s_api_corev1.Pod{ObjectMeta: k8s_api_metav1.ObjectMeta{Namespace: "default", Name: "sample-pod"}}
	require.NoError(t, cluster.Client.Create(ctx, pod))

	select {
	case req := <-reconcileRequestCh:
		require.Equal(t, k8s_ctl_client.ObjectKeyFromObject(pod), req.NamespacedName)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for reconcile request")
	}
}
```

## Running tests

From the project root:

```bash
go test ./...
```

Useful during iteration:

```bash
go test ./... -run TestFakeClientCache
```

## Notes

- This package is intentionally focused on single-process unit/integration-style controller tests.
- If you need full API-server semantics (admission, RBAC, full integration), pair this with broader integration tests.
