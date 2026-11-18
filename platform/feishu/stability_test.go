package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/chenhg5/cc-connect/core"
)

// fakeFeishu is an httptest Feishu Open API; handle answers everything but
// the tenant token endpoint.
type fakeFeishu struct {
	srv          *httptest.Server
	tokenFetches atomic.Int32
	mu           sync.Mutex
	bodies       []string
	handle       func(w http.ResponseWriter, r *http.Request, body string)
}

func newFakeFeishu(t *testing.T, handle func(w http.ResponseWriter, r *http.Request, body string)) (*fakeFeishu, *Platform) {
	t.Helper()
	f := &fakeFeishu{handle: handle}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			n := f.tokenFetches.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"code":0,"tenant_access_token":"token-%d","expire":7200}`, n)
			return
		}
		f.mu.Lock()
		f.bodies = append(f.bodies, string(data))
		f.mu.Unlock()
		f.handle(w, r, string(data))
	}))
	t.Cleanup(f.srv.Close)

	oldInitial, oldBase := transientRetryInitial, resourceBaseURL
	transientRetryInitial, resourceBaseURL = time.Millisecond, f.srv.URL
	t.Cleanup(func() { transientRetryInitial, resourceBaseURL = oldInitial, oldBase })

	p := &Platform{appID: "app", appSecret: "secret", tokens: newTokenCache(), httpClient: f.srv.Client()}
	p.client = lark.NewClient("app", "secret", lark.WithOpenBaseUrl(f.srv.URL), lark.WithTokenCache(p.tokens))
	return f, p
}

// dropConnection makes the client see a broken connection (transient).
func dropConnection(w http.ResponseWriter) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

func writeJSON(w http.ResponseWriter, v string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, v)
}

func strp(s string) *string { return &s }

func textEvent(id, chatType, text string, mentions ...string) *larkim.P2MessageReceiveV1 {
	content, _ := json.Marshal(map[string]string{"text": text})
	var ms []*larkim.MentionEvent
	for i, m := range mentions {
		ms = append(ms, &larkim.MentionEvent{Key: strp(fmt.Sprintf("@_user_%d", i+1)), Id: &larkim.UserId{OpenId: strp(m)}})
	}
	return &larkim.P2MessageReceiveV1{Event: &larkim.P2MessageReceiveV1Data{
		Sender: &larkim.EventSender{SenderId: &larkim.UserId{OpenId: strp("ou_user")}},
		Message: &larkim.EventMessage{
			MessageId: strp(id), ChatId: strp("oc_chat"), ChatType: strp(chatType),
			MessageType: strp("text"), Content: strp(string(content)), Mentions: ms,
		},
	}}
}

type recordingHandler struct {
	mu   sync.Mutex
	msgs []*core.Message
}

func (h *recordingHandler) handle(_ core.Platform, m *core.Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, m)
}

func (h *recordingHandler) got() []*core.Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]*core.Message(nil), h.msgs...)
}

// Without the bot's open_id, group messages are ignored (fail closed);
// direct messages still work. Once identified, mentions get through.
func TestGroupMessagesFailClosedWithoutBotIdentity(t *testing.T) {
	h := &recordingHandler{}
	p := &Platform{handler: h.handle}
	_ = p.onMessage(textEvent("m1", "group", "@_user_1 hi", "ou_bot"))
	if len(h.got()) != 0 {
		t.Fatal("group message handled while the bot's identity is unknown")
	}
	_ = p.onMessage(textEvent("m2", "p2p", "hi"))
	if len(h.got()) != 1 {
		t.Fatal("direct message dropped")
	}
	p.setBotOpenID("ou_bot")
	_ = p.onMessage(textEvent("m3", "group", "@_user_1 hi", "ou_bot"))
	_ = p.onMessage(textEvent("m4", "group", "chatter"))
	if got := h.got(); len(got) != 2 || got[1].MessageID != "m3" {
		t.Fatalf("handled = %d", len(got))
	}
}

func TestIdentifyBotRetriesTransientFailures(t *testing.T) {
	var calls atomic.Int32
	_, p := newFakeFeishu(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		if calls.Add(1) < 3 {
			dropConnection(w)
			return
		}
		writeJSON(w, `{"code":0,"bot":{"open_id":"ou_bot"}}`)
	})
	if err := p.identifyBot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.getBotOpenID() != "ou_bot" || calls.Load() != 3 {
		t.Fatalf("open_id=%q calls=%d", p.getBotOpenID(), calls.Load())
	}
}

// A send retried after a dropped connection carries the same dedup key, so
// Feishu delivers it once.
func TestSendRetriesWithOneDedupKey(t *testing.T) {
	var calls atomic.Int32
	f, p := newFakeFeishu(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		if calls.Add(1) == 1 {
			dropConnection(w)
			return
		}
		writeJSON(w, `{"code":0,"data":{"message_id":"om_1"}}`)
	})
	if err := p.Send(context.Background(), replyContext{chatID: "oc_chat"}, "hello"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	bodies := append([]string(nil), f.bodies...)
	f.mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("%d requests", len(bodies))
	}
	uuid := func(body string) string {
		var m struct {
			UUID string `json:"uuid"`
		}
		_ = json.Unmarshal([]byte(body), &m)
		return m.UUID
	}
	if uuid(bodies[0]) == "" || uuid(bodies[0]) != uuid(bodies[1]) {
		t.Fatalf("dedup keys %q %q", uuid(bodies[0]), uuid(bodies[1]))
	}
}

// A rejected tenant token is dropped from the cache and fetched again.
func TestRevokedTokenIsReplaced(t *testing.T) {
	var calls atomic.Int32
	f, p := newFakeFeishu(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		if r.Header.Get("Authorization") == "Bearer token-1" {
			calls.Add(1)
			writeJSON(w, `{"code":99991663,"msg":"Invalid access token for authorization"}`)
			return
		}
		writeJSON(w, `{"code":0,"data":{"message_id":"om_1"}}`)
	})
	if err := p.Send(context.Background(), replyContext{chatID: "oc_chat"}, "hello"); err != nil {
		t.Fatal(err)
	}
	if f.tokenFetches.Load() < 2 {
		t.Fatalf("token fetched %d times", f.tokenFetches.Load())
	}
}

// Resources are fetched in Range chunks (one download is cut off at a few
// MiB with code 234037); a server that ignores Range sends it whole.
func TestLargeResourceDownloadsInChunks(t *testing.T) {
	payload := []byte(strings.Repeat("0123456789", 5))
	oldChunk := resourceChunkSize
	resourceChunkSize = 8
	t.Cleanup(func() { resourceChunkSize = oldChunk })
	var ranges []string
	var mu sync.Mutex
	_, p := newFakeFeishu(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		rng := r.Header.Get("Range")
		if rng == "" || r.URL.Query().Get("type") != "file" || !strings.HasSuffix(r.URL.Path, "/messages/om_1/resources/file_v3_x") {
			writeJSON(w, `{"code":234037,"msg":"download interrupted"}`)
			return
		}
		mu.Lock()
		ranges = append(ranges, rng)
		mu.Unlock()
		var start, end int
		_, _ = fmt.Sscanf(rng, "bytes=%d-%d", &start, &end)
		if end >= len(payload) {
			end = len(payload) - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[start : end+1])
	})
	got, err := p.downloadResource("om_1", "file_v3_x", "file")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) || len(ranges) != 7 {
		t.Fatalf("got %q in %d ranges", got, len(ranges))
	}
}

func TestResourceSentWholeWhenRangeIgnored(t *testing.T) {
	_, p := newFakeFeishu(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.WriteString(w, "whole file")
	})
	got, err := p.downloadResource("om_1", "file_v3_x", "file")
	if err != nil || string(got) != "whole file" {
		t.Fatalf("got %q err=%v", got, err)
	}
}

func TestResourceErrorCodeReported(t *testing.T) {
	_, p := newFakeFeishu(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"code":234003,"msg":"File not in msg."}`)
	})
	if _, err := p.downloadResource("om_1", "file_v3_x", "file"); err == nil || !strings.Contains(err.Error(), "234003") {
		t.Fatalf("err = %v", err)
	}
}

