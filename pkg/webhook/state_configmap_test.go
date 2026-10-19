package webhook

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"go.opentelemetry.io/obi/pkg/appolly/services"
	"go.opentelemetry.io/obi/pkg/export"
	"go.opentelemetry.io/obi/pkg/export/otel/perapp"
	"go.opentelemetry.io/obi/pkg/transform"

	"github.com/grafana/beyla/v3/pkg/beyla"
	"github.com/grafana/beyla/v3/pkg/webhook/configmap"
)

func TestContainerIDMatches(t *testing.T) {
	tests := []struct {
		name     string
		statusID string
		ownID    string
		want     bool
	}{
		{
			name:     "exact match",
			statusID: "containerd://abc123def456abc123def456abc123def456abc123def456abc123def456abc1",
			ownID:    "abc123def456abc123def456abc123def456abc123def456abc123def456abc1",
			want:     true,
		},
		{
			name:     "short cgroup suffix match",
			statusID: "containerd://abc123def456abc123def456abc123def456abc123def456abc123def456abc1",
			ownID:    "def456abc1",
			want:     false,
		},
		{
			name:     "minimum length cgroup suffix match",
			statusID: "containerd://abc123def456abc123def456abc123def456abc123def456abc123def456abc1",
			ownID:    "123def456abc1",
			want:     false,
		},
		{
			name:     "mismatch",
			statusID: "containerd://abc123def456abc123def456abc123def456abc123def456abc123def456abc1",
			ownID:    "fff123def456abc123def456abc123def456abc123def456abc123def456abc1",
			want:     false,
		},
		{
			name:     "empty values do not match",
			statusID: "",
			ownID:    "",
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, containerIDMatches(tt.statusID, tt.ownID))
		})
	}
}

func TestPodHasContainerID(t *testing.T) {
	const containerID = "abc123def456abc123def456abc123def456abc123def456abc123def456abc1"

	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:        "beyla",
				ContainerID: "containerd://" + containerID,
			}},
		},
	}

	assert.True(t, podHasContainerID(pod, containerID))
}

func TestPodHasContainerIDNoMatch(t *testing.T) {
	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:        "other",
				ContainerID: "containerd://fff123def456abc123def456abc123def456abc123def456abc123def456abc1",
			}},
		},
	}

	assert.False(t, podHasContainerID(pod, "abc123def456abc123def456abc123def456abc123def456abc123def456abc1"))
}

func TestTrimContainerIDScheme(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{name: "containerd scheme", input: "containerd://abc123", expected: "abc123"},
		{name: "docker scheme", input: "docker://deadbeef", expected: "deadbeef"},
		{name: "cri-o scheme", input: "cri-o://feedface", expected: "feedface"},
		{name: "no scheme", input: "abc123", expected: "abc123"},
		{name: "empty", input: "", expected: ""},
		{name: "scheme only", input: "containerd://", expected: ""},
		{name: "extra colons in id", input: "containerd://abc://def", expected: "abc://def"},
		{name: "single colon (no //)", input: "containerd:abc", expected: "containerd:abc"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, trimContainerIDScheme(tt.input))
		})
	}
}

func TestStateConfigMapName(t *testing.T) {
	tests := []struct {
		name          string
		daemonSetName string
		nodeName      string
		podName       string
		expected      string
	}{
		{
			name:          "simple names",
			daemonSetName: "beyla",
			nodeName:      "node-1",
			podName:       "beyla-abcde",
			expected:      "beyla-injector-state-node-1",
		},
		{
			name:          "node name with uppercase and dots is sanitized",
			daemonSetName: "beyla",
			nodeName:      "Node.Example.COM",
			podName:       "pod-xyz",
			expected:      "beyla-injector-state-node-example-com",
		},
		{
			name:          "empty node name falls back to 'unknown'",
			daemonSetName: "beyla",
			nodeName:      "",
			podName:       "pod-xyz",
			expected:      "beyla-injector-state-unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, stateConfigMapName(tt.daemonSetName, tt.nodeName))
		})
	}
}

