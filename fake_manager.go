package k8sfakeutils

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/sync/errgroup"
	k8s_api_meta "k8s.io/apimachinery/pkg/api/meta"
	k8s_api_runtime "k8s.io/apimachinery/pkg/runtime"
	k8s_go_rest "k8s.io/client-go/rest"
	k8s_go_tools_events "k8s.io/client-go/tools/events"
	k8s_go_tools_record "k8s.io/client-go/tools/record"
	k8s_ctr_cache "sigs.k8s.io/controller-runtime/pkg/cache"
	k8s_ctr_client "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	k8s_ctr_config "sigs.k8s.io/controller-runtime/pkg/config"
	k8s_ctr_healthz "sigs.k8s.io/controller-runtime/pkg/healthz"
	k8s_ctr_manager "sigs.k8s.io/controller-runtime/pkg/manager"
	k8s_ctr_webhook "sigs.k8s.io/controller-runtime/pkg/webhook"
	k8s_ctr_webhook_conversion "sigs.k8s.io/controller-runtime/pkg/webhook/conversion"
)

var _stopContextTimeoutDefault = 5 * time.Second

// FakeCluster is a fake cluster configuration that combines all dependencies of a controller manager.
type FakeCluster struct {
	Scheme        *k8s_api_runtime.Scheme
	Client        k8s_ctr_client.Client
	Cache         k8s_ctr_cache.Cache
	Mapper        k8s_api_meta.RESTMapper
	EventRecorder k8s_go_tools_record.EventRecorder
}

// FakeClusterCtr is a collection of everything needed to run and control a controller manager from tests.
type FakeClusterCtr struct {
	Cluster *FakeCluster
	Client  *FakeClientCache
	Manager *FakeControllerManager

	stopContextTimeout time.Duration
}

// DefaultFakeClusterCtrParams are the parameters for creating a new default fake controller manager cluster.
type DefaultFakeClusterCtrParams struct {
	Logger *zap.Logger
	Scheme *k8s_api_runtime.Scheme
	Trace  bool

	StopContextTimeout time.Duration
}

// NewDefaultFakeClusterCtr creates a new fake controller manager.
func NewDefaultFakeClusterCtr(ctx context.Context, params DefaultFakeClusterCtrParams) FakeClusterCtr {
	mapper := NewDefaultRESTMapper(params.Scheme)
	store := NewDefaultStore(params.Scheme)

	logger := params.Logger
	if !params.Trace {
		logger = zap.NewNop()
	}

	client := NewFakeClientCache(ctx, FakeClientCacheParams{
		Logger: logger,
		Store:  store,
		Scheme: params.Scheme,
		Mapper: mapper,
		Trace:  params.Trace,
	})

	recorder := k8s_go_tools_record.NewFakeRecorder(1024)

	cluster := &FakeCluster{
		Scheme:        params.Scheme,
		Client:        client,
		Cache:         client,
		Mapper:        mapper,
		EventRecorder: recorder,
	}

	if params.StopContextTimeout == 0 {
		params.StopContextTimeout = _stopContextTimeoutDefault
	}

	return FakeClusterCtr{
		Cluster: cluster,
		Client:  client,
		Manager: NewFakeControllerManager(ctx, FakeControllerManagerParams{
			Logger:            params.Logger,
			Cluster:           cluster,
			ControllerOptions: ctrlconfig.Controller{},
			RESTConfig:        &k8s_go_rest.Config{},
			Trace:             params.Trace,
		}),

		stopContextTimeout: params.StopContextTimeout,
	}
}

// Start is a convenience function to start the controller manager and the fake client cache.
// Start blocks until ctx is cancelled. The manager is stopped first so controllers release watches
// before Client.Stop tears down the shared tracker; running those in parallel deadlocked or hung tests.
func (f *FakeClusterCtr) Start(ctx context.Context) error {
	mgrErr := f.Manager.Start(ctx)

	stopCtx, stopCtxCancel := context.WithTimeout(context.Background(), f.stopContextTimeout)
	defer stopCtxCancel()
	stopErr := f.Client.Stop(stopCtx)

	if mgrErr != nil {
		return mgrErr
	}
	return stopErr
}