func TestImageBurstBecomesOneMessage(t *testing.T) {
	h := &recordingHandler{}
	p := &Platform{handler: h.handle, imageBatchWindow: 50 * time.Millisecond}
	for i := 0; i < 3; i++ {
		p.dispatchImage(&core.Message{SessionKey: "k", MessageID: fmt.Sprintf("m%d", i), Images: []core.ImageAttachment{{Data: []byte{byte(i)}}}})
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(h.got()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	got := h.got()
	if len(got) != 1 || len(got[0].Images) != 3 || got[0].MessageID != "m2" {
		t.Fatalf("got %d messages", len(got))
	}

	// A text message flushes held images first.
	p.dispatchImage(&core.Message{SessionKey: "k", MessageID: "m9", Images: []core.ImageAttachment{{Data: []byte{9}}}})
	p.flushImagesLocked("k", nil)
	if got := h.got(); len(got) != 2 || got[1].MessageID != "m9" {
		t.Fatalf("held image not flushed: %d", len(got))
	}
}

func TestTransientErrorClassification(t *testing.T) {
	for err, want := range map[error]bool{
		io.EOF:                   true,
		context.DeadlineExceeded: true,
		context.Canceled:         false,
		fmt.Errorf("dial tcp: connection refused"): true,
		&apiError{code: 230001}:                    false,
	} {
		if got := isTransientError(err); got != want {
			t.Errorf("%v: %v", err, got)
		}
	}
}

// rangeServer serves payload by Range; tamper may change a response.
func rangeServer(t *testing.T, payload []byte, tamper func(w http.ResponseWriter, start, end int) bool) *Platform {
	t.Helper()
	oldChunk := resourceChunkSize
	resourceChunkSize = 8
	t.Cleanup(func() { resourceChunkSize = oldChunk })
	_, p := newFakeFeishu(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		var start, end int
		_, _ = fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end)
		if end >= len(payload) {
			end = len(payload) - 1
		}
		if tamper != nil && tamper(w, start, end) {
			return
		}
		w.Header().Set("Content-Type", "application/json") // a JSON file
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[start : end+1])
	})
	return p
}