func TestSanitizeDNS1123(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{name: "already valid", input: "node-1", expected: "node-1"},
		{name: "uppercase lowercased", input: "NODE-1", expected: "node-1"},
		{name: "dots replaced", input: "node.example.com", expected: "node-example-com"},
		{name: "underscores and slashes replaced", input: "node_1/foo", expected: "node-1-foo"},
		{name: "leading/trailing dashes trimmed", input: "---node---", expected: "node"},
		{name: "leading/trailing invalid chars trimmed after sub", input: "...node...", expected: "node"},
		{name: "empty input", input: "", expected: "unknown"},
		{name: "all invalid chars", input: "!!!", expected: "unknown"},
		{name: "collapses contiguous invalid runs", input: "a..b__c", expected: "a-b-c"},
		{
			name:     "truncates to 63 chars",
			input:    strings.Repeat("a", 70),
			expected: strings.Repeat("a", 63),
		},
		{
			name:     "trims trailing dash after truncation",
			input:    strings.Repeat("a", 62) + "-extra",
			expected: strings.Repeat("a", 62),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeDNS1123(tt.input)
			assert.Equal(t, tt.expected, got)
			assert.LessOrEqual(t, len(got), 63)
		})
	}
}

func TestOwnContainerID(t *testing.T) {
	prev := containerInfoFunc
	t.Cleanup(func() { containerInfoFunc = prev })

	t.Run("returns container id from container info", func(t *testing.T) {
		containerInfoFunc = func(_ uint32) (Info, error) {
			return Info{ContainerID: "abc123"}, nil
		}
		got, err := ownContainerID()
		require.NoError(t, err)
		assert.Equal(t, "abc123", got)
	})

	t.Run("propagates underlying error", func(t *testing.T) {
		boom := errors.New("boom")
		containerInfoFunc = func(_ uint32) (Info, error) {
			return Info{}, boom
		}
		_, err := ownContainerID()
		assert.ErrorIs(t, err, boom)
	})

	t.Run("errors when container id is empty", func(t *testing.T) {
		containerInfoFunc = func(_ uint32) (Info, error) {
			return Info{ContainerID: ""}, nil
		}
		_, err := ownContainerID()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "container ID is empty")
	})
}

func TestOwnNamespace(t *testing.T) {
	prev := saNamespacePath
	t.Cleanup(func() { saNamespacePath = prev })

	t.Run("reads and trims namespace from file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "namespace")
		require.NoError(t, os.WriteFile(path, []byte("  beyla-system\n"), 0o600))
		saNamespacePath = path

		got, err := ownNamespace()
		require.NoError(t, err)
		assert.Equal(t, "beyla-system", got)
	})

	t.Run("returns error when file is missing", func(t *testing.T) {
		saNamespacePath = filepath.Join(t.TempDir(), "does-not-exist")
		_, err := ownNamespace()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "read SA namespace")
	})

	t.Run("returns error when file is empty or only whitespace", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "namespace")
		require.NoError(t, os.WriteFile(path, []byte("   \n  "), 0o600))
		saNamespacePath = path

		_, err := ownNamespace()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty")
	})
}

// makePod builds a minimal Pod with the given owner refs and a single container
// status whose ContainerID points at containerID (without any runtime scheme
// prefix; the caller can pass a scheme-prefixed string directly to test that).
func makePod(name, namespace, nodeName, containerID string, owners []metav1.OwnerReference) *corev1.Pod {
	statusContainerID := containerID
	if statusContainerID != "" && !strings.Contains(statusContainerID, "://") {
		statusContainerID = "containerd://" + statusContainerID
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			UID:             types.UID("uid-" + name),
			OwnerReferences: owners,
		},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", ContainerID: statusContainerID},
			},
		},
	}
}

func newTestWriter(client *fake.Clientset, pod *corev1.Pod) *StateConfigMapWriter {
	return &StateConfigMapWriter{
		logger:     slog.Default(),
		kubeClient: client,
		ownPod:     pod,
	}
}

