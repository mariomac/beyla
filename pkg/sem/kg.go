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
	defaultExpirySeconds int64 = 1 * 60
	requestTimeout             = 30 * time.Second
)

type gatewayConfig struct {
	BaseURL             string
	AuthorizationHeader string
	Mode                string
}

type knowledgeGraph struct {
	client    *kgwrite.APIClient
	gateway   gatewayConfig
	namespace string
	stackID   string
	domain    string
	scope     map[string]string
	log       *slog.Logger
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
		log:       slog.With("component", "sem.KnowledgeGraph"),
		client:    kgwrite.NewAPIClient(kgCfg),
		gateway:   gateway,
		namespace: namespace,
		stackID:   strings.TrimSpace(cfg.StackID),
		domain:    cfg.Domain,
		scope:     map[string]string{"env": "demo"},
	}, nil
}

func (kg *knowledgeGraph) upsertEntity(ctx context.Context, entityType, name string) error {
	entity := kgwrite.NewEntityWriteRequestDto(kg.domain, entityType, name, defaultExpirySeconds)
	entity.SetScope(kg.scope)

	created, response, err := kg.client.KnowledgeGraphWriteAPIAPI.
		UpsertEntity(ctx, kg.namespace).
		XScopeOrgID(kg.stackID).
		EntityWriteRequestDto(*entity).
		AllowUndeclared(true).
		Execute()
	if err != nil {
		return fmt.Errorf("create entity %s/%s failed: %w", entityType, name, err)
	}
	if response == nil || (response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK) {
		return fmt.Errorf("create entity %s/%s returned unexpected status %s", entityType, name, status(response))
	}
	kg.log.Debug("created entity",
		"domain", created.GetDomain(), "type", created.GetType(), "name", created.GetName(), "status", status(response))
	return nil
}

func (kg *knowledgeGraph) upsertRelationship(ctx context.Context, fromType, fromName, toType, toName, relation string) error {
	from := *kgwrite.NewEntityRefDto(kg.domain, fromType, fromName)
	to := *kgwrite.NewEntityRefDto(kg.domain, toType, toName)
	from.SetScope(kg.scope)
	to.SetScope(kg.scope)

	kg.log.Debug("creating relationship", "type", relation,
		"fromType", from.GetType(), "fromName", from.GetName(),
		"toType", to.GetType(), "toName", to.GetName())
	created, response, err := kg.client.KnowledgeGraphWriteAPIAPI.
		UpsertRelationship(ctx, kg.namespace).
		XScopeOrgID(kg.stackID).
		RelationshipWriteRequestDto(*kgwrite.NewRelationshipWriteRequestDto(kg.domain, relation, from, to, defaultExpirySeconds)).
		AllowUndeclared(true).
		Execute()
	if err != nil {
		return fmt.Errorf("create relationship %s failed: %w", relation, err)
	}
	if response == nil || response.StatusCode != http.StatusOK {
		return fmt.Errorf("create relationship %s returned unexpected status %s", relation, status(response))
	}
	kg.log.Debug("created relationship", "domain", created.GetDomain(), "type", created.GetType(), "status", status(response))
	return nil
}

func (kg *knowledgeGraph) deleteRelationship(ctx context.Context, fromType, fromName, toType, toName, relation string) error {
	kg.log.Debug("deleting relationshipn", "type", relation,
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
	kg.log.Debug("Deleted relationship", "status", status(response))
	return nil
}

func (kg *knowledgeGraph) deleteEntity(ctx context.Context, entityType, name string) error {
	fmt.Printf("Deleting entity %s/%s\n", entityType, name)
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
	fmt.Printf("Deleted entity: type=%s name=%s status=%s\n", entityType, name, status(response))
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
		return "", errors.New("missing numeric stack ID; set GRAFANA_STACK_ID or pass -stack-id")
	}
	for _, r := range stackID {
		if r < '0' || r > '9' {
			return "", fmt.Errorf("stack ID %q must contain only digits", stackID)
		}
	}
	return "stacks-" + stackID, nil
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
