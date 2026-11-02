package pluginhost

import (
	"context"
	"reflect"
	"testing"

	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestRegisteredPluginCompatibleEndpoints(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps pluginapi.Capabilities
		want []string
	}{
		{"native-openai-with-native-claude-converters", pluginapi.Capabilities{Executor: &fakeExecutor{identifier: "test"}, ExecutorInputFormats: []string{"chat-completions"}, ExecutorOutputFormats: []string{"chat-completions"}}, []string{"/v1/chat/completions", "/v1/messages"}},
		{"auth-only", pluginapi.Capabilities{AuthProvider: fakeAuthProvider{identifier: "test"}}, []string{}},
		{"missing-formats", pluginapi.Capabilities{Executor: &fakeExecutor{identifier: "test"}}, []string{}},
		{"unknown-formats", pluginapi.Capabilities{Executor: &fakeExecutor{identifier: "test"}, ExecutorInputFormats: []string{"unknown-protocol"}, ExecutorOutputFormats: []string{"unknown-protocol"}}, []string{}},
		{"unknown-output", pluginapi.Capabilities{Executor: &fakeExecutor{identifier: "test"}, ExecutorInputFormats: []string{"openai"}, ExecutorOutputFormats: []string{"unknown-protocol"}}, []string{}},
		{"static-only", pluginapi.Capabilities{Executor: &fakeExecutor{identifier: "test"}, ExecutorModelScope: pluginapi.ExecutorModelScopeStatic, ExecutorInputFormats: []string{"openai"}, ExecutorOutputFormats: []string{"openai"}}, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := newHostWithRecords(capabilityRecord{id: "candidate", plugin: pluginapi.Plugin{Capabilities: tc.caps}})
			info := host.RegisteredPlugins()
			if len(info) != 1 || !reflect.DeepEqual(info[0].CompatibleEndpoints, tc.want) {
				t.Fatalf("got %+v want %v", info, tc.want)
			}
			setHostSnapshotForTest(host, false)
			if len(host.RegisteredPlugins()) != 0 {
				t.Fatal("disabled host advertised endpoints")
			}
		})
	}
}

func TestCompatibleEndpointsRequireBothResponseModes(t *testing.T) {
	for _, mode := range []string{"stream-only", "nonstream-only", "both"} {
		t.Run(mode, func(t *testing.T) {
			output := sdktranslator.Format("protocol-proposal-" + mode)
			transform := sdktranslator.ResponseTransform{}
			if mode != "nonstream-only" {
				transform.Stream = func(_ context.Context, _ string, _, _, raw []byte, _ *any) [][]byte { return [][]byte{raw} }
			}
			if mode != "stream-only" {
				transform.NonStream = func(_ context.Context, _ string, _, _, raw []byte, _ *any) []byte { return raw }
			}
			sdktranslator.Register(sdktranslator.FormatOpenAI, output, nil, transform)
			defer sdktranslator.Unregister(sdktranslator.FormatOpenAI, output)
			host := newHostWithRecords(capabilityRecord{id: "modes", plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{
				Executor: &fakeExecutor{identifier: "modes", countTokens: func(context.Context, pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
					t.Fatal("CountTokens must not be required")
					return pluginapi.ExecutorResponse{}, nil
				}},
				ExecutorInputFormats: []string{"openai"}, ExecutorOutputFormats: []string{output.String()},
			}}})
			entries := host.RegisteredPlugins()[0].CompatibleEndpoints
			want := []string{}
			if mode == "both" {
				want = []string{"/v1/chat/completions"}
			}
			if !reflect.DeepEqual(entries, want) {
				t.Fatalf("%s: %v want %v", mode, entries, want)
			}
			record := host.activeRecords()[0]
			setHostSnapshotForTest(host, true)
			if len(host.compatibleOAuthEndpoints(record)) != 0 {
				t.Fatal("stale registration advertised endpoints")
			}
		})
	}
}