// Review 5: a JSON file comes through intact (206 bodies are not API errors).
func TestJSONResourceDownloads(t *testing.T) {
	payload := []byte(`[{"a":1},{"b":2},{"c":3}]`)
	p := rangeServer(t, payload, nil)
	got, err := p.downloadResource("om_1", "file_v3_x", "file")
	if err != nil || string(got) != string(payload) {
		t.Fatalf("got %q err=%v", got, err)
	}
}

// Review 4: wrong ranges, overlong bodies and a changing size are errors,
// not corrupted files.
func TestBadRangesAreRejected(t *testing.T) {
	payload := []byte(strings.Repeat("abcdefgh", 3))
	cases := map[string]func(w http.ResponseWriter, start, end int) bool{
		"repeats the first chunk": func(w http.ResponseWriter, start, end int) bool {
			if start == 0 {
				return false
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-7/%d", len(payload)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(payload[0:8])
			return true
		},
		"overlong body": func(w http.ResponseWriter, start, end int) bool {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(payload)
			return true
		},
		"size changes": func(w http.ResponseWriter, start, end int) bool {
			if start == 0 {
				return false
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)+8))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(payload[start : end+1])
			return true
		},
	}
	for name, tamper := range cases {
		p := rangeServer(t, payload, tamper)
		if got, err := p.downloadResource("om_1", "file_v3_x", "file"); err == nil {
			t.Errorf("%s: accepted %q", name, got)
		}
	}
}

// Review 3: a whole-body answer larger than the cap is cut off while read.
func TestOversizedResourceRejected(t *testing.T) {
	oldMax := resourceMaxBytes
	resourceMaxBytes = 16
	t.Cleanup(func() { resourceMaxBytes = oldMax })
	_, p := newFakeFeishu(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		_, _ = io.WriteString(w, strings.Repeat("x", 64))
	})
	if _, err := p.downloadResource("om_1", "file_v3_x", "file"); err == nil {
		t.Fatal("oversized body accepted")
	}
}

