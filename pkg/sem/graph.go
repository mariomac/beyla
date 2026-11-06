package sem

import (
	"cmp"
	"slices"
	"strconv"

	"go.opentelemetry.io/obi/pkg/appolly/app"
	"go.opentelemetry.io/obi/pkg/appolly/app/svc"
	attr "go.opentelemetry.io/obi/pkg/export/attributes/names"
	"go.opentelemetry.io/obi/pkg/kube/kubecache/informer"
)

// Entity types. They must match the Knowledge Graph type pattern ^[A-Z][A-Za-z0-9]*$
const (
	typeKubePod          = "KubePod"
	typeKubeDeployment   = "KubeDeployment"
	typeKubeNamespace    = "KubeNamespace"
	typeService          = "Service"
	typeServiceNamespace = "ServiceNamespace"
	typeProcess          = "Process"
)

// Relationship types. They must match the Knowledge Graph pattern ^[A-Z][A-Z0-9_]*$
const (
	relBelongsTo = "BELONGS_TO"
	relRunsIn    = "RUNS_IN"
	relExecutes  = "EXECUTES"
)

// volatileTypes are the entity types whose lifecycle is precisely known by Beyla
// (Kubernetes informer deletion events, process termination events). When they disappear
// from the graph, Beyla explicitly deletes them, as well as any relation pointing to/from them.
// The rest of entities (namespaces, deployments, services...) might be shared with other Beyla
// instances, so they are never deleted explicitly and just expire by TTL when nobody refreshes them.
var volatileTypes = map[string]struct{}{
	typeKubePod: {},
	typeProcess: {},
}

func isVolatile(entityType string) bool {
	_, ok := volatileTypes[entityType]
	return ok
}

type entityKey struct {
	Type string
	Name string
}

// entity properties. Values can be either string, int64 or bool.
// Keys must not be any of the reservedPropertyKeys.
type properties map[string]any

// reservedPropertyKeys are identity fields of the Knowledge Graph entities, so they are
// rejected as property keys.
var reservedPropertyKeys = map[string]struct{}{
	"name": {},
	"type": {},
}

type relationKey struct {
	Type string
	From entityKey
	To   entityKey
}

// graph is a snapshot of all the entities and relations that Beyla knows about
type graph struct {
	entities  map[entityKey]properties
	relations map[relationKey]struct{}
}

func newGraph() *graph {
	return &graph{
		entities:  map[entityKey]properties{},
		relations: map[relationKey]struct{}{},
	}
}

func (g *graph) addEntity(eType, name string, props properties) entityKey {
	key := entityKey{Type: eType, Name: name}
	current, ok := g.entities[key]
	if !ok || current == nil {
		g.entities[key] = props
		return key
	}
	if props == nil {
		return key
	}
	// merge properties that might come from different sources
	for k, v := range props {
		if _, exists := current[k]; !exists {
			current[k] = v
		}
	}
	return key
}

func (g *graph) addRelation(rType string, from, to entityKey) {
	g.relations[relationKey{Type: rType, From: from, To: to}] = struct{}{}
}

// pruneDanglingRelations removes the relations whose source or destination entity is not part of
// the graph, as the Knowledge Graph does not create stub entities for unresolved relationship endpoints.
func (g *graph) pruneDanglingRelations() {
	for rel := range g.relations {
		_, fromOK := g.entities[rel.From]
		_, toOK := g.entities[rel.To]
		if !fromOK || !toOK {
			delete(g.relations, rel)
		}
	}
}

// sortedEntities returns the entities in a deterministic order, to facilitate testing and batching
func (g *graph) sortedEntities() []entityKey {
	keys := make([]entityKey, 0, len(g.entities))
	for k := range g.entities {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, compareEntityKeys)
	return keys
}

func (g *graph) sortedRelations() []relationKey {
	keys := make([]relationKey, 0, len(g.relations))
	for k := range g.relations {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b relationKey) int {
		return cmp.Or(
			cmp.Compare(a.Type, b.Type),
			compareEntityKeys(a.From, b.From),
			compareEntityKeys(a.To, b.To),
		)
	})
	return keys
}

func compareEntityKeys(a, b entityKey) int {
	return cmp.Or(cmp.Compare(a.Type, b.Type), cmp.Compare(a.Name, b.Name))
}

// removedVolatile returns the entities and relations from the old graph that do not exist anymore
// in the new graph, and that should be explicitly deleted from the Knowledge Graph
// because they refer to volatile entities.
func removedVolatile(old, current *graph) ([]entityKey, []relationKey) {
	if old == nil {
		return nil, nil
	}
	var entities []entityKey
	for _, e := range old.sortedEntities() {
		if _, ok := current.entities[e]; !ok && isVolatile(e.Type) {
			entities = append(entities, e)
		}
	}
	var relations []relationKey
	for _, r := range old.sortedRelations() {
		if _, ok := current.relations[r]; !ok && (isVolatile(r.From.Type) || isVolatile(r.To.Type)) {
			relations = append(relations, r)
		}
	}
	return entities, relations
}

// Entity naming. The Knowledge Graph identifies entities by domain + type + name + scope,
// so the names below must be unique within each entity type.

func kubeNamespaceName(ns string) string { return ns }

func kubeObjectName(ns, name string) string { return ns + "/" + name }

func serviceNamespaceName(ns string) string { return ns }

func serviceName(uid svc.UID) string {
	if uid.Namespace == "" {
		return uid.Name
	}
	return uid.Namespace + "/" + uid.Name
}