// FakeControllerManagerParams are the parameters for creating a new fake controller manager.
type FakeControllerManagerParams struct {
	Logger            *zap.Logger
	Cluster           *FakeCluster
	ControllerOptions ctrlconfig.Controller
	RESTConfig        *k8s_go_rest.Config
	Trace             bool
}

// FakeControllerManager is a fake controller manager that can be used to test controllers.
// It is a fake that can be transparently used in place of a real controller manager but is fully
// controllable and encapsulated (no side-effects and no network calls), making it simpler to use in unit tests.
//
// It is best paired with the FakeClientCache to provide the reactive backing store for the manager.
// With such pairing, you can test the signals that controllers are subscribing to, and set-up and teardown
// precise scenarios by creating real objects in the backing store, triggering events, and inspecting the final object state.
//
// For an end to end example of how to use this, see the [fake_manager_test.go] file.
type FakeControllerManager struct {
	logger            *zap.Logger
	cluster           *FakeCluster
	controllerOptions ctrlconfig.Controller
	restConfig        *k8s_go_rest.Config
	trace             bool

	runnablesM sync.Mutex
	runnables  []k8s_ctr_manager.Runnable

	stopSig      <-chan struct{}
	stopSigRaise func()

	mtx                   sync.Mutex
	controllersStarted    map[string]struct{} // protected by mtx
	controllersNewStarted chan struct{}

	converterRegistry k8s_ctr_webhook_conversion.Registry
}

// noopEventsRecorder implements [k8s_go_tools_events.EventRecorder] for tests that do not care about events v1.
type noopEventsRecorder struct{}

func (noopEventsRecorder) Eventf(regarding k8s_api_runtime.Object, related k8s_api_runtime.Object, eventtype, reason, action, note string, args ...interface{}) {
}

var _ k8s_ctr_manager.Manager = (*FakeControllerManager)(nil)

// NewFakeControllerManager creates a new fake controller manager.
func NewFakeControllerManager(
	ctx context.Context,
	params FakeControllerManagerParams,
) *FakeControllerManager {
	ctx, ctxCancel := context.WithCancel(ctx)

	f := &FakeControllerManager{
		cluster:           params.Cluster,
		controllerOptions: params.ControllerOptions,
		restConfig:        params.RESTConfig,
		trace:             params.Trace,

		stopSig:      ctx.Done(),
		stopSigRaise: ctxCancel,

		controllersStarted: make(map[string]struct{}),
		// Buffered channel to serve as a non-blocking signal for notifying when a new controller has started.
		controllersNewStarted: make(chan struct{}, 1),

		converterRegistry: k8s_ctr_webhook_conversion.NewRegistry(),
	}

	// Create a special zap logger core that logs all messages with the name of the controller manager.
	f.logger = params.Logger.WithOptions(zap.WithCaller(true), zap.WrapCore(func(core zapcore.Core) zapcore.Core {
		return &fakeLoggerCore{Core: core, onMessage: f.onMessage}
	}))

	return f
}

func (f *FakeControllerManager) onMessage(message loggerMessage) {
	f.mtx.Lock()
	defer f.mtx.Unlock()

	// HACK: kubernetes-runtime does not directly expose controller readiness.
	// This is the only way to detect when a controller has fully started.
	if strings.Contains(strings.ToLower(message.entry.Message), "starting workers") {
		for _, field := range message.fields {
			if field.Key == "controller" {
				controllerName := field.String
				if controllerName == "" {
					controllerName, _ = field.Interface.(string)
				}

				f.controllersStarted[controllerName] = struct{}{}
				f.onMessageSignalNewController()
				break
			}
		}
	}
}

func (f *FakeControllerManager) onMessageSignalNewController() {
	select {
	case f.controllersNewStarted <- struct{}{}:
	default:
	}
}

// AwaitControllerStarted waits for a controller to start.
//
// This is a function that waits for a controller to start by name.
// Use it to ensure that all controller watches have been setup in tests.
func (f *FakeControllerManager) AwaitControllerStarted(ctx context.Context, controllerName string) error {
	for {
		ok := func() bool {
			f.mtx.Lock()
			defer f.mtx.Unlock()

			_, ok := f.controllersStarted[controllerName]
			return ok
		}()
		if ok {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-f.controllersNewStarted:
		}
	}
}

