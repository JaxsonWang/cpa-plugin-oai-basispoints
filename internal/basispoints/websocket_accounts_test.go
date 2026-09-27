package basispoints

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
)

func TestWebSocketCredentialsRemainIndependent(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			type counters struct{ handshakes, creates, posts atomic.Int32 }
			counts := map[string]*counters{"a": {}, "b": {}}
			var rejectB atomic.Bool
			checkCredential := func(headers http.Header) (string, error) {
				// 回调携带尚未经过 net/http 规范化的头部 map。
				normalized := make(http.Header, len(headers))
				for name, values := range headers {
					normalized[http.CanonicalHeaderKey(name)] = values
				}
				headers = normalized
				account := strings.TrimPrefix(headers.Get("Authorization"), "Bearer fixture-")
				if counts[account] == nil || headers.Get("ChatGPT-Account-ID") != "account-"+account || headers.Get("X-OpenAI-Account-ID") != "account-"+account {
					return "", fmt.Errorf("请求混用了不同账号的凭据")
				}
				return account, nil
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				account, err := checkCredential(r.Header)
				if err != nil {
					t.Error(err)
					http.Error(w, "invalid fixture credential", http.StatusUnauthorized)
					return
				}
				counts[account].handshakes.Add(1)
				if account == "b" && rejectB.Load() {
					http.Error(w, `{"detail":"Responses websocket transport is not enabled."}`, http.StatusNotFound)
					return
				}
				conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				var body map[string]any
				if err := conn.ReadJSON(&body); err != nil {
					t.Error(err)
					return
				}
				if body["type"] != "response.create" || !bytes.Contains(jsonBytes(body["input"]), []byte("INPUT_"+account)) {
					t.Error("WS 正文与当前握手账号不一致")
				}
				counts[account].creates.Add(1)
				if err := writeWebSocketEvents(conn, syntheticStream(incrementalTerminal("WS_"+account))); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			svc := NewService()
			// 旧配置文件里的遗留字段不能继续强制账号走 HTTP；不增加兼容分流逻辑。
			config := fmt.Sprintf("data_dir: \"\"\nresponses_url: %s\nhttp_only_auth_ids: [bp-a, bp-b]\n", server.URL)
			if err := svc.configure(jsonBytes(map[string]any{"config_yaml": []byte(config)})); err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			frames := map[string][]byte{}
			svc.SetHost(func(method string, payload any, out any) error {
				switch method {
				case "host.http.do", "host.http.do_stream":
					account, err := checkCredential(payload.(map[string]any)["headers"].(http.Header))
					if err != nil {
						return err
					}
					counts[account].posts.Add(1)
					if method == "host.http.do" {
						*out.(*upstreamResponse) = upstreamResponse{StatusCode: 200, Body: jsonBytes(incrementalTerminal("HTTP_" + account))}
					} else {
						*out.(*upstreamStream) = upstreamStream{StatusCode: 200, StreamID: account}
					}
				case "host.http.stream_read":
					account := payload.(map[string]any)["stream_id"].(string)
					*out.(*streamChunk) = streamChunk{Payload: syntheticStream(incrementalTerminal("HTTP_" + account)), Done: true}
				case "host.stream.emit":
					p := payload.(map[string]any)
					id := p["stream_id"].(string)
					mu.Lock()
					frames[id] = append(frames[id], p["payload"].([]byte)...)
					mu.Unlock()
				case "host.log", "host.http.stream_close", "host.stream.close":
				default:
					return fmt.Errorf("unexpected callback %s", method)
				}
				return nil
			})
			for _, phase := range []struct {
				name    string
				rejectB bool
				enableB bool
			}{
				{"both-enabled", false, true},
				{"b-rejected-a-unaffected", true, true},
				{"b-disabled-a-unaffected", false, false},
				{"both-enabled-next-turn", false, true},
			} {
				t.Run(phase.name, func(t *testing.T) {
					rejectB.Store(phase.rejectB)
					before := map[string][3]int32{}
					for account, c := range counts {
						before[account] = [3]int32{c.handshakes.Load(), c.creates.Load(), c.posts.Load()}
					}
					type result struct {
						id, account string
						payload     []byte
						err         error
					}
					results := make(chan result, 8)
					var wg sync.WaitGroup
					for _, account := range []string{"a", "b"} {
						for i := range 4 {
							wg.Go(func() {
								request := websocketRequest(stream)
								request.AuthID = "bp-" + account
								request.AuthMetadata = map[string]any{"access_token": "fixture-" + account, "account_id": "account-" + account}
								request.AuthAttributes["websockets"] = fmt.Sprint(account == "a" || phase.enableB)
								request.StreamID = fmt.Sprintf("%s-%s-%d", phase.name, account, i)
								request.Metadata = map[string]any{"session_id": "shared-fixture-session"}
								request.Payload = jsonBytes(map[string]any{"model": DefaultModelID, "input": "INPUT_" + account})
								method := "executor.execute"
								if stream {
									method = "executor.execute_stream"
								}
								value, err := svc.Handle(method, jsonBytes(request))
								r := result{id: request.StreamID, account: account, err: err}
								if err == nil && !stream {
									r.payload = value.(map[string]any)["Payload"].([]byte)
								}
								results <- r
							})
						}
					}
					wg.Wait()
					svc.streamWG.Wait()
					close(results)
					for r := range results {
						if r.err != nil {
							t.Errorf("%s: %v", r.id, r.err)
							continue
						}
						if stream {
							mu.Lock()
							r.payload = bytes.Clone(frames[r.id])
							mu.Unlock()
						}
						transport := "WS_"
						if r.account == "b" && (!phase.enableB || phase.rejectB) {
							transport = "HTTP_"
						}
						if !bytes.Contains(r.payload, []byte(transport+r.account)) {
							t.Errorf("%s: 响应没有来自预期账号和传输", r.id)
						}
					}
					for account, c := range counts {
						want := [3]int32{4, 4, 0}
						if account == "b" && !phase.enableB {
							want = [3]int32{0, 0, 4}
						} else if account == "b" && phase.rejectB {
							want = [3]int32{4, 0, 4}
						}
						old := before[account]
						got := [3]int32{c.handshakes.Load() - old[0], c.creates.Load() - old[1], c.posts.Load() - old[2]}
						if got != want {
							t.Errorf("账号 %s 的握手/WS 生成/HTTP 次数为 %v，预期 %v", account, got, want)
						}
					}
				})
			}
		})
	}
}
