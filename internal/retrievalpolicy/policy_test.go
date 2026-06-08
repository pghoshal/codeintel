package retrievalpolicy

import "testing"

func TestMandatorySourceHintsOpenTelemetryOTLPAcrossRepos(t *testing.T) {
	query := "Explain OTLP trace export flow with ExportTraceServiceRequest from SDK to collector."
	repos := []string{
		"github.com/open-telemetry/opentelemetry-go",
		"github.com/open-telemetry/opentelemetry-collector",
		"github.com/open-telemetry/opentelemetry-proto",
	}
	hints := MandatorySourceHints(query, repos)
	seen := map[string]bool{}
	for _, hint := range hints {
		seen[hint.Repo+" "+hint.Path] = true
	}
	for _, want := range []string{
		"github.com/open-telemetry/opentelemetry-go exporters/otlp/otlptrace/otlptracehttp/client.go",
		"github.com/open-telemetry/opentelemetry-collector pdata/internal/generated_proto_exporttraceservicerequest.go",
		"github.com/open-telemetry/opentelemetry-proto opentelemetry/proto/collector/trace/v1/trace_service.proto",
	} {
		if !seen[want] {
			t.Fatalf("mandatory OTLP source hints missing %s: %+v", want, hints)
		}
	}
}

func TestPreferredPathFragmentsIncludesRuntimeAndOTLPFragments(t *testing.T) {
	got := PreferredPathFragments("Explain Python auto-instrumentation OTLP trace export flow")
	seen := map[string]bool{}
	for _, item := range got {
		seen[item] = true
	}
	for _, want := range []string{"internal/instrumentation/python.go", "trace_service.proto", "otlptracehttp"} {
		if !seen[want] {
			t.Fatalf("preferred path fragments missing %q: %+v", want, got)
		}
	}
}

func TestPreferredPathFragmentsDoesNotBoostOperatorPathsForGenericQuestions(t *testing.T) {
	got := PreferredPathFragments("Explain checkout payment flow across services")
	for _, item := range got {
		if item == "internal/instrumentation/sdk.go" || item == "apis/v1alpha1/instrumentation_types.go" {
			t.Fatalf("generic query should not receive operator-specific path boost: %+v", got)
		}
	}
}
