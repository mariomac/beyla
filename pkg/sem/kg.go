package sem

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/grafana/grafana-asserts-public-clients/go/kgwrite"
)

// relevant docs:
// https://github.com/grafana/kg-atlas/
// https://github.com/grafana/kg-schema

// TODO: investigate the meaning of
// - namespace
// - domain
// - scope
// - predefined types of relation

const (
	requestTimeout = 30 * time.Second
	// maximum number of entities+relationships in a single WriteGraph request
	// (default value of the kg-write.batch.max-items server property)
	maxBatchItems = 500
	// scope keys, as defined in the Asserts KG atlas
	scopeEnv       = "env"
	scopeSite      = "site"
	scopeNamespace = "namespace"

	// telemetryOwnershipRecheck is the time after which an entity that was found in the telemetry
	// domain is submitted again, in case it does not exist anymore in the telemetry domain.
	telemetryOwnershipRecheck = 10 * time.Minute
)

// telemetryConflict matches the message of the 409 Conflict response that the Knowledge Graph returns
// when we try to write an entity whose (type, name, scope) identity already exists in the default
// telemetry domain, e.g.:
// "entities[30]: an entity with this (type, name, scope) exists in the default domain"
var telemetryConflict = regexp.MustCompile(`^entities\[(\d+)\]: .*exists in the default domain`)

type gatewayConfig struct {
	BaseURL             string
	AuthorizationHeader string
	Mode                string
}

type knowledgeGraph struct {
	client     *kgwrite.APIClient
	gateway    gatewayConfig
	namespace  string
	stackID    string
	domain     string
	scope      map[string]string
	ttlSeconds int64
	log        *slog.Logger

	// telemetryDomain is the domain of the entities that are created from the telemetry
	// (e.g. the Asserts model rules), which Beyla can't write but can refer from its relationships
	telemetryDomain string
	// telemetryOwned contains the entities that already exist in the telemetry domain, and the time
	// when we found it. Instead of writing them, Beyla refers to them from its relationships.
	telemetryOwned map[entityKey]time.Time
	clock          func() time.Time
}

func createKnowledgeGraph(cfg SemanticConfig) (*knowledgeGraph, error) {
	namespace, err := namespaceForStack(cfg.StackID)
	if err != nil {
		return nil, err
	}
	gateway, err := configureGateway(cfg.CellGatewayURL, cfg.StackID, cfg.GrafanaGCOMToken)
	if err != nil {
		return nil, err
	}
	kgCfg := kgwrite.NewConfiguration()
	kgCfg.Servers = kgwrite.ServerConfigurations{{URL: gateway.BaseURL}}
	kgCfg.HTTPClient = &http.Client{Timeout: requestTimeout}
	kgCfg.AddDefaultHeader("Authorization", gateway.AuthorizationHeader)

	return &knowledgeGraph{
		log:        slog.With("component", "sem.KnowledgeGraph"),
		client:     kgwrite.NewAPIClient(kgCfg),
		gateway:    gateway,
		namespace:  namespace,
		stackID:    strings.TrimSpace(cfg.StackID),
		domain:     cfg.Domain,
		scope:      commonScope(&cfg),
		ttlSeconds: int64(cfg.TTL.Seconds()),

		telemetryDomain: cfg.TelemetryDomain,
		telemetryOwned:  map[entityKey]time.Time{},
		clock:           time.Now,
	}, nil
}

// writeGraph upserts all the entities and relationships of the graph. The Knowledge Graph
// limits the number of items per request, so the graph is split in multiple batches.
// All the entities are sent before the relationships, as the Knowledge Graph does not create
// stub entities for unresolved relationship endpoints.
// The entities that already exist in the telemetry domain are not written, and the relationships
// refer to the telemetry domain entities instead.
func (kg *knowledgeGraph) writeGraph(ctx context.Context, g *graph) error {
	entities := g.sortedEntities()
	relations := g.sortedRelations()
	var errs []error
	batch := kgwrite.GraphWriteRequestDto{}
	var batchEntities []entityKey
	items := 0
	send := func() {
		if items == 0 {
			return
		}
		if err := kg.writeBatch(ctx, &batch, batchEntities); err != nil {
			errs = append(errs, err)
		}
		batch = kgwrite.GraphWriteRequestDto{}
		batchEntities = nil
		items = 0
	}
	for _, e := range entities {
		if kg.isTelemetryOwned(e) {
			continue
		}
		dto := kgwrite.NewEntityWriteRequestDto(kg.domain, e.Type, e.Name, kg.ttlSeconds)
		dto.SetScope(kg.scopeFor(e))
		if props := g.entities[e]; len(props) > 0 {
			dto.SetProperties(toKGProperties(props))
		}
		batch.Entities = append(batch.Entities, *dto)
		batchEntities = append(batchEntities, e)
		if items++; items >= maxBatchItems {
			send()
		}
	}
	// flushing pending entities before starting sending relations, as the domain
	// of the relationship endpoints depends on the result of the entities' submission
	send()
	for i := range relations {
		r := &relations[i]
		batch.Relationships = append(batch.Relationships,
			*kgwrite.NewRelationshipWriteRequestDto(kg.domain, r.Type, kg.entityRef(r.From), kg.entityRef(r.To), kg.ttlSeconds))
		if items++; items >= maxBatchItems {
			send()
		}
	}
	send()
	return errors.Join(errs...)
}

