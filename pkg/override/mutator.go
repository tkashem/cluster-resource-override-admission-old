package override

import (
	"fmt"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog"
	coreapi "k8s.io/kubernetes/pkg/apis/core"
	corev1 "k8s.io/api/core/v1"
)

func newMutator(config *Config, nsCPUFloor, nsMemFloor *resource.Quantity) *mutator {
	return nil
}

type mutator struct {
	config *Config
	nsCPUFloor *resource.Quantity
	nsMemFloor *resource.Quantity
}

func (m *mutator) Mutate(container *coreapi.Container) error {
	resources := container.Resources
	memLimit, memFound := resources.Limits[coreapi.ResourceMemory]
	if memFound && m.config.MemoryRequestToLimitRatio != 0 {
		// memory is measured in whole bytes.
		// the plugin rounds down to the nearest MiB rather than bytes to improve ease of use for end-users.
		amount := memLimit.Value() * int64(m.config.MemoryRequestToLimitRatio*100) / 100
		// TODO: move into resource.Quantity
		var mod int64
		switch memLimit.Format {
		case resource.BinarySI:
			mod = 1024 * 1024
		default:
			mod = 1000 * 1000
		}
		if rem := amount % mod; rem != 0 {
			amount = amount - rem
		}
		q := resource.NewQuantity(int64(amount), memLimit.Format)
		if memFloor.Cmp(*q) > 0 {
			clone := memFloor.DeepCopy()
			q = &clone
		}
		if m.nsMemFloor != nil && q.Cmp(*m.nsMemFloor) < 0 {
			klog.V(5).Infof("%s: %s pod limit %q below namespace limit; setting limit to %q", PluginName, corev1.ResourceMemory, q.String(), m.nsMemFloor.String())
			clone := m.nsMemFloor.DeepCopy()
			q = &clone
		}
		if err := applyQuantity(resources.Requests, corev1.ResourceMemory, *q, true); err != nil {
			return fmt.Errorf("resources.requests.%s %v", corev1.ResourceMemory, err)
		}
	}
	if memFound && m.config.LimitCPUToMemoryRatio != 0 {
		amount := float64(memLimit.Value()) * m.config.LimitCPUToMemoryRatio * cpuBaseScaleFactor
		q := resource.NewMilliQuantity(int64(amount), resource.DecimalSI)
		if cpuFloor.Cmp(*q) > 0 {
			clone := cpuFloor.DeepCopy()
			q = &clone
		}
		if m.nsCPUFloor != nil && q.Cmp(*m.nsCPUFloor) < 0 {
			klog.V(5).Infof("%s: %s pod limit %q below namespace limit; setting limit to %q", PluginName, corev1.ResourceCPU, q.String(), m.nsCPUFloor.String())
			clone := m.nsCPUFloor.DeepCopy()
			q = &clone
		}
		if err := applyQuantity(resources.Limits, corev1.ResourceCPU, *q, true); err != nil {
			return fmt.Errorf("resources.limits.%s %v", corev1.ResourceCPU, err)
		}
	}

	cpuLimit, cpuFound := resources.Limits[coreapi.ResourceCPU]
	if cpuFound && m.config.CpuRequestToLimitRatio != 0 {
		amount := float64(cpuLimit.MilliValue()) * m.config.CpuRequestToLimitRatio
		q := resource.NewMilliQuantity(int64(amount), cpuLimit.Format)
		if cpuFloor.Cmp(*q) > 0 {
			clone := cpuFloor.DeepCopy()
			q = &clone
		}
		if m.nsCPUFloor != nil && q.Cmp(*m.nsCPUFloor) < 0 {
			klog.V(5).Infof("%s: %s pod limit %q below namespace limit; setting limit to %q", PluginName, corev1.ResourceCPU, q.String(), m.nsCPUFloor.String())
			clone := m.nsCPUFloor.DeepCopy()
			q = &clone
		}
		if err := applyQuantity(resources.Requests, corev1.ResourceCPU, *q, true); err != nil {
			return fmt.Errorf("resources.requests.%s %v", corev1.ResourceCPU, err)
		}
	}

	return nil
}

// TODO: separate validation and mutation.
func applyQuantity(l coreapi.ResourceList, r corev1.ResourceName, v resource.Quantity, mutationAllowed bool) error {
	if mutationAllowed {
		l[coreapi.ResourceName(r)] = v
		return nil
	}

	if oldValue, ok := l[coreapi.ResourceName(r)]; !ok {
		return fmt.Errorf("mutated, expected: %v, now absent", v)
	} else if oldValue.Cmp(v) != 0 {
		return fmt.Errorf("mutated, expected: %v, got %v", v, oldValue)
	}

	return nil
}