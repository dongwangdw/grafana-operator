package grafanadashboard

import (
	"context"
	defaultErrors "errors"
	"fmt"
	"time"

	grafanav1alpha1 "github.com/integr8ly/grafana-operator/v3/pkg/apis/integreatly/v1alpha1"
	"github.com/integr8ly/grafana-operator/v3/pkg/controller/common"
	"github.com/integr8ly/grafana-operator/v3/pkg/controller/config"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	ControllerName = "controller_grafanadashboard"
)

var log = logf.Log.WithName(ControllerName)

// Add creates a new GrafanaDashboard Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager, namespace string) error {
	return add(mgr, newReconciler(mgr), namespace)
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)

	return &ReconcileGrafanaDashboard{
		client:   mgr.GetClient(),
		config:   config.GetControllerConfig(),
		context:  ctx,
		cancel:   cancel,
		recorder: mgr.GetEventRecorderFor(ControllerName),
		state:    common.ControllerState{},
	}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler, namespace string) error {
	// Create a new controller
	c, err := controller.New("grafanadashboard-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource GrafanaDashboard
	err = c.Watch(&source.Kind{Type: &grafanav1alpha1.GrafanaDashboard{}}, &handler.EnqueueRequestForObject{})
	if err == nil {
		log.Info("Starting dashboard controller")
	}

	ref := r.(*ReconcileGrafanaDashboard)
	ticker := time.NewTicker(config.RequeueDelay)
	sendEmptyRequest := func() {
		request := reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: namespace,
				Name:      "",
			},
		}
		r.Reconcile(request)
	}

	go func() {
		for range ticker.C {
			log.Info("running periodic dashboard resync")
			sendEmptyRequest()
		}
	}()

	go func() {
		for stateChange := range common.ControllerEvents {
			// Controller state updated
			ref.state = stateChange
		}
	}()

	return err
}

var _ reconcile.Reconciler = &ReconcileGrafanaDashboard{}

// ReconcileGrafanaDashboard reconciles a GrafanaDashboard object
type ReconcileGrafanaDashboard struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client   client.Client
	config   *config.ControllerConfig
	context  context.Context
	cancel   context.CancelFunc
	recorder record.EventRecorder
	state    common.ControllerState
}

// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileGrafanaDashboard) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	// If Grafana is not running there is no need to continue
	if r.state.GrafanaReady == false {
		log.V(1).Info("no grafana instance available")
		return reconcile.Result{Requeue: false}, nil
	}

	client, err := r.getClient()
	if err != nil {
		return reconcile.Result{RequeueAfter: config.RequeueDelay}, nil
	}

	// Initial request?
	if request.Name == "" {
		return r.reconcileDashboards(request, client)
	}

	// Check if the label selectors are available yet. If not then the grafana controller
	// has not finished initializing and we can't continue. Reschedule for later.
	if r.state.DashboardSelectors == nil {
		return reconcile.Result{RequeueAfter: config.RequeueDelay}, nil
	}

	// Fetch the GrafanaDashboard instance
	instance := &grafanav1alpha1.GrafanaDashboard{}
	err = r.client.Get(r.context, request.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			// If some dashboard has been deleted, then always re sync the world
			log.Info(fmt.Sprintf("deleting dashboard %v/%v", request.Namespace, request.Name))
			return r.reconcileDashboards(request, client)
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	// If the dashboard does not match the label selectors then we ignore it
	cr := instance.DeepCopy()
	if !r.isMatch(cr) {
		log.V(1).Info("selectors does not match", "namespace", cr.Namespace, "Name", cr.Name)
		return reconcile.Result{}, nil
	}

	// Otherwise always re sync all dashboards in the namespace
	return r.reconcileDashboards(request, client)
}

// check if the labels on a namespace match a given label selector
func (r *ReconcileGrafanaDashboard) checkNamespaceLabels(dashboard *grafanav1alpha1.GrafanaDashboard) (bool, error) {
	key := client.ObjectKey{
		Name: dashboard.Namespace,
	}
	ns := &v1.Namespace{}
	err := r.client.Get(r.context, key, ns)
	if err != nil {
		return false, err
	}
	selector, err := metav1.LabelSelectorAsSelector(r.state.DashboardNamespaceSelector)
	if err != nil {
		return false, err
	}
	return selector.Empty() || selector.Matches(labels.Set(ns.Labels)), nil
}

