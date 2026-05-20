package webhook

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.opentelemetry.io/obi/pkg/appolly/services"
	"go.opentelemetry.io/obi/pkg/kube/kubecache/informer"
)

func TestDetectLanguage(t *testing.T) {
	t.Run("languageFromImageName", func(t *testing.T) {
		tests := []struct {
			image string
			want  string
		}{
			// Node.js
			{"node:20", "nodejs"},
			{"node:alpine", "nodejs"},
			// node-exporter must NOT match as nodejs (fixed false positive)
			{"node-exporter:latest", ""},
			{"prom/node-exporter:v1.8.0", ""},
			// Python
			{"python:3.12", "python"},
			{"python:3.12@sha256:abcdef1234", "python"},
			// Java
			{"openjdk:17", "java"},
			{"eclipse-temurin:21", "java"},
			{"amazoncorretto:17", "java"},
			// .NET — only the last path component is matched, so "aspnet" works but bare "runtime" does not
			{"mcr.microsoft.com/dotnet/aspnet:8.0", "dotnet"},
			{"dotnet/aspnet:8.0", "dotnet"},
			{"mcr.microsoft.com/dotnet/runtime:8.0", ""},
			// Registry with port — must not strip the image name component
			{"registry:5000/myapp:latest", ""},
			{"registry:5000/python:latest", "python"},
			// No language cue
			{"nginx:latest", ""},
			{"alpine:3.18", ""},
		}

		for _, tt := range tests {
			t.Run(tt.image, func(t *testing.T) {
				assert.Equal(t, tt.want, languageFromImageName(tt.image))
			})
		}
	})

	t.Run("detectLanguageFromPodSpec_command_args", func(t *testing.T) {
		tests := []struct {
			name    string
			image   string
			command []string
			args    []string
			want    string
		}{
			{
				name:    "path_stripped_python",
				image:   "ubuntu:22.04",
				command: []string{"/usr/bin/python3"},
				want:    "python",
			},
			{
				name:    "node_command",
				image:   "ubuntu:22.04",
				command: []string{"node"},
				args:    []string{"index.js"},
				want:    "nodejs",
			},
			{
				// node-exporter as a command must NOT match nodejs
				name:    "node_exporter_command_not_nodejs",
				image:   "ubuntu:22.04",
				command: []string{"node-exporter"},
				want:    "",
			},
			{
				name:  "image_takes_priority_over_command",
				image: "python:3.12",
				args:  []string{"node"},
				want:  "python",
			},
			{
				name:  "no_cues",
				image: "ubuntu:22.04",
				want:  "",
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				p := &corev1.Pod{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Image: tt.image, Command: tt.command, Args: tt.args},
						},
					},
				}
				assert.Equal(t, tt.want, detectLanguageFromPodSpec(p))
			})
		}
	})
}

