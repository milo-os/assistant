package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	openapinamer "k8s.io/apiserver/pkg/endpoints/openapi"
	genericapiserver "k8s.io/apiserver/pkg/server"
	genericfilters "k8s.io/apiserver/pkg/server/filters"
	"k8s.io/apiserver/pkg/server/healthz"
	"k8s.io/apiserver/pkg/server/options"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	basecompatibility "k8s.io/component-base/compatibility"
	"k8s.io/component-base/logs"
	logsapi "k8s.io/component-base/logs/api/v1"
	"k8s.io/klog/v2"
	openapicommon "k8s.io/kube-openapi/pkg/common"

	_ "k8s.io/component-base/logs/json/register"

	"github.com/milo-os/assistant/internal/agentwiring"
	assistantapiserver "github.com/milo-os/assistant/internal/apiserver"
	"github.com/milo-os/assistant/internal/config"
	"github.com/milo-os/assistant/internal/gapreport"
	"github.com/milo-os/assistant/internal/history"
	appmetrics "github.com/milo-os/assistant/internal/metrics"
	"github.com/milo-os/assistant/internal/tracing"
	generatedopenapi "github.com/milo-os/assistant/pkg/generated/openapi"
)

func init() {
	// Register the logging feature gates the recommended options' logging
	// config validates against (ContextualLogging, LoggingBetaOptions, …).
	utilruntime.Must(logsapi.AddFeatureGates(utilfeature.DefaultMutableFeatureGate))
}

// serverOptions bundles the recommended aggregated-apiserver options with the
// conversation store connection. Delegated authn/authz come from
// RecommendedOptions and are wired by flags (empty kubeconfig ⇒ in-cluster).
type serverOptions struct {
	Recommended *options.RecommendedOptions
	Logs        *logsapi.LoggingConfiguration
	PostgresDSN string
}

func newServerOptions() *serverOptions {
	o := &serverOptions{
		Recommended: options.NewRecommendedOptions(
			"/registry/assistant.miloapis.com",
			assistantapiserver.Codecs.LegacyCodec(assistantapiserver.Scheme.PrioritizedVersionsAllGroups()...),
		),
		Logs:        logsapi.NewLoggingConfiguration(),
		PostgresDSN: os.Getenv("CONVERSATION_STORE_URL"),
	}
	// This is a read view over Postgres via internal/history — never etcd — and
	// a delegating aggregated server whose front kube-apiserver already ran
	// admission, so both are disabled (their ApplyTo is nil-safe).
	o.Recommended.Etcd = nil
	o.Recommended.Admission = nil
	return o
}

func (o *serverOptions) addFlags(fs *pflag.FlagSet) {
	o.Recommended.AddFlags(fs)
	logsapi.AddFlags(o.Logs, fs)
	fs.StringVar(&o.PostgresDSN, "postgres-dsn", o.PostgresDSN,
		"PostgreSQL connection URL for the shared conversation store (defaults to $CONVERSATION_STORE_URL)")
}

func (o *serverOptions) validate() error {
	if strings.TrimSpace(o.PostgresDSN) == "" {
		return fmt.Errorf("--postgres-dsn (or $CONVERSATION_STORE_URL) is required")
	}
	return nil
}

func newServeCommand() *cobra.Command {
	o := newServerOptions()
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the conversations API server",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := o.validate(); err != nil {
				return err
			}
			return o.run(cmd.Context())
		},
	}
	o.addFlags(cmd.Flags())
	return cmd
}

