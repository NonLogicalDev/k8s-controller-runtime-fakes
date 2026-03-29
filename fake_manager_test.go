package k8sfakeutils_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/NonLogicalDev/k8s-controller-runtime-fakes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sync/errgroup"
	k8s_api_corev1 "k8s.io/api/core/v1"
	k8s_api_metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8s_go_workqueue "k8s.io/client-go/util/workqueue"
	k8s_ctr_client "sigs.k8s.io/controller-runtime/pkg/client"
	k8s_ctr_controller "sigs.k8s.io/controller-runtime/pkg/controller"
	k8s_ctr_event "sigs.k8s.io/controller-runtime/pkg/event"
	k8s_ctr_handler "sigs.k8s.io/controller-runtime/pkg/handler"
	k8s_ctr_manager "sigs.k8s.io/controller-runtime/pkg/manager"
	k8s_ctr_predicate "sigs.k8s.io/controller-runtime/pkg/predicate"
	k8s_ctr_reconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	k8s_ctr_source "sigs.k8s.io/controller-runtime/pkg/source"
)

const _defaultTestAwaitTimeout = 5 * time.Second

func AnExamplePodControllerUnderTest(
	ctx context.Context,
	t *testing.T,
	name string,
	logger *zap.Logger,
	manager k8s_ctr_manager.Manager,
	requestCh chan k8s_ctr_reconcile.Request,
	eventCh chan k8s_ctr_event.GenericEvent,
) (k8s_ctr_controller.Controller, error) {
	// Set up the reconciler.
	// ------------------------------------------------------------

	ctlReconcilerFn := k8s_ctr_reconcile.TypedFunc[k8s_ctr_reconcile.Request](func(ctx context.Context, req k8s_ctr_reconcile.Request) (k8s_ctr_reconcile.Result, error) {
		requestCh <- req
		return k8s_ctr_reconcile.Result{}, nil
	})
	ctl, err := k8s_ctr_controller.New(
		name,
		manager,
		k8s_ctr_controller.Options{
			Reconciler: ctlReconcilerFn,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create controller: %w", err)
	}

	// Set up the controller event watches.
	// ------------------------------------------------------------

	podHandler := &testLoggingTypedEventHandler[*k8s_api_corev1.Pod]{
		logger: logger,
		next:   &k8s_ctr_handler.TypedEnqueueRequestForObject[*k8s_api_corev1.Pod]{},
	}
	objHandler := &testLoggingTypedEventHandler[k8s_ctr_client.Object]{
		logger: logger,
		next:   &k8s_ctr_handler.TypedEnqueueRequestForObject[k8s_ctr_client.Object]{},
	}

	podPred := k8s_ctr_predicate.NewTypedPredicateFuncs(func(o *k8s_api_corev1.Pod) bool { return true })
	objPred := k8s_ctr_predicate.NewTypedPredicateFuncs(func(o k8s_ctr_client.Object) bool { return true })

	if err := ctl.Watch(k8s_ctr_source.Kind(manager.GetCache(), &k8s_api_corev1.Pod{}, podHandler, podPred)); err != nil {
		return nil, fmt.Errorf("watch pods: %w", err)
	}

	if err := ctl.Watch(k8s_ctr_source.Channel(eventCh, objHandler, k8s_ctr_source.WithPredicates[k8s_ctr_client.Object, k8s_ctr_reconcile.Request](objPred))); err != nil {
		return nil, fmt.Errorf("watch channel: %w", err)
	}

	return ctl, nil
}

// ExampleFakeControllerManager is an example of how to use the FakeControllerManager.
func TestExampleFakeControllerManager(t *testing.T) {
	k8sfakeutils.TestAssertNoLeakyK8SGoroutines(t, 10*time.Second)

	// Set up the test wait group.
	// ------------------------------------------------------------

	var wg errgroup.Group

	// Set up the test context.
	// ------------------------------------------------------------

	logger := zaptest.NewLogger(t)
	ctx, ctxCancel := context.WithTimeout(context.Background(), 100000*time.Second)
	defer ctxCancel()

	// Set up the fake controller manager dependencies, and the fake controller manager.
	// ------------------------------------------------------------

	scheme := k8sfakeutils.NewCombinedScheme(k8s_api_corev1.AddToScheme)
	cluster := k8sfakeutils.NewDefaultFakeClusterCtr(ctx, k8sfakeutils.DefaultFakeClusterCtrParams{
		Logger: logger,
		Scheme: scheme,
		Trace:  true,
	})

	// Function to create a controller under test.
	// ------------------------------------------------------------

	ctlReconcilerCh := make(chan k8s_ctr_reconcile.Request)
	ctlEventCh := make(chan k8s_ctr_event.GenericEvent)
	_, err := AnExamplePodControllerUnderTest(ctx, t, "test-controller", logger, cluster.Manager, ctlReconcilerCh, ctlEventCh)
	require.NoError(t, err, "failed to create controller")

	// Kick off the controller.
	// ------------------------------------------------------------

	wg.Go(func() error {
		return cluster.Start(ctx)
	})

	// Wait for the controller to start.
	require.NoError(t, cluster.Manager.AwaitControllerStarted(ctx, "test-controller"))

	// Test body.
	// ------------------------------------------------------------

	t.Run("test 1: Create pod", func(t *testing.T) {
		// Create a pod and wait for the controller/reconciler to process it.
		wg.Go(func() error {
			return cluster.Client.Create(ctx, &k8s_api_corev1.Pod{
				ObjectMeta: k8s_api_metav1.ObjectMeta{
					Namespace: "default",
					Name:      "just-a-pod",
				},
			})
		})

		// Wait for the controller to process the object event.
		select {
		case req := <-ctlReconcilerCh:
			require.Equal(t, "default/just-a-pod", req.NamespacedName.String())
		case <-time.After(_defaultTestAwaitTimeout):
			t.Fatal("timed out waiting for controller to process event")
		}
	})

	t.Run("test 2: Modify pod", func(t *testing.T) {
		// Modify a pod and wait for the controller/reconciler to process it.
		wg.Go(func() error {
			pod := &k8s_api_corev1.Pod{}
			require.NoError(t, cluster.Client.Get(ctx, k8s_ctr_client.ObjectKey{Namespace: "default", Name: "just-a-pod"}, pod))

			pod.Annotations = map[string]string{
				"test": "test",
			}

			return cluster.Client.Update(ctx, pod)
		})

		// Wait for the controller to process the object event.
		select {
		case req := <-ctlReconcilerCh:
			require.Equal(t, "default/just-a-pod", req.NamespacedName.String())
		case <-time.After(_defaultTestAwaitTimeout):
			t.Fatal("timed out waiting for controller to process event")
		}
	})

	t.Run("test 3: Delete pod", func(t *testing.T) {
		// Delete a pod and wait for the controller/reconciler to process it.
		wg.Go(func() error {
			return cluster.Client.Delete(ctx, &k8s_api_corev1.Pod{
				ObjectMeta: k8s_api_metav1.ObjectMeta{
					Namespace: "default",
					Name:      "just-a-pod",
				},
			})
		})

		// Wait for the controller to process the object event.
		select {
		case req := <-ctlReconcilerCh:
			require.Equal(t, "default/just-a-pod", req.NamespacedName.String())
		case <-time.After(_defaultTestAwaitTimeout):
			t.Fatal("timed out waiting for controller to process event")
		}
	})

	// Signal the manager to stop.
	ctxCancel()

	// Wait for the manager to stop.
	require.NoError(t, wg.Wait())
}

func TestFakeControllerManagerMethods(t *testing.T) {
	ctx, ctxCancel := context.WithCancel(context.Background())
	defer ctxCancel()

	obsCore, recorded := observer.New(zap.DebugLevel)
	logger := zap.New(obsCore)
	scheme := k8sfakeutils.NewCombinedScheme(k8s_api_corev1.AddToScheme)

	cluster := k8sfakeutils.NewDefaultFakeClusterCtr(ctx, k8sfakeutils.DefaultFakeClusterCtrParams{
		Logger: logger,
		Scheme: scheme,
		Trace:  true,
	})
	defer cluster.Client.Stop(ctx)

	// Scheme should be same as [fakeCluster.Scheme].
	assert.Equal(t, cluster.Cluster.Scheme, cluster.Manager.GetScheme())

	// Client should be same as [fakeClient].
	assert.Equal(t, cluster.Client, cluster.Manager.GetClient())

	// GetAPIReader should return the [fakeClient].
	assert.Equal(t, cluster.Client, cluster.Manager.GetAPIReader())

	// GetCache should return the [fakeCluster.Cache].
	assert.Equal(t, cluster.Client, cluster.Manager.GetCache())
	assert.Equal(t, cluster.Client, cluster.Manager.GetFieldIndexer())

	// GetRESTMapper should return the [fakeCluster.Mapper].
	assert.Equal(t, cluster.Cluster.Mapper, cluster.Manager.GetRESTMapper())

	// Check Logger by inspecting messages.
	mgrLogger := cluster.Manager.GetLogger()
	mgrLogger.Info("test [info]")
	mgrLogger.Error(errors.New("test error"), "test [error]")
	var mgrLogMsgs []string
	for _, e := range recorded.All() {
		if e.Message == "test [info]" || e.Message == "test [error]" {
			mgrLogMsgs = append(mgrLogMsgs, e.Message)
		}
	}
	assert.Equal(t, []string{"test [info]", "test [error]"}, mgrLogMsgs)

	// NoOps:
	assert.NoError(t, cluster.Manager.AddHealthzCheck("test", nil))
	assert.NoError(t, cluster.Manager.AddMetricsExtraHandler("/metrics", nil))
	assert.NoError(t, cluster.Manager.AddReadyzCheck("test", nil))

	// Config, ControllerOptions should not be nil.
	assert.NotNil(t, cluster.Manager.GetConfig())
	assert.NotNil(t, cluster.Manager.GetControllerOptions())

	// GetWebhookServer should panic as it is not possible to implement it side-effect free.
	assert.Panics(t, func() { cluster.Manager.GetWebhookServer() })

	// GetEventRecorderFor should return the fakeCluster.EventRecorder.
	assert.Equal(t, cluster.Cluster.EventRecorder, cluster.Manager.GetEventRecorderFor("test"))

	// Elected should return a closed channel.
	select {
	case <-cluster.Manager.Elected():
	default:
		t.Fatal("expected closed channel")
	}
}

type testReconciler struct{}

var _ k8s_ctr_reconcile.Reconciler = (*testReconciler)(nil)

// Reconcile implements [k8s_ctr_reconcile.Reconciler].
func (r *testReconciler) Reconcile(ctx context.Context, req k8s_ctr_reconcile.Request) (k8s_ctr_reconcile.Result, error) {
	return k8s_ctr_reconcile.Result{}, nil
}

type testLoggingTypedEventHandler[O k8s_ctr_client.Object] struct {
	logger *zap.Logger
	next   k8s_ctr_handler.TypedEventHandler[O, k8s_ctr_reconcile.Request]
}

var (
	_ k8s_ctr_handler.TypedEventHandler[*k8s_api_corev1.Pod, k8s_ctr_reconcile.Request] = (*testLoggingTypedEventHandler[*k8s_api_corev1.Pod])(nil)
	_ k8s_ctr_handler.TypedEventHandler[k8s_ctr_client.Object, k8s_ctr_reconcile.Request] = (*testLoggingTypedEventHandler[k8s_ctr_client.Object])(nil)
)

func (h *testLoggingTypedEventHandler[O]) Create(ctx context.Context, evt k8s_ctr_event.TypedCreateEvent[O], q k8s_go_workqueue.TypedRateLimitingInterface[k8s_ctr_reconcile.Request]) {
	h.logger.Info("Received Create event", zap.String("event", "Create"), zap.Any("event", evt))
	h.next.Create(ctx, evt, q)
}

func (h *testLoggingTypedEventHandler[O]) Update(ctx context.Context, evt k8s_ctr_event.TypedUpdateEvent[O], q k8s_go_workqueue.TypedRateLimitingInterface[k8s_ctr_reconcile.Request]) {
	h.logger.Info("Received Update event", zap.String("event", "Update"), zap.Any("event", evt))
	h.next.Update(ctx, evt, q)
}

func (h *testLoggingTypedEventHandler[O]) Delete(ctx context.Context, evt k8s_ctr_event.TypedDeleteEvent[O], q k8s_go_workqueue.TypedRateLimitingInterface[k8s_ctr_reconcile.Request]) {
	h.logger.Info("Received Delete event", zap.String("event", "Delete"), zap.Any("event", evt))
	h.next.Delete(ctx, evt, q)
}

func (h *testLoggingTypedEventHandler[O]) Generic(ctx context.Context, evt k8s_ctr_event.TypedGenericEvent[O], q k8s_go_workqueue.TypedRateLimitingInterface[k8s_ctr_reconcile.Request]) {
	h.logger.Info("Received Generic event", zap.String("event", "Generic"), zap.Any("event", evt))
	h.next.Generic(ctx, evt, q)
}
