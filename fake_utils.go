package k8sfakeutils

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/stretchr/testify/require"
	k8s_api_meta "k8s.io/apimachinery/pkg/api/meta"
	k8s_api_metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8s_api_runtime "k8s.io/apimachinery/pkg/runtime"
	k8s_api_runtime_schema "k8s.io/apimachinery/pkg/runtime/schema"
	k8s_go_client_scheme "k8s.io/client-go/kubernetes/scheme"
	k8s_go_testing "k8s.io/client-go/testing"
	k8s_ctr_client "sigs.k8s.io/controller-runtime/pkg/client"
	k8s_ctr_client_apiutil "sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

// TestingTB is a subset of testing.TB that for `require`
type TestingTB interface {
	Errorf(format string, args ...interface{})
	Cleanup(func())
	FailNow()
}

// TestAssertNoLeakyK8SGoroutines is a helper function that asserts that no k8s goroutines are leaking.
// It should be used in tests to ensure that the test does not leak any k8s goroutines.
func TestAssertNoLeakyK8SGoroutines(t TestingTB, maxWait time.Duration) {
	t.Cleanup(func() {
		ctx, ctxCancel := context.WithTimeout(context.Background(), maxWait)
		defer ctxCancel()
		require.NoError(t, AwaitLeakyK8SGoroutines(ctx))
	})
}

// AwaitLeakyK8SGoroutines awaits known leaky goroutines to finish to enable clean test teardown.
func AwaitLeakyK8SGoroutines(ctx context.Context) error {
	return awaitGoroutinesToFinish(ctx, 1*time.Millisecond,
		"k8s.io/client-go/util/workqueue.(*Type).updateUnfinishedWorkLoop",
	)
}

// NewCombinedScheme creates a new scheme that combines the given schemes.
func NewCombinedScheme(fns ...func(*k8s_api_runtime.Scheme) error) *k8s_api_runtime.Scheme {
	scheme := k8s_api_runtime.NewScheme()
	for _, fn := range fns {
		panicOnErrorValue(fn(scheme))
	}
	return scheme
}

// NewDefaultRESTMapper creates a new REST mapper for the given scheme.
func NewDefaultRESTMapper(scheme *k8s_api_runtime.Scheme) k8s_api_meta.RESTMapper {
	mapper := k8s_api_meta.NewDefaultRESTMapper(scheme.PrioritizedVersionsAllGroups())
	return mapper
}

// NewDefaultStore creates a new store for the given scheme.
func NewDefaultStore(scheme *k8s_api_runtime.Scheme) k8s_go_testing.ObjectTracker {
	store := k8s_go_testing.NewObjectTracker(scheme, k8s_go_client_scheme.Codecs.UniversalDecoder())
	return store
}

// GVKFromObject returns the GroupVersionKind for the given object.
func GVKFromObject(obj k8s_api_runtime.Object, scheme *k8s_api_runtime.Scheme) (k8s_api_runtime_schema.GroupVersionKind, error) {
	return k8s_ctr_client_apiutil.GVKForObject(obj, scheme)
}

// GVRFromObject returns the GroupVersionResource for the given object.
func GVRFromObject(obj k8s_api_runtime.Object, scheme *k8s_api_runtime.Scheme) (k8s_api_runtime_schema.GroupVersionResource, error) {
	gvk, err := GVKFromObject(obj, scheme)
	if err != nil {
		return k8s_api_runtime_schema.GroupVersionResource{}, err
	}
	gvr, _ := k8s_api_meta.UnsafeGuessKindToResource(gvk)
	return gvr, nil
}

func awaitGoroutinesToFinish(ctx context.Context, pollInterval time.Duration, names ...string) error {
	if _, ok := ctx.Deadline(); !ok {
		return fmt.Errorf("context does not have a deadline")
	}

	rerun := time.NewTicker(pollInterval)
	defer rerun.Stop()

	// Poll all goroutines until we don't see any specified goroutine names in the stack traces.
	for {
		stacks := stackGetEntries(true)

		noLeaks := true
		for _, stack := range stacks {
			for _, name := range names {
				if strings.Contains(stack, name) {
					noLeaks = false
					break
				}
			}
		}
		if noLeaks {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-rerun.C:
			continue
		}
	}
}

func stackGetEntries(all bool) []string {
	const _defaultBufferSize = 64 * 1024 // 64 KiB

	chunks := []string{}
	for i := _defaultBufferSize; ; i *= 2 {
		buf := make([]byte, i)
		if n := runtime.Stack(buf, all); n < i {
			chunks = append(chunks, string(buf[:n]))
			break
		}
	}
	return strings.Split(strings.Join(chunks, ""), "\n\n")
}

func mustCast[To any](obj interface{}) To {
	outObj, ok := obj.(To)
	if !ok {
		panic(fmt.Errorf("expected object %v to be of type %T, but it's not", obj, outObj))
	}
	return outObj
}

func mustGetAccessor(obj interface{}) k8s_api_metav1.Object {
	return panicOnError(func() (k8s_api_metav1.Object, error) {
		return k8s_api_meta.Accessor(obj)
	})
}

func mustExtractList(obj k8s_ctr_client.ObjectList) []k8s_api_runtime.Object {
	return panicOnError(func() ([]k8s_api_runtime.Object, error) {
		return k8s_api_meta.ExtractList(obj)
	})
}

func panicOnError[V any](fn func() (V, error)) V {
	out, err := fn()
	panicOnErrorValue(err)
	return out
}

func panicOnErrorValue(err error) {
	if err != nil {
		panic(err)
	}
}