// processName uses the service instance ID of the process, which is unique per process
// (<hostname>:<pid>) or per container (<namespace>.<pod>.<container>) in Kubernetes.
// If two instrumented processes share the same container, they would be merged into the same
// entity. It falls back to <hostID>/<pid> if the instance ID is not known.
func processName(instance, hostID string, pid app.PID) string {
	if instance != "" {
		return instance
	}
	return hostID + "/" + strconv.Itoa(int(pid))
}

// podInfo is the information about a Kubernetes Pod, as received from the Kubernetes informer
type podInfo struct {
	namespace   string
	name        string
	uid         string
	nodeName    string
	deployments []string
}

func podInfoFrom(meta *informer.ObjectMeta) *podInfo {
	pi := &podInfo{
		namespace: meta.Namespace,
		name:      meta.Name,
		uid:       meta.Pod.Uid,
		nodeName:  meta.Pod.NodeName,
	}
	for _, owner := range meta.Pod.Owners {
		if owner.Kind == "Deployment" {
			pi.deployments = append(pi.deployments, owner.Name)
		}
	}
	return pi
}

// serviceInfo is the information about an instrumented service, as received from
// the process discovery events and the instrumented spans
type serviceInfo struct {
	uid        svc.UID
	language   string
	kubeNs     string
	kubePod    string
	kubeDeploy string
}

func serviceInfoFrom(attrs *svc.Attrs) serviceInfo {
	return serviceInfo{
		uid:        attrs.UID,
		language:   attrs.SDKLanguage.String(),
		kubeNs:     attrs.Metadata[attr.K8sNamespaceName],
		kubePod:    attrs.Metadata[attr.K8sPodName],
		kubeDeploy: attrs.Metadata[attr.K8sDeploymentName],
	}
}

// processInfo is the information about an instrumented process, as received from the process discovery events
type processInfo struct {
	pid      app.PID
	ppid     app.PID
	exePath  string
	exeName  string
	service  serviceInfo
	hostName string
}

func (g *graph) addPod(pod *podInfo) {
	nsKey := g.addEntity(typeKubeNamespace, kubeNamespaceName(pod.namespace), nil)
	podProps := properties{
		"podName":       pod.name,
		"kubeNamespace": pod.namespace,
	}
	if pod.uid != "" {
		podProps["uid"] = pod.uid
	}
	if pod.nodeName != "" {
		podProps["nodeName"] = pod.nodeName
	}
	podKey := g.addEntity(typeKubePod, kubeObjectName(pod.namespace, pod.name), podProps)
	g.addRelation(relBelongsTo, podKey, nsKey)
	for _, deploy := range pod.deployments {
		deployKey := g.addEntity(typeKubeDeployment, kubeObjectName(pod.namespace, deploy), properties{
			"deploymentName": deploy,
			"kubeNamespace":  pod.namespace,
		})
		g.addRelation(relBelongsTo, deployKey, nsKey)
		g.addRelation(relBelongsTo, podKey, deployKey)
	}
}

// addService adds the Service entity, its OTEL namespace and its Kubernetes relations.
// It returns false if the service does not have enough information to be added.
func (g *graph) addService(si *serviceInfo) (entityKey, bool) {
	if si.uid.Name == "" {
		return entityKey{}, false
	}
	props := properties{"serviceName": si.uid.Name}
	if si.uid.Namespace != "" {
		props["serviceNamespace"] = si.uid.Namespace
	}
	if si.language != "" {
		props["sdkLanguage"] = si.language
	}
	svcKey := g.addEntity(typeService, serviceName(si.uid), props)
	if si.uid.Namespace != "" {
		nsKey := g.addEntity(typeServiceNamespace, serviceNamespaceName(si.uid.Namespace), nil)
		g.addRelation(relBelongsTo, svcKey, nsKey)
	}
	// The Kubernetes entities are added from the informer. Here we just add the relations,
	// that will be pruned later if the Kubernetes entities are not known
	if si.kubeNs != "" {
		if si.kubePod != "" {
			g.addRelation(relRunsIn, svcKey, entityKey{Type: typeKubePod, Name: kubeObjectName(si.kubeNs, si.kubePod)})
		}
		if si.kubeDeploy != "" {
			g.addRelation(relRunsIn, svcKey, entityKey{Type: typeKubeDeployment, Name: kubeObjectName(si.kubeNs, si.kubeDeploy)})
		}
	}
	return svcKey, true
}

func (g *graph) addProcess(hostID string, proc *processInfo) entityKey {
	props := properties{"pid": int64(proc.pid)}
	if proc.ppid != 0 {
		props["ppid"] = int64(proc.ppid)
	}
	if proc.exePath != "" {
		props["executablePath"] = proc.exePath
	}
	if proc.exeName != "" {
		props["executableName"] = proc.exeName
	}
	if hostID != "" {
		props["hostId"] = hostID
	}
	if proc.hostName != "" {
		props["hostName"] = proc.hostName
	}
	procKey := g.addEntity(typeProcess, processName(proc.service.uid.Instance, hostID, proc.pid), props)
	// as for the services, the relation is pruned later if the Pod is not known by the informer
	if proc.service.kubeNs != "" && proc.service.kubePod != "" {
		g.addRelation(relRunsIn, procKey,
			entityKey{Type: typeKubePod, Name: kubeObjectName(proc.service.kubeNs, proc.service.kubePod)})
	}
	if svcKey, ok := g.addService(&proc.service); ok {
		g.addRelation(relExecutes, procKey, svcKey)
	}
	return procKey
}
