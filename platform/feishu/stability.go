package feishu

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/chenhg5/cc-connect/core"
)

// Stability fixes ported from upstream cc-connect (T8.2):
//   - the bot's open_id is required for group chats: when it cannot be
//     learned the bot ignores group messages (fails closed) and keeps
//     retrying, instead of answering every message in every group;
//   - every API call has a deadline;
//   - sends retry transient network errors with one dedup key, so Feishu
//     delivers a message at most once, and recover from a revoked tenant
//     token by dropping the cached one;
//   - resources larger than what one download returns (code 234037) are
//     fetched in HTTP Range chunks;
//   - a burst of images from one chat becomes one message.

var (
	apiCallTimeout       = 20 * time.Second // one Feishu API request
	resourceChunkTimeout = 60 * time.Second // one chunk of a resource download
	uploadTimeout        = 5 * time.Minute  // one file upload
	botIdentityRetry     = 5 * time.Minute  // retry period while the open_id is unknown

	maxTransientRetries    = 3
	transientRetryInitial  = 500 * time.Millisecond
	transientRetryMaxDelay = 5 * time.Second

	resourceChunkSize int64 = 8 << 20
	resourceMaxBytes  int64 = 100 << 20

	defaultImageBatchWindow = 500 * time.Millisecond
)

const (
	codeTenantTokenInvalid = 99991663
)

// ── bot identity ───────────────────────────────────────────────

func (p *Platform) getBotOpenID() string {
	p.botMu.RLock()
	defer p.botMu.RUnlock()
	return p.botOpenID
}

func (p *Platform) setBotOpenID(id string) {
	p.botMu.Lock()
	p.botOpenID = id
	p.botMu.Unlock()
}

// identifyBot learns the bot's open_id, retrying transient failures.
func (p *Platform) identifyBot(ctx context.Context) error {
	return p.withTransientRetry(ctx, "bot info", func() error {
		callCtx, cancel := context.WithTimeout(ctx, apiCallTimeout)
		defer cancel()
		id, err := p.fetchBotOpenID(callCtx)
		if err != nil {
			return err
		}
		if id == "" {
			return errors.New("bot info returned no open_id")
		}
		p.setBotOpenID(id)
		return nil
	})
}

// superviseBotIdentity keeps trying to learn the open_id until it succeeds
// or the platform stops; group messages are ignored meanwhile.
func (p *Platform) superviseBotIdentity(ctx context.Context) {
	ticker := time.NewTicker(botIdentityRetry)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := p.identifyBot(ctx); err != nil {
			slog.Error("feishu: bot open_id still unknown; group messages are ignored until it is", "error", err)
			continue
		}
		slog.Info("feishu: bot identified; group messages are handled again", "open_id", p.getBotOpenID())
		return
	}
}

// ── deadlines, retries, dedup ──────────────────────────────────

