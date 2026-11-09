package sem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.opentelemetry.io/obi/pkg/appolly/app"
	"go.opentelemetry.io/obi/pkg/appolly/app/request"
	"go.opentelemetry.io/obi/pkg/appolly/app/svc"
	"go.opentelemetry.io/obi/pkg/appolly/discover/exec"
	attr "go.opentelemetry.io/obi/pkg/export/attributes/names"
	"go.opentelemetry.io/obi/pkg/kube/kubecache/informer"
	"go.opentelemetry.io/obi/pkg/pipe/msg"
)

const testHost = "host1"

var testCfg = (&SemanticConfig{RefreshInterval: time.Second}).withDefaults()

func podEvent(eventType informer.EventType, ns, name string, owners ...*informer.Owner) *informer.Event {
	return &informer.Event{Type: eventType, Resource: &informer.ObjectMeta{
		Name: name, Namespace: ns, Kind: "Pod",
		Pod: &informer.PodInfo{Uid: "uid-" + name, NodeName: "node1", Owners: owners},
	}}
}

func deploymentOwners(deploy string) []*informer.Owner {
	return []*informer.Owner{
		{Kind: "ReplicaSet", Name: deploy + "-1234"},
		{Kind: "Deployment", Name: deploy},
	}
}

// kubeService returns the attributes of a service running in a Pod owned by a Deployment
func kubeService(name, ns, kubeNs, pod, deploy string) svc.Attrs {
	return kubeServiceOwner(name, ns, kubeNs, pod, "Deployment", deploy)
}

// kubeServiceOwner returns the attributes of a service as decorated by OBI, where ownerKind and
// ownerName are the top-level owner of the Pod (the Pod itself if it does not have any owner)
func kubeServiceOwner(name, ns, kubeNs, pod, ownerKind, ownerName string) svc.Attrs {
	return svc.Attrs{
		UID:         svc.UID{Name: name, Namespace: ns, Instance: kubeNs + "." + pod + "." + name},
		SDKLanguage: svc.InstrumentableGolang,
		HostName:    pod,
		Metadata: map[attr.Name]string{
			attr.K8sNamespaceName: kubeNs,
			attr.K8sPodName:       pod,
			attr.K8sKind:          ownerKind,
			attr.K8sOwnerName:     ownerName,
		},
	}
}

func processEvent(eventType exec.ProcessEventType, pid int, attrs svc.Attrs) *exec.ProcessEvent {
	return &exec.ProcessEvent{Type: eventType, File: exec.New(exec.Init{
		Service:    attrs,
		CmdExePath: "/usr/bin/server",
		Pid:        app.PID(pid),
		Ppid:       1,
	})}
}

func relationsOf(g *graph) []relationKey { return g.sortedRelations() }

// withoutNodes returns a copy of the graph without the Node entities and their relations,
// for the tests that do not focus on them
func withoutNodes(g *graph) *graph {
	out := newGraph()
	for e, props := range g.entities {
		if e.Type != typeNode {
			out.entities[e] = props
		}
	}
	for r := range g.relations {
		if r.From.Type != typeNode && r.To.Type != typeNode {
			out.relations[r] = struct{}{}
		}
	}
	return out
}

// testHostName is the host name of Beyla in the tests
const testHostName = "localnode"

// newTestExporter returns a semantic exporter that does not depend on the host name of the machine running the tests
func newTestExporter(cfg *SemanticConfig, kg graphWriter, hostID string) *semanticExporter {
	se := newSemanticExporter(cfg, kg, hostID)
	se.host.name = testHostName
	return se
}

// ek returns the key of an entity that is not in a Kubernetes namespace
func ek(t, n string) entityKey { return entityKey{Type: t, Env: defaultEnvironment, Name: n} }

// ekn returns the key of an entity in a Kubernetes namespace
func ekn(t, ns, n string) entityKey {
	return entityKey{Type: t, Env: defaultEnvironment, Namespace: ns, Name: n}
}

