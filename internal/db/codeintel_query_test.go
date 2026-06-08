package db

import "testing"

func TestCompactCodeGraphTermsKeepsHighSignalRuntimeAnchors(t *testing.T) {
	terms := tokenizeCodeIntelQuery("architecture flow inject injectNodeJS injectPython injectDotNet sdkInjector instrumentation Instrumentation InstrumentationSpec injectCommonSDKConfig injectCommonEnvVar NodeJS nodejs Python python DotNet dotnet")
	got := compactCodeGraphTerms(terms)
	for _, bad := range []string{"architecture", "flow", "inject", "SDK", "instrumentation", "InstrumentationSpec"} {
		if testContainsString(got, bad) {
			t.Fatalf("compact terms kept broad token %q: %+v", bad, got)
		}
	}
	for _, want := range []string{"injectNodeJS", "injectPython", "injectDotNet", "sdkInjector", "injectCommonSDKConfig", "injectCommonEnvVar"} {
		if !testContainsString(got, want) {
			t.Fatalf("compact terms missing high-signal token %q: %+v", want, got)
		}
	}
}

func testContainsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