// isTransientError reports network failures worth retrying.
func isTransientError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	// The SDK turns request timeouts, dial failures and HTTP 504 into its
	// own error types (no Unwrap, no net.Error).
	var clientTimeout *larkcore.ClientTimeoutError
	var serverTimeout *larkcore.ServerTimeoutError
	var dialFailed *larkcore.DialFailedError
	if errors.As(err, &clientTimeout) || errors.As(err, &serverTimeout) || errors.As(err, &dialFailed) {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	msg := err.Error()
	for _, s := range []string{"connection reset by peer", "broken pipe", "i/o timeout", "TLS handshake timeout", "server misbehaving", "connection refused", "no such host"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// withTransientRetry runs fn, retrying transient network errors with
// exponential backoff while ctx allows.
func (p *Platform) withTransientRetry(ctx context.Context, op string, fn func() error) error {
	delay := transientRetryInitial
	var err error
	for attempt := 0; ; attempt++ {
		if err = fn(); err == nil || !isTransientError(err) || attempt >= maxTransientRetries || ctx.Err() != nil {
			return err
		}
		slog.Warn("feishu: transient error, retrying", "operation", op, "attempt", attempt+1, "delay", delay, "error", err)
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (last error: %v)", ctx.Err(), err)
		case <-time.After(delay):
		}
		delay = min(delay*2, transientRetryMaxDelay)
	}
}

// apiError is a Feishu API reply with a non-zero code.
type apiError struct {
	op   string
	code int
	msg  string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("feishu: %s failed code=%d msg=%s", e.op, e.code, e.msg)
}

func isTokenInvalid(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.code == codeTenantTokenInvalid
}

// tokenCache is the SDK's token cache, kept here so a token Feishu has
// revoked can be dropped: the SDK would otherwise reuse it until it expires.
type tokenCache struct {
	mu    sync.Mutex
	items map[string]tokenCacheItem
}

type tokenCacheItem struct {
	value   string
	expires time.Time
}

func newTokenCache() *tokenCache { return &tokenCache{items: map[string]tokenCacheItem{}} }

func (c *tokenCache) Set(_ context.Context, key, value string, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = tokenCacheItem{value: value, expires: time.Now().Add(ttl)}
	return nil
}

func (c *tokenCache) Get(_ context.Context, key string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	item, ok := c.items[key]
	if !ok || time.Now().After(item.expires) {
		delete(c.items, key)
		return "", nil
	}
	return item.value, nil
}

func (c *tokenCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = map[string]tokenCacheItem{}
}

var _ larkcore.Cache = (*tokenCache)(nil)

// sharedTokens is the one token cache of every Feishu client in the
// process: the SDK keeps a single, process-wide token manager, and the last
// client created with WithTokenCache sets its cache for all of them.
var sharedTokens = newTokenCache()

func newLarkClient(appID, appSecret string, cache *tokenCache) *lark.Client {
	return lark.NewClient(appID, appSecret, lark.WithTokenCache(cache))
}

// withFreshToken runs fn and, if Feishu says the tenant token is invalid,
// drops the cached token and runs it once more.
func (p *Platform) withFreshToken(op string, fn func() error) error {
	err := fn()
	if !isTokenInvalid(err) || p.tokens == nil {
		return err
	}
	slog.Warn("feishu: tenant token rejected; fetching a new one", "operation", op)
	p.tokens.clear()
	return fn()
}

// newDedupKey is a Feishu message uuid: a repeated send with the same key
// within an hour is delivered once.
func newDedupKey() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b)
}

// postMessage sends a message, as a reply to replyTo when set or as a new
// message in chatID otherwise, and returns its message ID. Every attempt has
// a deadline; transient failures are retried with the same dedup key.
func (p *Platform) postMessage(ctx context.Context, op, replyTo, chatID, msgType, content string) (string, error) {
	if replyTo == "" && chatID == "" {
		return "", fmt.Errorf("feishu: %s: no chat to send to", op)
	}
	uuid := newDedupKey()
	msgID, err := p.postMessageOnce(ctx, op, replyTo, chatID, msgType, content, uuid)
	var ae *apiError
	if err != nil && replyTo != "" && chatID != "" && errors.As(err, &ae) {
		// Feishu refused the reply (e.g. the message was deleted or
		// recalled meanwhile); nothing was delivered, so send it to the chat.
		slog.Warn("feishu: reply refused; sending to the chat instead", "operation", op, "code", ae.code, "msg", ae.msg)
		msgID, err = p.postMessageOnce(ctx, op, "", chatID, msgType, content, uuid+"c")
	}
	if err != nil && isTransientError(err) {
		// The request may have reached Feishu before the connection failed.
		err = fmt.Errorf("%w (%w)", err, core.ErrDeliveryUnknown)
	}
	return msgID, err
}

func (p *Platform) postMessageOnce(ctx context.Context, op, replyTo, chatID, msgType, content, uuid string) (string, error) {
	var msgID string
	err := p.withTransientRetry(ctx, op, func() error {
		return p.withFreshToken(op, func() error {
			callCtx, cancel := context.WithTimeout(ctx, apiCallTimeout)
			defer cancel()
			if replyTo != "" {
				resp, err := p.client.Im.Message.Reply(callCtx, larkim.NewReplyMessageReqBuilder().
					MessageId(replyTo).
					Body(larkim.NewReplyMessageReqBodyBuilder().MsgType(msgType).Content(content).Uuid(uuid).Build()).
					Build())
				if err != nil {
					return fmt.Errorf("feishu: %s: %w", op, err)
				}
				if !resp.Success() {
					return &apiError{op: op, code: resp.Code, msg: resp.Msg}
				}
				if resp.Data != nil && resp.Data.MessageId != nil {
					msgID = *resp.Data.MessageId
				}
				return nil
			}
			resp, err := p.client.Im.Message.Create(callCtx, larkim.NewCreateMessageReqBuilder().
				ReceiveIdType(larkim.CreateMessageV1ReceiveIDTypeChatId).
				Body(larkim.NewCreateMessageReqBodyBuilder().
					ReceiveId(chatID).MsgType(msgType).Content(content).Uuid(uuid).Build()).
				Build())
			if err != nil {
				return fmt.Errorf("feishu: %s: %w", op, err)
			}
			if !resp.Success() {
				return &apiError{op: op, code: resp.Code, msg: resp.Msg}
			}
			if resp.Data != nil && resp.Data.MessageId != nil {
				msgID = *resp.Data.MessageId
			}
			return nil
		})
	})
	return msgID, err
}