func TestBuildGraph(t *testing.T) {
	se := newTestExporter(&testCfg, &fakeWriter{}, testHost)
	attrs := kubeService("frontend", "shop", "default", "frontend-1234-abcd", "frontend")

	require.NoError(t, se.On(podEvent(informer.EventType_CREATED, "default", "frontend-1234-abcd", deploymentOwners("frontend")...)))
	se.onProcessEvent(processEvent(exec.ProcessEventCreated, 33, attrs))
	attrs.ProcPID = 33
	se.onSpans([]request.Span{{Service: attrs}})

	g := se.buildGraph()

	deploy := ekn(typeDeployment, "default", "frontend")
	pod := ekn(typePod, "default", "frontend-1234-abcd")
	proc := ekn(typeProcess, "default", "default.frontend-1234-abcd.frontend")
	service := ekn(typeService, "default", "frontend")
	// the OTEL service namespace is not tied to any Kubernetes namespace
	serviceNs := ek(typeServiceNamespace, "shop")
	node := ek(typeNode, "node1")
	assert.Equal(t, []entityKey{deploy, node, pod, proc, service, serviceNs}, g.sortedEntities())

	assert.Equal(t, []relationKey{
		{Type: relControls, From: deploy, To: pod},
		{Type: relExecutes, From: proc, To: service},
		{Type: relHasMember, From: serviceNs, To: service},
		{Type: relHosts, From: deploy, To: service},
		{Type: relHosts, From: node, To: pod},
		{Type: relHosts, From: node, To: proc},
		{Type: relHosts, From: pod, To: proc},
		{Type: relHosts, From: pod, To: service},
	}, relationsOf(g))

	assert.Equal(t, properties{
		"pid": int64(33), "ppid": int64(1), "executable_path": "/usr/bin/server", "process_executable_name": "server",
		"host_id": testHost, "host_name": "frontend-1234-abcd",
	}, g.entities[proc])
	assert.Equal(t, properties{
		"uid": "uid-frontend-1234-abcd", "node": "node1", "workload": "frontend", "workload_type": "Deployment",
	}, g.entities[pod])
	assert.Equal(t, properties{
		"otel_service": "frontend", "otel_namespace": "shop", "telemetry_sdk_language": "go",
	}, g.entities[service])

	// the Knowledge Graph rejects the whole batch if any property uses a reserved identity key
	for e, props := range g.entities {
		for k := range props {
			assert.NotContains(t, reservedPropertyKeys, k, "entity %v uses reserved property key %q", e, k)
		}
	}
}

func TestBuildGraph_WorkloadExecutesService(t *testing.T) {
	se := newTestExporter(&testCfg, &fakeWriter{}, testHost)
	// a standalone pod, without owners
	require.NoError(t, se.On(podEvent(informer.EventType_CREATED, "default", "standalone",
		&informer.Owner{Kind: "Pod", Name: "standalone"})))
	// a pod owned by a StatefulSet
	require.NoError(t, se.On(podEvent(informer.EventType_CREATED, "default", "db-0",
		&informer.Owner{Kind: "StatefulSet", Name: "db"})))
	// a pod owned by a DaemonSet
	require.NoError(t, se.On(podEvent(informer.EventType_CREATED, "default", "agent-x1",
		&informer.Owner{Kind: "DaemonSet", Name: "agent"})))
	// a pod owned by a Job, which is owned by a CronJob
	require.NoError(t, se.On(podEvent(informer.EventType_CREATED, "default", "backup-123-abc",
		&informer.Owner{Kind: "Job", Name: "backup-123"}, &informer.Owner{Kind: "CronJob", Name: "backup"})))
	// a pod owned by a non-workload object: it has no workload entity, but it still hosts the service
	require.NoError(t, se.On(podEvent(informer.EventType_CREATED, "default", "custom-1",
		&informer.Owner{Kind: "Rollout", Name: "custom"})))
	se.onSpans([]request.Span{
		{Service: kubeServiceOwner("standalone", "", "default", "standalone", "Pod", "standalone")},
		{Service: kubeServiceOwner("db", "", "default", "db-0", "StatefulSet", "db")},
		{Service: kubeServiceOwner("agent", "", "default", "agent-x1", "DaemonSet", "agent")},
		{Service: kubeServiceOwner("backup", "", "default", "backup-123-abc", "CronJob", "backup")},
		{Service: kubeServiceOwner("custom", "", "default", "custom-1", "Rollout", "custom")},
	})

	g := withoutNodes(se.buildGraph())
	k := func(t, n string) entityKey { return ekn(t, "default", n) }
	assert.Equal(t, []entityKey{
		k(typeCronJob, "backup"),
		k(typeDaemonSet, "agent"),
		k(typeJob, "backup-123"),
		k(typePod, "agent-x1"),
		k(typePod, "backup-123-abc"),
		k(typePod, "custom-1"),
		k(typePod, "db-0"),
		k(typePod, "standalone"),
		k(typeService, "agent"),
		k(typeService, "backup"),
		k(typeService, "custom"),
		k(typeService, "db"),
		k(typeService, "standalone"),
		k(typeStatefulSet, "db"),
	}, g.sortedEntities())
	assert.Equal(t, []relationKey{
		{Type: relControls, From: k(typeCronJob, "backup"), To: k(typeJob, "backup-123")},
		{Type: relControls, From: k(typeDaemonSet, "agent"), To: k(typePod, "agent-x1")},
		{Type: relControls, From: k(typeJob, "backup-123"), To: k(typePod, "backup-123-abc")},
		{Type: relControls, From: k(typeStatefulSet, "db"), To: k(typePod, "db-0")},
		// the top-level workload hosts the service
		{Type: relHosts, From: k(typeCronJob, "backup"), To: k(typeService, "backup")},
		{Type: relHosts, From: k(typeDaemonSet, "agent"), To: k(typeService, "agent")},
		// every pod hosts its services
		{Type: relHosts, From: k(typePod, "agent-x1"), To: k(typeService, "agent")},
		{Type: relHosts, From: k(typePod, "backup-123-abc"), To: k(typeService, "backup")},
		{Type: relHosts, From: k(typePod, "custom-1"), To: k(typeService, "custom")},
		{Type: relHosts, From: k(typePod, "db-0"), To: k(typeService, "db")},
		{Type: relHosts, From: k(typePod, "standalone"), To: k(typeService, "standalone")},
		{Type: relHosts, From: k(typeStatefulSet, "db"), To: k(typeService, "db")},
	}, relationsOf(g))

	// the workload properties refer to the top-level owner, even if it is not a workload entity
	assert.Equal(t, properties{"uid": "uid-custom-1", "node": "node1", "workload": "custom", "workload_type": "Rollout"},
		g.entities[k(typePod, "custom-1")])
	assert.Equal(t, properties{"uid": "uid-backup-123-abc", "node": "node1", "workload": "backup", "workload_type": "CronJob"},
		g.entities[k(typePod, "backup-123-abc")])
	assert.Equal(t, properties{"uid": "uid-standalone", "node": "node1"}, g.entities[k(typePod, "standalone")])
}

