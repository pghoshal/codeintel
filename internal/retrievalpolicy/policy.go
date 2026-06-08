package retrievalpolicy

import "strings"

// SourceHint is a deterministic retrieval playbook hint. Generic MCP
// code can score and validate the hinted source slice, but repo-family
// path knowledge lives here instead of in the tool orchestration path.
type SourceHint struct {
	Repo       string
	Path       string
	Line       int
	Source     string
	ScoreBoost int
}

// OTLPFlowPattern returns high-signal protocol/SDK/collector terms for
// OpenTelemetry OTLP trace-export questions. It is intentionally scoped
// as a repo-family policy; generic retrieval should call this through
// policy dispatch instead of embedding these constants inline.
func OTLPFlowPattern(query string) string {
	lower := strings.ToLower(query)
	if !strings.Contains(lower, "otlp") &&
		!strings.Contains(lower, "exporttrace") &&
		!strings.Contains(lower, "trace export") &&
		!strings.Contains(lower, "traceservice") &&
		!strings.Contains(lower, "collector") {
		return ""
	}
	return strings.Join(cleanTerms([]string{
		"ExportTraceServiceRequest",
		"TraceService",
		"otlptracegrpc",
		"otlptracehttp",
		"NewExporter",
		"ExportSpans",
		"SpanExporter",
		"NewTracerProvider",
		"TracerProvider",
		"WithEndpoint",
		"ConsumeTraces",
		"pdata.Traces",
		"trace_service.proto",
		"generated_proto_exporttraceservicerequest",
		"receiver",
		"exporter",
	}, 32), "|")
}

// PreferredPathFragments returns source-slice path fragments that are
// useful when the query clearly asks about a known architecture family.
func PreferredPathFragments(query string) []string {
	lower := strings.ToLower(query)
	out := []string{}
	if hasInstrumentationHints(lower) || strings.Contains(lower, "webhook") || strings.Contains(lower, "admission") || strings.Contains(lower, "controller") || strings.Contains(lower, "crd") {
		out = append(out,
			"apis/v1alpha1/instrumentation_types.go",
			"apis/v1beta1/instrumentation_types.go",
			"config/webhook/manifests.yaml",
			"config/default/manager_webhook_patch.yaml",
			"main.go",
			"internal/webhook/podmutation/webhookhandler.go",
			"internal/instrumentation/podmutator.go",
			"internal/instrumentation/sdk.go",
			"internal/instrumentation/annotation.go",
		)
		if strings.Contains(lower, "node") || strings.Contains(lower, "javascript") || strings.Contains(lower, "js") {
			out = append(out, "internal/instrumentation/nodejs.go")
		}
		if strings.Contains(lower, "python") {
			out = append(out, "internal/instrumentation/python.go")
		}
		if strings.Contains(lower, ".net") || strings.Contains(lower, "dotnet") || strings.Contains(lower, "c#") {
			out = append(out, "internal/instrumentation/dotnet.go")
		}
	}
	if OTLPFlowPattern(query) != "" {
		out = append(out,
			"pdata/internal/generated_proto_exporttraceservicerequest.go",
			"trace_service.proto",
			"otlptracegrpc",
			"otlptracehttp",
			"exporter",
			"receiver",
		)
	}
	return out
}

// MandatorySourceHints returns repo-family source slices that should be
// force-read when a broad architecture question would otherwise miss a
// critical cross-repo contract file. These are hints, not trusted facts:
// callers still read the source and include only available local proof.
func MandatorySourceHints(query string, repos []string) []SourceHint {
	const maxHints = 96
	lower := strings.ToLower(query)
	wantsPythonConstants := strings.Contains(lower, "python") || strings.Contains(lower, "pythonpath") || strings.Contains(lower, "envpythonpath") || strings.Contains(lower, "pythonpathprefix") || strings.Contains(lower, "pythonpathsuffix")
	wantsOTLPFlow := OTLPFlowPattern(query) != ""
	if !hasInstrumentationHints(lower) && !strings.Contains(lower, "crd") && !strings.Contains(lower, "webhook") && !strings.Contains(lower, "admission") && !wantsPythonConstants && !wantsOTLPFlow {
		return nil
	}
	out := make([]SourceHint, 0, minInt(maxHints, len(repos)*8))
	seen := map[string]bool{}
	add := func(repo, path string, line int, source string, scoreBoost int) {
		if len(out) >= maxHints {
			return
		}
		key := repo + "\x00" + path + "\x00" + source
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, SourceHint{Repo: repo, Path: path, Line: line, Source: source, ScoreBoost: scoreBoost})
	}
	for _, repo := range repos {
		repoLower := strings.ToLower(repo)
		if wantsOTLPFlow {
			addOTLPHints(add, repo, repoLower)
		}
		if !strings.Contains(repoLower, "operator") {
			continue
		}
		for _, item := range []struct {
			path   string
			line   int
			source string
		}{
			{path: "apis/v1alpha1/instrumentation_types.go", line: 13, source: "crd-spec"},
			{path: "apis/v1alpha1/instrumentation_types.go", line: 232, source: "crd-spec"},
			{path: "apis/v1alpha1/instrumentation_types.go", line: 259, source: "crd-spec"},
			{path: "config/webhook/manifests.yaml", line: 1, source: "webhook-registration"},
			{path: "config/default/manager_webhook_patch.yaml", line: 1, source: "webhook-registration"},
			{path: "main.go", line: 448, source: "webhook-registration"},
		} {
			add(repo, item.path, item.line, item.source, 40)
		}
		if wantsPythonConstants {
			add(repo, "internal/instrumentation/python.go", 1, "python-constants", 40)
		}
	}
	return out
}