func (o *serverOptions) config(ctx context.Context) (*assistantapiserver.Config, func(), error) {
	if err := o.Recommended.SecureServing.MaybeDefaultWithSelfSignedCerts("localhost", nil, nil); err != nil {
		return nil, nil, fmt.Errorf("create self-signed certificates: %w", err)
	}

	genericConfig := genericapiserver.NewRecommendedConfig(assistantapiserver.Codecs)
	genericConfig.EffectiveVersion = basecompatibility.NewEffectiveVersionFromString("1.36", "", "")

	// NewRecommendedConfig's default LongRunningFunc only treats the
	// upstream subresource set (watch/attach/exec/proxy/log/portforward) as
	// long-running. sendmessage streams an SSE response for the duration of
	// a full agent turn (tool calls included), so without this it's wrapped
	// by WithTimeoutForNonLongRunningRequests like any ordinary read and got
	// cut off mid-turn — the browser saw "upstream connect error ... reset
	// reason: protocol error" and the apiserver logged "Timeout or abort
	// while handling" / agent.turn.completed outcome=canceled around 10s in.
	// Same fix pods/exec and pods/log --follow needed upstream.
	genericConfig.LongRunningFunc = genericfilters.BasicLongRunningRequestCheck(
		sets.NewString("watch", "proxy"),
		sets.NewString("attach", "exec", "proxy", "log", "portforward", "sendmessage"),
	)

	// Wrap the generic-apiserver's normal handler chain (auth, audit,
	// panic-recovery, etc. — DefaultBuildHandlerChain) with an outer otelhttp
	// span per request, same as cmd/assistant's plain net/http server. No-op
	// (a single no-op span, no exporter, no dial) unless [tracing.Setup]
	// installed a real tracer provider.
	genericConfig.BuildHandlerChainFunc = func(apiHandler http.Handler, c *genericapiserver.Config) http.Handler {
		return otelhttp.NewHandler(genericapiserver.DefaultBuildHandlerChain(apiHandler, c), "assistant-apiserver.http")
	}

	namer := openapinamer.NewDefinitionNamer(assistantapiserver.Scheme)
	getDefs := func(ref openapicommon.ReferenceCallback) map[string]openapicommon.OpenAPIDefinition {
		return generatedopenapi.GetOpenAPIDefinitions(ref)
	}
	genericConfig.OpenAPIV3Config = genericapiserver.DefaultOpenAPIV3Config(getDefs, namer)
	genericConfig.OpenAPIV3Config.Info.Title = "conversations"
	genericConfig.OpenAPIConfig = genericapiserver.DefaultOpenAPIConfig(getDefs, namer)
	genericConfig.OpenAPIConfig.Info.Title = "conversations"

	// Installs the delegated authenticator/authorizer (TokenReview/SAR) into
	// genericConfig from the --authentication-kubeconfig/--authorization-kubeconfig
	// flags; empty ⇒ in-cluster fallback (dev uses the kind apiserver).
	if err := o.Recommended.ApplyTo(genericConfig); err != nil {
		return nil, nil, fmt.Errorf("apply recommended options: %w", err)
	}

	store, err := history.NewPostgresStore(ctx, o.PostgresDSN, slog.Default())
	if err != nil {
		return nil, nil, fmt.Errorf("connect conversation store: %w", err)
	}
	genericConfig.AddReadyzChecks(healthz.NamedCheck("postgres", func(r *http.Request) error {
		pingCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		return store.Ping(pingCtx)
	}))

	// Same database, a separate table (internal/gapreport) — the
	// capabilitygapreports resource is a read view over it, same shape as
	// conversations over internal/history.
	gapStore, err := gapreport.NewPostgresStore(ctx, o.PostgresDSN, slog.Default())
	if err != nil {
		store.Close()
		return nil, nil, fmt.Errorf("connect gap-report store: %w", err)
	}

	// The conversations/sendmessage subresource needs the same agent-execution
	// stack (model, capability source, memory/gap-report stores, usage
	// emitter) as cmd/assistant — built by the same internal/agentwiring
	// constructor, from the same [config.Config] shape, so browser (SSE) and
	// A2A traffic never drift onto different wiring. This reuses
	// cmd/assistant's exact env var names (MODEL_MODE, ANTHROPIC_API_KEY,
	// CAPABILITY_*, PERSONA_PROMPT_FILE, USAGE_GATEWAY_*, …; see
	// internal/config's doc comment) rather than inventing a second set.
	//
	// config.Load also requires AUTHN_TOKENREVIEW_API_URL/AUTHZ_SAR_API_URL,
	// which this process does not otherwise use (its own authn/authz is the
	// delegated TokenReview/SAR wired by o.Recommended.ApplyTo above) — but
	// both derive automatically from KUBERNETES_SERVICE_HOST/PORT in any
	// in-cluster deployment, so no new required setting reaches operators in
	// practice; only off-cluster/local runs need to set them explicitly, the
	// same as cmd/assistant already does.
	agentCfg, err := config.Load(os.Getenv)
	if err != nil {
		store.Close()
		gapStore.Close()
		return nil, nil, fmt.Errorf("load agent config: %w", err)
	}
	// The DSN this process actually connected the read stores with above
	// (which may come from --postgres-dsn rather than $CONVERSATION_STORE_URL)
	// is authoritative: the agent runner's own history/memory/gap-report
	// stores must point at the same database, never silently fall back to
	// in-memory because the env var alone was empty.
	agentCfg.ConversationStoreURL = o.PostgresDSN

	runner, _, runnerCleanup, err := agentwiring.NewRunner(ctx, agentCfg, slog.Default(), appmetrics.New())
	if err != nil {
		store.Close()
		gapStore.Close()
		return nil, nil, fmt.Errorf("build agent runner: %w", err)
	}

	return &assistantapiserver.Config{
			GenericConfig: genericConfig,
			ExtraConfig: assistantapiserver.ExtraConfig{
				Reader:     store,
				GapReports: gapStore,
				Runner:     runner,
				// The address clients should send A2A traffic to. Read from the
				// same env the service uses for its agent card, so discovery and
				// the card cannot disagree.
				PublicBaseURL: os.Getenv("PUBLIC_BASE_URL"),
			},
		}, func() {
			store.Close()
			gapStore.Close()
			runnerCleanup()
		}, nil
}

func (o *serverOptions) run(ctx context.Context) error {
	if err := logsapi.ValidateAndApply(o.Logs, utilfeature.DefaultMutableFeatureGate); err != nil {
		return fmt.Errorf("apply logging configuration: %w", err)
	}
	defer logs.FlushLogs()

	// Tracing: no-op unless OTEL_EXPORTER_OTLP_ENDPOINT is set (see
	// internal/tracing). Safe to call unconditionally.
	tracingShutdown, err := tracing.Setup(ctx, "assistant-apiserver")
	if err != nil {
		return fmt.Errorf("failed to initialize tracing: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tracingShutdown(shutdownCtx); err != nil {
			klog.ErrorS(err, "tracing shutdown failed")
		}
	}()

	cfg, cleanup, err := o.config(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	server, err := cfg.Complete().New()
	if err != nil {
		return err
	}

	klog.InfoS("starting conversations apiserver")
	return server.GenericAPIServer.PrepareRun().RunWithContext(ctx)
}
