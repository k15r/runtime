/*
Copyright 2019 The Kyma Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package function

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/go-test/deep"
	buildv1alpha1 "github.com/knative/build/pkg/apis/build/v1alpha1"
	servingv1alpha1 "github.com/knative/serving/pkg/apis/serving/v1alpha1"
	runtimev1alpha1 "github.com/kyma-incubator/runtime/pkg/apis/runtime/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
	"sigs.k8s.io/controller-runtime/pkg/source"

	runtimeUtil "github.com/kyma-incubator/runtime/pkg/utils"
)

var log = logf.Log.WithName("controller")

const (
	ConfigName            string = "fn-config"
	ConfigNamespace       string = "default"
	ConfigMapNameEnv      string = "CONTROLLER_CONFIGMAP"
	ConfigMapNamespaceEnv string = "CONTROLLER_CONFIGMAP_NS"
)

// Add creates a new Function Controller and adds it to the Manager with default RBAC. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return add(mgr, newReconciler(mgr))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	return &ReconcileFunction{Client: mgr.GetClient(), scheme: mgr.GetScheme()}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("function-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to Function
	err = c.Watch(&source.Kind{Type: &runtimev1alpha1.Function{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	// TODO(user): Modify this to be the types you create
	// Uncomment watch a Deployment created by Function - change this for objects you create
	err = c.Watch(&source.Kind{Type: &servingv1alpha1.Service{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &runtimev1alpha1.Function{},
	})
	if err != nil {
		return err
	}

	return nil
}

var _ reconcile.Reconciler = &ReconcileFunction{}

// ReconcileFunction reconciles a Function object
type ReconcileFunction struct {
	client.Client
	scheme *runtime.Scheme
}

func getEnvDefault(name, defaultValue string) string {
	value := os.Getenv(name)
	if len(value) == 0 {
		return defaultValue
	}
	return value

}

func (r *ReconcileFunction) loadConfig() (*runtimeUtil.RuntimeInfo, error) {

	fnConfigName := getEnvDefault(ConfigMapNameEnv, ConfigName)
	fnConfigNamespace := getEnvDefault(ConfigMapNamespaceEnv, ConfigNamespace)

	fnConfig := &corev1.ConfigMap{}
	err := r.Get(context.TODO(), types.NamespacedName{Name: fnConfigName, Namespace: fnConfigNamespace}, fnConfig)

	if err != nil {
		log.Error(err, "Unable to read Function controller config: %v from Namespace: %v", fnConfigName, fnConfigNamespace)
		return nil, err
	}

	return runtimeUtil.New(fnConfig)
}

// Reconcile reads that state of the cluster for a Function object and makes changes based on the state read
// and what is in the Function.Spec
// + kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// + kubebuilder:rbac:groups=apps,resources=deployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=runtime.kyma-project.io,resources=functions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=runtime.kyma-project.io,resources=functions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=serving.knative.dev,resources=configurations,verbs=get;list;watch
// +kubebuilder:rbac:groups=serving.knative.dev,resources=configurations/status,verbs=get
func (r *ReconcileFunction) Reconcile(request reconcile.Request) (reconcile.Result, error) {

	rnInfo, err := r.loadConfig()

	if err != nil {
		return reconcile.Result{}, err
	}

	// Fetch the Function instance
	fn := &runtimev1alpha1.Function{}
	err = r.Get(context.TODO(), request.NamespacedName, fn)
	if err != nil {
		if errors.IsNotFound(err) {
			// Object not found, return.  Created objects are automatically garbage collected.
			// For additional cleanup logic use finalizers.
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	data := make(map[string]string)
	data["handler"] = "handler.main"
	data["handler.js"] = fn.Spec.Function
	if len(strings.Trim(fn.Spec.Deps, " ")) == 0 {
		data["package.json"] = "{}"
	} else {
		data["package.json"] = fn.Spec.Deps
	}

	// Managing a ConfigMap
	deployCm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Labels:    fn.Labels,
			Namespace: fn.Namespace,
			Name:      fn.Name,
		},
		Data: data,
	}
	if err := controllerutil.SetControllerReference(fn, deployCm, r.scheme); err != nil {
		return reconcile.Result{}, err
	}

	foundCm := &corev1.ConfigMap{}
	err = r.Get(context.TODO(), types.NamespacedName{Name: deployCm.Name, Namespace: deployCm.Namespace}, foundCm)
	if err != nil && errors.IsNotFound(err) {
		fn.Status.Status = runtimev1alpha1.FunctionDeploying
		fn.Status.Description = fn.Status.Description + "createCM;"
		// r.Status().Update(context.Background(), fn)

		log.Info("Creating ConfigMap", "namespace", deployCm.Namespace, "name", deployCm.Name)

		err = r.Create(context.TODO(), deployCm)
		if err != nil {
			return reconcile.Result{}, err
		}
	} else if err != nil {
		return reconcile.Result{}, err
	} else if !reflect.DeepEqual(deployCm.Data, foundCm.Data) {
		foundCm.Data = deployCm.Data
		log.Info("Updating ConfigMap", "namespace", deployCm.Namespace, "name", deployCm.Name)
		fn.Status.Description = fn.Status.Description + "updateCM;"
		fn.Status.Status = runtimev1alpha1.FunctionUpdating
		// r.Status().Update(context.Background(), fn)
		err = r.Update(context.TODO(), foundCm)
		if err != nil {
			return reconcile.Result{}, err
		}
	}

	err = r.Get(context.TODO(), types.NamespacedName{Name: deployCm.Name, Namespace: deployCm.Namespace}, foundCm)
	if err != nil {
		log.Error(err, "namespace", deployCm.Namespace, "name", deployCm.Name)
		return reconcile.Result{}, err
	}
	hash := sha256.New()
	hash.Write([]byte(foundCm.Data["handler.js"] + foundCm.Data["package.json"]))
	functionSha := fmt.Sprintf("%x", hash.Sum(nil))

	dockerRegistry := rnInfo.RegistryInfo
	imageName := fmt.Sprintf("%s/%s-%s:%s", dockerRegistry, fn.Namespace, fn.Name, functionSha)
	deployService := &servingv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Labels:    fn.Labels,
			Namespace: fn.Namespace,
			Name:      fn.Name,
		},
		Spec: runtimeUtil.GetServiceSpec(imageName, *fn, rnInfo),
	}

	if len(deployService.Spec.RunLatest.Configuration.Build.BuildSpec.Steps) == 0 {
		fn.Status.Status = runtimev1alpha1.FunctionError
		fn.Status.Description = "invalid runtime configured"
		r.Status().Update(context.Background(), fn)
	}

	if err := controllerutil.SetControllerReference(fn, deployService, r.scheme); err != nil {
		return reconcile.Result{}, err
	}

	foundService := &servingv1alpha1.Service{}
	err = r.Get(context.TODO(), types.NamespacedName{Name: deployService.Name, Namespace: deployService.Namespace}, foundService)
	// Nasty hack to compare the build spec. It seems the knative folks want to get rid off the buildspec in the servicespec anyway.
	//	This will force us to find a different solution. https://knative.dev/docs/reference/serving-api/#RawExtension
	if err == nil {
		buildspec := &buildv1alpha1.BuildSpec{}
		if err := foundService.Spec.RunLatest.Configuration.Build.As(buildspec); err != nil {
			log.Error(err, "Failed to decode BuildSpec")
		}
		foundService.Spec.RunLatest.Configuration.Build.BuildSpec = buildspec
		foundService.Spec.RunLatest.Configuration.Build.Raw = nil
		foundService.Spec.RunLatest.Configuration.RevisionTemplate.Spec.TimeoutSeconds = int64(0)
		foundService.Spec.RunLatest.Configuration.RevisionTemplate.Spec.Container.Resources.Requests = nil
	}

	if err != nil && errors.IsNotFound(err) {
		log.Info("Creating Service", "namespace", deployService.Namespace, "name", deployService.Name)

		fn.Status.Status = runtimev1alpha1.FunctionDeploying
		r.Status().Update(context.Background(), fn)

		err = r.Create(context.TODO(), deployService)
		return reconcile.Result{}, err
	} else if err != nil {
		fmt.Printf("Error while creating: %v", err)
		return reconcile.Result{}, err
	} else if !reflect.DeepEqual(deployService.Spec, foundService.Spec) {
		diff := deep.Equal(deployService.Spec, foundService.Spec)
		log.Info(fmt.Sprintf("%v", diff))
		foundService.Spec = deployService.Spec
		log.Info("Updating Service", "namespace", deployService.Namespace, "name", deployService.Name)
		fn.Status.Status = runtimev1alpha1.FunctionUpdating
		fn.Status.Description = fn.Status.Description + "updateSvc;"
		err = r.Update(context.TODO(), foundService)
		r.Status().Update(context.Background(), fn)
		return reconcile.Result{}, err
	}

	for _, condition := range foundService.Status.Conditions {
		if condition.Type == servingv1alpha1.ServiceConditionReady {
			if condition.Status == corev1.ConditionTrue {
				fn.Status.Status = runtimev1alpha1.FunctionRunning
			}
		}

	}
	r.Status().Update(context.Background(), fn)

	return reconcile.Result{}, nil

}