func TestBuildGraph_Environment(t *testing.T) {
	cfg := (&SemanticConfig{Environment: "staging"}).withDefaults()
	se := newTestExporter(&cfg, &fakeWriter{}, testHost)

	// the environment of a pod is taken from the resource attributes in the annotations and
	// the containers OTEL_RESOURCE_ATTRIBUTES, which takes precedence
	annotated := podEvent(informer.EventType_CREATED, "default", "annotated-abc", deploymentOwners("annotated")...)
	annotated.Resource.Annotations = map[string]string{"resource.opentelemetry.io/deployment.environment": "qa"}
	require.NoError(t, se.On(annotated))
	withEnv := podEvent(informer.EventType_CREATED, "default", "env-abc")
	withEnv.Resource.Annotations = map[string]string{"resource.opentelemetry.io/deployment.environment.name": "qa"}
	withEnv.Resource.Pod.Containers = []*informer.ContainerInfo{{Name: "c", Env: map[string]string{
		"OTEL_RESOURCE_ATTRIBUTES": "foo=bar,deployment.environment.name=prod",
	}}}
	require.NoError(t, se.On(withEnv))
	// pods without environment take the configured one
	require.NoError(t, se.On(podEvent(informer.EventType_CREATED, "default", "plain-abc")))

	// the service environment is taken from its own resource attributes. It can differ from the pod's one,
	// so the relation must refer to the pod as known by the informer
	attrs := kubeService("annotated", "", "default", "annotated-abc", "annotated")
	attrs.Metadata["deployment.environment.name"] = "prod"
	plain := kubeService("plain", "", "default", "plain-abc", "")
	se.onSpans([]request.Span{{Service: attrs}, {Service: plain}})

	g := withoutNodes(se.buildGraph())
	key := func(t, env, n string) entityKey { return entityKey{Type: t, Env: env, Namespace: "default", Name: n} }
	assert.Equal(t, []entityKey{
		key(typeDeployment, "qa", "annotated"),
		key(typePod, "prod", "env-abc"),
		key(typePod, "qa", "annotated-abc"),
		key(typePod, "staging", "plain-abc"),
		key(typeService, "prod", "annotated"),
		key(typeService, "staging", "plain"),
	}, g.sortedEntities())
	assert.Equal(t, []relationKey{
		{Type: relControls, From: key(typeDeployment, "qa", "annotated"), To: key(typePod, "qa", "annotated-abc")},
		{Type: relHosts, From: key(typeDeployment, "qa", "annotated"), To: key(typeService, "prod", "annotated")},
		{Type: relHosts, From: key(typePod, "qa", "annotated-abc"), To: key(typeService, "prod", "annotated")},
		{Type: relHosts, From: key(typePod, "staging", "plain-abc"), To: key(typeService, "staging", "plain")},
	}, relationsOf(g))
}

