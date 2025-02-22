package crd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/rancher/wrangler/pkg/name"
	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apimachineerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	helmcontrollercrd "github.com/k3s-io/helm-controller/pkg/crd"
	helmlockercrd "github.com/rancher/prometheus-federator/pkg/helm-locker/crd"
	"github.com/rancher/prometheus-federator/pkg/helm-project-operator/apis/helm.cattle.io/v1alpha1"
	"github.com/rancher/prometheus-federator/pkg/helm-project-operator/experimental"
	"github.com/rancher/wrangler/pkg/crd"
	"github.com/rancher/wrangler/pkg/yaml"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
)

// WriteFiles writes CRDs and dependent CRDs to the paths specified
//
// Note: It is recommended to write CRDs to the templates directory (or similar) and to write
// CRD dependencies to the crds/ directory since you do not want the uninstall or upgrade of the
// CRD chart to destroy existing dependent CRDs in the cluster as that could break other components
//
// i.e. if you uninstall the HelmChart CRD, it can destroy an RKE2 or K3s cluster that also uses those CRs
// to manage internal Kubernetes component state
func WriteFiles(crdDirpath, crdDepDirpath string) error {
	objs, crdLockerDeps, crdControllerDeps, err := Objects(false)
	if err != nil {
		return err
	}
	if err := writeFiles(crdDirpath, objs); err != nil {
		return err
	}
	return writeFiles(crdDepDirpath, append(crdLockerDeps, crdControllerDeps...))
}

func writeFiles(dirpath string, objs []runtime.Object) error {
	if err := os.MkdirAll(dirpath, 0755); err != nil {
		return err
	}

	objMap := make(map[string][]byte)

	for _, o := range objs {
		data, err := yaml.Export(o)
		if err != nil {
			return err
		}
		metaData, err := meta.Accessor(o)
		if err != nil {
			return err
		}
		key := strings.SplitN(metaData.GetName(), ".", 2)[0]
		objMap[key] = data
	}

	var wg sync.WaitGroup
	wg.Add(len(objMap))
	for key, data := range objMap {
		go func(key string, data []byte) {
			defer wg.Done()
			f, err := os.Create(filepath.Join(dirpath, fmt.Sprintf("%s.yaml", key)))
			if err != nil {
				logrus.Error(err)
			}
			defer f.Close()
			_, err = f.Write(data)
			if err != nil {
				logrus.Error(err)
			}
		}(key, data)
	}
	wg.Wait()

	return nil
}

// Print prints CRDs to out and dependent CRDs to depOut
func Print(out io.Writer, depOut io.Writer) {
	objs, crdLockerDeps, crdControllerDeps, err := Objects(false)
	if err != nil {
		logrus.Fatalf("%s", err)
	}
	if err := printCrd(out, objs); err != nil {
		logrus.Fatalf("%s", err)
	}
	if err := printCrd(depOut, crdLockerDeps); err != nil {
		logrus.Fatalf("%s", err)
	}
	if err := printCrd(depOut, crdControllerDeps); err != nil {
		logrus.Fatalf("%s", err)
	}
}

func printCrd(out io.Writer, objs []runtime.Object) error {
	data, err := yaml.Export(objs...)
	if err != nil {
		return err
	}
	_, err = out.Write(data)
	return err
}

// Objects returns runtime.Objects for every CRD or CRD Dependency this operator relies on
func Objects(v1beta1 bool) (crds []runtime.Object, crdLockerDeps []runtime.Object, crdControllerDeps []runtime.Object, err error) {
	crdDefs, helmLockerCrdDefs, helmControllerCrdDefs := List()
	crds, err = objects(v1beta1, crdDefs)
	if err != nil {
		return nil, nil, nil, err
	}
	crdLockerDeps, err = objects(v1beta1, helmLockerCrdDefs)
	if err != nil {
		return nil, nil, nil, err
	}
	crdControllerDeps, err = objects(v1beta1, helmControllerCrdDefs)
	if err != nil {
		return nil, nil, nil, err
	}

	return crds, crdLockerDeps, crdControllerDeps, nil
}

func objects(v1beta1 bool, crdDefs []crd.CRD) (crds []runtime.Object, err error) {
	for _, crdDef := range crdDefs {
		if v1beta1 {
			crdDefInstance, err := crdDef.ToCustomResourceDefinitionV1Beta1()
			if err != nil {
				return nil, err
			}
			crds = append(crds, crdDefInstance)
		} else {
			crdDefInstance, err := crdDef.ToCustomResourceDefinition()
			if err != nil {
				return nil, err
			}
			crds = append(crds, crdDefInstance)
		}
	}
	return
}