func (r *ReconcileGrafanaDashboard) reconcileDashboards(request reconcile.Request, grafanaClient GrafanaClient) (reconcile.Result, error) {
	// Collect known and namespace dashboards
	knownDashboards := r.config.GetDashboards(request.Namespace)
	namespaceDashboards := &grafanav1alpha1.GrafanaDashboardList{}

	opts := &client.ListOptions{
		Namespace: request.Namespace,
	}

	err := r.client.List(r.context, namespaceDashboards, opts)
	if err != nil {
		return reconcile.Result{}, err
	}

	// Prepare lists
	var dashboardsToDelete []*grafanav1alpha1.GrafanaDashboardRef

	// Check if a given dashboard (by name) is present in the list of
	// dashboards in the namespace
	inNamespace := func(item *grafanav1alpha1.GrafanaDashboardRef) bool {
		for _, d := range namespaceDashboards.Items {
			if d.Name == item.Name && d.Namespace == item.Namespace {
				return true
			}
		}
		return false
	}

	// Returns the hash of a dashboard if it is known
	findHash := func(item *grafanav1alpha1.GrafanaDashboard) string {
		for _, d := range knownDashboards {
			if item.Name == d.Name && item.Namespace == d.Namespace {
				return d.Hash
			}
		}
		return ""
	}

	// Dashboards to delete: dashboards that are known but not found
	// any longer in the namespace
	for _, dashboard := range knownDashboards {
		if !inNamespace(dashboard) {
			dashboardsToDelete = append(dashboardsToDelete, dashboard)
		}
	}

	// Process new/updated dashboards
	for _, dashboard := range namespaceDashboards.Items {
		// Is this a dashboard we care about (matches the label selectors)?
		if !r.isMatch(&dashboard) {
			log.V(1).Info("dashboard selector does not match", "namespace"
				dashboard.Namespace, "name", dashboard.Name))
			continue
		}

		// Process the dashboard. Use the known hash of an existing dashboard
		// to determine if an update is required
		knownHash := findHash(&dashboard)
		pipeline := NewDashboardPipeline(r.client, &dashboard)
		processed, err := pipeline.ProcessDashboard(knownHash)

		if err != nil {
			log.Error(err, "cannot process dashboard", "namespace", dashboard.Namespace, "name", dashboard.Name))
			r.manageError(&dashboard, err)
			continue
		}

		if processed == nil {
			r.config.SetPluginsFor(&dashboard)
			continue
		}
		// Check labels only when DashboardNamespaceSelector isnt empty
		if r.state.DashboardNamespaceSelector != nil {
			matchesNamespaceLabels, err := r.checkNamespaceLabels(&dashboard)
			if err != nil {
				r.manageError(&dashboard, err)
				continue
			}

			if matchesNamespaceLabels == false {
				log.V(1).Info("dashboard skipped because the namespace labels do not match", "name", dashboard.Name))
				continue
			}
		}

		folder, err := grafanaClient.GetOrCreateNamespaceFolder(dashboard.Namespace)
		if err != nil {
			log.Error(err, "failed to get or create namespace folder", "namespace", dashboard.Namespace, "name", dashboard.Name))
			r.manageError(&dashboard, err)
			continue
		}

		var folderId int64
		if folder.ID == nil {
			folderId = 0
		} else {
			folderId = *folder.ID
		}

		_, err = grafanaClient.CreateOrUpdateDashboard(processed, folderId)
		if err != nil {
			log.Error(err, "cannot submit dashboard %v/%v", dashboard.Namespace, dashboard.Name)
			r.manageError(&dashboard, err)
			continue
		}
		r.manageSuccess(&dashboard)
	}

	for _, dashboard := range dashboardsToDelete {
		status, err := grafanaClient.DeleteDashboardByUID(dashboard.UID)
		if err != nil {
			log.Error(err, "fail deleting dashboard", "UID", dashboard.UID, "Status", *status.Status, "Message", *status.Message)
		}
		log.Info("dashboard deleted", "status", *status.Message))
		r.config.RemovePluginsFor(dashboard.Namespace, dashboard.Name)
		r.config.RemoveDashboard(dashboard.Namespace, dashboard.Name)
	}

	// Mark the dashboards as synced so that the current state can be written
	// to the Grafana CR by the grafana controller
	r.config.AddConfigItem(config.ConfigGrafanaDashboardsSynced, true)
	return reconcile.Result{Requeue: false}, nil
}

// Handle success case: update dashboard metadata (id, uid) and update the list
// of plugins
func (r *ReconcileGrafanaDashboard) manageSuccess(dashboard *grafanav1alpha1.GrafanaDashboard) {
	r.recorder.Event(dashboard, "Normal", "Success", msg)
	log.Info("dashboard successfully submitted",
		"Namespace" dashboard.Namespace,
		"Name", dashboard.Name)
	r.config.AddDashboard(dashboard)
	r.config.SetPluginsFor(dashboard)
}

// Handle error case: update dashboard with error message and status
func (r *ReconcileGrafanaDashboard) manageError(dashboard *grafanav1alpha1.GrafanaDashboard, issue error) {
	r.recorder.Event(dashboard, "Warning", "ProcessingError", issue.Error())

	// Ignore conclicts. Resource might just be outdated.
	if errors.IsConflict(issue) {
		return
	}
	log.Error(issue, "error updating dashboard")
}

// Get an authenticated grafana API client
func (r *ReconcileGrafanaDashboard) getClient() (GrafanaClient, error) {
	url := r.state.AdminUrl
	if url == "" {
		return nil, defaultErrors.New("cannot get grafana admin url")
	}

	username := r.state.AdminUsername
	if username == "" {
		return nil, defaultErrors.New("invalid credentials (username)")
	}

	password := r.state.AdminPassword
	if password == "" {
		return nil, defaultErrors.New("invalid credentials (password)")
	}

	duration := time.Duration(r.state.ClientTimeout)
	return NewGrafanaClient(url, username, password, duration), nil
}

// Test if a given dashboard matches an array of label selectors
func (r *ReconcileGrafanaDashboard) isMatch(item *grafanav1alpha1.GrafanaDashboard) bool {
	if r.state.DashboardSelectors == nil {
		return false
	}

	match, err := item.MatchesSelectors(r.state.DashboardSelectors)
	if err != nil {
		log.Error(err, "error matching selectors against", "Namespace", item.Namespace, "Name", item.Name)
		return false
	}
	return match
}
