package monitor

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// podEffectiveRequests returns the Kubernetes scheduling-effective CPU and
// memory requests for a Pod, matching kube-scheduler / kubelet accounting
// for Kubernetes 1.29 (the API version this module depends on):
//
//	max(sum(regular containers) + sum(restartable init/sidecars),
//	    each non-restartable init container) + Pod overhead
//
// Missing requests are treated as zero, matching BestEffort / unspecified
// request semantics. The input Pod is never mutated.
func podEffectiveRequests(pod *corev1.Pod) (cpu, mem resource.Quantity) {
	cpu = resource.Quantity{}
	mem = resource.Quantity{}
	if pod == nil {
		return cpu, mem
	}

	add := func(dst *resource.Quantity, list corev1.ResourceList, name corev1.ResourceName) {
		if list == nil {
			return
		}
		if v, ok := list[name]; ok {
			dst.Add(v)
		}
	}
	maxQ := func(dst *resource.Quantity, list corev1.ResourceList, name corev1.ResourceName) {
		if list == nil {
			return
		}
		v, ok := list[name]
		if !ok {
			return
		}
		if dst.Cmp(v) < 0 {
			*dst = v.DeepCopy()
		}
	}

	for i := range pod.Spec.Containers {
		reqs := pod.Spec.Containers[i].Resources.Requests
		add(&cpu, reqs, corev1.ResourceCPU)
		add(&mem, reqs, corev1.ResourceMemory)
	}

	for i := range pod.Spec.InitContainers {
		c := pod.Spec.InitContainers[i]
		if isRestartableInitContainer(c) {
			add(&cpu, c.Resources.Requests, corev1.ResourceCPU)
			add(&mem, c.Resources.Requests, corev1.ResourceMemory)
		}
	}

	for i := range pod.Spec.InitContainers {
		c := pod.Spec.InitContainers[i]
		if isRestartableInitContainer(c) {
			continue
		}
		maxQ(&cpu, c.Resources.Requests, corev1.ResourceCPU)
		maxQ(&mem, c.Resources.Requests, corev1.ResourceMemory)
	}

	if pod.Spec.Overhead != nil {
		add(&cpu, pod.Spec.Overhead, corev1.ResourceCPU)
		add(&mem, pod.Spec.Overhead, corev1.ResourceMemory)
	}

	return cpu, mem
}

func isRestartableInitContainer(c corev1.Container) bool {
	return c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways
}

func copyStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func copyTolerations(in []corev1.Toleration) []corev1.Toleration {
	if in == nil {
		return nil
	}
	out := make([]corev1.Toleration, len(in))
	copy(out, in)
	return out
}
