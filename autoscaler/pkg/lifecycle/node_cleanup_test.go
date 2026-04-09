package lifecycle

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/kuber-scale/autoscaler/pkg/utils"
)

func TestNodeCleanup_Cleanup(t *testing.T) {
	nodeName := "worker-node-1"

	tests := []struct {
		name          string
		existingNode  *corev1.Node
		hook          func(ctx context.Context, name string) error
		expectedError bool
	}{
		{
			name: "Successful deletion with hook",
			existingNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
					Annotations: map[string]string{
						utils.AnnotationManagedBy: utils.ManagedByValue,
					},
				},
			},
			hook: func(ctx context.Context, name string) error {
				if name != nodeName {
					t.Errorf("hook received wrong name: %s", name)
				}
				return nil
			},
			expectedError: false,
		},
		{
			name: "Refuse to delete unmanaged node",
			existingNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
					Annotations: map[string]string{
						// missing fully managed annotation
					},
				},
			},
			expectedError: true,
		},
		{
			name:          "Node not found",
			existingNode:  nil, // no node in api
			expectedError: true,
		},
		{
			name: "Provider hook error is non-fatal",
			existingNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
					Annotations: map[string]string{
						utils.AnnotationManagedBy: utils.ManagedByValue,
					},
				},
			},
			hook: func(ctx context.Context, name string) error {
				return errors.New("provider error")
			},
			expectedError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			if tt.existingNode != nil {
				_, err := client.CoreV1().Nodes().Create(context.Background(), tt.existingNode, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("failed to create fake node: %v", err)
				}
			}

			cleanup := NewNodeCleanup(client)
			cleanup.ProviderCleanupHook = tt.hook

			err := cleanup.Cleanup(context.Background(), nodeName)
			if (err != nil) != tt.expectedError {
				t.Fatalf("expected error: %v, got: %v", tt.expectedError, err)
			}

			if !tt.expectedError {
				// Verify node was actually deleted
				_, err := client.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
				if err == nil {
					t.Errorf("expected node to be deleted, but it still exists")
				}
			}
		})
	}
}