func addOTLPHints(add func(repo, path string, line int, source string, scoreBoost int), repo, repoLower string) {
	switch {
	case strings.Contains(repoLower, "opentelemetry-proto"):
		add(repo, "opentelemetry/proto/collector/trace/v1/trace_service.proto", 30, "otlp-flow", 80)
		add(repo, "opentelemetry/proto/trace/v1/trace.proto", 48, "otlp-flow", 70)
	case strings.Contains(repoLower, "opentelemetry-specification"):
		add(repo, "oteps/0099-otlp-http.md", 32, "otlp-flow", 70)
		add(repo, "specification/protocol/exporter.md", 1, "otlp-flow", 55)
	case strings.Contains(repoLower, "opentelemetry-go"):
		add(repo, "exporters/otlp/otlptrace/otlptracehttp/client.go", 159, "otlp-flow", 80)
		add(repo, "exporters/otlp/otlptrace/otlptracegrpc/client.go", 197, "otlp-flow", 75)
	case strings.Contains(repoLower, "opentelemetry-js"):
		add(repo, "experimental/packages/otlp-transformer/src/trace/internal.ts", 97, "otlp-flow", 80)
		add(repo, "experimental/packages/otlp-exporter-base/src/transport/http-exporter-transport.ts", 28, "otlp-flow", 65)
	case strings.Contains(repoLower, "opentelemetry-java"):
		add(repo, "exporters/otlp/all/src/main/java/io/opentelemetry/exporter/otlp/trace/OtlpGrpcSpanExporter.java", 21, "otlp-flow", 80)
		add(repo, "exporters/otlp/all/src/main/java/io/opentelemetry/exporter/otlp/http/trace/OtlpHttpSpanExporter.java", 25, "otlp-flow", 75)
	case strings.Contains(repoLower, "opentelemetry-python"):
		add(repo, "exporter/opentelemetry-exporter-otlp-proto-grpc/src/opentelemetry/exporter/otlp/proto/grpc/trace_exporter/__init__.py", 60, "otlp-flow", 80)
		add(repo, "exporter/opentelemetry-exporter-otlp-proto-http/src/opentelemetry/exporter/otlp/proto/http/trace_exporter/__init__.py", 72, "otlp-flow", 75)
	case strings.Contains(repoLower, "opentelemetry-dotnet"):
		add(repo, "src/OpenTelemetry.Exporter.OpenTelemetryProtocol/OtlpTraceExporter.cs", 17, "otlp-flow", 80)
		add(repo, "src/OpenTelemetry.Exporter.OpenTelemetryProtocol/OtlpTraceExporterHelperExtensions.cs", 23, "otlp-flow", 75)
	case strings.Contains(repoLower, "opentelemetry-rust"):
		add(repo, "opentelemetry-otlp/src/span.rs", 145, "otlp-flow", 80)
		add(repo, "opentelemetry-otlp/src/exporter/tonic/trace.rs", 22, "otlp-flow", 75)
	case strings.Contains(repoLower, "opentelemetry-collector"):
		add(repo, "pdata/internal/generated_proto_exporttraceservicerequest.go", 20, "otlp-flow", 85)
		add(repo, "exporter/otlphttpexporter/otlp.go", 93, "otlp-flow", 80)
		add(repo, "service/internal/builders/exporter.go", 24, "otlp-flow", 65)
	}
}

func hasInstrumentationHints(lower string) bool {
	for _, marker := range []string{"instrumentation", "auto-instrument", "inject", "sdk", "runtime"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func cleanTerms(candidates []string, max int) []string {
	out := make([]string, 0, max)
	seen := map[string]bool{}
	for _, candidate := range candidates {
		candidate = strings.Trim(candidate, ".,:;()[]{}<>\"'`")
		lower := strings.ToLower(candidate)
		if len(candidate) < 3 || seen[lower] {
			continue
		}
		seen[lower] = true
		out = append(out, candidate)
		if len(out) >= max {
			break
		}
	}
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
