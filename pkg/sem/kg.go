package sem

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
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
)

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
		scope:      map[string]string{"env": "demo"},
		ttlSeconds: int64(cfg.TTL.Seconds()),
	}, nil
}

// writeGraph upserts all the entities and relationships of the graph. The Knowledge Graph
// limits the number of items per request, so the graph is split in multiple batches.
// All the entities are sent before the relationships, as the Knowledge Graph does not create
// stub entities for unresolved relationship endpoints.
func (kg *knowledgeGraph) writeGraph(ctx context.Context, g *graph) error {
	entities := g.sortedEntities()
	relations := g.sortedRelations()
	var errs []error
	batch := kgwrite.GraphWriteRequestDto{}
	items := 0
	send := func() {
		if items == 0 {
			return
		}
		if err := kg.writeBatch(ctx, &batch); err != nil {
			errs = append(errs, err)
		}
		batch = kgwrite.GraphWriteRequestDto{}
		items = 0
	}
	for _, e := range entities {
		dto := kgwrite.NewEntityWriteRequestDto(kg.domain, e.Type, e.Name, kg.ttlSeconds)
		dto.SetScope(kg.scope)
		if props := g.entities[e]; len(props) > 0 {
			dto.SetProperties(toKGProperties(props))
		}
		batch.Entities = append(batch.Entities, *dto)
		if items++; items >= maxBatchItems {
			send()
		}
	}
	// flushing pending entities before starting sending relations
	send()
	for _, r := range relations {
		from := kgwrite.NewEntityRefDto(kg.domain, r.From.Type, r.From.Name)
		to := kgwrite.NewEntityRefDto(kg.domain, r.To.Type, r.To.Name)
		from.SetScope(kg.scope)
		to.SetScope(kg.scope)
		batch.Relationships = append(batch.Relationships,
			*kgwrite.NewRelationshipWriteRequestDto(kg.domain, r.Type, *from, *to, kg.ttlSeconds))
		if items++; items >= maxBatchItems {
			send()
		}
	}
	send()
	return errors.Join(errs...)
}

func (kg *knowledgeGraph) writeBatch(ctx context.Context, batch *kgwrite.GraphWriteRequestDto) error {
	written, response, err := kg.client.KnowledgeGraphWriteAPIAPI.
		WriteGraph(ctx, kg.namespace).
		XScopeOrgID(kg.stackID).
		GraphWriteRequestDto(*batch).
		AllowUndeclared(true).
		Execute()
	if err != nil {
		return fmt.Errorf("write graph (%d entities, %d relationships) failed with status %v: %w%s",
			len(batch.Entities), len(batch.Relationships), status(response), err, errorBody(err))
	}
	kg.log.Debug("written graph batch",
		"entities", written.GetEntitiesWritten(), "relationships", written.GetRelationshipsWritten(),
		"status", status(response))
	return nil
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

func (kg *knowledgeGraph) deleteRelationship(ctx context.Context, fromType, fromName, toType, toName, relation string) error {
	kg.log.Debug("deleting relationship", "type", relation,
		"fromType", fromType, "fromName", fromName,
		"toType", toType, "toName", toName)
	response, err := kg.client.KnowledgeGraphWriteAPIAPI.
		DeleteRelationship(ctx, kg.namespace).
		XScopeOrgID(kg.stackID).
		Type_(relation).
		FromDomain(kg.domain).
		FromType(fromType).
		FromName(fromName).
		FromScope(kg.scope).
		ToDomain(kg.domain).
		ToType(toType).
		ToName(toName).
		ToScope(kg.scope).
		Execute()
	if err != nil {
		return fmt.Errorf("delete relationship %s failed: %w", relation, err)
	}
	if response == nil || response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("delete relationship %s returned unexpected status %s", relation, status(response))
	}
	kg.log.Debug("deleted relationship", "status", status(response))
	return nil
}

func (kg *knowledgeGraph) deleteEntity(ctx context.Context, entityType, name string) error {
	kg.log.Debug("deleting entity", "type", entityType, "name", name)
	response, err := kg.client.KnowledgeGraphWriteAPIAPI.
		DeleteEntity(ctx, kg.namespace).
		XScopeOrgID(kg.stackID).
		Domain(kg.domain).
		Type_(entityType).
		Name(name).
		Scope(kg.scope).
		Execute()
	if err != nil {
		return fmt.Errorf("delete entity %s/%s failed: %w", entityType, name, err)
	}
	if response == nil || response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("delete entity %s/%s returned unexpected status %s", entityType, name, status(response))
	}
	kg.log.Debug("deleted entity", "type", entityType, "name", name, "status", status(response))
	return nil
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
