package override

import (
	"fmt"
	"errors"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/labels"
	// "k8s.io/apiserver/pkg/admission"
	"k8s.io/klog"
	// "k8s.io/apiserver/pkg/admission"
	// webhookrequest "k8s.io/apiserver/pkg/admission/plugin/webhook/request"
	admissionresponse "github.com/openshift/cluster-resource-override-admission/pkg/response"
	admissionv1beta1 "k8s.io/api/admission/v1beta1"
	corev1 "k8s.io/api/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	coreapi "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/plugin/pkg/admission/limitranger"
)

const (
	PluginName                        = ""
	clusterResourceOverrideAnnotation = "autoscaling.openshift.io/cluster-resource-override-enabled"
	cpuBaseScaleFactor                = 1000.0 / (1024.0 * 1024.0 * 1024.0) // 1000 milliCores per 1GiB
)

var (
	cpuFloor = resource.MustParse("1m")
	memFloor = resource.MustParse("1Mi")
	BadRequestErr = errors.New("unexpected object")
)

type Admission interface {
	IsApplicable(admissionSpec *admissionv1beta1.AdmissionRequest) bool
	IsExempt(admissionSpec *admissionv1beta1.AdmissionRequest) (exempt bool, response *admissionv1beta1.AdmissionResponse)
	Admit(admissionSpec *admissionv1beta1.AdmissionRequest) *admissionv1beta1.AdmissionResponse
}

type clusterResourceOverridePlugin struct {
	config            *Config
	nsLister          corev1listers.NamespaceLister
	LimitRanger       *limitranger.LimitRanger
	limitRangesLister corev1listers.LimitRangeLister
}

func (p *clusterResourceOverridePlugin) IsApplicable(admissionSpec *admissionv1beta1.AdmissionRequest) bool {
	if admissionSpec.Resource.Resource == string(coreapi.ResourcePods) &&
		admissionSpec.SubResource == "" &&
		(admissionSpec.Operation == admissionv1beta1.Create || admissionSpec.Operation == admissionv1beta1.Update) {

		return true
	}

	return false
}

func (p *clusterResourceOverridePlugin) IsExempt(admissionSpec *admissionv1beta1.AdmissionRequest) (exempt bool, response *admissionv1beta1.AdmissionResponse) {
	pod, ok := admissionSpec.Object.Object.(*coreapi.Pod)
	if !ok {
		response = admissionresponse.WithBadRequest(admissionSpec, BadRequestErr)
		return
	}

	klog.V(5).Infof("%s is looking at creating pod %s in project %s", PluginName, pod.Name, admissionSpec.Namespace)

	// allow annotations on project to override
	ns, err := p.nsLister.Get(admissionSpec.Namespace)
	if err != nil {
		klog.Warningf("%s got an error retrieving namespace: %v", PluginName, err)
		response = admissionresponse.WithForbidden(admissionSpec, err)
		return
	}

	projectEnabledPlugin, exists := ns.Annotations[clusterResourceOverrideAnnotation]
	if exists && projectEnabledPlugin != "true" {
		klog.V(5).Infof("%s is disabled for project %s", PluginName, admissionSpec.Namespace)
		exempt = true
		return
	}

	if isExemptedNamespace(ns.Name) {
		klog.V(5).Infof("%s is skipping exempted project %s", PluginName, admissionSpec.Namespace)
		exempt = true // project is exempted, do nothing
		return
	}

	return
}

func (p *clusterResourceOverridePlugin) Admit(request *admissionv1beta1.AdmissionRequest) *admissionv1beta1.AdmissionResponse {
	pod, ok := request.Object.Object.(*coreapi.Pod)
	if !ok {
		return admissionresponse.WithBadRequest(request, BadRequestErr)
	}

	namespaceLimits := []*corev1.LimitRange{}

	if p.limitRangesLister != nil {
		limits, err := p.limitRangesLister.LimitRanges(request.Namespace).List(labels.Everything())
		if err != nil {
			return admissionresponse.WithForbidden(request, err)
		}
		namespaceLimits = limits
	}

	// Don't mutate resource requirements below the namespace
	// limit minimums.
	nsCPUFloor := minResourceLimits(namespaceLimits, corev1.ResourceCPU)
	nsMemFloor := minResourceLimits(namespaceLimits, corev1.ResourceMemory)

	klog.V(5).Infof("%s: initial pod limits are: %#v", PluginName, pod.Spec)

	// Reuse LimitRanger logic to apply limit/req defaults from the project. Ignore validation
	// errors, assume that LimitRanger will run after this plugin to validate.
	// TODO: Figure out whether we need to invoke LimitRanger.Admit

	klog.V(5).Infof("%s: pod limits after LimitRanger: %#v", PluginName, pod.Spec)
	mutator := newMutator(p.config, nsCPUFloor, nsMemFloor)

	original := pod
	current := original.DeepCopy()
	for i := range current.Spec.InitContainers {
		if mutationErr := mutator.Mutate(&current.Spec.InitContainers[i]); mutationErr != nil {
			err := fmt.Errorf("spec.initContainers[%d].%v", i, mutationErr)
			return admissionresponse.WithInternalServerError(request, err)
		}
	}

	for i := range current.Spec.Containers {
		if mutationErr := mutator.Mutate(&current.Spec.Containers[i]); mutationErr != nil {
			err := fmt.Errorf("spec.Containers[%d].%v", i, mutationErr)
			return admissionresponse.WithInternalServerError(request, err)
		}
	}

	klog.V(5).Infof("%s: pod limits after overrides are: %#v", PluginName, current.Spec)

	patch, patchErr := Patch(request.Object, current)
	if patchErr != nil {
		return admissionresponse.WithInternalServerError(request, patchErr)
	}

	return admissionresponse.WithPatch(request, patch)
}

// this a real shame to be special cased.
var (
	forbiddenNames    = []string{"openshift", "kubernetes", "kube"}
	forbiddenPrefixes = []string{"openshift-", "kubernetes-", "kube-"}
)

func isExemptedNamespace(name string) bool {
	for _, s := range forbiddenNames {
		if name == s {
			return true
		}
	}
	for _, s := range forbiddenPrefixes {
		if strings.HasPrefix(name, s) {
			return true
		}
	}
	return false
}

// minResourceLimits finds the Min limit for resourceName. Nil is
// returned if limitRanges is empty or limits contains no resourceName
// limits.
func minResourceLimits(limitRanges []*corev1.LimitRange, resourceName corev1.ResourceName) *resource.Quantity {
	limits := []*resource.Quantity{}

	for _, limitRange := range limitRanges {
		for _, limit := range limitRange.Spec.Limits {
			if limit.Type == corev1.LimitTypeContainer {
				if limit, found := limit.Min[resourceName]; found {
					clone := limit.DeepCopy()
					limits = append(limits, &clone)
				}
			}
		}
	}

	if len(limits) == 0 {
		return nil
	}

	return minQuantity(limits)
}

func minQuantity(quantities []*resource.Quantity) *resource.Quantity {
	min := quantities[0].DeepCopy()

	for i := range quantities {
		if quantities[i].Cmp(min) < 0 {
			min = quantities[i].DeepCopy()
		}
	}

	return &min
}
