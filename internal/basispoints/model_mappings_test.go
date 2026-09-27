package basispoints

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func mappedConfig(count int) Config {
	cfg := defaultConfig()
	cfg.UpstreamTransport = "http"
	cfg.DataDir = ""
	cfg.Models = nil
	cfg.ModelMappings = make(map[string]string, count)
	for i := 0; i < count; i++ {
		upstream := fmt.Sprintf("test-model-%d", i)
		alias := upstream + "-basispoints"
		cfg.Models = append(cfg.Models, alias)
		cfg.ModelMappings[alias] = upstream
	}
	return cfg
}

func configureForTest(t *testing.T, svc *Service, cfg Config) {
	t.Helper()
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Handle("plugin.reconfigure", jsonBytes(map[string]any{"config_yaml": raw})); err != nil {
		t.Fatal(err)
	}
}

func TestModelMappingsArbitraryCount(t *testing.T) {
	for _, count := range []int{1, 2, 3, 25, 100} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			cfg := mappedConfig(count)
			if err := cfg.normalize(); err != nil {
				t.Fatal(err)
			}
			models := modelRegistration(cfg)["Models"].([]map[string]any)
			if len(models) != count {
				t.Fatalf("registered %d models, want %d", len(models), count)
			}
			for i, alias := range cfg.Models {
				want := cfg.ModelMappings[alias]
				if models[i]["ID"] != alias || models[i]["Name"] != want {
					t.Fatalf("incorrect registration: %#v", models[i])
				}
				for _, model := range []string{alias, want} {
					body, err := prepareResponsesBody(map[string]any{"model": model, "input": "hello"}, cfg)
					if err != nil || body["model"] != want {
						t.Fatalf("%s routed to %v, want %s: %v", model, body["model"], want, err)
					}
				}
			}
		})
	}
}