func TestOwnersFrom(t *testing.T) {
	tests := []struct {
		name        string
		meta        *metav1.ObjectMeta
		expected    int
		checkOwners func(t *testing.T, owners []*informer.Owner)
	}{
		{
			name: "no owner references",
			meta: &metav1.ObjectMeta{
				Name: "test-pod",
			},
			expected: 1,
			checkOwners: func(t *testing.T, owners []*informer.Owner) {
				assert.Equal(t, "Pod", owners[0].Kind)
				assert.Equal(t, "test-pod", owners[0].Name)
			},
		},
		{
			name: "single owner reference",
			meta: &metav1.ObjectMeta{
				Name: "test-pod",
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: "apps/v1",
						Kind:       "StatefulSet",
						Name:       "test-statefulset",
					},
				},
			},
			expected: 1,
			checkOwners: func(t *testing.T, owners []*informer.Owner) {
				assert.Equal(t, "StatefulSet", owners[0].Kind)
				assert.Equal(t, "test-statefulset", owners[0].Name)
			},
		},
		{
			name: "replicaset owner extracts deployment",
			meta: &metav1.ObjectMeta{
				Name: "test-pod",
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: "apps/v1",
						Kind:       "ReplicaSet",
						Name:       "my-deployment-abc123",
					},
				},
			},
			expected: 2,
			checkOwners: func(t *testing.T, owners []*informer.Owner) {
				assert.Equal(t, "ReplicaSet", owners[0].Kind)
				assert.Equal(t, "my-deployment-abc123", owners[0].Name)
				assert.Equal(t, "Deployment", owners[1].Kind)
				assert.Equal(t, "my-deployment", owners[1].Name)
			},
		},
		{
			name: "job owner extracts cronjob",
			meta: &metav1.ObjectMeta{
				Name: "test-pod",
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: "batch/v1",
						Kind:       "Job",
						Name:       "my-cronjob-1234567890",
					},
				},
			},
			expected: 2,
			checkOwners: func(t *testing.T, owners []*informer.Owner) {
				assert.Equal(t, "Job", owners[0].Kind)
				assert.Equal(t, "my-cronjob-1234567890", owners[0].Name)
				assert.Equal(t, "CronJob", owners[1].Kind)
				assert.Equal(t, "my-cronjob", owners[1].Name)
			},
		},
		{
			name: "multiple owner references",
			meta: &metav1.ObjectMeta{
				Name: "test-pod",
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: "apps/v1",
						Kind:       "DaemonSet",
						Name:       "my-daemonset",
					},
					{
						APIVersion: "v1",
						Kind:       "Node",
						Name:       "my-node",
					},
				},
			},
			expected: 2,
			checkOwners: func(t *testing.T, owners []*informer.Owner) {
				assert.Equal(t, "DaemonSet", owners[0].Kind)
				assert.Equal(t, "my-daemonset", owners[0].Name)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			owners := ownersFrom(tt.meta)
			assert.Len(t, owners, tt.expected)
			if tt.checkOwners != nil {
				tt.checkOwners(t, owners)
			}
		})
	}
}

func TestProcessMetadata(t *testing.T) {
	tests := []struct {
		name             string
		meta             *metav1.ObjectMeta
		checkMetadata    map[string]string
		checkLabels      map[string]string
		checkAnnotations map[string]string
	}{
		{
			name: "simple pod metadata",
			meta: &metav1.ObjectMeta{
				Name:      "test-pod",
				Namespace: "default",
				Labels: map[string]string{
					"app": "test-app",
				},
				Annotations: map[string]string{
					"annotation": "value",
				},
			},
			checkMetadata: map[string]string{
				services.AttrNamespace: "default",
				services.AttrPodName:   "test-pod",
				services.AttrOwnerName: "test-pod",
			},
			checkLabels: map[string]string{
				"app": "test-app",
			},
			checkAnnotations: map[string]string{
				"annotation": "value",
			},
		},
		{
			name: "pod with replicaset owner",
			meta: &metav1.ObjectMeta{
				Name:      "test-pod-abc",
				Namespace: "production",
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: "apps/v1",
						Kind:       "ReplicaSet",
						Name:       "my-deployment-xyz123",
					},
				},
			},
			checkMetadata: map[string]string{
				services.AttrNamespace: "production",
				services.AttrPodName:   "test-pod-abc",
				services.AttrOwnerName: "my-deployment",
			},
		},
		{
			name: "pod with job owner",
			meta: &metav1.ObjectMeta{
				Name:      "test-pod-xyz",
				Namespace: "batch-jobs",
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: "batch/v1",
						Kind:       "Job",
						Name:       "my-cronjob-1234567890",
					},
				},
			},
			checkMetadata: map[string]string{
				services.AttrNamespace: "batch-jobs",
				services.AttrPodName:   "test-pod-xyz",
				services.AttrOwnerName: "my-cronjob",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := processMetadata(tt.meta)

			assert.NotNil(t, info)

			if tt.checkMetadata != nil {
				for key, expected := range tt.checkMetadata {
					actual, ok := info.metadata[key]
					assert.True(t, ok, "metadata key %s not found", key)
					assert.Equal(t, expected, actual, "metadata key %s has wrong value", key)
				}
			}

			if tt.checkLabels != nil {
				assert.Equal(t, tt.checkLabels, info.podLabels)
			}

			if tt.checkAnnotations != nil {
				assert.Equal(t, tt.checkAnnotations, info.podAnnotations)
			}
		})
	}
}