// writeBatch submits a batch. If the Knowledge Graph rejects it because an entity already exists in the
// telemetry domain, it marks the entity as telemetry-owned and retries the batch without it.
// The entities argument contains the keys of the batch entities, in the same order.
func (kg *knowledgeGraph) writeBatch(ctx context.Context, batch *kgwrite.GraphWriteRequestDto, entities []entityKey) error {
	for {
		written, response, err := kg.client.KnowledgeGraphWriteAPIAPI.
			WriteGraph(ctx, kg.namespace).
			XScopeOrgID(kg.stackID).
			GraphWriteRequestDto(*batch).
			AllowUndeclared(true).
			Execute()
		if err == nil {
			kg.log.Debug("written graph batch",
				"entities", written.GetEntitiesWritten(), "relationships", written.GetRelationshipsWritten(),
				"status", status(response))
			return nil
		}
		idx, conflict := telemetryConflictIndex(err)
		if !conflict || idx >= len(entities) {
			return fmt.Errorf("write graph (%d entities, %d relationships) failed with status %v: %w%s",
				len(batch.Entities), len(batch.Relationships), status(response), err, errorBody(err))
		}
		kg.log.Debug("entity already exists in the telemetry domain. Referring to it instead of writing it",
			"entity", entities[idx], "telemetryDomain", kg.telemetryDomain)
		kg.telemetryOwned[entities[idx]] = kg.clock()
		batch.Entities = slices.Delete(batch.Entities, idx, idx+1)
		entities = slices.Delete(entities, idx, idx+1)
		if len(batch.Entities) == 0 && len(batch.Relationships) == 0 {
			return nil
		}
	}
}

// telemetryConflictIndex returns the index of the batch entity that already exists in the telemetry domain,
// if this is the cause of the error
func telemetryConflictIndex(err error) (int, bool) {
	var apiErr *kgwrite.GenericOpenAPIError
	if !errors.As(err, &apiErr) {
		return 0, false
	}
	model, ok := apiErr.Model().(kgwrite.ApiError)
	if !ok {
		return 0, false
	}
	return telemetryConflictIndexFromMessage(model.GetMessage())
}

func telemetryConflictIndexFromMessage(msg string) (int, bool) {
	match := telemetryConflict.FindStringSubmatch(msg)
	if match == nil {
		return 0, false
	}
	idx, err := strconv.Atoi(match[1])
	return idx, err == nil
}

func (kg *knowledgeGraph) isTelemetryOwned(e entityKey) bool {
	foundAt, ok := kg.telemetryOwned[e]
	if !ok {
		return false
	}
	if kg.clock().Sub(foundAt) > telemetryOwnershipRecheck {
		// we will try to write it again, in case it does not exist anymore in the telemetry domain
		delete(kg.telemetryOwned, e)
		return false
	}
	return true
}

// domainOf returns the domain where the entity lives: the Beyla domain, or the telemetry domain
// if the entity was found there
func (kg *knowledgeGraph) domainOf(e entityKey) string {
	if _, ok := kg.telemetryOwned[e]; ok {
		return kg.telemetryDomain
	}
	return kg.domain
}

func (kg *knowledgeGraph) entityRef(e entityKey) kgwrite.EntityRefDto {
	ref := kgwrite.NewEntityRefDto(kg.domainOf(e), e.Type, e.Name)
	ref.SetScope(kg.scopeFor(e))
	return *ref
}

func toKGProperties(props properties) map[string]kgwrite.KgPropertyValue {
	out := make(map[string]kgwrite.KgPropertyValue, len(props))
	for k, v := range props {
		// a single invalid property would make the whole batch fail
		if _, reserved := reservedPropertyKeys[k]; reserved {
			continue
		}
		switch tv := v.(type) {
		case string:
			out[k] = kgwrite.StringAsKgPropertyValue(&tv)
		case int64:
			f := float64(tv)
			out[k] = kgwrite.Float64AsKgPropertyValue(&f)
		case bool:
			out[k] = kgwrite.BoolAsKgPropertyValue(&tv)
		}
	}
	return out
}