// Review 2: the SDK's own timeout and dial errors are retried.
func TestSDKTimeoutsAreTransient(t *testing.T) {
	for _, err := range []error{&larkcore.ClientTimeoutError{}, &larkcore.ServerTimeoutError{}, &larkcore.DialFailedError{}, fmt.Errorf("send: %w", &larkcore.ClientTimeoutError{})} {
		if !isTransientError(err) {
			t.Errorf("%T not transient", err)
		}
	}
}

// Review 1: every Feishu client shares one token cache (the SDK's token
// manager is process-wide).
func TestPlatformsShareTheTokenCache(t *testing.T) {
	a, err := New(map[string]any{"app_id": "a", "app_secret": "s"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(map[string]any{"app_id": "b", "app_secret": "t"})
	if err != nil {
		t.Fatal(err)
	}
	if a.(*Platform).tokens != sharedTokens || b.(*Platform).tokens != sharedTokens {
		t.Fatal("platforms have their own token caches")
	}
}

// Review 8: an upload whose token was revoked is sent again with a new one.
func TestUploadReplacesRevokedToken(t *testing.T) {
	f, p := newFakeFeishu(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		if r.Header.Get("Authorization") == "Bearer token-1" {
			writeJSON(w, `{"code":99991663,"msg":"Invalid access token for authorization"}`)
			return
		}
		writeJSON(w, `{"code":0,"data":{"file_key":"file_v3_new"}}`)
	})
	opens := 0
	key, err := p.uploadFile(context.Background(), "upload file", larkim.CreateFileFileTypeStream, "a.txt", func() (io.Reader, error) {
		opens++
		return strings.NewReader("data"), nil
	})
	if err != nil || key != "file_v3_new" || opens != 2 || f.tokenFetches.Load() < 2 {
		t.Fatalf("key=%q err=%v opens=%d tokens=%d", key, err, opens, f.tokenFetches.Load())
	}
}

// Review 7: images held when the platform stops, or arriving after, are
// not delivered.
func TestStoppedPlatformDropsHeldImages(t *testing.T) {
	h := &recordingHandler{}
	p := &Platform{handler: h.handle, imageBatchWindow: 20 * time.Millisecond}
	p.dispatchImage(&core.Message{SessionKey: "k", Images: []core.ImageAttachment{{Data: []byte{1}}}})
	_ = p.Stop()
	p.dispatchImage(&core.Message{SessionKey: "k", Images: []core.ImageAttachment{{Data: []byte{2}}}})
	time.Sleep(80 * time.Millisecond)
	if n := len(h.got()); n != 0 {
		t.Fatalf("%d messages delivered after Stop", n)
	}
	// A batch of an earlier run is dropped too.
	p.stopped.Store(false)
	p.dispatchImage(&core.Message{SessionKey: "k", Images: []core.ImageAttachment{{Data: []byte{3}}}})
	p.gen.Add(1)
	time.Sleep(80 * time.Millisecond)
	if n := len(h.got()); n != 0 {
		t.Fatalf("%d messages of an old run delivered", n)
	}
}

// Review 6: the image timer and a following text message cannot swap: both
// go through the chat's inbound lock, and the text flushes held images first.
func TestHeldImagesGoBeforeText(t *testing.T) {
	h := &recordingHandler{}
	p := &Platform{handler: h.handle, imageBatchWindow: time.Hour}
	l := p.inboundLock("k")
	l.Lock()
	p.dispatchImage(&core.Message{SessionKey: "k", MessageID: "img", Images: []core.ImageAttachment{{Data: []byte{1}}}})
	p.flushImagesLocked("k", nil) // what onMessage does before a text message
	h.handle(p, &core.Message{SessionKey: "k", MessageID: "text"})
	l.Unlock()
	got := h.got()
	if len(got) != 2 || got[0].MessageID != "img" || got[1].MessageID != "text" {
		t.Fatalf("order = %v", got)
	}
}

// A burst caps at maxImageBatch images per message.
func TestImageBurstCap(t *testing.T) {
	h := &recordingHandler{}
	p := &Platform{handler: h.handle, imageBatchWindow: time.Hour}
	for i := 0; i < maxImageBatch+1; i++ {
		p.dispatchImage(&core.Message{SessionKey: "k", Images: []core.ImageAttachment{{Data: []byte{byte(i)}}}})
	}
	if got := h.got(); len(got) != 1 || len(got[0].Images) != maxImageBatch {
		t.Fatalf("got %d messages", len(got))
	}
	p.dropImageBatches()
}

// Kimi 8: a reply Feishu refuses (target deleted or recalled) is sent to
// the chat instead.
func TestReplyFallsBackToChatWhenTargetGone(t *testing.T) {
	var created atomic.Int32
	_, p := newFakeFeishu(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		if strings.HasSuffix(r.URL.Path, "/reply") {
			writeJSON(w, `{"code":230011,"msg":"The message was withdrawn."}`)
			return
		}
		created.Add(1)
		writeJSON(w, `{"code":0,"data":{"message_id":"om_2"}}`)
	})
	if err := p.Reply(context.Background(), replyContext{messageID: "om_gone", chatID: "oc_chat"}, "answer"); err != nil {
		t.Fatal(err)
	}
	if created.Load() != 1 {
		t.Fatalf("created %d messages", created.Load())
	}
}