func (f *FakeControllerManager) injector(obj interface{}) {
	type injectorCache interface {
		InjectCache(k8s_ctr_cache.Cache) error
	}

	type injectorStopChannel interface {
		InjectStopChannel(<-chan struct{}) error
	}

	switch v := obj.(type) {
	case injectorCache:
		if f.trace {
			f.logger.Debug("Injecting cache", zap.String("obj", fmt.Sprintf("%T", obj)))
		}
		v.InjectCache(f.cluster.Cache)
	case injectorStopChannel:
		if f.trace {
			f.logger.Debug("Injecting stop channel", zap.String("obj", fmt.Sprintf("%T", obj)))
		}
		v.InjectStopChannel(f.stopSig)
	}
}

// Add implements [k8s_ctr_manager.Manager].
func (f *FakeControllerManager) Add(obj k8s_ctr_manager.Runnable) error {
	if f.trace {
		f.logger.Debug("Adding Runnable", zap.String("obj", fmt.Sprintf("%T", obj)))
	}
	f.runnablesM.Lock()
	defer f.runnablesM.Unlock()
	f.runnables = append(f.runnables, obj)
	return nil
}

// AddHealthzCheck implements [k8s_ctr_manager.Manager].
func (f *FakeControllerManager) AddHealthzCheck(name string, check k8s_ctr_healthz.Checker) error {
	// No-Op
	if f.trace {
		f.logger.Debug("AddHealthzCheck", zap.String("name", name))
	}
	return nil
}

// AddMetricsExtraHandler implements [k8s_ctr_manager.Manager].
func (f *FakeControllerManager) AddMetricsExtraHandler(path string, handler http.Handler) error {
	// No-Op
	if f.trace {
		f.logger.Debug("AddMetricsExtraHandler", zap.String("path", path))
	}
	return nil
}

// AddMetricsServerExtraHandler implements [k8s_ctr_manager.Manager].
func (f *FakeControllerManager) AddMetricsServerExtraHandler(path string, handler http.Handler) error {
	// No-Op
	if f.trace {
		f.logger.Debug("AddMetricsServerExtraHandler", zap.String("path", path))
	}
	return nil
}

// AddReadyzCheck implements [k8s_ctr_manager.Manager].
func (f *FakeControllerManager) AddReadyzCheck(name string, check k8s_ctr_healthz.Checker) error {
	// No-Op
	if f.trace {
		f.logger.Debug("AddReadyzCheck", zap.String("name", name))
	}
	return nil
}

// Elected implements [k8s_ctr_manager.Manager].
func (f *FakeControllerManager) Elected() <-chan struct{} {
	// Always return elected.
	ch := make(chan struct{})
	close(ch)
	return ch
}

// GetAPIReader implements [k8s_ctr_manager.Manager].
func (f *FakeControllerManager) GetAPIReader() k8s_ctr_client.Reader {
	if f.trace {
		f.logger.Debug("GetAPIReader")
	}
	return f.cluster.Client
}

// GetCache implements [k8s_ctr_manager.Manager].
func (f *FakeControllerManager) GetCache() k8s_ctr_cache.Cache {
	if f.trace {
		f.logger.Debug("GetCache")
	}
	return f.cluster.Cache
}

// GetClient implements [k8s_ctr_manager.Manager].
func (f *FakeControllerManager) GetClient() k8s_ctr_client.Client {
	if f.trace {
		f.logger.Debug("GetClient")
	}
	return f.cluster.Client
}

// GetConfig implements [k8s_ctr_manager.Manager].
func (f *FakeControllerManager) GetConfig() *k8s_go_rest.Config {
	if f.trace {
		f.logger.Debug("GetConfig")
	}
	return f.restConfig
}

// GetControllerOptions implements [k8s_ctr_manager.Manager].
func (f *FakeControllerManager) GetControllerOptions() k8s_ctr_config.Controller {
	if f.trace {
		f.logger.Debug("GetControllerOptions")
	}
	co := f.controllerOptions
	// Controllers must use the manager zap logger (with fakeLoggerCore) so onMessage can observe
	// "Starting workers" and the "controller" field; otherwise DefaultFromConfig uses a nil logr
	// and AwaitControllerStarted never unblocks.
	if co.Logger.GetSink() == nil {
		co.Logger = zapr.NewLogger(f.logger)
	}
	return co
}

