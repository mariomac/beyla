// Package sem provides a Semantic Exporter that submits to the Grafana Knowledge Graph the
// entities discovered by Beyla (Kubernetes Pods, Deployments, Namespaces, OTEL Services and
// Namespaces, and OS processes), as well as the relations between them.
package sem

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"go.opentelemetry.io/obi/pkg/appolly/app"
	"go.opentelemetry.io/obi/pkg/appolly/app/request"
	"go.opentelemetry.io/obi/pkg/appolly/app/svc"
	"go.opentelemetry.io/obi/pkg/appolly/discover/exec"
	"go.opentelemetry.io/obi/pkg/kube"
	"go.opentelemetry.io/obi/pkg/kube/kubecache/informer"
	"go.opentelemetry.io/obi/pkg/pipe/global"
	"go.opentelemetry.io/obi/pkg/pipe/msg"
	"go.opentelemetry.io/obi/pkg/pipe/swarm"
)

const (
	defaultDomain          = "beyla"
	defaultRefreshInterval = 30 * time.Second
	// default TTL, as a multiple of the refresh interval, so an entity is not expired
	// if a single refresh fails
	defaultTTLIntervals = 3
)

type SemanticConfig struct {
	// CellGatewayURL from Grafana, for example https://asserts-dev-us-central-0.grafana-dev.net
	CellGatewayURL string `yaml:"cell_gateway_url" env:"GRAFANA_CELL_GATEWAY_URL"`
	// StackID is the numeric Grafana stack id
	StackID string `yaml:"stack_id" env:"GRAFANA_STACK_ID"`

	// TODO: reuse beyla global credentials
	GrafanaGCOMToken string `yaml:"-" env:"GRAFANA_GCOM_TOKEN"`

	// Domain is the default knowledge graph domain for the generated entities. Defaults to "beyla"
	// TODO: override it by using pod annotations
	Domain string `yaml:"domain" env:"GRAFANA_CELL_DOMAIN"`

	// RefreshInterval is the period at which the whole known graph is submitted to the Knowledge Graph.
	// Defaults to 30s.
	RefreshInterval time.Duration `yaml:"refresh_interval" env:"GRAFANA_CELL_REFRESH_INTERVAL"`

	// TTL of the submitted entities and relations. If an entity or relation is not refreshed during
	// this time, the Knowledge Graph expires it. It is also used to forget the services
	// that haven't been seen in any span for this time. Defaults to 3 times the RefreshInterval.
	TTL time.Duration `yaml:"ttl" env:"GRAFANA_CELL_TTL"`
}

func (sc *SemanticConfig) Enabled() bool {
	return sc.CellGatewayURL != "" && sc.StackID != "" && sc.GrafanaGCOMToken != ""
}

func (sc *SemanticConfig) withDefaults() SemanticConfig {
	cfg := *sc
	if cfg.Domain == "" {
		cfg.Domain = defaultDomain
	}
	if cfg.RefreshInterval <= 0 {
		cfg.RefreshInterval = defaultRefreshInterval
	}
	if cfg.TTL <= 0 {
		cfg.TTL = defaultTTLIntervals * cfg.RefreshInterval
	}
	return cfg
}

func elog() *slog.Logger {
	return slog.With("component", "sem.SemanticExporter")
}

