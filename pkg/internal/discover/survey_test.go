package discover

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	obiDiscover "go.opentelemetry.io/obi/pkg/appolly/discover"
	"go.opentelemetry.io/obi/pkg/appolly/services"

	"github.com/grafana/beyla/v3/pkg/beyla"
	servicesextra "github.com/grafana/beyla/v3/pkg/services"
)

func TestSurveyKeepsSDKInjectedProcesses(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		cfg := beyla.DefaultConfig()
		cfg.Injector.Webhook.ExternalWebhook = "system/injector"
		cfg.Discovery.Instrument = services.GlobDefinitionCriteria{{Path: services.NewGlob("*")}}
		if legacy {
			cfg.Discovery.Instrument = nil
			cfg.Discovery.Services = services.RegexDefinitionCriteria{{Path: services.NewRegexp(".*")}}
		}
		original := *cfg
		original.Injector.Webhook.ExternalWebhook = ""
		want := obiDiscover.ExcludingCriteria(original.AsOBI())

		assert.Greater(t, len(obiDiscover.ExcludingCriteria(cfg.AsOBI())), len(want))
		assert.Equal(t, want, surveyExcludingCriteria(cfg), "survey retains only the user's exclusions")
	}
}

func TestSurveyCriteriaPreservesSocketSelection(t *testing.T) {
	cfg, err := beyla.LoadConfig(strings.NewReader(`
discovery:
  survey:
    - exe_path: "/opt/apps/*"
      socket_apps:
        enabled: true
        non_root: true
    - k8s_namespace: "prod-*"
    - socket_apps:
        enabled: true
`))
	require.NoError(t, err)
	criteria := surveyCriteria(cfg)
	require.Len(t, criteria, 3)
	for i, wantsSocket := range []bool{true, false, true} {
		selector, ok := criteria[i].(*servicesextra.SurveySelector)
		require.True(t, ok, "normalized selectors must retain the survey extension")
		assert.Equal(t, wantsSocket, selector.SocketApps.Enabled)
		assert.Equal(t, i == 0, selector.SocketApps.NonRoot)
	}
	assert.True(t, criteria[0].GetPath().MatchString("/opt/apps/worker"))
	assert.False(t, criteria[0].GetPath().MatchString("/usr/bin/system-service"))
	assert.True(t, criteria[1].GetPath().IsSet(), "preserve OBI's metadata-only normalization")
	assert.True(t, criteria[1].GetPath().MatchString("/app"))
	assert.True(t, criteria[2].GetPath().IsSet(), "socket-only criteria match any executable")
	assert.True(t, criteria[2].GetPath().MatchString("/app"))
	assert.False(t, cfg.Discovery.Survey[1].Path.IsSet(), "normalization must not mutate the configuration")
	assert.False(t, cfg.Discovery.Survey[2].Path.IsSet())
}