// GetConverterRegistry implements [k8s_ctr_manager.Manager].
func (f *FakeControllerManager) GetConverterRegistry() k8s_ctr_webhook_conversion.Registry {
	if f.trace {
		f.logger.Debug("GetConverterRegistry")
	}
	return f.converterRegistry
}

// GetEventRecorderFor implements [k8s_ctr_manager.Manager].
func (f *FakeControllerManager) GetEventRecorderFor(name string) k8s_go_tools_record.EventRecorder {
	if f.trace {
		f.logger.Debug("GetEventRecorderFor", zap.String("name", name))
	}
	return f.cluster.EventRecorder
}

// GetEventRecorder implements [recorder.Provider].
func (f *FakeControllerManager) GetEventRecorder(name string) k8s_go_tools_events.EventRecorder {
	if f.trace {
		f.logger.Debug("GetEventRecorder", zap.String("name", name))
	}
	return noopEventsRecorder{}
}

// GetFieldIndexer implements [k8s_ctr_manager.Manager].
func (f *FakeControllerManager) GetFieldIndexer() k8s_ctr_client.FieldIndexer {
	if f.trace {
		f.logger.Debug("GetFieldIndexer")
	}
	return f.cluster.Cache
}

// GetLogger implements [k8s_ctr_manager.Manager].
func (f *FakeControllerManager) GetLogger() logr.Logger {
	if f.trace {
		f.logger.Debug("GetLogger")
	}
	return zapr.NewLogger(f.logger)
}

// GetRESTMapper implements [k8s_ctr_manager.Manager].
func (f *FakeControllerManager) GetRESTMapper() k8s_api_meta.RESTMapper {
	if f.trace {
		f.logger.Debug("GetRESTMapper")
	}
	return f.cluster.Mapper
}

// GetScheme implements [k8s_ctr_manager.Manager].
func (f *FakeControllerManager) GetScheme() *k8s_api_runtime.Scheme {
	if f.trace {
		f.logger.Info("GetScheme")
	}
	return f.cluster.Scheme
}

// GetWebhookServer implements [k8s_ctr_manager.Manager].
func (f *FakeControllerManager) GetWebhookServer() k8s_ctr_webhook.Server {
	if f.trace {
		f.logger.Debug("GetWebhookServer")
	}
	panic("unimplemented")
}

// GetHTTPClient implements [k8s_ctr_manager.Manager].
func (f *FakeControllerManager) GetHTTPClient() *http.Client {
	if f.trace {
		f.logger.Debug("GetHTTPClient")
	}
	return &http.Client{}
}

// SetFields implements [k8s_ctr_manager.Manager].
func (f *FakeControllerManager) SetFields(obj interface{}) error {
	if f.trace {
		f.logger.Debug("Setting fields", zap.String("obj", fmt.Sprintf("%T", obj)))
	}
	f.injector(obj)
	return nil
}

// Start implements [k8s_ctr_manager.Manager].
func (f *FakeControllerManager) Start(ctx context.Context) error {
	if f.trace {
		f.logger.Debug("Starting")
	}
	wg := errgroup.Group{}
	wg.Go(func() error {
		<-ctx.Done()
		f.stopSigRaise()
		return nil
	})
	for _, r := range f.runnables {
		wg.Go(func() error {
			return r.Start(ctx)
		})
	}
	return wg.Wait()
}

type loggerMessage struct {
	entry  zapcore.Entry
	fields []zapcore.Field
}

type fakeLoggerCore struct {
	zapcore.Core

	fields []zapcore.Field

	onMessage func(message loggerMessage)
}

func (f *fakeLoggerCore) With(fields []zapcore.Field) zapcore.Core {
	return &fakeLoggerCore{Core: f.Core.With(fields), onMessage: f.onMessage, fields: append(f.fields, fields...)}
}

func (f *fakeLoggerCore) Check(entry zapcore.Entry, checkedEntry *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	return f.Core.Check(entry, checkedEntry.AddCore(entry, f))
}

func (f *fakeLoggerCore) Write(entry zapcore.Entry, fields []zapcore.Field) error {
	f.onMessage(loggerMessage{
		entry:  entry,
		fields: append(f.fields, fields...),
	})
	return nil
}

func (f *fakeLoggerCore) Sync() error {
	return f.Core.Sync()
}
