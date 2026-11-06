package sem

import (
	"cmp"
	"slices"
	"strconv"
	"strings"

	"go.opentelemetry.io/obi/pkg/appolly/app"
	"go.opentelemetry.io/obi/pkg/appolly/app/svc"
	"go.opentelemetry.io/obi/pkg/export/attributes"
	attr "go.opentelemetry.io/obi/pkg/export/attributes/names"
	"go.opentelemetry.io/obi/pkg/kube"
	"go.opentelemetry.io/obi/pkg/kube/kubecache/informer"
)

// Entity types. They must match the Knowledge Graph type pattern ^[A-Z][A-Za-z0-9]*$
// Pod, Process and Service follow the names of the Asserts KG atlas (metrics domain):
// https://github.com/grafana/kg-atlas/blob/main/definitions/system-schemas/metrics-kg-atlas-v1.yaml
// The atlas does not define Kubernetes workload types (it models them as a Service with
// the workload and workload_type properties), nor the OTEL service namespace.
const (
	typePod              = "Pod"
	typeDeployment       = "Deployment"
	typeStatefulSet      = "StatefulSet"
	typeDaemonSet        = "DaemonSet"
	typeJob              = "Job"
	typeCronJob          = "CronJob"
	typeService          = "Service"
	typeServiceNamespace = "ServiceNamespace"
	typeProcess          = "Process"
)

// Relationship types. They must match the Knowledge Graph pattern ^[A-Z][A-Z0-9_]*$
// CONTROLS and HOSTS come from the Asserts KG atlas (metrics domain), and HAS_MEMBER from the
// core domain: https://github.com/grafana/kg-atlas/blob/main/definitions/system-schemas/core-kg-atlas-v1.yaml
// EXECUTES has no equivalent in the atlas.
const (
	// relControls is the management and lifecycle control of one entity by another
	relControls = "CONTROLS"
	// relHosts is the physical or logical containment of one entity by another
	relHosts = "HOSTS"
	// relHasMember means that the source group includes the target as a member
	relHasMember = "HAS_MEMBER"
	// relExecutes means that the source process runs the target service
	relExecutes = "EXECUTES"
)

// workloadTypes maps the Kubernetes owner kinds to the workload entity types.
// Other owner kinds (e.g. ReplicaSet, which is usually owned by a Deployment) are not
// submitted to the Knowledge Graph.
var workloadTypes = map[string]string{
	"Deployment":  typeDeployment,
	"StatefulSet": typeStatefulSet,
	"DaemonSet":   typeDaemonSet,
	"Job":         typeJob,
	"CronJob":     typeCronJob,
}

// kindPod is the owner kind that the Kubernetes informer reports when a Pod does not have any owner
const kindPod = "Pod"

// Relations between the Kubernetes workloads, pods, processes and services:
//   - Each workload CONTROLS the Pod or workload that it directly owns
//     (e.g. Deployment CONTROLS Pod, CronJob CONTROLS Job CONTROLS Pod).
//   - Pod HOSTS the Services and Processes that run on it.
//   - The top-level workload of the Pod HOSTS the Service (e.g. Deployment HOSTS Service).
//   - Process EXECUTES Service.
//   - ServiceNamespace HAS_MEMBER Service.

// volatileTypes are the entity types whose lifecycle is precisely known by Beyla
// (Kubernetes informer deletion events, process termination events). When they disappear
// from the graph, Beyla explicitly deletes them, as well as any relation pointing to/from them.
// The rest of entities (deployments, services...) might be shared with other Beyla
// instances, so they are never deleted explicitly and just expire by TTL when nobody refreshes them.
var volatileTypes = map[string]struct{}{
	typePod:     {},
	typeProcess: {},
}

func isVolatile(entityType string) bool {
	_, ok := volatileTypes[entityType]
	return ok
}

