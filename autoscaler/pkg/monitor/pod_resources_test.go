package monitor

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func qtyCPU(m int64) resource.Quantity {
	return *resource.NewMilliQuantity(m, resource.DecimalSI)
}
func qtyMem(mib int64) resource.Quantity {
	return *resource.NewQuantity(mib*1024*1024, resource.BinarySI)
}

func container(name string, cpuM, memMiB int64) corev1.Container {
	reqs := corev1.ResourceList{}
	if cpuM > 0 {
		reqs[corev1.ResourceCPU] = qtyCPU(cpuM)
	}
	if memMiB > 0 {
		reqs[corev1.ResourceMemory] = qtyMem(memMiB)
	}
	return corev1.Container{
		Name: name,
		Resources: corev1.ResourceRequirements{
			Requests: reqs,
		},
	}
}

func TestPodToInfo_NilPod(t *testing.T) {
	info := PodToInfo(nil)
	if info.Name != "" || !info.CPURequest.IsZero() || !info.MemoryRequest.IsZero() {
		t.Errorf("nil pod should convert to empty PodInfo, got %+v", info)
	}
}

func TestPodToInfo_SingleRegularContainer(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "one", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{container("app", 250, 128)},
		},
	}
	info := PodToInfo(pod)
	if info.CPURequest.MilliValue() != 250 {
		t.Errorf("CPU want 250, got %d", info.CPURequest.MilliValue())
	}
	if info.MemoryRequest.Value() != 128*1024*1024 {
		t.Errorf("mem want 128MiB, got %d", info.MemoryRequest.Value())
	}
}

func TestPodToInfo_MultipleRegularContainersSummed(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "multi"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				container("a", 100, 64),
				container("b", 200, 128),
			},
		},
	}
	info := PodToInfo(pod)
	if info.CPURequest.MilliValue() != 300 {
		t.Errorf("CPU want 300, got %d", info.CPURequest.MilliValue())
	}
	if info.MemoryRequest.Value() != 192*1024*1024 {
		t.Errorf("mem want 192MiB, got %d", info.MemoryRequest.Value())
	}
}

func TestPodToInfo_InitContainerLargerCPU(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "init-cpu"},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{container("init", 800, 64)},
			Containers:     []corev1.Container{container("app", 100, 64)},
		},
	}
	info := PodToInfo(pod)
	if info.CPURequest.MilliValue() != 800 {
		t.Errorf("effective CPU must follow larger init request, want 800 got %d", info.CPURequest.MilliValue())
	}
}

func TestPodToInfo_InitContainerLargerMemory(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "init-mem"},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{container("init", 50, 1024)},
			Containers:     []corev1.Container{container("app", 50, 128)},
		},
	}
	info := PodToInfo(pod)
	if info.MemoryRequest.Value() != 1024*1024*1024 {
		t.Errorf("effective memory must follow larger init request, got %d", info.MemoryRequest.Value())
	}
}

func TestPodToInfo_RestartableInitSidecar(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	sidecar := container("sidecar", 150, 64)
	sidecar.RestartPolicy = &always

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "sidecar"},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{sidecar},
			Containers:     []corev1.Container{container("app", 200, 128)},
		},
	}
	info := PodToInfo(pod)
	if info.CPURequest.MilliValue() != 350 {
		t.Errorf("sidecar CPU must be added to app containers, want 350 got %d", info.CPURequest.MilliValue())
	}
	if info.MemoryRequest.Value() != 192*1024*1024 {
		t.Errorf("sidecar memory must be added, want 192MiB got %d", info.MemoryRequest.Value())
	}
}

func TestPodToInfo_PodOverhead(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "oh"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{container("app", 100, 64)},
			Overhead: corev1.ResourceList{
				corev1.ResourceCPU:    qtyCPU(50),
				corev1.ResourceMemory: qtyMem(32),
			},
		},
	}
	info := PodToInfo(pod)
	if info.CPURequest.MilliValue() != 150 {
		t.Errorf("CPU with overhead want 150, got %d", info.CPURequest.MilliValue())
	}
	if info.MemoryRequest.Value() != 96*1024*1024 {
		t.Errorf("mem with overhead want 96MiB, got %d", info.MemoryRequest.Value())
	}
}

func TestPodToInfo_MissingRequests(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "besteffort"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app"},
			},
		},
	}
	info := PodToInfo(pod)
	if !info.CPURequest.IsZero() || !info.MemoryRequest.IsZero() {
		t.Errorf("missing requests must be treated as zero, got cpu=%s mem=%s", info.CPURequest.String(), info.MemoryRequest.String())
	}
}

func TestPodToInfo_MalformedEmptySpec(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "empty"},
	}
	info := PodToInfo(pod)
	if info.Name != "empty" {
		t.Errorf("name want empty, got %q", info.Name)
	}
	if !info.CPURequest.IsZero() {
		t.Errorf("empty spec CPU should be zero")
	}
}

func TestPodToInfo_DoesNotMutateSource(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "src"},
		Spec: corev1.PodSpec{
			NodeSelector: map[string]string{"zone": "a"},
			Tolerations: []corev1.Toleration{
				{Key: "k", Operator: corev1.TolerationOpExists},
			},
			Containers: []corev1.Container{container("app", 100, 64)},
		},
	}
	origCPU := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU].DeepCopy()
	info := PodToInfo(pod)
	info.NodeSelector["zone"] = "mutated"
	info.Tolerations[0].Key = "mutated"
	info.CPURequest.Add(qtyCPU(999))

	if pod.Spec.NodeSelector["zone"] != "a" {
		t.Error("conversion mutated source NodeSelector")
	}
	if pod.Spec.Tolerations[0].Key != "k" {
		t.Error("conversion mutated source Tolerations")
	}
	q := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
	if (&q).Cmp(origCPU) != 0 {
		t.Error("conversion mutated source container requests")
	}
}