func TestBuildGraph_ServiceNames(t *testing.T) {
	se := newTestExporter(&testCfg, &fakeWriter{}, testHost)
	require.NoError(t, se.On(podEvent(informer.EventType_CREATED, "tapas", "tapas-backend-abc", deploymentOwners("tapas-backend")...)))
	se.onSpans([]request.Span{
		// in Kubernetes, the OTEL service namespace defaults to the Kubernetes namespace. The name
		// must not include the namespace, which is already in the scope
		{Service: kubeService("tapas-backend", "tapas", "tapas", "tapas-backend-abc", "tapas-backend")},
		// out of Kubernetes, the OTEL service namespace is the namespace scope key
		{Service: svc.Attrs{UID: svc.UID{Name: "api", Namespace: "shop"}}},
		{Service: svc.Attrs{UID: svc.UID{Name: "api", Namespace: "billing"}}},
		{Service: svc.Attrs{UID: svc.UID{Name: "api"}}},
	})

	g := withoutNodes(se.buildGraph())
	var services []entityKey
	for _, e := range g.sortedEntities() {
		if e.Type == typeService {
			services = append(services, e)
		}
	}
	assert.Equal(t, []entityKey{
		ek(typeService, "api"),
		ekn(typeService, "billing", "api"),
		ekn(typeService, "shop", "api"),
		ekn(typeService, "tapas", "tapas-backend"),
	}, services)
}

func TestBuildGraph_Nodes(t *testing.T) {
	cfg := (&SemanticConfig{Environment: "staging"}).withDefaults()
	se := newTestExporter(&cfg, &fakeWriter{}, testHost)

	// a pod in node2, whose environment differs from the default one
	pod := podEvent(informer.EventType_CREATED, "default", "api-abc", deploymentOwners("api")...)
	pod.Resource.Pod.NodeName = "node2"
	pod.Resource.Annotations = map[string]string{"resource.opentelemetry.io/deployment.environment.name": "prod"}
	require.NoError(t, se.On(pod))
	se.onProcessEvent(processEvent(exec.ProcessEventCreated, 10, kubeService("api", "", "default", "api-abc", "api")))
	// a Kubernetes process whose pod is not known yet: we don't know its node
	se.onProcessEvent(processEvent(exec.ProcessEventCreated, 11, kubeService("unknown", "", "default", "unknown-abc", "unknown")))
	// processes out of Kubernetes run in the same host as Beyla
	se.onProcessEvent(processEvent(exec.ProcessEventCreated, 12,
		svc.Attrs{UID: svc.UID{Name: "local", Instance: "myhost:12"}, HostName: "myhost"}))
	se.onProcessEvent(processEvent(exec.ProcessEventCreated, 13,
		svc.Attrs{UID: svc.UID{Name: "nohost", Instance: "x:13"}}))

	g := se.buildGraph()
	// nodes are shared by entities from different environments and namespaces,
	// so they take the default environment and no namespace
	node2 := entityKey{Type: typeNode, Env: "staging", Name: "node2"}
	myhost := entityKey{Type: typeNode, Env: "staging", Name: "myhost"}
	localnode := entityKey{Type: typeNode, Env: "staging", Name: testHostName}
	var nodeRelations []relationKey
	for _, r := range g.sortedRelations() {
		if r.From.Type == typeNode {
			nodeRelations = append(nodeRelations, r)
		}
	}
	assert.Equal(t, []relationKey{
		{Type: relHosts, From: localnode, To: entityKey{Type: typeProcess, Env: "staging", Name: "x:13"}},
		{Type: relHosts, From: myhost, To: entityKey{Type: typeProcess, Env: "staging", Name: "myhost:12"}},
		{Type: relHosts, From: node2, To: entityKey{Type: typePod, Env: "prod", Namespace: "default", Name: "api-abc"}},
		{Type: relHosts, From: node2, To: entityKey{Type: typeProcess, Env: "staging", Namespace: "default", Name: "default.api-abc.api"}},
	}, nodeRelations)
	assert.Equal(t, properties{"host_id": testHost}, g.entities[myhost])
	assert.Empty(t, g.entities[node2])
}

func TestBuildGraph_ProcessNames(t *testing.T) {
	se := newTestExporter(&testCfg, &fakeWriter{}, testHost)
	// two processes from the same executable and service, outside Kubernetes
	se.onProcessEvent(processEvent(exec.ProcessEventCreated, 10,
		svc.Attrs{UID: svc.UID{Name: "worker", Instance: "myhost:10"}}))
	se.onProcessEvent(processEvent(exec.ProcessEventCreated, 11,
		svc.Attrs{UID: svc.UID{Name: "worker", Instance: "myhost:11"}}))
	// unknown instance ID falls back to the host ID and PID
	se.onProcessEvent(processEvent(exec.ProcessEventCreated, 12,
		svc.Attrs{UID: svc.UID{Name: "worker"}}))

	g := withoutNodes(se.buildGraph())
	assert.Equal(t, []entityKey{
		ek(typeProcess, "host1/12"),
		ek(typeProcess, "myhost:10"),
		ek(typeProcess, "myhost:11"),
		ek(typeService, "worker"),
	}, g.sortedEntities())
	assert.Equal(t, []relationKey{
		{Type: relExecutes, From: ek(typeProcess, "host1/12"), To: ek(typeService, "worker")},
		{Type: relExecutes, From: ek(typeProcess, "myhost:10"), To: ek(typeService, "worker")},
		{Type: relExecutes, From: ek(typeProcess, "myhost:11"), To: ek(typeService, "worker")},
	}, relationsOf(g))
}

