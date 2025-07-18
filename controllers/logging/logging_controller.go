// Copyright © 2019 Banzai Cloud
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controllers

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"

	"emperror.dev/errors"
	"github.com/cisco-open/operator-tools/pkg/reconciler"
	"github.com/cisco-open/operator-tools/pkg/secret"
	"github.com/cisco-open/operator-tools/pkg/utils"
	"github.com/go-logr/logr"
	v1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/kube-logging/logging-operator/pkg/resources"
	"github.com/kube-logging/logging-operator/pkg/resources/fluentbit"
	"github.com/kube-logging/logging-operator/pkg/resources/fluentd"
	"github.com/kube-logging/logging-operator/pkg/resources/loggingdataprovider"
	"github.com/kube-logging/logging-operator/pkg/resources/model"
	"github.com/kube-logging/logging-operator/pkg/resources/syslogng"
	"github.com/kube-logging/logging-operator/pkg/sdk/logging/model/render"
	syslogngconfig "github.com/kube-logging/logging-operator/pkg/sdk/logging/model/syslogng/config"
	loggingmodeltypes "github.com/kube-logging/logging-operator/pkg/sdk/logging/model/types"

	loggingv1beta1 "github.com/kube-logging/logging-operator/pkg/sdk/logging/api/v1beta1"
)

const (
	SyslogNGConfigFinalizer = "syslogngconfig.logging.banzaicloud.io/finalizer"
	FluentdConfigFinalizer  = "fluentdconfig.logging.banzaicloud.io/finalizer"
)

var fluentbitWarning sync.Once
var promCrdWarning sync.Once

func init() {
	fluentbitWarning = sync.Once{}
	promCrdWarning = sync.Once{}
}

// NewLoggingReconciler returns a new LoggingReconciler instance
func NewLoggingReconciler(client client.Client, eventRecorder record.EventRecorder, log logr.Logger) *LoggingReconciler {
	return &LoggingReconciler{
		Client:        client,
		EventRecorder: eventRecorder,
		Log:           log,
	}
}

// LoggingReconciler reconciles a Logging object
type LoggingReconciler struct {
	client.Client
	EventRecorder record.EventRecorder
	Log           logr.Logger
}

// +kubebuilder:rbac:groups=logging.banzaicloud.io,resources=loggings;fluentbitagents;flows;clusterflows;outputs;clusteroutputs;fluentdconfigs;syslogngconfigs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=logging.banzaicloud.io,resources=loggings/status;fluentbitagents/status;flows/status;clusterflows/status;outputs/status;clusteroutputs/status;fluentdconfigs/status;syslogngconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=logging.banzaicloud.io,resources=syslogngflows;syslogngclusterflows;syslogngoutputs;syslogngclusteroutputs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=logging.banzaicloud.io,resources=syslogngflows/status;syslogngclusterflows/status;syslogngoutputs/status;syslogngclusteroutputs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=logging.banzaicloud.io,resources=loggings/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=configmaps;secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=extensions;apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=extensions;networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=extensions;policy,resources=podsecuritypolicies,verbs=get;list;watch;create;update;patch;delete;use
// +kubebuilder:rbac:groups=apps,resources=statefulsets;daemonsets;replicasets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;persistentvolumeclaims;serviceaccounts;pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=nodes;namespaces;endpoints;nodes/proxy,verbs=get;list;watch
// +kubebuilder:rbac:groups="";events.k8s.io,resources=events,verbs=create;get;list;watch
// +kubebuilder:rbac:groups="rbac.authorization.k8s.io",resources=clusterroles;clusterrolebindings;roles;rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=prometheusrules;servicemonitors,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=*
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=security.openshift.io,resources=securitycontextconstraints,resourceNames=anyuid;privileged,verbs=use