func TestFindDaemonSetOwner(t *testing.T) {
	const (
		ns          = "beyla-system"
		node        = "node-1"
		containerID = "abc123"
	)

	dsOwnerRef := metav1.OwnerReference{
		APIVersion: "apps/v1",
		Kind:       "DaemonSet",
		Name:       "beyla",
		UID:        types.UID("ds-uid"),
	}
	rsOwnerRef := metav1.OwnerReference{
		APIVersion: "apps/v1",
		Kind:       "ReplicaSet",
		Name:       "beyla-rs",
		UID:        types.UID("rs-uid"),
	}

	t.Run("returns owner ref for a daemonset-owned pod", func(t *testing.T) {
		pod := makePod("beyla-xyz", ns, node, containerID, []metav1.OwnerReference{dsOwnerRef})
		client := fake.NewSimpleClientset(pod)
		w := newTestWriter(client, pod)

		owner := w.findDaemonSetOwner()
		require.NotNil(t, owner)
		assert.Equal(t, "apps/v1", owner.APIVersion)
		assert.Equal(t, "DaemonSet", owner.Kind)
		assert.Equal(t, "beyla", owner.Name)
		assert.Equal(t, types.UID("ds-uid"), owner.UID)
		require.NotNil(t, owner.Controller)
		assert.True(t, *owner.Controller)
		require.NotNil(t, owner.BlockOwnerDeletion)
		assert.False(t, *owner.BlockOwnerDeletion)
	})

	t.Run("nil owner when pod has no daemonset owner", func(t *testing.T) {
		pod := makePod("beyla-xyz", ns, node, containerID, []metav1.OwnerReference{rsOwnerRef})
		client := fake.NewSimpleClientset(pod)
		w := newTestWriter(client, pod)

		owner := w.findDaemonSetOwner()
		assert.Nil(t, owner)
	})

	t.Run("nil owner when pod has no owner references at all", func(t *testing.T) {
		pod := makePod("beyla-xyz", ns, node, containerID, nil)
		client := fake.NewSimpleClientset(pod)
		w := newTestWriter(client, pod)

		owner := w.findDaemonSetOwner()
		assert.Nil(t, owner)
	})

	t.Run("picks the daemonset owner among multiple owner refs", func(t *testing.T) {
		pod := makePod("beyla-xyz", ns, node, containerID,
			[]metav1.OwnerReference{rsOwnerRef, dsOwnerRef})
		client := fake.NewSimpleClientset(pod)
		w := newTestWriter(client, pod)

		owner := w.findDaemonSetOwner()
		require.NotNil(t, owner)
		assert.Equal(t, "DaemonSet", owner.Kind)
		assert.Equal(t, "beyla", owner.Name)
	})
}

func TestInjectedSDKDoesNotAdvertiseBeylaSpanMetrics(t *testing.T) {
	for _, tc := range []struct {
		name   string
		global export.Features
		app    export.Features
	}{
		{name: "no span metrics", global: export.FeatureApplicationRED},
		{name: "global OTel span metrics", global: export.FeatureSpanOTel},
		{name: "global legacy span metrics", global: export.FeatureSpanLegacy},
		{name: "per-application OTel span metrics", global: export.FeatureApplicationRED, app: export.FeatureSpanOTel},
		{name: "per-application legacy span metrics", global: export.FeatureApplicationRED, app: export.FeatureSpanLegacy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := beyla.DefaultConfig()
			cfg.Injector.Webhook.ExternalWebhook = "system/injector"
			cfg.Metrics.Features = tc.global
			cfg.OTELMetrics.CommonEndpoint = "http://otel:4318"
			cfg.Discovery.Instrument = services.GlobDefinitionCriteria{{Metrics: perapp.SvcMetricsConfig{Features: tc.app}}}

			injection := buildInjectConfig(cfg, "http://otel:4318", "http/protobuf")
			assert.Zero(t, injection.BPFConfig)
			encoded, err := yaml.Marshal(injection)
			require.NoError(t, err)
			assert.NotContains(t, string(encoded), "bpf:")
			assert.NotContains(t, string(encoded), "span.metrics.skip")
		})
	}
}

