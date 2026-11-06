package sem

import (
	"context"
	"fmt"
	"sync"

	"go.opentelemetry.io/obi/pkg/kube"
	"go.opentelemetry.io/obi/pkg/kube/kubecache/informer"
	"go.opentelemetry.io/obi/pkg/pipe/swarm"
)

type SemanticConfig struct {
	// CellGatewayURL from Grafana, for example https://asserts-dev-us-central-0.grafana-dev.net
	CellGatewayURL string `yaml:"cell_gateway_url" env:"GRAFANA_CELL_GATEWAY_URL"`
	// StackID is the numeric Grafana stack id
	StackID string `yaml:"stack_id" env:"GRAFANA_CELL_STACK_ID"`

	// TODO: reuse beyla global credentials
	GrafanaGCOMToken string `env:"GRAFANA_GCOM_TOKEN"`

	// Domain is the default knowledge graph domain for the generated entities
	// TODO: override it by using pod annotations
	Domain string `yaml:"domain" env:"GRAFANA_CELL_DOMAIN" envDefault:"beyla"`
}

func SemanticExporter(
	ctx context.Context,
	sc SemanticConfig,
	kubeStore *kube.Store,
) swarm.InstanceFunc {
	return func(ctx context.Context) (swarm.RunFunc, error) {
		if sc.CellGatewayURL == "" || sc.StackID == "" || sc.GrafanaGCOMToken == "" {
			return swarm.EmptyRunFunc()
		}
		kg, err := createKnowledgeGraph(sc)
		if err != nil {
			return nil, fmt.Errorf("creating knowledge graph interface: %w", err)
		}
		se := semanticExporter{
			kg:        kg,
			kubeStore: kubeStore,
		}
		return se.run, nil
	}
}

type entity struct {
	eType string
	name  string
}

type relation struct {
	rType string
	src   entity
	dst   entity
}

type semanticExporter struct {
	mut       sync.Mutex
	entities  []entity
	relations []relation
	kg        *knowledgeGraph
	kubeStore *kube.Store
}

func (se *semanticExporter) run(ctx context.Context) {
	if se.kubeStore != nil {
		se.kubeStore.Subscribe(se)
	}

}

func (se *semanticExporter) ID() string { return "semantic-exporter" }

func (se *semanticExporter) On(event *informer.Event) error {
	//TODO implement me
	panic("implement me")
}