// entityKey identifies an entity in the Knowledge Graph, together with the common scope.
type entityKey struct {
	Type string
	// Env is the deployment environment of the entity, submitted as the "env" scope key.
	Env string
	// Namespace is the Kubernetes namespace of the entity. It is submitted as the "namespace"
	// scope key, so entities with the same name in different namespaces are different entities.
	// Empty for entities that are not in a Kubernetes namespace.
	Namespace string
	Name      string
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

func (g *graph) addEntity(key entityKey, props properties) entityKey {
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
	return cmp.Or(cmp.Compare(a.Type, b.Type), cmp.Compare(a.Env, b.Env),
		cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
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
	oldRelations := old.sortedRelations()
	for i := range oldRelations {
		r := &oldRelations[i]
		if _, ok := current.relations[*r]; !ok && (isVolatile(r.From.Type) || isVolatile(r.To.Type)) {
			relations = append(relations, *r)
		}
	}
	return entities, relations
}

// Entity naming. The Knowledge Graph identifies entities by domain + type + name + scope,
// so the names below must be unique within each entity type and Kubernetes namespace.

// workloadKey returns false if the owner kind is not submitted as a workload entity
func workloadKey(env, ns, kind, name string) (entityKey, bool) {
	eType, ok := workloadTypes[kind]
	if !ok || name == "" {
		return entityKey{}, false
	}
	return entityKey{Type: eType, Env: env, Namespace: ns, Name: name}, true
}

// serviceNamespaceKey refers to the OTEL service.namespace, which is not tied to any Kubernetes namespace
func serviceNamespaceKey(env, ns string) entityKey {
	return entityKey{Type: typeServiceNamespace, Env: env, Name: ns}
}

// Resource attributes that define the deployment environment, in order of preference.
// This is the same criteria that Asserts follows to set the asserts_env label of the OTEL metrics.
var environmentAttrs = []attr.Name{"deployment.environment.name", "deployment.environment"}

// environmentFrom returns the deployment environment from the resource attributes, or the
// default environment if they do not define it
func environmentFrom(resourceAttrs map[attr.Name]string, defaultEnv string) string {
	for _, name := range environmentAttrs {
		if env := resourceAttrs[name]; env != "" {
			return env
		}
	}
	return defaultEnv
}

// podResourceAttributes returns the OTEL resource attributes of a Pod, with the same precedence as the
// Kubernetes metadata decorator: OTEL_RESOURCE_ATTRIBUTES in the containers, then resource.opentelemetry.io/ annotations
func podResourceAttributes(meta *informer.ObjectMeta) map[attr.Name]string {
	resAttrs := map[attr.Name]string{}
	for name, value := range meta.Annotations {
		if propertyName, ok := strings.CutPrefix(name, kube.ResourceAttributesPrefix); ok {
			resAttrs[attr.Name(propertyName)] = value
		}
	}
	for _, cnt := range meta.GetPod().GetContainers() {
		attributes.ParseOTELResourceVariable(cnt.Env[kube.EnvResourceAttributes], func(k, v string) {
			if v != "" && !strings.HasPrefix(v, "$") {
				resAttrs[attr.Name(k)] = v
			}
		})
	}
	return resAttrs
}

// serviceKey identifies a Service by its OTEL service.name and its namespace, which is submitted
// as the "namespace" scope key: the Kubernetes namespace or, out of Kubernetes, the OTEL service.namespace.
// The name does not include the namespace, as the Knowledge Graph already shows the namespace
// from the scope (e.g. a "tapas/tapas-backend" name would be shown as "tapas/tapas/tapas-backend").
func serviceKey(si *serviceInfo) entityKey {
	ns := si.kubeNs
	if ns == "" {
		ns = si.uid.Namespace
	}
	return entityKey{Type: typeService, Env: si.env, Namespace: ns, Name: si.uid.Name}
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

// podRef identifies a Pod in the Kubernetes informer
type podRef struct {
	namespace string
	name      string
}

// podInfo is the information about a Kubernetes Pod, as received from the Kubernetes informer
type podInfo struct {
	env       string
	namespace string
	name      string
	uid       string
	nodeName  string
	// kind and name of the top-level owner (e.g. Deployment), even if it is not a workload entity.
	// Empty if the Pod does not have any owner.
	topOwnerKind string
	topOwnerName string
	// workloads that own the Pod, from the direct owner to the top-level owner
	// (e.g. Job, CronJob). Owners that are not workload entities (e.g. ReplicaSet) are skipped.
	workloads []entityKey
}

func podInfoFrom(meta *informer.ObjectMeta, defaultEnv string) *podInfo {
	env := environmentFrom(podResourceAttributes(meta), defaultEnv)
	pi := &podInfo{
		env:       env,
		namespace: meta.Namespace,
		name:      meta.Name,
		uid:       meta.Pod.Uid,
		nodeName:  meta.Pod.NodeName,
	}
	// the informer reports the owners from the direct owner to the top-level owner,
	// e.g. ReplicaSet, Deployment
	for _, owner := range meta.Pod.Owners {
		if wk, ok := workloadKey(env, meta.Namespace, owner.Kind, owner.Name); ok {
			pi.workloads = append(pi.workloads, wk)
		}
	}
	if n := len(meta.Pod.Owners); n > 0 && meta.Pod.Owners[n-1].Kind != kindPod {
		pi.topOwnerKind = meta.Pod.Owners[n-1].Kind
		pi.topOwnerName = meta.Pod.Owners[n-1].Name
	}
	return pi
}

func (pi *podInfo) key() entityKey {
	return entityKey{Type: typePod, Env: pi.env, Namespace: pi.namespace, Name: pi.name}
}

// topWorkload returns the top-level workload entity that owns the Pod, if any
func (pi *podInfo) topWorkload() (entityKey, bool) {
	if len(pi.workloads) == 0 {
		return entityKey{}, false
	}
	return pi.workloads[len(pi.workloads)-1], true
}

// serviceInfo is the information about an instrumented service, as received from
// the process discovery events and the instrumented spans
type serviceInfo struct {
	uid      svc.UID
	env      string
	language string
	kubeNs   string
	kubePod  string
}

func serviceInfoFrom(attrs *svc.Attrs, defaultEnv string) serviceInfo {
	return serviceInfo{
		uid:      attrs.UID,
		env:      environmentFrom(attrs.Metadata, defaultEnv),
		language: attrs.SDKLanguage.String(),
		kubeNs:   attrs.Metadata[attr.K8sNamespaceName],
		kubePod:  attrs.Metadata[attr.K8sPodName],
	}
}

// pod returns the Pod where the service runs, if it is known by the Kubernetes informer
func (si *serviceInfo) pod(pods map[podRef]*podInfo) (*podInfo, bool) {
	if si.kubeNs == "" || si.kubePod == "" {
		return nil, false
	}
	pod, ok := pods[podRef{namespace: si.kubeNs, name: si.kubePod}]
	return pod, ok
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
	podProps := properties{}
	if pod.uid != "" {
		podProps["uid"] = pod.uid
	}
	if pod.nodeName != "" {
		podProps["node"] = pod.nodeName
	}
	if pod.topOwnerKind != "" {
		podProps["workload"] = pod.topOwnerName
		podProps["workload_type"] = pod.topOwnerKind
	}
	child := g.addEntity(pod.key(), podProps)
	for _, wk := range pod.workloads {
		g.addEntity(wk, nil)
		g.addRelation(relControls, wk, child)
		child = wk
	}
}

// addService adds the Service entity, its OTEL namespace and its Kubernetes relations.
// The pods argument contains the Pods known by the Kubernetes informer.
// It returns false if the service does not have enough information to be added.
func (g *graph) addService(si *serviceInfo, pods map[podRef]*podInfo) (entityKey, bool) {
	if si.uid.Name == "" {
		return entityKey{}, false
	}
	props := properties{"otel_service": si.uid.Name}
	if si.uid.Namespace != "" {
		props["otel_namespace"] = si.uid.Namespace
	}
	if si.language != "" {
		props["telemetry_sdk_language"] = si.language
	}
	svcKey := g.addEntity(serviceKey(si), props)
	if si.uid.Namespace != "" {
		nsKey := g.addEntity(serviceNamespaceKey(si.env, si.uid.Namespace), nil)
		g.addRelation(relHasMember, nsKey, svcKey)
	}
	// The Kubernetes entities are added from the informer, so we take their identity from there,
	// as their environment might differ from the service environment.
	if pod, ok := si.pod(pods); ok {
		g.addRelation(relHosts, pod.key(), svcKey)
		if wk, ok := pod.topWorkload(); ok {
			g.addRelation(relHosts, wk, svcKey)
		}
	}
	return svcKey, true
}

func (g *graph) addProcess(hostID string, proc *processInfo, pods map[podRef]*podInfo) entityKey {
	props := properties{"pid": int64(proc.pid)}
	if proc.ppid != 0 {
		props["ppid"] = int64(proc.ppid)
	}
	if proc.exePath != "" {
		props["executable_path"] = proc.exePath
	}
	if proc.exeName != "" {
		props["process_executable_name"] = proc.exeName
	}
	if hostID != "" {
		props["host_id"] = hostID
	}
	if proc.hostName != "" {
		props["host_name"] = proc.hostName
	}
	procKey := g.addEntity(entityKey{
		Type:      typeProcess,
		Env:       proc.service.env,
		Namespace: proc.service.kubeNs,
		Name:      processName(proc.service.uid.Instance, hostID, proc.pid),
	}, props)
	if pod, ok := proc.service.pod(pods); ok {
		g.addRelation(relHosts, pod.key(), procKey)
	}
	if svcKey, ok := g.addService(&proc.service, pods); ok {
		g.addRelation(relExecutes, procKey, svcKey)
	}
	return procKey
}