func TestBuildInjectConfig(t *testing.T) {
	// defaultEnv returns the minimum env var set produced for any rule when no
	// optional fields (propagators, sampler, debug, resources) are set.
	defaultEnv := func(endpoint, protocol string) []corev1.EnvVar {
		return []corev1.EnvVar{
			{Name: "OTEL_EXPORTER_OTLP_ENDPOINT", Value: endpoint},
			{Name: "OTEL_EXPORTER_OTLP_PROTOCOL", Value: protocol},
			{Name: "OTEL_TRACES_EXPORTER", Value: "otlp"},
			{Name: "OTEL_METRICS_EXPORTER", Value: "otlp"},
			{Name: "OTEL_LOGS_EXPORTER", Value: "none"},
		}
	}

	test := services.NewGlob("test")

	tests := []struct {
		name     string
		cfg      beyla.Config
		endpoint string
		protocol string
		want     configmap.InjectConfig
	}{
		{
			name: "exclude_instrument becomes a leading skip rule",
			cfg: beyla.Config{
				Injector: beyla.SDKInject{
					Instrument:        services.GlobDefinitionCriteria{{Metadata: services.MetadataGlobMap{services.AttrDeploymentName: &test}}},
					ExcludeInstrument: services.GlobDefinitionCriteria{{Metadata: services.MetadataGlobMap{services.AttrDaemonSetName: &test}}},
				},
				NameResolver: &transform.NameResolverConfig{},
			},
			endpoint: "http://otel:4318",
			protocol: "http/protobuf",
			want: configmap.InjectConfig{Rules: []configmap.Rule{
				{
					Selector: configmap.K8sSelector{OwnerKinds: []string{"DaemonSet"}, OwnerNames: []services.GlobAttr{test}},
					Config:   configmap.RuleConfig{Mode: configmap.ModeSkip},
				},
				{
					Selector: configmap.K8sSelector{OwnerKinds: []string{"Deployment"}, OwnerNames: []services.GlobAttr{test}},
					Config:   configmap.RuleConfig{Env: defaultEnv("http://otel:4318", "http/protobuf")},
				},
			}},
		},
		{
			name:     "empty instrument yields empty config",
			cfg:      beyla.Config{Injector: beyla.SDKInject{}, NameResolver: &transform.NameResolverConfig{}},
			endpoint: "http://otel:4318",
			protocol: "http/protobuf",
			want:     configmap.InjectConfig{},
		},
		{
			name: "single selector becomes one rule with all default env vars",
			cfg: beyla.Config{
				Injector: beyla.SDKInject{
					Instrument: services.GlobDefinitionCriteria{{Metadata: services.MetadataGlobMap{services.AttrDeploymentName: &test}}},
				},
				NameResolver: &transform.NameResolverConfig{},
			},
			endpoint: "http://otel:4318",
			protocol: "http/protobuf",
			want: configmap.InjectConfig{Rules: []configmap.Rule{{
				Selector: configmap.K8sSelector{OwnerKinds: []string{"Deployment"}, OwnerNames: []services.GlobAttr{test}},
				Config:   configmap.RuleConfig{Env: defaultEnv("http://otel:4318", "http/protobuf")},
			}}},
		},
		{
			name: "multiple selectors each get the same env",
			cfg: beyla.Config{
				Injector: beyla.SDKInject{
					Instrument: services.GlobDefinitionCriteria{
						{Metadata: services.MetadataGlobMap{services.AttrDeploymentName: &test}},
						{Metadata: services.MetadataGlobMap{services.AttrStatefulSetName: &test}},
					},
				},
				NameResolver: &transform.NameResolverConfig{},
			},
			endpoint: "http://otel:4318",
			protocol: "grpc",
			want: configmap.InjectConfig{Rules: []configmap.Rule{
				{Selector: configmap.K8sSelector{OwnerKinds: []string{"Deployment"}, OwnerNames: []services.GlobAttr{test}}, Config: configmap.RuleConfig{Env: defaultEnv("http://otel:4318", "grpc")}},
				{Selector: configmap.K8sSelector{OwnerKinds: []string{"StatefulSet"}, OwnerNames: []services.GlobAttr{test}}, Config: configmap.RuleConfig{Env: defaultEnv("http://otel:4318", "grpc")}},
			}},
		},
		{
			name: "ImageVersion is set at the top level",
			cfg: beyla.Config{
				Injector: beyla.SDKInject{
					ImageVersion: "ghcr.io/grafana/beyla/inject-sdk-image:v1.2.3",
					Instrument:   services.GlobDefinitionCriteria{{Metadata: services.MetadataGlobMap{services.AttrDeploymentName: &test}}},
				},
				NameResolver: &transform.NameResolverConfig{},
			},
			endpoint: "http://otel:4318",
			protocol: "http/protobuf",
			want: configmap.InjectConfig{
				ImageVersion: "ghcr.io/grafana/beyla/inject-sdk-image:v1.2.3",
				Rules: []configmap.Rule{{
					Selector: configmap.K8sSelector{OwnerKinds: []string{"Deployment"}, OwnerNames: []services.GlobAttr{test}},
					Config:   configmap.RuleConfig{Env: defaultEnv("http://otel:4318", "http/protobuf")},
				}},
			},
		},
		{
			name: "propagators written as OTEL_PROPAGATORS",
			cfg: beyla.Config{
				Injector: beyla.SDKInject{
					Instrument:  services.GlobDefinitionCriteria{{Metadata: services.MetadataGlobMap{services.AttrDeploymentName: &test}}},
					Propagators: []string{"tracecontext", "baggage"},
				},
				NameResolver: &transform.NameResolverConfig{},
			},
			endpoint: "http://otel:4318",
			protocol: "http/protobuf",
			want: configmap.InjectConfig{Rules: []configmap.Rule{{
				Selector: configmap.K8sSelector{OwnerKinds: []string{"Deployment"}, OwnerNames: []services.GlobAttr{test}},
				Config: configmap.RuleConfig{Env: append(
					defaultEnv("http://otel:4318", "http/protobuf"),
					corev1.EnvVar{Name: "OTEL_PROPAGATORS", Value: "tracecontext,baggage"},
				)},
			}}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildInjectConfig(&tt.cfg, tt.endpoint, tt.protocol)
			assert.Equal(t, tt.want, got)
		})
	}
}