// Reconcile logging resources
func (r *LoggingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("logging", req.Name)

	log.V(1).Info("reconciling")

	var logging loggingv1beta1.Logging
	if err := r.Get(ctx, req.NamespacedName, &logging); err != nil {
		// If object is not found, return without error.
		// Created objects are automatically garbage collected.
		// For additional cleanup logic use finalizers.
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	var missingCRDs []string

	if err := r.List(ctx, &v1.ServiceMonitorList{}); err == nil {
		//nolint:staticcheck
		ctx = context.WithValue(ctx, resources.ServiceMonitorKey, true)
	} else {
		missingCRDs = append(missingCRDs, "ServiceMonitor")
	}

	if err := r.List(ctx, &v1.PrometheusRuleList{}); err == nil {
		//nolint:staticcheck
		ctx = context.WithValue(ctx, resources.PrometheusRuleKey, true)
	} else {
		missingCRDs = append(missingCRDs, "PrometheusRule")
	}

	if len(missingCRDs) > 0 {
		promCrdWarning.Do(func() {
			log.Info(fmt.Sprintf("WARNING Prometheus Operator CRDs (%s) are not supported in the cluster", strings.Join(missingCRDs, ",")))
		})
	}

	if err := logging.SetDefaults(); err != nil {
		return reconcile.Result{}, err
	}
	reconcilerOpts := reconciler.ReconcilerOpts{
		RecreateErrorMessageCondition:                reconciler.MatchImmutableErrorMessages,
		EnableRecreateWorkloadOnImmutableFieldChange: logging.Spec.EnableRecreateWorkloadOnImmutableFieldChange,
		EnableRecreateWorkloadOnImmutableFieldChangeHelp: "Object has to be recreated, but refusing to remove without explicitly being told so. " +
			"Use logging.spec.enableRecreateWorkloadOnImmutableFieldChange to move on but make sure to understand the consequences. " +
			"As of fluentd, to avoid data loss, make sure to use a persistent volume for buffers, which is the default, unless explicitly disabled or configured differently. " +
			"As of fluent-bit, to avoid duplicated logs, make sure to configure a hostPath volume for the positions through `logging.spec.fluentbit.spec.positiondb`. ",
	}

	loggingResourceRepo := model.NewLoggingResourceRepository(r.Client, log)

	loggingResources, err := loggingResourceRepo.LoggingResourcesFor(ctx, logging)
	if err != nil {
		return reconcile.Result{}, errors.WrapIfWithDetails(err, "failed to get logging resources", "logging", logging)
	}

	_, syslogNGSPec := loggingResources.GetSyslogNGSpec()
	r.dynamicDefaults(ctx, log, syslogNGSPec)

	// metrics
	defer func() {
		stateMetrics, problemsMetrics := getResourceStateMetrics(log)
		// reseting the vectors should remove all orphaned metrics
		stateMetrics.Reset()
		problemsMetrics.Reset()
		for _, ob := range loggingResources.Fluentd.Flows {
			updateResourceStateMetrics(&ob, utils.PointerToBool(ob.Status.Active), ob.Status.ProblemsCount, stateMetrics, problemsMetrics)
		}
		for _, ob := range loggingResources.Fluentd.ClusterFlows {
			updateResourceStateMetrics(&ob, utils.PointerToBool(ob.Status.Active), ob.Status.ProblemsCount, stateMetrics, problemsMetrics)
		}
		for _, ob := range loggingResources.Fluentd.Outputs {
			updateResourceStateMetrics(&ob, utils.PointerToBool(ob.Status.Active), ob.Status.ProblemsCount, stateMetrics, problemsMetrics)
		}
		for _, ob := range loggingResources.Fluentd.ClusterOutputs {
			updateResourceStateMetrics(&ob, utils.PointerToBool(ob.Status.Active), ob.Status.ProblemsCount, stateMetrics, problemsMetrics)
		}
		for _, ob := range loggingResources.SyslogNG.Flows {
			updateResourceStateMetrics(&ob, utils.PointerToBool(ob.Status.Active), ob.Status.ProblemsCount, stateMetrics, problemsMetrics)
		}
		for _, ob := range loggingResources.SyslogNG.ClusterFlows {
			updateResourceStateMetrics(&ob, utils.PointerToBool(ob.Status.Active), ob.Status.ProblemsCount, stateMetrics, problemsMetrics)
		}
		for _, ob := range loggingResources.SyslogNG.Outputs {
			updateResourceStateMetrics(&ob, utils.PointerToBool(ob.Status.Active), ob.Status.ProblemsCount, stateMetrics, problemsMetrics)
		}
		for _, ob := range loggingResources.SyslogNG.ClusterOutputs {
			updateResourceStateMetrics(&ob, utils.PointerToBool(ob.Status.Active), ob.Status.ProblemsCount, stateMetrics, problemsMetrics)
		}
	}()

	reconcilers := []resources.ContextAwareComponentReconciler{
		model.NewValidationReconciler(
			r.Client,
			loggingResources,
			&secretLoaderFactory{
				Client:  r.Client,
				Path:    fluentd.OutputSecretPath,
				Logging: loggingResources.Logging,
			},
			log.WithName("validation"),
		),
	}

	if logging.AreMultipleAggregatorsSet() {
		return ctrl.Result{}, errors.New("fluentd and syslogNG cannot be enabled simultaneously")
	}

	var loggingDataProvider loggingdataprovider.LoggingDataProvider

	fluentdExternal, fluentdSpec := loggingResources.GetFluentd()
	if fluentdSpec != nil {
		logging.AggregatorLevelConfigCheck(fluentdSpec.ConfigCheck)
		fluentdConfig, secretList, err := r.clusterConfigurationFluentd(loggingResources)
		if err != nil {
			// TODO: move config generation into Fluentd reconciler
			reconcilers = append(reconcilers, func(ctx context.Context) (*reconcile.Result, error) {
				return &reconcile.Result{}, err
			})
		} else {
			if os.Getenv("SHOW_FLOW_CONFIG") != "" {
				log.Info("flow configuration", "config", fluentdConfig)
			}

			reconcilers = append(reconcilers, fluentd.New(r.Client, r.Log, &logging, fluentdSpec, fluentdExternal, &fluentdConfig, secretList, reconcilerOpts).Reconcile)
		}
		loggingDataProvider = fluentd.NewDataProvider(r.Client, &logging, fluentdSpec, fluentdExternal)
	}

	syslogNGExternal, syslogNGSpec := loggingResources.GetSyslogNGSpec()
	if syslogNGSpec != nil {
		logging.AggregatorLevelConfigCheck(syslogNGSPec.ConfigCheck)
		syslogNGConfig, secretList, err := r.clusterConfigurationSyslogNG(loggingResources)
		if err != nil {
			// TODO: move config generation into Syslog-NG reconciler
			reconcilers = append(reconcilers, func(ctx context.Context) (*reconcile.Result, error) {
				return &reconcile.Result{}, err
			})
		} else {
			if os.Getenv("SHOW_FLOW_CONFIG") != "" {
				log.Info("flow configuration", "config", syslogNGConfig)
			}

			reconcilers = append(reconcilers, syslogng.New(r.Client, r.Log, &logging, syslogNGSpec, syslogNGExternal, syslogNGConfig, secretList, reconcilerOpts).Reconcile)
		}
		loggingDataProvider = syslogng.NewDataProvider(r.Client, &logging, syslogNGExternal)
	}

	switch len(loggingResources.Fluentbits) {
	case 0:
		// check for legacy definition
		if logging.Spec.FluentbitSpec != nil {
			fluentbitWarning.Do(func() {
				log.Info("WARNING fluentbit definition inside the Logging resource is deprecated and will be removed in the next major release")
			})
			nameProvider := fluentbit.NewLegacyFluentbitNameProvider(&logging)
			reconcilers = append(reconcilers, fluentbit.New(
				r.Client,
				log.WithName("fluentbit-legacy"),
				&logging,
				reconcilerOpts,
				logging.Spec.FluentbitSpec,
				loggingDataProvider,
				nameProvider,
				loggingResourceRepo,
			).Reconcile)
		}
	default:
		if logging.Spec.FluentbitSpec != nil {
			return ctrl.Result{}, errors.New("fluentbit has to be removed from the logging resource before the new FluentbitAgent can be reconciled")
		}
		l := log.WithName("fluentbit")
		for _, f := range loggingResources.Fluentbits {
			f := f
			reconcilers = append(reconcilers, fluentbit.New(
				r.Client,
				l.WithValues("fluentbitagent", f.Name),
				&logging,
				reconcilerOpts,
				&f.Spec,
				loggingDataProvider,
				fluentbit.NewStandaloneFluentbitNameProvider(&f),
				loggingResourceRepo,
			).Reconcile)
		}
	}

	for _, rec := range reconcilers {
		result, err := rec(ctx)
		if err != nil {
			return reconcile.Result{}, err
		}
		if result != nil {
			// short circuit if requested explicitly
			return *result, err
		}
	}

	if shouldReturn, err := r.fluentdConfigFinalizer(ctx, &logging, fluentdExternal); shouldReturn || err != nil {
		return ctrl.Result{}, err
	}

	if shouldReturn, err := r.syslogNGConfigFinalizer(ctx, &logging, syslogNGExternal); shouldReturn || err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *LoggingReconciler) fluentdConfigFinalizer(ctx context.Context, logging *loggingv1beta1.Logging, externalFluentd *loggingv1beta1.FluentdConfig) (bool, error) {
	if logging.DeletionTimestamp.IsZero() {
		if externalFluentd != nil && !controllerutil.ContainsFinalizer(logging, FluentdConfigFinalizer) {
			r.Log.Info("adding fluentdconfig finalizer")
			controllerutil.AddFinalizer(logging, FluentdConfigFinalizer)
			if err := r.Update(ctx, logging); err != nil {
				return true, err
			}
		}
	} else if externalFluentd != nil {
		msg := fmt.Sprintf("refused to delete logging resource while fluentdConfig %s exists", client.ObjectKeyFromObject(externalFluentd))
		r.EventRecorder.Event(logging, corev1.EventTypeWarning, "DeletionRefused", msg)
		return false, errors.New(msg)
	}

	if controllerutil.ContainsFinalizer(logging, FluentdConfigFinalizer) && externalFluentd == nil {
		r.Log.Info("removing fluentdconfig finalizer")
		controllerutil.RemoveFinalizer(logging, FluentdConfigFinalizer)
		if err := r.Update(ctx, logging); err != nil {
			return true, err
		}
	}

	return false, nil
}

func (r *LoggingReconciler) syslogNGConfigFinalizer(ctx context.Context, logging *loggingv1beta1.Logging, externalSyslogNG *loggingv1beta1.SyslogNGConfig) (bool, error) {
	if logging.DeletionTimestamp.IsZero() {
		if externalSyslogNG != nil && !controllerutil.ContainsFinalizer(logging, SyslogNGConfigFinalizer) {
			r.Log.Info("adding syslogngconfig finalizer")
			controllerutil.AddFinalizer(logging, SyslogNGConfigFinalizer)
			if err := r.Update(ctx, logging); err != nil {
				return true, err
			}
		}
	} else if externalSyslogNG != nil {
		msg := fmt.Sprintf("refused to delete logging resource while syslogNGConfig %s exists", client.ObjectKeyFromObject(externalSyslogNG))
		r.EventRecorder.Event(logging, corev1.EventTypeWarning, "DeletionRefused", msg)
		return false, errors.New(msg)
	}

	if controllerutil.ContainsFinalizer(logging, SyslogNGConfigFinalizer) && externalSyslogNG == nil {
		r.Log.Info("removing syslogngconfig finalizer")
		controllerutil.RemoveFinalizer(logging, SyslogNGConfigFinalizer)
		if err := r.Update(ctx, logging); err != nil {
			return true, err
		}
	}

	return false, nil
}

func (r *LoggingReconciler) dynamicDefaults(ctx context.Context, log logr.Logger, syslogNGSpec *loggingv1beta1.SyslogNGSpec) {
	nodes := corev1.NodeList{}
	if err := r.List(ctx, &nodes); err != nil {
		log.Error(err, "listing nodes")
	}
	if syslogNGSpec != nil && syslogNGSpec.MaxConnections == 0 {
		syslogNGSpec.MaxConnections = max(100, min(1000, len(nodes.Items)*10))
	}
}

func updateResourceStateMetrics(obj client.Object, active bool, problemsCount int, statusMetric *prometheus.GaugeVec, problemsMetric *prometheus.GaugeVec) {
	statusMetric.With(prometheus.Labels{"name": obj.GetName(), "namespace": obj.GetNamespace(), "status": "active", "kind": obj.GetObjectKind().GroupVersionKind().Kind}).Set(boolToFloat64(active))
	statusMetric.With(prometheus.Labels{"name": obj.GetName(), "namespace": obj.GetNamespace(), "status": "inactive", "kind": obj.GetObjectKind().GroupVersionKind().Kind}).Set(boolToFloat64(!active))

	problemsMetric.With(prometheus.Labels{"name": obj.GetName(), "namespace": obj.GetNamespace(), "kind": obj.GetObjectKind().GroupVersionKind().Kind}).Set(float64(problemsCount))
}

func getResourceStateMetrics(logger logr.Logger) (stateMetrics *prometheus.GaugeVec, problemsMetrics *prometheus.GaugeVec) {
	var err error

	stateMetrics = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "logging_resource_state"}, []string{"name", "namespace", "status", "kind"})
	stateMetrics, err = getOrRegisterGaugeVec(metrics.Registry, stateMetrics)
	if err != nil {
		logger.Error(err, "couldn't register metrics vector for resource", "metric", stateMetrics)
	}

	problemsMetrics = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "logging_resource_problems"}, []string{"name", "namespace", "kind"})
	problemsMetrics, err = getOrRegisterGaugeVec(metrics.Registry, problemsMetrics)
	if err != nil {
		logger.Error(err, "couldn't register metrics vector for resource", "metric", problemsMetrics)
	}

	return
}