// List returns the list of CRDs and dependent CRDs for this operator
func List() ([]crd.CRD, []crd.CRD, []crd.CRD) {
	// TODO: The underlying crd.CRD is deprectated and will eventually be removed
	// We simply do what `helm-controller` does, so we should work with them to update in tandem.
	crds := []crd.CRD{
		newCRD(
			"ProjectHelmChart.helm.cattle.io/v1alpha1",
			&v1alpha1.ProjectHelmChart{},
			func(c crd.CRD) crd.CRD {
				return c.
					WithColumn("Status", ".status.status").
					WithColumn("System Namespace", ".status.systemNamespace").
					WithColumn("Release Namespace", ".status.releaseNamespace").
					WithColumn("Release Name", ".status.releaseName").
					WithColumn("Target Namespaces", ".status.targetNamespaces")
			},
		),
	}
	return crds, helmlockercrd.List(), helmcontrollercrd.List()
}

// Create creates all CRDs and dependent CRDs in the cluster
func Create(ctx context.Context, cfg *rest.Config, shouldUpdateCRDs bool) error {
	factory, err := crd.NewFactoryFromClient(cfg)
	if err != nil {
		return err
	}

	crdClientSet := factory.CRDClient.(*clientset.Clientset)
	crdDefs, helmLockerCrdDefs, helmControllerCrdDefs := List()
	// When updateCRDs is true we will skip filtering the CRDs, in turn all CRDs will be re-installed.
	if !shouldUpdateCRDs {
		err = filterMissingCRDs(crdClientSet, &crdDefs)
		if err != nil {
			return err
		}

		err = filterMissingCRDs(crdClientSet, &helmLockerCrdDefs)
		if err != nil {
			return err
		}

		err = filterMissingCRDs(crdClientSet, &helmControllerCrdDefs)
		if err != nil {
			return err
		}
	} else {
		logrus.Debug("UpdateCRDs is Enabled; all CRDs will be installed.")
	}

	crdDefs = append(crdDefs, helmLockerCrdDefs...)
	if shouldManageHelmControllerCRDs(cfg) {
		crdDefs = append(crdDefs, helmControllerCrdDefs...)
	}

	return factory.BatchCreateCRDs(ctx, crdDefs...).BatchWait()
}

func newCRD(namespacedType string, obj interface{}, customize func(crd.CRD) crd.CRD) crd.CRD {
	newCrd := crd.NamespacedType(namespacedType).
		WithSchemaFromStruct(obj).
		WithStatus()

	if customize != nil {
		newCrd = customize(newCrd)
	}
	return newCrd
}

// filterMissingCRDs takes a list of expected CRDs and returns a filtered list of missing CRDs.
func filterMissingCRDs(apiExtClient *clientset.Clientset, expectedCRDs *[]crd.CRD) error {
	for i := len(*expectedCRDs) - 1; i >= 0; i-- {
		currentCRD := (*expectedCRDs)[i]
		crdName := currentCRD.GVK.GroupVersion().WithKind(strings.ToLower(name.GuessPluralName(currentCRD.GVK.Kind))).GroupKind().String()

		// try to get the given CRD just to check for error, verifying if it exists
		foundCRD, err := apiExtClient.ApiextensionsV1().CustomResourceDefinitions().Get(context.TODO(), crdName, metav1.GetOptions{})

		if err == nil {
			logrus.Debugf(
				"Found `%s` at version `%s`, expecting version `%s`",
				crdName,
				foundCRD.Status.StoredVersions[0],
				currentCRD.GVK.Version,
			)
			logrus.Debugf("Installing `%s` will be skipped; a suitible version exists on the cluster", crdName)
			// Update the list to remove the current item since the CRD is in the cluster already
			*expectedCRDs = append((*expectedCRDs)[:i], (*expectedCRDs)[i+1:]...)
		} else if !apimachineerrors.IsNotFound(err) {
			*expectedCRDs = []crd.CRD{}
			return fmt.Errorf("failed to check CRD %s: %v", crdName, err)
		} else {
			logrus.Debugf("Did not find `%s` on the cluster, it will be installed", crdName)
		}
	}

	return nil
}

// shouldManageHelmControllerCRDs determines if the controller should manage the CRDs for Helm Controller.
func shouldManageHelmControllerCRDs(cfg *rest.Config) bool {
	if os.Getenv("DETECT_K3S_RKE2") != "true" {
		logrus.Debug("k3s/rke2 detection feature is disabled; `helm-controller` CRDs will be managed")
		return true
	}

	// TODO: In the future, this should not rely on detecting k8s runtime type
	// The root question is "what component 'owns' this CRD" - and therefore updates it.
	// Instead we should rely on verifiable details directly on the CRDs in question.
	k8sRuntimeType, err := experimental.IdentifyKubernetesRuntimeType(cfg)
	if err != nil {
		logrus.Error(err)
	}

	onK3sRke2 := k8sRuntimeType == "k3s" || k8sRuntimeType == "rke2"
	if onK3sRke2 {
		logrus.Debug("the cluster is running on k3s (or rke2), `helm-controller` CRDs will not be managed by `prometheus-federator`")
	}

	return !onK3sRke2
}