func newGlobDef(namespace, ownerName, kind string, labels, annotations map[string]string) services.GlobAttributes {
	ptrGlobs := func(m map[string]string) map[string]*services.GlobAttr {
		if m == nil {
			return nil
		}
		out := make(map[string]*services.GlobAttr, len(m))
		for k, v := range m {
			g := services.NewGlob(v)
			out[k] = &g
		}
		return out
	}
	ns := services.NewGlob(namespace)
	owner := services.NewGlob(ownerName)
	metadata := services.MetadataGlobMap{
		services.AttrNamespace: &ns,
	}
	switch kind {
	case "Deployment":
		metadata[services.AttrDeploymentName] = &owner
	case "DaemonSet":
		metadata[services.AttrDaemonSetName] = &owner
	case "ReplicaSet":
		metadata[services.AttrReplicaSetName] = &owner
	case "StatefulSet":
		metadata[services.AttrStatefulSetName] = &owner
	case "Job":
		metadata[services.AttrJobName] = &owner
	case "CronJob":
		metadata[services.AttrCronJobName] = &owner
	case "Pod":
		metadata[services.AttrPodName] = &owner
	default:
		metadata[services.AttrOwnerName] = &owner
	}
	return services.GlobAttributes{
		Metadata:       metadata,
		PodLabels:      ptrGlobs(labels),
		PodAnnotations: ptrGlobs(annotations),
	}
}
