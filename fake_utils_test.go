package k8sfakeutils

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	k8s_api_corev1 "k8s.io/api/core/v1"
	k8s_api_metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPanicOnError(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		val := panicOnError(func() (string, error) {
			return "test", nil
		})
		assert.Equal(t, "test", val)
	})
	t.Run("panic", func(t *testing.T) {
		assert.Panics(t, func() {
			panicOnError(func() (struct{}, error) {
				return struct{}{}, fmt.Errorf("test")
			})
		})
	})
}

func TestMustCast(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		obj := "test"
		casted := mustCast[string](obj)
		assert.Equal(t, obj, casted)
	})
	t.Run("panic", func(t *testing.T) {
		obj := 123
		assert.Panics(t, func() {
			mustCast[string](obj)
		})
	})
}

func TestMustGetAccessor(t *testing.T) {
	obj := &k8s_api_metav1.ObjectMeta{
		Name: "test",
	}
	accessor := mustGetAccessor(obj)
	assert.Equal(t, obj, accessor)
}

func TestMustExtractList(t *testing.T) {
	list := &k8s_api_corev1.PodList{
		Items: []k8s_api_corev1.Pod{
			{
				ObjectMeta: k8s_api_metav1.ObjectMeta{
					Name: "test",
				},
			},
		},
	}
	extracted := mustExtractList(list)
	assert.Len(t, extracted, len(list.Items), "extracted list should have same length")
	for i, item := range list.Items {
		// ExtractList returns []runtime.Object, so we need to compare the values
		assert.Equal(t, &item, extracted[i], "item %d should match", i)
	}
}