// SemanticExporter creates a pipeline node that listens for:
//   - Kubernetes Pod events (from the Kubernetes informer, if enabled), to create
//     KubePod, KubeDeployment and KubeNamespace entities.
//   - Instrumented process events, to create Process entities and the Services they execute.
//   - Decorated application spans, to keep track of the active Services and where they run.
//
// It periodically submits the whole known graph to the Grafana Knowledge Graph.
func SemanticExporter(
	ctxInfo *global.ContextInfo,
	cfg *SemanticConfig,
	spans *msg.Queue[[]request.Span],
	processEvents *msg.Queue[exec.ProcessEvent],
) swarm.InstanceFunc {
	return func(ctx context.Context) (swarm.RunFunc, error) {
		if !cfg.Enabled() {
			return swarm.EmptyRunFunc()
		}
		sc := cfg.withDefaults()
		kg, err := createKnowledgeGraph(sc)
		if err != nil {
			return nil, fmt.Errorf("creating knowledge graph interface: %w", err)
		}
		var kubeStore *kube.Store
		if ctxInfo.K8sInformer != nil && ctxInfo.K8sInformer.IsKubeEnabled() {
			if kubeStore, err = ctxInfo.K8sInformer.Get(ctx); err != nil {
				return nil, fmt.Errorf("getting Kubernetes store: %w", err)
			}
		}
		se := newSemanticExporter(&sc, kg, hostID(ctxInfo))
		se.kubeStore = kubeStore
		if spans != nil {
			se.spans = spans.Subscribe(msg.SubscriberName("sem.SemanticExporter.spans"))
		}
		if processEvents != nil {
			se.processEvents = processEvents.Subscribe(msg.SubscriberName("sem.SemanticExporter.processEvents"))
		}
		return se.run, nil
	}
}

func hostID(ctxInfo *global.ContextInfo) string {
	if ctxInfo.NodeMeta.HostID != "" {
		return ctxInfo.NodeMeta.HostID
	}
	hn, _ := os.Hostname()
	return hn
}

// graphWriter abstracts the Knowledge Graph client, for testing purposes
type graphWriter interface {
	writeGraph(ctx context.Context, g *graph) error
	deleteEntity(ctx context.Context, e entityKey) error
	deleteRelationship(ctx context.Context, r relationKey) error
}

type seenService struct {
	info     serviceInfo
	pid      app.PID
	lastSeen time.Time
}

type semanticExporter struct {
	cfg    *SemanticConfig
	log    *slog.Logger
	kg     graphWriter
	hostID string
	clock  func() time.Time

	kubeStore     *kube.Store
	spans         <-chan []request.Span
	processEvents <-chan exec.ProcessEvent

	mut sync.Mutex
	// pods by namespace and name
	pods map[entityKey]*podInfo
	// instrumented processes by PID
	processes map[app.PID]*processInfo
	// services seen in the spans, by service instance
	services map[svc.UID]*seenService

	// last graph that has been submitted, used to calculate which volatile entities
	// need to be explicitly removed
	lastGraph *graph
}

func newSemanticExporter(cfg *SemanticConfig, kg graphWriter, hostID string) *semanticExporter {
	return &semanticExporter{
		cfg:       cfg,
		log:       elog(),
		kg:        kg,
		hostID:    hostID,
		clock:     time.Now,
		pods:      map[entityKey]*podInfo{},
		processes: map[app.PID]*processInfo{},
		services:  map[svc.UID]*seenService{},
	}
}

func (se *semanticExporter) run(ctx context.Context) {
	if se.kubeStore != nil {
		se.kubeStore.Subscribe(se)
		defer se.kubeStore.Unsubscribe(se)
	}
	se.log.Debug("starting semantic exporter")

	// the submission to the Knowledge Graph happens in a separate goroutine,
	// to avoid blocking the pipeline while waiting for the remote endpoint.
	flushDone := make(chan struct{})
	go func() {
		defer close(flushDone)
		ticker := time.NewTicker(se.cfg.RefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				se.flush(ctx)
			}
		}
	}()
	defer func() { <-flushDone }()

	spans, processEvents := se.spans, se.processEvents
	for {
		select {
		case <-ctx.Done():
			se.log.Debug("context done, stopping semantic exporter")
			return
		case s, ok := <-spans:
			if !ok {
				spans = nil
				continue
			}
			se.onSpans(s)
		case pe, ok := <-processEvents:
			if !ok {
				processEvents = nil
				continue
			}
			se.onProcessEvent(&pe)
		}
	}
}

// ID implements the meta.Observer interface for the Kubernetes Store
func (se *semanticExporter) ID() string { return "semantic-exporter" }