func getOrRegisterGaugeVec(reg prometheus.Registerer, gv *prometheus.GaugeVec) (*prometheus.GaugeVec, error) {
	if err := reg.Register(gv); err != nil {
		if suberr, ok := err.(prometheus.AlreadyRegisteredError); ok {
			if gv, ok := suberr.ExistingCollector.(*prometheus.GaugeVec); ok {
				return gv, nil
			} else {
				return nil, errors.WrapIfWithDetails(suberr, "already registered metric name with different type ", "metric", gv)
			}
		} else {
			return nil, err
		}
	}
	return gv, nil
}

func boolToFloat64(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func (r *LoggingReconciler) clusterConfigurationFluentd(resources model.LoggingResources) (string, *secret.MountSecrets, error) {
	if cfg := resources.Logging.Spec.FlowConfigOverride; cfg != "" {
		return cfg, nil, nil
	}

	slf := secretLoaderFactory{
		Client:  r.Client,
		Path:    fluentd.OutputSecretPath,
		Logging: resources.Logging,
	}

	fluentConfig, err := model.CreateSystem(resources, &slf, r.Log)
	if err != nil {
		return "", nil, errors.WrapIfWithDetails(err, "failed to build model", "logging", resources.Logging)
	}

	output := &bytes.Buffer{}
	renderer := render.FluentRender{
		Out:    output,
		Indent: 2,
	}
	if err := renderer.Render(fluentConfig); err != nil {
		return "", nil, errors.WrapIfWithDetails(err, "failed to render fluentd config", "logging", resources.Logging)
	}

	return output.String(), &slf.Secrets, nil
}

func (r *LoggingReconciler) clusterConfigurationSyslogNG(resources model.LoggingResources) (string, *secret.MountSecrets, error) {
	if cfg := resources.Logging.Spec.FlowConfigOverride; cfg != "" {
		return cfg, nil, nil
	}

	slf := secretLoaderFactory{
		Client:  r.Client,
		Path:    syslogng.OutputSecretPath,
		Logging: resources.Logging,
	}

	_, syslogngSpec := resources.GetSyslogNGSpec()
	in := syslogngconfig.Input{
		Name:                resources.Logging.Name,
		Namespace:           resources.Logging.Namespace,
		ClusterOutputs:      resources.SyslogNG.ClusterOutputs,
		Outputs:             resources.SyslogNG.Outputs,
		ClusterFlows:        resources.SyslogNG.ClusterFlows,
		Flows:               resources.SyslogNG.Flows,
		SecretLoaderFactory: &slf,
		SourcePort:          syslogng.ServicePort,
		SyslogNGSpec:        syslogngSpec,
	}
	var b strings.Builder
	if err := syslogngconfig.RenderConfigInto(in, &b); err != nil {
		return "", nil, errors.WrapIfWithDetails(err, "failed to render syslog-ng config", "logging", resources.Logging)
	}

	return b.String(), &slf.Secrets, nil
}

type SecretLoaderWithLogKeyProvider struct {
	SecretLoader secret.SecretLoader
	Logging      loggingv1beta1.Logging
}

func (s *SecretLoaderWithLogKeyProvider) Load(secret *secret.Secret) (string, error) {
	return s.SecretLoader.Load(secret)
}

func (s *SecretLoaderWithLogKeyProvider) GetLogKey() string {
	if s.Logging.Spec.EnableDockerParserCompatibilityForCRI {
		return "log"
	}
	return loggingmodeltypes.GetLogKey()
}

type secretLoaderFactory struct {
	Client  client.Client
	Secrets secret.MountSecrets
	Path    string
	Logging loggingv1beta1.Logging
}

// Deprecated: use SecretLoaderForNamespace instead
func (f *secretLoaderFactory) OutputSecretLoaderForNamespace(namespace string) secret.SecretLoader {
	return f.SecretLoaderForNamespace(namespace)
}

func (f *secretLoaderFactory) SecretLoaderForNamespace(namespace string) secret.SecretLoader {
	return &SecretLoaderWithLogKeyProvider{
		SecretLoader: secret.NewSecretLoader(f.Client, namespace, f.Path, &f.Secrets),
		Logging:      f.Logging,
	}
}

// SetupLoggingWithManager setup logging manager
func SetupLoggingWithManager(mgr ctrl.Manager, logger logr.Logger) *ctrl.Builder {
	requestMapper := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		// get all the logging resources from the cache
		var loggingList loggingv1beta1.LoggingList
		if err := mgr.GetCache().List(ctx, &loggingList); err != nil {
			logger.Error(err, "failed to list logging resources")
			return nil
		}

		for _, ref := range obj.GetOwnerReferences() {
			refGV, err := schema.ParseGroupVersion(ref.APIVersion)
			if err != nil {
				logger.Error(err, "failed to parse group version", "apiVersion", ref.APIVersion)
				continue
			}

			const (
				FluentbitAgentKind = "FluentbitAgent"
				FluentdConfigKind  = "FluentdConfig"
				SyslogNGConfigKind = "SyslogNGConfig"
			)

			// Check if this is owned by FluentbitAgent, FluentdConfig, or SyslogNGConfig
			// and then map back to the relevant Logging resource
			if refGV.Group == loggingv1beta1.GroupVersion.Group {
				switch ref.Kind {
				case FluentbitAgentKind:
					var agent loggingv1beta1.FluentbitAgent
					if err := mgr.GetClient().Get(ctx, types.NamespacedName{
						Name:      ref.Name,
						Namespace: obj.GetNamespace(),
					}, &agent); err == nil {
						return reconcileRequestsForLoggingRef(loggingList.Items, agent.Spec.LoggingRef)
					}

				case FluentdConfigKind, SyslogNGConfigKind:
					return reconcileRequestsForMatchingControlNamespace(loggingList.Items, obj.GetNamespace())
				}
			}
		}

		switch o := obj.(type) {
		case *loggingv1beta1.ClusterOutput:
			return reconcileRequestsForLoggingRef(loggingList.Items, o.Spec.LoggingRef)
		case *loggingv1beta1.Output:
			return reconcileRequestsForLoggingRef(loggingList.Items, o.Spec.LoggingRef)
		case *loggingv1beta1.Flow:
			return reconcileRequestsForLoggingRef(loggingList.Items, o.Spec.LoggingRef)
		case *loggingv1beta1.ClusterFlow:
			return reconcileRequestsForLoggingRef(loggingList.Items, o.Spec.LoggingRef)
		case *loggingv1beta1.SyslogNGClusterOutput:
			return reconcileRequestsForLoggingRef(loggingList.Items, o.Spec.LoggingRef)
		case *loggingv1beta1.SyslogNGOutput:
			return reconcileRequestsForLoggingRef(loggingList.Items, o.Spec.LoggingRef)
		case *loggingv1beta1.SyslogNGClusterFlow:
			return reconcileRequestsForLoggingRef(loggingList.Items, o.Spec.LoggingRef)
		case *loggingv1beta1.SyslogNGFlow:
			return reconcileRequestsForLoggingRef(loggingList.Items, o.Spec.LoggingRef)
		case *loggingv1beta1.FluentbitAgent:
			return reconcileRequestsForLoggingRef(loggingList.Items, o.Spec.LoggingRef)
		case *loggingv1beta1.LoggingRoute:
			return reconcileRequestsForLoggingRef(loggingList.Items, o.Spec.Source)
		case *loggingv1beta1.FluentdConfig:
			return reconcileRequestsForMatchingControlNamespace(loggingList.Items, o.Namespace)
		case *loggingv1beta1.SyslogNGConfig:
			return reconcileRequestsForMatchingControlNamespace(loggingList.Items, o.Namespace)
		case *corev1.Secret:
			r := regexp.MustCompile(`^logging\.banzaicloud\.io/(.*)`)
			var requestList []reconcile.Request
			for key := range o.Annotations {
				if result := r.FindStringSubmatch(key); len(result) > 1 {
					loggingRef := result[1]
					// When loggingRef is "default" we also trigger for the empty ("") loggingRef as well, because the empty string cannot be used in the annotation, thus "default" refers to the empty case.
					if loggingRef == "default" {
						requestList = append(requestList, reconcileRequestsForLoggingRef(loggingList.Items, "")...)
					}
					requestList = append(requestList, reconcileRequestsForLoggingRef(loggingList.Items, loggingRef)...)
				}
			}
			return requestList
		}
		return nil
	})

	// Trigger reconcile for all logging resources on namespace changes that define a watchNamespaceSelector
	namespaceRequestMapper := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		var loggingList loggingv1beta1.LoggingList
		if err := mgr.GetCache().List(ctx, &loggingList); err != nil {
			logger.Error(err, "failed to list logging resources")
			return nil
		}
		requests := make([]reconcile.Request, 0)
		for _, l := range loggingList.Items {
			if l.Spec.WatchNamespaceSelector != nil {
				requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{
					Name: l.Name,
				}})
			}
		}
		return requests
	})

	builder := ctrl.NewControllerManagedBy(mgr).
		For(&loggingv1beta1.Logging{}).
		Owns(&corev1.Pod{}).
		Watches(&corev1.Namespace{}, namespaceRequestMapper).
		Watches(&loggingv1beta1.ClusterOutput{}, requestMapper).
		Watches(&loggingv1beta1.ClusterFlow{}, requestMapper).
		Watches(&loggingv1beta1.Output{}, requestMapper).
		Watches(&loggingv1beta1.Flow{}, requestMapper).
		Watches(&loggingv1beta1.SyslogNGClusterOutput{}, requestMapper).
		Watches(&loggingv1beta1.SyslogNGClusterFlow{}, requestMapper).
		Watches(&loggingv1beta1.SyslogNGOutput{}, requestMapper).
		Watches(&loggingv1beta1.SyslogNGFlow{}, requestMapper).
		Watches(&corev1.Secret{}, requestMapper).
		Watches(&loggingv1beta1.LoggingRoute{}, requestMapper).
		Watches(&loggingv1beta1.FluentdConfig{}, requestMapper).
		Watches(&loggingv1beta1.SyslogNGConfig{}, requestMapper)

	builder.Watches(&loggingv1beta1.FluentbitAgent{}, requestMapper)

	fluentd.RegisterWatches(builder)
	fluentbit.RegisterWatches(builder)
	syslogng.RegisterWatches(builder)

	return builder
}

func reconcileRequestsForLoggingRef(loggings []loggingv1beta1.Logging, loggingRef string) (reqs []reconcile.Request) {
	for _, l := range loggings {
		if l.Spec.LoggingRef == loggingRef {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: l.Namespace, // this happens to be empty as long as Logging is cluster scoped
					Name:      l.Name,
				},
			})
		}
	}
	return
}

func reconcileRequestsForMatchingControlNamespace(loggings []loggingv1beta1.Logging, ControlNamespace string) (reqs []reconcile.Request) {
	for _, l := range loggings {
		if l.Spec.ControlNamespace == ControlNamespace {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: l.Namespace, // this happens to be empty as long as Logging is cluster scoped
					Name:      l.Name,
				},
			})
		}
	}
	return
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