func TestBuildGraph_SpansOnly_NoKubernetes(t *testing.T) {
	se := newTestExporter(&testCfg, &fakeWriter{}, testHost)
	// the service decorates with Kubernetes metadata, but the pod is not known by the informer
	// (e.g. it is still being synchronized), so no dangling relations must be submitted
	attrs := kubeService("backend", "", "default", "backend-abcd", "backend")
	attrs.ProcPID = 44
	se.onSpans([]request.Span{{Service: attrs}, {Service: attrs}})

	g := withoutNodes(se.buildGraph())
	assert.Equal(t, []entityKey{ekn(typeService, "default", "backend")}, g.sortedEntities())
	assert.Empty(t, relationsOf(g))
}

func TestBuildGraph_PodWithoutDeployment(t *testing.T) {
	se := newTestExporter(&testCfg, &fakeWriter{}, testHost)
	require.NoError(t, se.On(podEvent(informer.EventType_CREATED, "kube-system", "standalone",
		&informer.Owner{Kind: "Pod", Name: "standalone"})))
	// non-pod events and finished pods are ignored
	require.NoError(t, se.On(&informer.Event{Type: informer.EventType_CREATED,
		Resource: &informer.ObjectMeta{Name: "svc", Namespace: "default", Kind: "Service"}}))
	finished := podEvent(informer.EventType_CREATED, "default", "finished")
	finished.Resource.Pod.Finished = true
	require.NoError(t, se.On(finished))

	g := withoutNodes(se.buildGraph())
	assert.Equal(t, []entityKey{ekn(typePod, "kube-system", "standalone")}, g.sortedEntities())
	assert.Empty(t, relationsOf(g))
}

func TestServicesExpireAfterTTL(t *testing.T) {
	now := time.Unix(1000, 0)
	se := newTestExporter(&testCfg, &fakeWriter{}, testHost)
	se.clock = func() time.Time { return now }

	se.onSpans([]request.Span{{Service: svc.Attrs{UID: svc.UID{Name: "svc1"}}}})
	now = now.Add(testCfg.TTL / 2)
	se.onSpans([]request.Span{{Service: svc.Attrs{UID: svc.UID{Name: "svc2"}}}})
	assert.Len(t, se.buildGraph().entities, 2)

	now = now.Add(testCfg.TTL/2 + time.Second)
	assert.Equal(t, []entityKey{ek(typeService, "svc2")}, se.buildGraph().sortedEntities())

	// seeing it again in the spans refreshes it
	se.onSpans([]request.Span{{Service: svc.Attrs{UID: svc.UID{Name: "svc2"}}}})
	now = now.Add(testCfg.TTL / 2)
	assert.Equal(t, []entityKey{ek(typeService, "svc2")}, se.buildGraph().sortedEntities())
}

func TestFlush_DeletesOnlyVolatileEntities(t *testing.T) {
	fw := &fakeWriter{}
	se := newTestExporter(&testCfg, fw, testHost)
	attrs := kubeService("frontend", "shop", "default", "frontend-1234-abcd", "frontend")
	require.NoError(t, se.On(podEvent(informer.EventType_CREATED, "default", "frontend-1234-abcd", deploymentOwners("frontend")...)))
	require.NoError(t, se.On(podEvent(informer.EventType_CREATED, "default", "frontend-1234-efgh", deploymentOwners("frontend")...)))
	se.onProcessEvent(processEvent(exec.ProcessEventCreated, 33, attrs))

	se.flush(t.Context())
	require.Len(t, fw.written, 1)
	assert.Empty(t, fw.deletedEntities)
	assert.Empty(t, fw.deletedRelations)

	// the pod is removed and the process terminates
	require.NoError(t, se.On(podEvent(informer.EventType_DELETED, "default", "frontend-1234-abcd")))
	se.onProcessEvent(processEvent(exec.ProcessEventTerminated, 33, attrs))

	se.flush(t.Context())
	require.Len(t, fw.written, 2)
	pod := ekn(typePod, "default", "frontend-1234-abcd")
	proc := ekn(typeProcess, "default", "default.frontend-1234-abcd.frontend")
	service := ekn(typeService, "default", "frontend")
	// Deployment, Node, Service and ServiceNamespace are not explicitly deleted: they will expire by TTL
	assert.Equal(t, []entityKey{pod, proc}, fw.deletedEntities)
	assert.Equal(t, []relationKey{
		{Type: relControls, From: ekn(typeDeployment, "default", "frontend"), To: pod},
		{Type: relExecutes, From: proc, To: service},
		{Type: relHosts, From: ek(typeNode, "node1"), To: pod},
		{Type: relHosts, From: ek(typeNode, "node1"), To: proc},
		{Type: relHosts, From: pod, To: proc},
		{Type: relHosts, From: pod, To: service},
	}, fw.deletedRelations)
	// the remaining pod is still submitted
	assert.Contains(t, fw.written[1].entities, ekn(typePod, "default", "frontend-1234-efgh"))
	assert.Contains(t, fw.written[1].entities, ekn(typeDeployment, "default", "frontend"))
}