// uploadFile uploads a file and returns its key. open makes the reader for
// each attempt: a rejected token is replaced and the upload sent again.
func (p *Platform) uploadFile(ctx context.Context, op, fileType, name string, open func() (io.Reader, error)) (string, error) {
	var key string
	err := p.withFreshToken(op, func() error {
		r, err := open()
		if err != nil {
			return fmt.Errorf("feishu: %s: %w", op, err)
		}
		uploadCtx, cancel := context.WithTimeout(ctx, uploadTimeout)
		defer cancel()
		resp, err := p.client.Im.File.Create(uploadCtx, larkim.NewCreateFileReqBuilder().
			Body(larkim.NewCreateFileReqBodyBuilder().FileType(fileType).FileName(name).File(r).Build()).
			Build())
		if err != nil {
			return fmt.Errorf("feishu: %s: %w", op, err)
		}
		if !resp.Success() {
			return &apiError{op: op, code: resp.Code, msg: resp.Msg}
		}
		if resp.Data == nil || resp.Data.FileKey == nil {
			return fmt.Errorf("feishu: %s: no file_key returned", op)
		}
		key = *resp.Data.FileKey
		return nil
	})
	return key, err
}

// ── large resources ────────────────────────────────────────────

// Downloads use their own HTTP requests (the SDK reads whole bodies into
// memory and decodes 206 JSON bodies as API errors), with a tenant token
// fetched and cached here.

// resourceBaseURL is the Feishu API base for downloads.
var resourceBaseURL = lark.FeishuBaseUrl

// downloadToken returns a tenant token for downloads, fetching a new one
// when it is about to expire or after Feishu rejected it (stale).
func (p *Platform) downloadToken(ctx context.Context, stale string) (string, error) {
	p.dlMu.Lock()
	defer p.dlMu.Unlock()
	if p.dlToken != "" && p.dlToken != stale && time.Now().Before(p.dlTokenExpires) {
		return p.dlToken, nil
	}
	callCtx, cancel := context.WithTimeout(ctx, apiCallTimeout)
	defer cancel()
	resp, err := p.client.GetTenantAccessTokenBySelfBuiltApp(callCtx, &larkcore.SelfBuiltTenantAccessTokenReq{AppID: p.appID, AppSecret: p.appSecret})
	if err != nil {
		return "", fmt.Errorf("feishu: download token: %w", err)
	}
	if !resp.Success() || resp.TenantAccessToken == "" {
		return "", &apiError{op: "download token", code: resp.Code, msg: resp.Msg}
	}
	ttl := time.Duration(resp.Expire) * time.Second
	if ttl > 10*time.Minute {
		ttl -= 5 * time.Minute // renew early
	}
	p.dlToken, p.dlTokenExpires = resp.TenantAccessToken, time.Now().Add(ttl)
	return p.dlToken, nil
}