// On implements the meta.Observer interface for the Kubernetes Store.
// It is invoked synchronously by the Kubernetes store, so it should return quickly.
func (se *semanticExporter) On(event *informer.Event) error {
	meta := event.GetResource()
	if meta == nil || meta.Pod == nil || meta.Kind != "Pod" {
		return nil
	}
	key := podKey(meta.Namespace, meta.Name)
	se.mut.Lock()
	defer se.mut.Unlock()
	switch event.Type {
	case informer.EventType_CREATED, informer.EventType_UPDATED:
		if meta.Pod.Finished {
			delete(se.pods, key)
		} else {
			se.pods[key] = podInfoFrom(meta)
		}
	case informer.EventType_DELETED:
		delete(se.pods, key)
	}
	return nil
}

func (se *semanticExporter) onProcessEvent(pe *exec.ProcessEvent) {
	if pe.File == nil {
		return
	}
	pid := pe.File.Pid()
	se.mut.Lock()
	defer se.mut.Unlock()
	switch pe.Type {
	case exec.ProcessEventCreated:
		attrs := pe.File.ServiceAttrs()
		se.processes[pid] = &processInfo{
			pid:      pid,
			ppid:     pe.File.Ppid(),
			exePath:  pe.File.CmdExePath(),
			exeName:  pe.File.ExecutableName(),
			service:  serviceInfoFrom(&attrs),
			hostName: attrs.HostName,
		}
	case exec.ProcessEventTerminated:
		delete(se.processes, pid)
	}
}

func (se *semanticExporter) onSpans(spans []request.Span) {
	if len(spans) == 0 {
		return
	}
	now := se.clock()
	se.mut.Lock()
	defer se.mut.Unlock()
	for i := range spans {
		s := &spans[i].Service
		if s.UID.Name == "" {
			continue
		}
		if seen, ok := se.services[s.UID]; ok {
			seen.lastSeen = now
			continue
		}
		se.services[s.UID] = &seenService{
			info:     serviceInfoFrom(s),
			pid:      s.ProcPID,
			lastSeen: now,
		}
	}
}

// buildGraph composes the whole graph from the current known state.
// It also forgets the services that haven't been seen in the spans for longer than the TTL.
func (se *semanticExporter) buildGraph() *graph {
	now := se.clock()
	se.mut.Lock()
	defer se.mut.Unlock()

	g := newGraph()
	for _, pod := range se.pods {
		g.addPod(pod)
	}
	procKeys := make(map[app.PID]entityKey, len(se.processes))
	for pid, proc := range se.processes {
		procKeys[pid] = g.addProcess(se.hostID, proc)
	}
	for uid, seen := range se.services {
		if now.Sub(seen.lastSeen) > se.cfg.TTL {
			delete(se.services, uid)
			continue
		}
		svcKey, ok := g.addService(&seen.info)
		if !ok {
			continue
		}
		if procKey, ok := procKeys[seen.pid]; ok {
			g.addRelation(relExecutes, procKey, svcKey)
		}
	}
	g.pruneDanglingRelations()
	return g
}

// flush submits the current graph to the Knowledge Graph, and explicitly removes the
// volatile entities and relations that have disappeared since the last flush.
func (se *semanticExporter) flush(ctx context.Context) {
	g := se.buildGraph()
	removedEntities, removedRelations := removedVolatile(se.lastGraph, g)
	se.lastGraph = g

	se.log.Debug("submitting graph", "entities", len(g.entities), "relations", len(g.relations),
		"removedEntities", len(removedEntities), "removedRelations", len(removedRelations))

	if err := se.kg.writeGraph(ctx, g); err != nil {
		se.log.Warn("can't submit graph to the Knowledge Graph. Will retry in the next refresh", "error", err)
	}
	// relations must be removed before their entities
	for _, r := range removedRelations {
		if err := se.kg.deleteRelationship(ctx, r); err != nil {
			se.log.Debug("can't delete relationship. It will expire by TTL", "error", err)
		}
	}
	for _, e := range removedEntities {
		if err := se.kg.deleteEntity(ctx, e); err != nil {
			se.log.Debug("can't delete entity. It will expire by TTL", "error", err)
		}
	}
}