func TestRun_ConsumesQueues(t *testing.T) {
	fw := &fakeWriter{}
	cfg := (&SemanticConfig{RefreshInterval: 10 * time.Millisecond}).withDefaults()
	spans := msg.NewQueue[[]request.Span](msg.ChannelBufferLen(10))
	procs := msg.NewQueue[exec.ProcessEvent](msg.ChannelBufferLen(10))

	se := newTestExporter(&cfg, fw, testHost)
	se.spans = spans.Subscribe()
	se.processEvents = procs.Subscribe()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		se.run(ctx)
		close(done)
	}()

	procs.SendCtx(ctx, *processEvent(exec.ProcessEventCreated, 12, svc.Attrs{UID: svc.UID{Name: "svc"}}))
	spans.SendCtx(ctx, []request.Span{{Service: svc.Attrs{UID: svc.UID{Name: "other"}, ProcPID: 12}}})

	assert.EventuallyWithT(t, func(ct *assert.CollectT) {
		g := fw.lastWritten()
		require.NotNil(ct, g)
		assert.Equal(ct, []relationKey{
			{Type: relExecutes, From: ek(typeProcess, "host1/12"), To: ek(typeService, "other")},
			{Type: relExecutes, From: ek(typeProcess, "host1/12"), To: ek(typeService, "svc")},
		}, withoutNodes(g).sortedRelations())
	}, 5*time.Second, 10*time.Millisecond)

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("semantic exporter did not stop")
	}
}

func TestToKGProperties_SkipsReservedKeys(t *testing.T) {
	out := toKGProperties(properties{"name": "foo", "type": "bar", "pid": int64(3), "podName": "foo"})
	assert.Len(t, out, 2)
	assert.Contains(t, out, "pid")
	assert.Contains(t, out, "podName")
}

func TestKnowledgeGraph_ScopeFor(t *testing.T) {
	kg := &knowledgeGraph{scope: commonScope(&SemanticConfig{Site: "eu-west"})}
	assert.Equal(t, map[string]string{"site": "eu-west", "env": "unknown"},
		kg.scopeFor(ek(typeServiceNamespace, "otelns")))
	assert.Equal(t, map[string]string{"site": "eu-west", "env": "prod", "namespace": "ns"},
		kg.scopeFor(entityKey{Type: typePod, Env: "prod", Namespace: "ns", Name: "pod"}))
	// the common scope is not modified
	assert.Equal(t, map[string]string{"site": "eu-west"}, kg.scope)

	// no site by default
	assert.Empty(t, commonScope(&SemanticConfig{}))
}

