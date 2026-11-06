package sem

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

func kubeService(name, ns, kubeNs, pod, deploy string) svc.Attrs {
	return svc.Attrs{
		UID:         svc.UID{Name: name, Namespace: ns, Instance: kubeNs + "." + pod + "." + name},
		SDKLanguage: svc.InstrumentableGolang,
		HostName:    pod,
		Metadata: map[attr.Name]string{
			attr.K8sNamespaceName:  kubeNs,
			attr.K8sPodName:        pod,
			attr.K8sDeploymentName: deploy,
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

func ek(t, n string) entityKey { return entityKey{Type: t, Name: n} }

func TestBuildGraph(t *testing.T) {
	se := newSemanticExporter(&testCfg, &fakeWriter{}, testHost)
	attrs := kubeService("frontend", "shop", "default", "frontend-1234-abcd", "frontend")

	require.NoError(t, se.On(podEvent(informer.EventType_CREATED, "default", "frontend-1234-abcd", deploymentOwners("frontend")...)))
	se.onProcessEvent(processEvent(exec.ProcessEventCreated, 33, attrs))
	attrs.ProcPID = 33
	se.onSpans([]request.Span{{Service: attrs}})

	g := se.buildGraph()

	assert.Equal(t, []entityKey{
		ek(typeKubeDeployment, "default/frontend"),
		ek(typeKubeNamespace, "default"),
		ek(typeKubePod, "default/frontend-1234-abcd"),
		ek(typeProcess, "default.frontend-1234-abcd.frontend"),
		ek(typeService, "shop/frontend"),
		ek(typeServiceNamespace, "shop"),
	}, g.sortedEntities())

	assert.Equal(t, []relationKey{
		{Type: relBelongsTo, From: ek(typeKubeDeployment, "default/frontend"), To: ek(typeKubeNamespace, "default")},
		{Type: relBelongsTo, From: ek(typeKubePod, "default/frontend-1234-abcd"), To: ek(typeKubeDeployment, "default/frontend")},
		{Type: relBelongsTo, From: ek(typeKubePod, "default/frontend-1234-abcd"), To: ek(typeKubeNamespace, "default")},
		{Type: relBelongsTo, From: ek(typeService, "shop/frontend"), To: ek(typeServiceNamespace, "shop")},
		{Type: relExecutes, From: ek(typeProcess, "default.frontend-1234-abcd.frontend"), To: ek(typeService, "shop/frontend")},
		{Type: relRunsIn, From: ek(typeProcess, "default.frontend-1234-abcd.frontend"), To: ek(typeKubePod, "default/frontend-1234-abcd")},
		{Type: relRunsIn, From: ek(typeService, "shop/frontend"), To: ek(typeKubeDeployment, "default/frontend")},
		{Type: relRunsIn, From: ek(typeService, "shop/frontend"), To: ek(typeKubePod, "default/frontend-1234-abcd")},
	}, relationsOf(g))

	assert.Equal(t, properties{
		"pid": int64(33), "ppid": int64(1), "executablePath": "/usr/bin/server", "executableName": "server",
		"hostId": testHost, "hostName": "frontend-1234-abcd",
	}, g.entities[ek(typeProcess, "default.frontend-1234-abcd.frontend")])
	assert.Equal(t, properties{
		"podName": "frontend-1234-abcd", "kubeNamespace": "default", "uid": "uid-frontend-1234-abcd", "nodeName": "node1",
	}, g.entities[ek(typeKubePod, "default/frontend-1234-abcd")])
	assert.Equal(t, properties{
		"serviceName": "frontend", "serviceNamespace": "shop", "sdkLanguage": "go",
	}, g.entities[ek(typeService, "shop/frontend")])

	// the Knowledge Graph rejects the whole batch if any property uses a reserved identity key
	for e, props := range g.entities {
		for k := range props {
			assert.NotContains(t, reservedPropertyKeys, k, "entity %v uses reserved property key %q", e, k)
		}
	}
}

func TestBuildGraph_ProcessNames(t *testing.T) {
	se := newSemanticExporter(&testCfg, &fakeWriter{}, testHost)
	// two processes from the same executable and service, outside Kubernetes
	se.onProcessEvent(processEvent(exec.ProcessEventCreated, 10,
		svc.Attrs{UID: svc.UID{Name: "worker", Instance: "myhost:10"}}))
	se.onProcessEvent(processEvent(exec.ProcessEventCreated, 11,
		svc.Attrs{UID: svc.UID{Name: "worker", Instance: "myhost:11"}}))
	// unknown instance ID falls back to the host ID and PID
	se.onProcessEvent(processEvent(exec.ProcessEventCreated, 12,
		svc.Attrs{UID: svc.UID{Name: "worker"}}))

	g := se.buildGraph()
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
	se := newSemanticExporter(&testCfg, &fakeWriter{}, testHost)
	// the service decorates with Kubernetes metadata, but the pod is not known by the informer
	// (e.g. it is still being synchronized), so no dangling relations must be submitted
	attrs := kubeService("backend", "", "default", "backend-abcd", "backend")
	attrs.ProcPID = 44
	se.onSpans([]request.Span{{Service: attrs}, {Service: attrs}})

	g := se.buildGraph()
	assert.Equal(t, []entityKey{ek(typeService, "backend")}, g.sortedEntities())
	assert.Empty(t, relationsOf(g))
}

func TestBuildGraph_PodWithoutDeployment(t *testing.T) {
	se := newSemanticExporter(&testCfg, &fakeWriter{}, testHost)
	require.NoError(t, se.On(podEvent(informer.EventType_CREATED, "kube-system", "standalone",
		&informer.Owner{Kind: "Pod", Name: "standalone"})))
	// non-pod events and finished pods are ignored
	require.NoError(t, se.On(&informer.Event{Type: informer.EventType_CREATED,
		Resource: &informer.ObjectMeta{Name: "svc", Namespace: "default", Kind: "Service"}}))
	finished := podEvent(informer.EventType_CREATED, "default", "finished")
	finished.Resource.Pod.Finished = true
	require.NoError(t, se.On(finished))

	g := se.buildGraph()
	assert.Equal(t, []entityKey{
		ek(typeKubeNamespace, "kube-system"),
		ek(typeKubePod, "kube-system/standalone"),
	}, g.sortedEntities())
	assert.Equal(t, []relationKey{
		{Type: relBelongsTo, From: ek(typeKubePod, "kube-system/standalone"), To: ek(typeKubeNamespace, "kube-system")},
	}, relationsOf(g))
}

func TestServicesExpireAfterTTL(t *testing.T) {
	now := time.Unix(1000, 0)
	se := newSemanticExporter(&testCfg, &fakeWriter{}, testHost)
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
	se := newSemanticExporter(&testCfg, fw, testHost)
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
	// Deployment, Namespaces and Service are not explicitly deleted: they will expire by TTL
	assert.Equal(t, []entityKey{
		ek(typeKubePod, "default/frontend-1234-abcd"),
		ek(typeProcess, "default.frontend-1234-abcd.frontend"),
	}, fw.deletedEntities)
	assert.Equal(t, []relationKey{
		{Type: relBelongsTo, From: ek(typeKubePod, "default/frontend-1234-abcd"), To: ek(typeKubeDeployment, "default/frontend")},
		{Type: relBelongsTo, From: ek(typeKubePod, "default/frontend-1234-abcd"), To: ek(typeKubeNamespace, "default")},
		{Type: relExecutes, From: ek(typeProcess, "default.frontend-1234-abcd.frontend"), To: ek(typeService, "shop/frontend")},
		{Type: relRunsIn, From: ek(typeProcess, "default.frontend-1234-abcd.frontend"), To: ek(typeKubePod, "default/frontend-1234-abcd")},
		{Type: relRunsIn, From: ek(typeService, "shop/frontend"), To: ek(typeKubePod, "default/frontend-1234-abcd")},
	}, fw.deletedRelations)
	// the remaining pod is still submitted
	assert.Contains(t, fw.written[1].entities, ek(typeKubePod, "default/frontend-1234-efgh"))
	assert.Contains(t, fw.written[1].entities, ek(typeKubeDeployment, "default/frontend"))
}

func TestRun_ConsumesQueues(t *testing.T) {
	fw := &fakeWriter{}
	cfg := (&SemanticConfig{RefreshInterval: 10 * time.Millisecond}).withDefaults()
	spans := msg.NewQueue[[]request.Span](msg.ChannelBufferLen(10))
	procs := msg.NewQueue[exec.ProcessEvent](msg.ChannelBufferLen(10))

	se := newSemanticExporter(&cfg, fw, testHost)
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
		}, g.sortedRelations())
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
		g.addPod(&podInfo{namespace: "ns", name: fmt.Sprintf("pod%d", i)})
	}
	g.pruneDanglingRelations()
	require.NoError(t, kg.writeGraph(t.Context(), g))

	// 511 entities (510 pods + 1 namespace) and 510 relations: 4 batches
	require.Len(t, requests, 4)
	assert.Len(t, requests[0].Entities, maxBatchItems)
	assert.Empty(t, requests[0].Relationships)
	// pending entities are flushed before starting submitting relations
	assert.Len(t, requests[1].Entities, 11)
	assert.Empty(t, requests[1].Relationships)
	assert.Empty(t, requests[2].Entities)
	assert.Len(t, requests[2].Relationships, maxBatchItems)
	assert.Empty(t, requests[3].Entities)
	assert.Len(t, requests[3].Relationships, 10)

	e := requests[0].Entities[0]
	assert.Equal(t, "beyla", e.Domain)
	assert.Equal(t, typeKubeNamespace, e.Type)
	assert.Equal(t, "ns", e.Name)
	assert.Equal(t, map[string]string{"env": "demo"}, e.Scope)
	assert.Equal(t, int64(90), e.TTLSeconds)
	assert.Empty(t, e.Properties)
	pod := requests[0].Entities[1]
	assert.Equal(t, typeKubePod, pod.Type)
	assert.Equal(t, "ns/pod0", pod.Name)
	assert.Equal(t, map[string]any{"podName": "pod0", "kubeNamespace": "ns"}, pod.Properties)
}

type graphRequest struct {
	Entities      []graphItem `json:"entities"`
	Relationships []graphItem `json:"relationships"`
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

func (f *fakeWriter) deleteEntity(_ context.Context, entityType, name string) error {
	f.mt.Lock()
	defer f.mt.Unlock()
	f.deletedEntities = append(f.deletedEntities, ek(entityType, name))
	return nil
}

func (f *fakeWriter) deleteRelationship(_ context.Context, fromType, fromName, toType, toName, relation string) error {
	f.mt.Lock()
	defer f.mt.Unlock()
	f.deletedRelations = append(f.deletedRelations, relationKey{
		Type: relation, From: ek(fromType, fromName), To: ek(toType, toName),
	})
	return nil
}
