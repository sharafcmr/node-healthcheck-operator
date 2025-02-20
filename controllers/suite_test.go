/*
Copyright 2021.

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

package controllers

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.uber.org/zap/zapcore"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	machinev1beta1 "github.com/openshift/api/machine/v1beta1"

	remediationv1alpha1 "github.com/medik8s/node-healthcheck-operator/api/v1alpha1"
	"github.com/medik8s/node-healthcheck-operator/controllers/cluster"
	"github.com/medik8s/node-healthcheck-operator/controllers/mhc"
	// +kubebuilder:scaffold:imports
)

// These tests use Ginkgo (BDD-style Go testing framework). Refer to
// http://onsi.github.io/ginkgo/ to learn more about Ginkgo.

const (
	DeploymentNamespace = "testns"
	MachineNamespace    = "openshift-machine-api"
)

var cfg *rest.Config
var k8sClient client.Client
var k8sManager manager.Manager
var testEnv *envtest.Environment
var ctx context.Context
var cancel context.CancelFunc

var upgradeChecker *fakeClusterUpgradeChecker
var fakeTime *time.Time

func TestAPIs(t *testing.T) {
	RegisterFailHandler(Fail)
	// debugging time values needs much place...
	//format.MaxLength = 10000
	RunSpecs(t, "Controller Suite")
}

var _ = BeforeSuite(func() {
	opts := zap.Options{
		Development: true,
		TimeEncoder: zapcore.RFC3339NanoTimeEncoder,
	}
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseFlagOptions(&opts)))

	testScheme := runtime.NewScheme()

	By("bootstrapping test environment")
	testEnv = &envtest.Environment{
		CRDInstallOptions: envtest.CRDInstallOptions{
			Scheme: testScheme,
			Paths: []string{
				filepath.Join("..", "vendor", "github.com", "openshift", "api", "machine", "v1beta1"),
				filepath.Join("..", "config", "crd", "bases"),
			},
			ErrorIfPathMissing: true,
		},
	}

	var err error
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	scheme.AddToScheme(testScheme)
	Expect(remediationv1alpha1.AddToScheme(testScheme)).To(Succeed())
	Expect(machinev1beta1.Install(testScheme)).To(Succeed())
	Expect(apiextensionsv1.AddToScheme(testScheme)).To(Succeed())
	// +kubebuilder:scaffold:scheme

	k8sManager, err = ctrl.NewManager(cfg, ctrl.Options{Scheme: testScheme})
	Expect(err).NotTo(HaveOccurred())

	k8sClient, err = client.New(cfg, client.Options{Scheme: testScheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())

	// Deploy test remediation CRDs and CR
	testKind := "InfrastructureRemediation"
	Expect(k8sClient.Create(context.Background(), newTestRemediationTemplateCRD(testKind))).To(Succeed())
	Expect(k8sClient.Create(context.Background(), newTestRemediationCRD(testKind))).To(Succeed())
	time.Sleep(time.Second)
	Expect(k8sClient.Create(context.Background(), newTestRemediationTemplateCR(testKind, "default", "template"))).To(Succeed())

	testKind = "Metal3Remediation"
	Expect(k8sClient.Create(context.Background(), newTestRemediationTemplateCRD(testKind))).To(Succeed())
	Expect(k8sClient.Create(context.Background(), newTestRemediationCRD(testKind))).To(Succeed())
	time.Sleep(time.Second)
	ns := &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "openshift-machine-api",
		},
		Spec: v1.NamespaceSpec{},
	}
	Expect(k8sClient.Create(context.Background(), ns)).To(Succeed())
	Expect(k8sClient.Create(context.Background(), newTestRemediationTemplateCR(testKind, MachineNamespace, "ok"))).To(Succeed())
	Expect(k8sClient.Create(context.Background(), newTestRemediationTemplateCR(testKind, "default", "nok"))).To(Succeed())

	upgradeChecker = &fakeClusterUpgradeChecker{
		Err:       nil,
		Upgrading: false,
	}

	mhcChecker, err := mhc.NewMHCChecker(k8sManager, false)
	Expect(err).NotTo(HaveOccurred())

	os.Setenv("DEPLOYMENT_NAMESPACE", DeploymentNamespace)

	// to be able faking the current time for tests
	currentTime = func() time.Time {
		if fakeTime != nil {
			return *fakeTime
		}
		return time.Now()
	}

	err = (&NodeHealthCheckReconciler{
		Client:                      k8sManager.GetClient(),
		Log:                         k8sManager.GetLogger().WithName("test reconciler"),
		Scheme:                      k8sManager.GetScheme(),
		Recorder:                    k8sManager.GetEventRecorderFor("NodeHealthCheck"),
		ClusterUpgradeStatusChecker: upgradeChecker,
		MHCChecker:                  mhcChecker,
		OnOpenShift:                 true,
	}).SetupWithManager(k8sManager)
	Expect(err).NotTo(HaveOccurred())

	err = (&MachineHealthCheckReconciler{
		Client:                      k8sManager.GetClient(),
		Log:                         k8sManager.GetLogger().WithName("test reconciler"),
		Scheme:                      k8sManager.GetScheme(),
		Recorder:                    k8sManager.GetEventRecorderFor("NodeHealthCheck"),
		ClusterUpgradeStatusChecker: upgradeChecker,
		MHCChecker:                  mhcChecker,
	}).SetupWithManager(k8sManager)
	Expect(err).NotTo(HaveOccurred())

	go func() {
		// https://github.com/kubernetes-sigs/controller-runtime/issues/1571
		ctx, cancel = context.WithCancel(ctrl.SetupSignalHandler())
		err := k8sManager.Start(ctx)
		Expect(err).NotTo(HaveOccurred())
	}()

})

var _ = AfterSuite(func() {
	By("tearing down the test environment")
	cancel()
	err := testEnv.Stop()
	Expect(err).NotTo(HaveOccurred())
})

type fakeClusterUpgradeChecker struct {
	Upgrading bool
	Err       error
}

// force implementation of interface
var _ cluster.UpgradeChecker = &fakeClusterUpgradeChecker{}

func (c *fakeClusterUpgradeChecker) Check() (bool, error) {
	return c.Upgrading, c.Err
}

func newTestRemediationTemplateCRD(kind string) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiextensionsv1.SchemeGroupVersion.String(),
			Kind:       "CustomResourceDefinition",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: strings.ToLower(kind) + "templates.test.medik8s.io",
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "test.medik8s.io",
			Scope: apiextensionsv1.NamespaceScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Kind:   kind + "Template",
				Plural: strings.ToLower(kind) + "templates",
			},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{
					Name:    "v1alpha1",
					Served:  true,
					Storage: true,
					Subresources: &apiextensionsv1.CustomResourceSubresources{
						Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
					},
					Schema: &apiextensionsv1.CustomResourceValidation{
						OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
							Type: "object",
							Properties: map[string]apiextensionsv1.JSONSchemaProps{
								"spec": {
									Type:                   "object",
									XPreserveUnknownFields: pointer.Bool(true),
								},
								"status": {
									Type:                   "object",
									XPreserveUnknownFields: pointer.Bool(true),
								},
							},
						},
					},
				},
			},
		},
	}
}

func newTestRemediationCRD(kind string) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiextensions.SchemeGroupVersion.String(),
			Kind:       "CustomResourceDefinition",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: strings.ToLower(kind) + "s.test.medik8s.io",
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "test.medik8s.io",
			Scope: apiextensionsv1.NamespaceScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Kind:   kind,
				Plural: strings.ToLower(kind) + "s",
			},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{
					Name:    "v1alpha1",
					Served:  true,
					Storage: true,
					Subresources: &apiextensionsv1.CustomResourceSubresources{
						Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
					},
					Schema: &apiextensionsv1.CustomResourceValidation{
						OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
							Type: "object",
							Properties: map[string]apiextensionsv1.JSONSchemaProps{
								"spec": {
									Type:                   "object",
									XPreserveUnknownFields: pointer.Bool(true),
								},
								"status": {
									Type:                   "object",
									XPreserveUnknownFields: pointer.Bool(true),
								},
							},
						},
					},
				},
			},
		},
	}
}

func newTestRemediationTemplateCR(kind, namespace, name string) client.Object {
	template := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"spec": map[string]interface{}{
				"template": map[string]interface{}{
					"spec": map[string]interface{}{
						"size": "foo",
					},
				},
			},
		},
	}
	template.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "test.medik8s.io",
		Version: "v1alpha1",
		Kind:    kind + "Template",
	})
	template.SetNamespace(namespace)
	template.SetName(name)
	return template
}