func TestKnowledgeGraph_WriteGraphBatches(t *testing.T) {
	var mt sync.Mutex
	var requests []graphRequest
	srv := httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		assert.Equal(t, "/apis/kg.grafana.com/v1alpha1/namespaces/stacks-1234/graph", req.URL.Path)
		assert.Equal(t, "1234", req.Header.Get("X-Scope-OrgID"))
		assert.Equal(t, "true", req.URL.Query().Get("allowUndeclared"))
		var gr graphRequest
		assert.NoError(t, json.NewDecoder(req.Body).Decode(&gr))
		mt.Lock()
		requests = append(requests, gr)
		mt.Unlock()
		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write([]byte(`{}`))
	}))
	defer srv.Close()

	kg, err := createKnowledgeGraph((&SemanticConfig{
		CellGatewayURL: srv.URL, StackID: "1234", GrafanaGCOMToken: "test-token",
	}).withDefaults())
	require.NoError(t, err)
	kg.client.GetConfig().HTTPClient = srv.Client()

	g := newGraph()
	for i := range maxBatchItems + 10 {
		g.addPod(&podInfo{
			env: "prod", namespace: "ns", name: fmt.Sprintf("pod%d", i), uid: fmt.Sprintf("uid%d", i),
			workloads: []entityKey{{Type: typeDeployment, Env: "prod", Namespace: "ns", Name: fmt.Sprintf("deploy%d", i)}},
		}, "prod")
	}
	// an entity out of any Kubernetes namespace
	g.addEntity(serviceNamespaceKey("prod", "otelns"), nil)
	g.pruneDanglingRelations()
	require.NoError(t, kg.writeGraph(t.Context(), g))

	// 1021 entities (510 pods + 510 deployments + 1 service namespace) and 510 relations: 5 batches
	require.Len(t, requests, 5)
	assert.Len(t, requests[0].Entities, maxBatchItems)
	assert.Len(t, requests[1].Entities, maxBatchItems)
	// pending entities are flushed before starting submitting relations
	assert.Len(t, requests[2].Entities, 21)
	for i := range 3 {
		assert.Empty(t, requests[i].Relationships)
	}
	assert.Empty(t, requests[3].Entities)
	assert.Len(t, requests[3].Relationships, maxBatchItems)
	assert.Empty(t, requests[4].Entities)
	assert.Len(t, requests[4].Relationships, 10)

	// entities are sorted by type, namespace and name
	deploy := requests[0].Entities[0]
	assert.Equal(t, "beyla", deploy.Domain)
	assert.Equal(t, typeDeployment, deploy.Type)
	assert.Equal(t, "deploy0", deploy.Name)
	assert.Equal(t, map[string]string{"env": "prod", "namespace": "ns"}, deploy.Scope)
	assert.Equal(t, int64(90), deploy.TTLSeconds)
	assert.Empty(t, deploy.Properties)

	pod := requests[1].Entities[10]
	assert.Equal(t, typePod, pod.Type)
	assert.Equal(t, "pod0", pod.Name)
	assert.Equal(t, map[string]string{"env": "prod", "namespace": "ns"}, pod.Scope)
	assert.Equal(t, map[string]any{"uid": "uid0"}, pod.Properties)

	serviceNs := requests[2].Entities[20]
	assert.Equal(t, typeServiceNamespace, serviceNs.Type)
	assert.Equal(t, "otelns", serviceNs.Name)
	assert.Equal(t, map[string]string{"env": "prod"}, serviceNs.Scope)

	rel := requests[3].Relationships[0]
	assert.Equal(t, relControls, rel.Type)
	assert.Equal(t, graphRef{Domain: "beyla", Type: typeDeployment, Name: "deploy0",
		Scope: map[string]string{"env": "prod", "namespace": "ns"}}, rel.From)
	assert.Equal(t, graphRef{Domain: "beyla", Type: typePod, Name: "pod0",
		Scope: map[string]string{"env": "prod", "namespace": "ns"}}, rel.To)
}