func (kg *knowledgeGraph) deleteRelationship(ctx context.Context, r *relationKey) error {
	kg.log.Debug("deleting relationship", "type", r.Type, "from", r.From, "to", r.To)
	response, err := kg.client.KnowledgeGraphWriteAPIAPI.
		DeleteRelationship(ctx, kg.namespace).
		XScopeOrgID(kg.stackID).
		Type_(r.Type).
		FromDomain(kg.domainOf(r.From)).
		FromType(r.From.Type).
		FromName(r.From.Name).
		FromScope(kg.scopeFor(r.From)).
		ToDomain(kg.domainOf(r.To)).
		ToType(r.To.Type).
		ToName(r.To.Name).
		ToScope(kg.scopeFor(r.To)).
		Execute()
	if err != nil {
		return fmt.Errorf("delete relationship %s failed: %w%s", r.Type, err, errorBody(err))
	}
	if response == nil || response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("delete relationship %s returned unexpected status %s", r.Type, status(response))
	}
	kg.log.Debug("deleted relationship", "status", status(response))
	return nil
}

func (kg *knowledgeGraph) deleteEntity(ctx context.Context, e entityKey) error {
	if _, ok := kg.telemetryOwned[e]; ok {
		// Beyla never deletes the telemetry entities
		delete(kg.telemetryOwned, e)
		return nil
	}
	kg.log.Debug("deleting entity", "entity", e)
	response, err := kg.client.KnowledgeGraphWriteAPIAPI.
		DeleteEntity(ctx, kg.namespace).
		XScopeOrgID(kg.stackID).
		Domain(kg.domain).
		Type_(e.Type).
		Name(e.Name).
		Scope(kg.scopeFor(e)).
		Execute()
	if err != nil {
		return fmt.Errorf("delete entity %s/%s failed: %w%s", e.Type, e.Name, err, errorBody(err))
	}
	if response == nil || response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("delete entity %s/%s returned unexpected status %s", e.Type, e.Name, status(response))
	}
	kg.log.Debug("deleted entity", "entity", e, "status", status(response))
	return nil
}

// commonScope returns the scope keys that are shared by all the entities
func commonScope(cfg *SemanticConfig) map[string]string {
	scope := map[string]string{}
	if cfg.Site != "" {
		scope[scopeSite] = cfg.Site
	}
	return scope
}

// scopeFor returns the scope of the entity: the common scope plus the "env" scope key and,
// if the entity is in a Kubernetes namespace, the "namespace" scope key
func (kg *knowledgeGraph) scopeFor(e entityKey) map[string]string {
	scope := make(map[string]string, len(kg.scope)+2)
	maps.Copy(scope, kg.scope)
	if e.Env != "" {
		scope[scopeEnv] = e.Env
	}
	if e.Namespace != "" {
		scope[scopeNamespace] = e.Namespace
	}
	return scope
}

func configureGateway(cellGatewayURL, stackID, token string) (gatewayConfig, error) {
	stackID = strings.TrimSpace(stackID)
	baseURL, err := normalizeURL(cellGatewayURL, "cell gateway URL")
	if err != nil {
		return gatewayConfig{}, err
	}
	return gatewayConfig{
		BaseURL:             baseURL,
		AuthorizationHeader: basicAuthHeader(stackID, token),
		Mode:                "cell gateway Basic auth",
	}, nil
}

func normalizeURL(value string, label string) (string, error) {
	raw := strings.TrimRight(strings.TrimSpace(value), "/")
	if raw == "" {
		return "", fmt.Errorf("missing %s", label)
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse %s %q: %w", label, value, err)
	}
	if parsed.Scheme != "https" {
		return "", fmt.Errorf("%s must use https, got scheme %q", label, parsed.Scheme)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("%s %q does not include a host", label, value)
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func basicAuthHeader(username string, password string) string {
	token := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	return "Basic " + token
}

func namespaceForStack(stackID string) (string, error) {
	stackID = strings.TrimSpace(stackID)
	if stackID == "" {
		return "", errors.New("missing numeric stack ID; set GRAFANA_CELL_STACK_ID")
	}
	for _, r := range stackID {
		if r < '0' || r > '9' {
			return "", fmt.Errorf("stack ID %q must contain only digits", stackID)
		}
	}
	return "stacks-" + stackID, nil
}

// errorBody returns the response body of a failed Knowledge Graph request, as the
// generated client's error message does not contain the validation error details
func errorBody(err error) string {
	var apiErr *kgwrite.GenericOpenAPIError
	if errors.As(err, &apiErr) && len(apiErr.Body()) > 0 {
		return " (response: " + string(apiErr.Body()) + ")"
	}
	return ""
}

func status(response *http.Response) any {
	if response == nil {
		return "<nil>"
	}
	if response.Status != "" {
		return response.Status
	}
	return response.StatusCode
}