// getResource downloads a message resource in HTTP Range chunks: Feishu
// cuts a single download off at a few MiB (code 234037, "download
// interrupted"). A small resource takes one request; a server that ignores
// Range answers 200 with the whole body.
func (p *Platform) getResource(ctx context.Context, messageID, key, resType string) ([]byte, error) {
	endpoint := fmt.Sprintf("%s/open-apis/im/v1/messages/%s/resources/%s?type=%s",
		strings.TrimRight(resourceBaseURL, "/"), url.PathEscape(messageID), url.PathEscape(key), url.QueryEscape(resType))
	var buf bytes.Buffer
	total := int64(-1)
	for offset := int64(0); total < 0 || offset < total; {
		end := offset + resourceChunkSize - 1
		if total > 0 && end >= total {
			end = total - 1
		}
		var part rangePart
		err := p.withTransientRetry(ctx, "download "+resType, func() error {
			var err error
			part, err = p.fetchRange(ctx, endpoint, offset, end)
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("feishu: download %s: %w", resType, err)
		}
		if part.whole {
			if offset != 0 {
				return nil, fmt.Errorf("feishu: download %s: server stopped honoring Range at %d", resType, offset)
			}
			return part.data, nil
		}
		// The part must be exactly the bytes asked for, of an unchanged total.
		switch {
		case part.start != offset || part.end > end || part.end < part.start:
			return nil, fmt.Errorf("feishu: download %s: got bytes %d-%d, asked for %d-%d", resType, part.start, part.end, offset, end)
		case int64(len(part.data)) != part.end-part.start+1:
			return nil, fmt.Errorf("feishu: download %s: %d bytes for range %d-%d", resType, len(part.data), part.start, part.end)
		case total >= 0 && part.total != total:
			return nil, fmt.Errorf("feishu: download %s: size changed from %d to %d", resType, total, part.total)
		case part.total > resourceMaxBytes:
			return nil, fmt.Errorf("feishu: %s is larger than %d MiB", resType, resourceMaxBytes>>20)
		}
		if total < 0 {
			total = part.total
			buf.Grow(int(total))
		}
		buf.Write(part.data)
		offset = part.end + 1
	}
	return buf.Bytes(), nil
}

type rangePart struct {
	data              []byte
	start, end, total int64
	whole             bool // the server sent the whole resource (200)
}

// fetchRange gets bytes [start, end], reading at most what was asked for
// (or resourceMaxBytes for a whole-body answer). A rejected token is
// replaced once.
func (p *Platform) fetchRange(ctx context.Context, endpoint string, start, end int64) (rangePart, error) {
	stale := ""
	for attempt := 0; ; attempt++ {
		token, err := p.downloadToken(ctx, stale)
		if err != nil {
			return rangePart{}, err
		}
		part, err := p.fetchRangeWith(ctx, endpoint, token, start, end)
		if isTokenInvalid(err) && attempt == 0 {
			stale = token
			continue
		}
		return part, err
	}
}

func (p *Platform) fetchRangeWith(ctx context.Context, endpoint, token string, start, end int64) (rangePart, error) {
	callCtx, cancel := context.WithTimeout(ctx, resourceChunkTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return rangePart{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	client := p.httpClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return rangePart{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	status := resp.StatusCode
	if status == http.StatusPartialContent && strings.TrimSpace(resp.Header.Get("Content-Range")) == "" {
		// Feishu ignores Range for images: it answers 206 with the whole
		// image and no Content-Range (files get real ranges).
		status = http.StatusOK
	}
	switch status {
	case http.StatusPartialContent:
		s, e, t, ok := parseContentRange(resp.Header.Get("Content-Range"))
		if !ok {
			return rangePart{}, fmt.Errorf("bad Content-Range %q", resp.Header.Get("Content-Range"))
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, end-start+2)) // one byte more shows an overlong body
		if err != nil {
			return rangePart{}, err
		}
		return rangePart{data: data, start: s, end: e, total: t}, nil
	case http.StatusOK:
		data, err := io.ReadAll(io.LimitReader(resp.Body, resourceMaxBytes+1))
		if err != nil {
			return rangePart{}, err
		}
		if int64(len(data)) > resourceMaxBytes {
			return rangePart{}, fmt.Errorf("larger than %d MiB", resourceMaxBytes>>20)
		}
		// A request with a Range header that comes back 200 as a Feishu
		// API envelope is an error (e.g. a revoked token), not the file.
		if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
			if ae := feishuEnvelopeError(data); ae != nil {
				return rangePart{}, ae
			}
		}
		return rangePart{data: data, whole: true}, nil
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var apiErr struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		}
		if json.Unmarshal(body, &apiErr) == nil && apiErr.Code != 0 {
			return rangePart{}, &apiError{op: "download", code: apiErr.Code, msg: apiErr.Msg}
		}
		return rangePart{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
}

// feishuEnvelopeError recognizes {"code":N,"msg":"..."} with a non-zero
// code (and nothing else of substance) as a Feishu API error.
func feishuEnvelopeError(body []byte) *apiError {
	var env map[string]json.RawMessage
	if json.Unmarshal(body, &env) != nil {
		return nil
	}
	var code int
	var msg string
	if json.Unmarshal(env["code"], &code) != nil || code == 0 || json.Unmarshal(env["msg"], &msg) != nil {
		return nil
	}
	for k := range env {
		if k != "code" && k != "msg" && k != "data" && k != "error" {
			return nil
		}
	}
	return &apiError{op: "download", code: code, msg: msg}
}

// parseContentRange reads "bytes 0-99/12345".
func parseContentRange(h string) (start, end, total int64, ok bool) {
	h = strings.TrimSpace(h)
	rest, found := strings.CutPrefix(h, "bytes ")
	if !found {
		return 0, 0, 0, false
	}
	span, totalText, found := strings.Cut(rest, "/")
	if !found {
		return 0, 0, 0, false
	}
	startText, endText, found := strings.Cut(span, "-")
	if !found {
		return 0, 0, 0, false
	}
	var err1, err2, err3 error
	start, err1 = strconv.ParseInt(strings.TrimSpace(startText), 10, 64)
	end, err2 = strconv.ParseInt(strings.TrimSpace(endText), 10, 64)
	total, err3 = strconv.ParseInt(strings.TrimSpace(totalText), 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || start < 0 || end < start || total <= end {
		return 0, 0, 0, false
	}
	return start, end, total, true
}

// ── inbound order, image bursts ────────────────────────────────

// maxImageBatch caps one coalesced message; maxImageBatchAge caps how long
// a burst can keep extending its window.
var (
	maxImageBatch      = 20
	maxImageBatchBytes = 50 << 20
	maxImageBatchAge   = 5 * time.Second
)

// inboundLock serializes one chat's inbound messages, from download to
// dispatch, so the engine sees them in the order they arrived (an image
// still downloading is not overtaken by the text sent after it).
func (p *Platform) inboundLock(sessionKey string) *sync.Mutex {
	l, _ := p.inbound.LoadOrStore(sessionKey, &sync.Mutex{})
	return l.(*sync.Mutex)
}

// runContext is the context of the current run (downloads stop with it).
func (p *Platform) runContext() context.Context {
	p.runMu.Lock()
	defer p.runMu.Unlock()
	if p.runCtx == nil {
		return context.Background()
	}
	return p.runCtx
}

type imageBatch struct {
	msg     *core.Message
	timer   *time.Timer
	started time.Time
	gen     uint64 // the run it belongs to
}

func imageBatchKey(m *core.Message) string { return m.SessionKey + "\x00" + m.UserID }

// dispatchImage holds an image message briefly so a burst of images from
// one sender becomes one message. The caller holds the chat's inbound lock.
func (p *Platform) dispatchImage(m *core.Message) {
	if p.stopped.Load() {
		return
	}
	window := p.imageBatchWindow
	if window <= 0 {
		p.handler(p, m)
		return
	}
	key := imageBatchKey(m)
	p.batchMu.Lock()
	if p.imageBatches == nil {
		p.imageBatches = map[string]*imageBatch{}
	}
	if b := p.imageBatches[key]; b != nil {
		b.msg.Images = append(b.msg.Images, m.Images...)
		b.msg.MessageID, b.msg.ReplyCtx = m.MessageID, m.ReplyCtx
		size := 0
		for _, img := range b.msg.Images {
			size += len(img.Data)
		}
		full := len(b.msg.Images) >= maxImageBatch || size >= maxImageBatchBytes
		if !full && time.Since(b.started) < maxImageBatchAge {
			b.timer.Reset(window)
		}
		p.batchMu.Unlock()
		if full {
			p.flushImagesLocked(m.SessionKey, nil)
		}
		return
	}
	b := &imageBatch{msg: m, started: time.Now(), gen: p.gen.Load()}
	p.imageBatches[key] = b
	sessionKey := m.SessionKey
	b.timer = time.AfterFunc(window, func() {
		l := p.inboundLock(sessionKey)
		l.Lock()
		defer l.Unlock()
		p.flushImagesLocked(sessionKey, b)
	})
	p.batchMu.Unlock()
}

// flushImagesLocked dispatches the chat's held images (only batch b, when
// given). The caller holds the chat's inbound lock.
func (p *Platform) flushImagesLocked(sessionKey string, only *imageBatch) {
	p.batchMu.Lock()
	var ready []*imageBatch
	for key, b := range p.imageBatches {
		if b.msg.SessionKey != sessionKey || (only != nil && b != only) {
			continue
		}
		delete(p.imageBatches, key)
		b.timer.Stop()
		ready = append(ready, b)
	}
	p.batchMu.Unlock()
	for _, b := range ready {
		// Held images of a stopped (or earlier) run are not delivered.
		if p.stopped.Load() || b.gen != p.gen.Load() {
			continue
		}
		if len(b.msg.Images) > 1 {
			slog.Debug("feishu: images sent together dispatched as one message", "count", len(b.msg.Images))
		}
		p.handler(p, b.msg)
	}
}

// dropImageBatches discards held images (platform stopping).
func (p *Platform) dropImageBatches() {
	p.batchMu.Lock()
	defer p.batchMu.Unlock()
	for key, b := range p.imageBatches {
		b.timer.Stop()
		delete(p.imageBatches, key)
	}
}