// Kimi 7: a send that may have been delivered says so (no fallback copy).
func TestUnconfirmedSendIsMarked(t *testing.T) {
	_, p := newFakeFeishu(t, func(w http.ResponseWriter, r *http.Request, _ string) { dropConnection(w) })
	err := p.SendWithButtons(context.Background(), replyContext{chatID: "oc_chat"}, "pick", [][]core.ButtonOption{{{Text: "a", Data: "a"}}})
	if !errors.Is(err, core.ErrDeliveryUnknown) {
		t.Fatalf("err = %v", err)
	}
	// A refused send is definite.
	_, p2 := newFakeFeishu(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		writeJSON(w, `{"code":230001,"msg":"invalid content"}`)
	})
	if err := p2.Send(context.Background(), replyContext{chatID: "oc_chat"}, "x"); err == nil || errors.Is(err, core.ErrDeliveryUnknown) {
		t.Fatalf("err = %v", err)
	}
}

// Kimi 3: a 200 Feishu error envelope is an error (here a revoked token,
// replaced), not the file's contents.
func TestDownloadErrorEnvelopeIsNotTheFile(t *testing.T) {
	f, p := newFakeFeishu(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		if r.Header.Get("Authorization") == "Bearer token-1" {
			writeJSON(w, `{"code":99991663,"msg":"Invalid access token"}`)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = io.WriteString(w, "PNGDATA")
	})
	got, err := p.downloadResource("om_1", "img_v3_x", "image")
	if err != nil || string(got) != "PNGDATA" || f.tokenFetches.Load() != 2 {
		t.Fatalf("got %q err=%v tokens=%d", got, err, f.tokenFetches.Load())
	}
	// A JSON file that merely has a code field stays a file.
	if feishuEnvelopeError([]byte(`{"code":1,"msg":"x","items":[1]}`)) != nil {
		t.Fatal("JSON file taken for an API error")
	}
}

// Kimi 2: a burst is also capped by size.
func TestImageBurstByteCap(t *testing.T) {
	old := maxImageBatchBytes
	maxImageBatchBytes = 10
	t.Cleanup(func() { maxImageBatchBytes = old })
	h := &recordingHandler{}
	p := &Platform{handler: h.handle, imageBatchWindow: time.Hour}
	p.dispatchImage(&core.Message{SessionKey: "k", Images: []core.ImageAttachment{{Data: make([]byte, 6)}}})
	p.dispatchImage(&core.Message{SessionKey: "k", Images: []core.ImageAttachment{{Data: make([]byte, 6)}}})
	if got := h.got(); len(got) != 1 || len(got[0].Images) != 2 {
		t.Fatalf("got %d messages", len(got))
	}
}