func TestKnowledgeGraph_TelemetryOwnedEntities(t *testing.T) {
	var mt sync.Mutex
	var requests []graphRequest
	var otherRequests []string
	// entities that already exist in the telemetry domain, by name
	telemetryEntities := map[string]bool{"tapas-backend": true}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		mt.Lock()
		defer mt.Unlock()
		rw.Header().Set("Content-Type", "application/json")
		if !strings.HasSuffix(req.URL.Path, "/graph") {
			otherRequests = append(otherRequests, req.Method+" "+req.URL.Path)
			rw.WriteHeader(http.StatusNoContent)
			return
		}
		var gr graphRequest
		assert.NoError(t, json.NewDecoder(req.Body).Decode(&gr))
		requests = append(requests, gr)
		for i, e := range gr.Entities {
			if telemetryEntities[e.Name] {
				rw.WriteHeader(http.StatusConflict)
				_, _ = fmt.Fprintf(rw, `{"status":"CONFLICT","message":"entities[%d]: an entity with this (type, name, scope) exists in the default domain"}`, i)
				return
			}
		}
		_, _ = rw.Write([]byte(`{}`))
	}))
	defer srv.Close()

	kg, err := createKnowledgeGraph((&SemanticConfig{
		CellGatewayURL: srv.URL, StackID: "1234", GrafanaGCOMToken: "test-token",
	}).withDefaults())
	require.NoError(t, err)
	kg.client.GetConfig().HTTPClient = srv.Client()
	now := time.Unix(1000, 0)
	kg.clock = func() time.Time { return now }

	g := newGraph()
	deploy := g.addEntity(ekn(typeDeployment, "tapas", "tapas-db"), nil)
	service := g.addEntity(ekn(typeService, "tapas", "tapas-backend"), properties{"otel_service": "tapas-backend"})
	g.addRelation(relHosts, deploy, service)

	names := func(gr graphRequest) []string {
		var n []string
		for _, e := range gr.Entities {
			n = append(n, e.Name)
		}
		return n
	}

	// the conflicting entity is removed from the batch, which is retried
	require.NoError(t, kg.writeGraph(t.Context(), g))
	require.Len(t, requests, 3)
	assert.Equal(t, []string{"tapas-db", "tapas-backend"}, names(requests[0]))
	assert.Equal(t, []string{"tapas-db"}, names(requests[1]))
	// the relationship refers to the telemetry domain entity
	require.Len(t, requests[2].Relationships, 1)
	rel := requests[2].Relationships[0]
	assert.Equal(t, graphRef{Domain: "beyla", Type: typeDeployment, Name: "tapas-db",
		Scope: map[string]string{"env": "unknown", "namespace": "tapas"}}, rel.From)
	assert.Equal(t, graphRef{Domain: "metrics", Type: typeService, Name: "tapas-backend",
		Scope: map[string]string{"env": "unknown", "namespace": "tapas"}}, rel.To)

	// in the next submissions, the telemetry entity is not written anymore
	requests = nil
	require.NoError(t, kg.writeGraph(t.Context(), g))
	require.Len(t, requests, 2)
	assert.Equal(t, []string{"tapas-db"}, names(requests[0]))
	assert.Equal(t, "metrics", requests[1].Relationships[0].To.Domain)

	// after some time, Beyla checks again whether the entity is still in the telemetry domain
	now = now.Add(telemetryOwnershipRecheck + time.Second)
	requests = nil
	require.NoError(t, kg.writeGraph(t.Context(), g))
	require.Len(t, requests, 3)
	assert.Equal(t, []string{"tapas-db", "tapas-backend"}, names(requests[0]))

	// if it does not exist anymore in the telemetry domain, Beyla writes it in its own domain
	mt.Lock()
	delete(telemetryEntities, "tapas-backend")
	mt.Unlock()
	now = now.Add(telemetryOwnershipRecheck + time.Second)
	requests = nil
	require.NoError(t, kg.writeGraph(t.Context(), g))
	require.Len(t, requests, 2)
	assert.Equal(t, []string{"tapas-db", "tapas-backend"}, names(requests[0]))
	assert.Equal(t, "beyla", requests[1].Relationships[0].To.Domain)

	// Beyla never deletes the telemetry entities
	now = now.Add(-telemetryOwnershipRecheck)
	kg.telemetryOwned[service] = now
	require.NoError(t, kg.deleteEntity(t.Context(), service))
	assert.Empty(t, otherRequests)
	require.NoError(t, kg.deleteEntity(t.Context(), deploy))
	assert.Len(t, otherRequests, 1)
}

func TestTelemetryConflictIndex(t *testing.T) {
	idx, ok := telemetryConflictIndexFromMessage(
		"entities[30]: an entity with this (type, name, scope) exists in the default domain")
	assert.True(t, ok)
	assert.Equal(t, 30, idx)
	_, ok = telemetryConflictIndexFromMessage("graph writes are paused for a graph migration")
	assert.False(t, ok)
	_, ok = telemetryConflictIndex(errors.New("connection refused"))
	assert.False(t, ok)
}

type graphRequest struct {
	Entities      []graphItem `json:"entities"`
	Relationships []graphRel  `json:"relationships"`
}

type graphRel struct {
	Type string   `json:"type"`
	From graphRef `json:"from"`
	To   graphRef `json:"to"`
}

type graphRef struct {
	Domain string            `json:"domain"`
	Type   string            `json:"type"`
	Name   string            `json:"name"`
	Scope  map[string]string `json:"scope"`
}

type graphItem struct {
	Domain     string            `json:"domain"`
	Type       string            `json:"type"`
	Name       string            `json:"name"`
	Scope      map[string]string `json:"scope"`
	Properties map[string]any    `json:"properties"`
	TTLSeconds int64             `json:"ttlSeconds"`
}

type fakeWriter struct {
	mt               sync.Mutex
	written          []*graph
	deletedEntities  []entityKey
	deletedRelations []relationKey
}

func (f *fakeWriter) lastWritten() *graph {
	f.mt.Lock()
	defer f.mt.Unlock()
	if len(f.written) == 0 {
		return nil
	}
	return f.written[len(f.written)-1]
}

func (f *fakeWriter) writeGraph(_ context.Context, g *graph) error {
	f.mt.Lock()
	defer f.mt.Unlock()
	f.written = append(f.written, g)
	return nil
}

func (f *fakeWriter) deleteEntity(_ context.Context, e entityKey) error {
	f.mt.Lock()
	defer f.mt.Unlock()
	f.deletedEntities = append(f.deletedEntities, e)
	return nil
}

func (f *fakeWriter) deleteRelationship(_ context.Context, r *relationKey) error {
	f.mt.Lock()
	defer f.mt.Unlock()
	f.deletedRelations = append(f.deletedRelations, *r)
	return nil
}