func TestConfigModelMappingsValidation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mappings map[string]string
		wantErr  bool
	}{
		{"absent", nil, false},
		{"empty", map[string]string{}, false},
		{"trim", map[string]string{" " + DefaultModelID + " ": " gpt-6-astra "}, false},
		{"unknown-alias", map[string]string{"disabled": "gpt-5.6-sol"}, true},
		{"empty-alias", map[string]string{" ": "gpt-5.6-sol"}, true},
		{"empty-target", map[string]string{DefaultModelID: " "}, true},
		{"duplicate-normalized-alias", map[string]string{DefaultModelID: "gpt-6-astra", " " + DefaultModelID: "gpt-5.6-sol"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaultConfig()
			cfg.ModelMappings = tc.mappings
			err := cfg.normalize()
			if tc.wantErr {
				apiErr, ok := err.(*APIError)
				if !ok || apiErr.Kind != "invalid_config" || apiErr.Status != 400 {
					t.Fatalf("invalid mapping accepted: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got, ok := cfg.upstreamModelForAlias(DefaultModelID); !ok || got != DefaultUpstreamModel {
				t.Fatalf("normalized mapping = %s, %t", got, ok)
			}
		})
	}
}

func TestModelMappingsPreserveUnmappedAliases(t *testing.T) {
	cfg := defaultConfig()
	cfg.Models = append(cfg.Models, "gpt-5.6-sol-basispoints", "another-astra-alias")
	cfg.ModelMappings = map[string]string{"gpt-5.6-sol-basispoints": "gpt-5.6-sol"}
	if err := cfg.normalize(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ alias, upstream string }{
		{DefaultModelID, DefaultUpstreamModel},
		{"another-astra-alias", DefaultUpstreamModel},
		{"gpt-5.6-sol-basispoints", "gpt-5.6-sol"},
	} {
		if got, ok := cfg.upstreamModelForAlias(tc.alias); !ok || got != tc.upstream {
			t.Fatalf("%s routed to %s, want %s", tc.alias, got, tc.upstream)
		}
	}
}

func TestModelMappingsRequestSelection(t *testing.T) {
	svc := newHTTPTestService()
	cfg := mappedConfig(3)
	configureForTest(t, svc, cfg)
	for _, tc := range []struct {
		name, payloadModel, originalModel, executorModel, want string
		wantErr                                                bool
	}{
		{name: "payload-alias", payloadModel: cfg.Models[1], want: "test-model-1"},
		{name: "executor-alias", executorModel: cfg.Models[2], want: "test-model-2"},
		{name: "executor-canonical", executorModel: "test-model-2", want: "test-model-2"},
		{name: "first-enabled", want: "test-model-0"},
		{name: "payload-precedes-executor", payloadModel: cfg.Models[1], executorModel: cfg.Models[0], want: "test-model-1"},
		{name: "original-precedes-payload", originalModel: cfg.Models[2], payloadModel: cfg.Models[1], executorModel: cfg.Models[0], want: "test-model-2"},
		{name: "unknown-alias", payloadModel: "unknown-basispoints", wantErr: true},
		{name: "unknown-canonical", executorModel: "unknown-model", wantErr: true},
		{name: "unused-global-model", payloadModel: DefaultUpstreamModel, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := ExecutorRequest{
				Model:       tc.executorModel,
				Payload:     jsonBytes(map[string]any{"model": tc.payloadModel, "input": "hello"}),
				StorageJSON: jsonBytes(map[string]any{"access_token": "test-access", "account_id": "test-account"}),
			}
			if tc.originalModel != "" {
				req.OriginalRequest = jsonBytes(map[string]any{"model": tc.originalModel, "input": "hello"})
			}
			body, _, err := svc.prepareRequest(req)
			if tc.wantErr {
				apiErr, ok := err.(*APIError)
				if !ok || apiErr.Kind != "unsupported_model" || apiErr.Status != 400 {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err != nil || body["model"] != tc.want {
				t.Fatalf("model = %v, want %s: %v", body["model"], tc.want, err)
			}
		})
	}
	if !reflect.DeepEqual(svc.config(), cfg) {
		t.Fatal("request routing mutated shared configuration")
	}
}

func TestModelMappingsPersistAndClone(t *testing.T) {
	cfg := mappedConfig(3)
	cfg.DataDir = t.TempDir()
	svc := newHTTPTestService()
	configureForTest(t, svc, cfg)
	raw, err := os.ReadFile(filepath.Join(cfg.DataDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved Config
	if err := json.Unmarshal(raw, &saved); err != nil || !reflect.DeepEqual(saved, cfg) {
		t.Fatalf("mapping persistence differs: %v", err)
	}
	restored := newHTTPTestService()
	if err := restored.configure(jsonBytes(map[string]any{"config_yaml": []byte("data_dir: " + cfg.DataDir)})); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored.config(), cfg) {
		t.Fatal("persisted configuration was not restored")
	}
	snapshot := svc.config()
	snapshot.ModelMappings[cfg.Models[0]] = "modified"
	snapshot.Models[0] = "modified-alias"
	if !reflect.DeepEqual(svc.config(), cfg) {
		t.Fatal("configuration snapshots share mutable mapping data")
	}
}

func TestModelMappingsReconfigure(t *testing.T) {
	svc := newHTTPTestService()
	configureForTest(t, svc, mappedConfig(3))
	next := mappedConfig(1)
	next.ModelMappings[next.Models[0]] = "changed-model"
	configureForTest(t, svc, next)
	for _, removed := range []string{"test-model-1-basispoints", "test-model-1", "test-model-0"} {
		if _, ok := svc.config().resolveUpstreamModel(removed); ok {
			t.Fatalf("removed route still enabled: %s", removed)
		}
	}
	if got, ok := svc.config().resolveUpstreamModel(next.Models[0]); !ok || got != "changed-model" {
		t.Fatalf("updated mapping = %s, %t", got, ok)
	}
	invalid := []byte(`data_dir: ""
models: [enabled]
model_mappings: {disabled: invalid}
`)
	if err := svc.configure(jsonBytes(map[string]any{"config_yaml": invalid})); err == nil {
		t.Fatal("invalid reconfiguration accepted")
	}
	if !reflect.DeepEqual(svc.config(), next) {
		t.Fatal("invalid reconfiguration replaced active routes")
	}
}

func TestModelMappingsExecutorRoutesBothModes(t *testing.T) {
	cfg := mappedConfig(3)
	for _, stream := range []bool{false, true} {
		for _, alias := range cfg.Models {
			t.Run(fmt.Sprintf("%s/stream=%t", alias, stream), func(t *testing.T) {
				want := cfg.ModelMappings[alias]
				svc := newHTTPTestService()
				configureForTest(t, svc, cfg)
				response := map[string]any{"id": "resp_model_mapping", "model": want, "status": "completed", "output": []any{messageItem("assistant", "ok")}}
				closed := make(chan map[string]any, 1)
				var emitted []byte
				requests := 0
				svc.SetHost(func(method string, payload any, out any) error {
					switch method {
					case "host.http.do", "host.http.do_stream":
						requests++
						var wire map[string]any
						if err := json.Unmarshal(payload.(map[string]any)["body"].([]byte), &wire); err != nil {
							return err
						}
						if wire["model"] != want || wire["stream"] != stream {
							return fmt.Errorf("wrong upstream model=%v stream=%v", wire["model"], wire["stream"])
						}
						if stream {
							*out.(*upstreamStream) = upstreamStream{StatusCode: 200, StreamID: "upstream-model-mapping"}
						} else {
							*out.(*upstreamResponse) = upstreamResponse{StatusCode: 200, Body: jsonBytes(response)}
						}
					case "host.http.stream_read":
						data := "event: response.completed\ndata: " + string(jsonBytes(map[string]any{"type": "response.completed", "response": response})) + "\n\n"
						*out.(*streamChunk) = streamChunk{Payload: []byte(data), Done: true}
					case "host.http.stream_close":
					case "host.stream.emit":
						emitted = payload.(map[string]any)["payload"].([]byte)
					case "host.stream.close":
						closed <- payload.(map[string]any)
					default:
						return fmt.Errorf("unexpected callback %s", method)
					}
					return nil
				})
				req := ExecutorRequest{Model: alias, Stream: stream, StreamID: "client-model-mapping",
					Payload:     jsonBytes(map[string]any{"model": alias, "input": "hello"}),
					StorageJSON: jsonBytes(map[string]any{"access_token": "test-access", "account_id": "test-account"})}
				method := "executor.execute"
				if stream {
					method = "executor.execute_stream"
				}
				result, err := svc.Handle(method, jsonBytes(req))
				if err != nil {
					t.Fatal(err)
				}
				if stream {
					select {
					case completion := <-closed:
						if completion["error"] != nil || len(emitted) == 0 {
							t.Fatalf("stream failed: %#v", completion)
						}
						clientStreamEvents(t, emitted)
					case <-time.After(5 * time.Second):
						t.Fatal("stream did not close")
					}
				} else {
					var body map[string]any
					if err := json.Unmarshal(result.(map[string]any)["Payload"].([]byte), &body); err != nil || body["model"] != want {
						t.Fatalf("unexpected response model: %v, %v", body["model"], err)
					}
				}
				if requests != 1 {
					t.Fatalf("upstream requests = %d, want 1", requests)
				}
			})
		}
	}
}

func TestModelMappingsParallelRequests(t *testing.T) {
	svc := newHTTPTestService()
	cfg := mappedConfig(25)
	configureForTest(t, svc, cfg)
	t.Cleanup(func() {
		if !reflect.DeepEqual(svc.config(), cfg) {
			t.Fatal("parallel requests changed shared routes")
		}
	})
	for _, alias := range cfg.Models {
		t.Run(alias, func(t *testing.T) {
			t.Parallel()
			req := ExecutorRequest{
				Payload:     jsonBytes(map[string]any{"model": alias, "input": "hello"}),
				StorageJSON: jsonBytes(map[string]any{"access_token": "test-access", "account_id": "test-account"}),
			}
			body, _, err := svc.prepareRequest(req)
			if err != nil || body["model"] != cfg.ModelMappings[alias] {
				t.Fatalf("parallel route = %v, error=%v", body["model"], err)
			}
		})
	}
}

func TestModelMappingsExampleConfig(t *testing.T) {
	raw, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var host struct {
		Plugins struct {
			Enabled bool                 `yaml:"enabled"`
			Configs map[string]yaml.Node `yaml:"configs"`
		} `yaml:"plugins"`
	}
	if err := yaml.Unmarshal(raw, &host); err != nil || !host.Plugins.Enabled {
		t.Fatalf("invalid host example: %v", err)
	}
	node, ok := host.Plugins.Configs[PluginID]
	if !ok {
		t.Fatal("plugin configuration is not nested under plugins.configs")
	}
	var plugin struct {
		Enabled bool   `yaml:"enabled"`
		Config  Config `yaml:",inline"`
	}
	if err := node.Decode(&plugin); err != nil || !plugin.Enabled || plugin.Config.DataDir != "" {
		t.Fatalf("example must enable the plugin without persistent overrides: %v", err)
	}
	configYAML, err := yaml.Marshal(node)
	if err != nil {
		t.Fatal(err)
	}
	svc := newHTTPTestService()
	if _, err := svc.Handle("plugin.register", jsonBytes(map[string]any{"config_yaml": configYAML})); err != nil {
		t.Fatal(err)
	}
	for alias, upstream := range map[string]string{
		"gpt-6-astra-basispoints": "gpt-6-astra",
		"gpt-5.6-sol-basispoints": "gpt-5.6-sol",
	} {
		body, err := prepareResponsesBody(map[string]any{"model": alias, "input": "hello"}, svc.config())
		if err != nil || body["model"] != upstream {
			t.Fatalf("example alias %s routes to %v, want %s: %v", alias, body["model"], upstream, err)
		}
	}
	if !reflect.DeepEqual(svc.status()["model_mappings"], svc.config().ModelMappings) {
		t.Fatal("status omitted configured model mappings")
	}
	metadata := registration(svc.config())["metadata"].(map[string]any)
	for _, field := range metadata["ConfigFields"].([]map[string]any) {
		if field["Name"] == "model_mappings" && field["Type"] == "object" {
			return
		}
	}
	t.Fatal("mapping field is missing from plugin metadata")
}
