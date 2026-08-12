// Package wa is a thin, gomobile-friendly wrapper around whatsmeow.
//
// It exposes the minimal surface MeGPT needs from iOS:
//   - Start: open the local session store and connect (resumes if paired)
//   - RequestPairingCode: link this device to a phone via an 8-char code
//   - SendText: send a text message to a phone number
//   - Disconnect / Logout
//
// Only gomobile-supported types are exported (string/bool/error). Async
// updates are delivered to the host (Swift) via the Events callback interface.
//
// Storage uses modernc.org/sqlite (pure Go, no CGo) so it cross-compiles for
// iOS via gomobile without a C toolchain on the device target.
package wa

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go sqlite driver, registers as "sqlite"

	"google.golang.org/protobuf/proto"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

const historyBridgeBatchSize = 50
const wrapperSchemaVersion = 1

// maxOutboundMediaBytes bounds how much we download for an outbound media send.
// WhatsApp's practical image/video ceiling is ~16 MB; we read one byte past it
// so we can detect (and reject) anything larger instead of streaming forever.
const maxOutboundMediaBytes = 16 * 1024 * 1024

// Set by mobile/build-ios.sh so a shipped framework can be traced back to the
// exact source and gomobile toolchain that produced it.
var (
	buildSourceCommit    = "unknown"
	buildGomobileVersion = "unknown"
)

// BuildInfo returns traceable release metadata embedded by the build script.
func BuildInfo() string {
	return fmt.Sprintf("source=%s;gomobile=%s", buildSourceCommit, buildGomobileVersion)
}

type wrapperCapabilities struct {
	SchemaVersion int      `json:"schemaVersion"`
	Features      []string `json:"features"`
}

// Capabilities lets the host gate native features without inferring support
// from the app or framework release version.
func Capabilities() string {
	payload, _ := json.Marshal(wrapperCapabilities{
		SchemaVersion: wrapperSchemaVersion,
		Features: []string{
			"group_metadata_v1",
			"groups_v1",
			"list_groups_v1",
			"media_caption_v1",
			"reply_context_v1",
			"stable_send_id",
		},
	})
	return string(payload)
}

// Events is implemented on the host (Swift) side to receive async updates.
//
// All methods may be invoked from a background goroutine; the host is
// responsible for dispatching to the main thread before touching UI.
type Events interface {
	OnConnected()
	OnLoggedOut()
	OnPairSuccess()
	// OnMessage delivers a single live message as a JSON object (see waMessage).
	OnMessage(payload string)
	// OnHistorySync delivers a small batch of historical messages as JSON each
	// time the phone pushes a history blob. Large blobs are split before they
	// cross the Swift/JS bridge to avoid transient memory spikes.
	OnHistorySync(payload string)
	// OnGroupInfo delivers a group metadata update independently of messages.
	// The host coalesces these by group JID and uploads them through the native
	// sync engine without routing them through React Native.
	OnGroupInfo(payload string)
	OnError(stage string, message string)
}

var (
	mu        sync.Mutex
	client    *whatsmeow.Client
	container *sqlstore.Container
	dbConn    *sql.DB
	rootCtx   context.Context
	cancelCtx context.CancelFunc

	// evtMu guards currentEvt, which always points at the most recent host
	// listener. React Native hands us a fresh Events bridge on every screen
	// mount / fast-refresh, so the event handler resolves it dynamically
	// instead of capturing a single bridge for the life of the client.
	evtMu      sync.RWMutex
	currentEvt Events

	groupStateMu         sync.Mutex
	groupNameByJID       = make(map[string]string)
	groupRefreshInFlight = make(map[string]struct{})
)

func setEvt(e Events) {
	evtMu.Lock()
	currentEvt = e
	evtMu.Unlock()
}

func getEvt() Events {
	evtMu.RLock()
	defer evtMu.RUnlock()
	return currentEvt
}

// CoreLinked reports that the whatsmeow core linked into the framework.
// Useful as a trivial bridge sanity check from Swift.
func CoreLinked() bool { return true }

// Start opens (or creates) the session database under storeDir and connects.
// If a paired session already exists it resumes; otherwise it connects so
// that RequestPairingCode can be called next. Calling Start again is safe: it
// refreshes the host listener and reconnects the socket if it has dropped.
func Start(storeDir string, evt Events) error {
	mu.Lock()
	defer mu.Unlock()
	if evt == nil {
		return errors.New("events listener is required")
	}

	// How this device appears in WhatsApp > Linked Devices. WhatsApp renders its
	// own icon from PlatformType (custom images aren't possible); DESKTOP makes
	// it show the Os string verbatim ("MeGPT") instead of a "Browser (OS)" label.
	// These props are only sent at pair time, so changing them requires
	// unlinking and pairing again.
	store.DeviceProps.Os = proto.String("MeGPT")
	store.DeviceProps.PlatformType = waCompanionReg.DeviceProps_DESKTOP.Enum()

	// Always route callbacks to the latest host listener.
	setEvt(evt)

	// Already initialized. If the device was unlinked — by us, or from the phone's
	// WhatsApp > Linked Devices — whatsmeow deletes the session store (marking it
	// Deleted) and then refuses to reconnect or re-pair on that client. Reusing it
	// is exactly what makes the next pair attempt hang on "Logging in…" in
	// WhatsApp: a code is issued, but the new session keys can't be persisted to a
	// deleted store, so the link never completes. Tear the dead client down here
	// so we rebuild a fresh, pairable one below. Otherwise reuse it (including the
	// normal not-yet-paired state, where Store.ID is nil) and ensure the socket.
	if client != nil {
		if client.Store == nil || client.Store.Deleted {
			teardownLocked()
		} else {
			if !client.IsConnected() {
				if err := client.Connect(); err != nil {
					return fmt.Errorf("reconnect: %w", err)
				}
			}
			return nil
		}
	}

	rootCtx, cancelCtx = context.WithCancel(context.Background())

	dbPath := filepath.Join(storeDir, "whatsmeow.db")
	dsn := "file:" + dbPath + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	dbConn = db

	container = sqlstore.NewWithDB(db, "sqlite3", waLog.Stdout("WA-DB", "WARN", false))
	if err := container.Upgrade(rootCtx); err != nil {
		return fmt.Errorf("upgrade db: %w", err)
	}

	device, err := container.GetFirstDevice(rootCtx)
	if err != nil {
		return fmt.Errorf("get device: %w", err)
	}

	client = whatsmeow.NewClient(device, waLog.Stdout("WA", "INFO", false))
	c := client
	// Capture this client's root context for the handler's lifetime so JID
	// canonicalization (LID lookups) shares the client's cancellation scope.
	handlerCtx := rootCtx
	client.AddEventHandler(func(raw interface{}) {
		if e := getEvt(); e != nil {
			dispatch(handlerCtx, c, e, raw)
		}
	})

	if err := client.Connect(); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	return nil
}

// IsLoggedIn reports whether a paired session is currently active.
func IsLoggedIn() bool {
	mu.Lock()
	defer mu.Unlock()
	return client != nil && client.Store != nil && client.Store.ID != nil
}

// OwnJID returns this device's own canonical WhatsApp JID (assigned at pairing), or "" when
// not paired. It's the authenticated anchor the host uses to attribute the owner's messages.
func OwnJID() string {
	mu.Lock()
	c, ctx := client, rootCtx
	mu.Unlock()
	if c == nil || c.Store == nil || c.Store.ID == nil {
		return ""
	}
	return canonicalJID(ctx, c, *c.Store.ID).String()
}

// OwnLIDJID returns this device's own LID (the account's privacy alias, delivered at
// pairing), or "" when not paired or the LID is not yet known. It reads Store.LID —
// authenticated self-identity — never the per-contact LID map, which can go stale and
// mis-pair identities. Shipped alongside OwnJID so the server can attribute the owner's
// LID-addressed messages without per-message inference.
func OwnLIDJID() string {
	mu.Lock()
	defer mu.Unlock()
	if client == nil || client.Store == nil || client.Store.ID == nil {
		return ""
	}
	lid := client.Store.LID
	if lid.IsEmpty() {
		return ""
	}
	return lid.ToNonAD().String()
}

// IsConnected reports whether the websocket is currently connected. This is
// independent of login state (an unpaired client can be connected while it
// waits to be linked).
func IsConnected() bool {
	mu.Lock()
	defer mu.Unlock()
	return client != nil && client.IsConnected()
}

type groupListPayload struct {
	SchemaVersion int               `json:"schemaVersion"`
	Groups        []waGroupMetadata `json:"groups"`
}

// ListGroups returns metadata-only shells for all joined, message-bearing
// groups. Participant rosters are deliberately excluded; MeGPT learns people
// only from accepted message senders.
func ListGroups() (string, error) {
	mu.Lock()
	c, ctx := client, rootCtx
	mu.Unlock()
	if c == nil {
		return "", errors.New("not started")
	}
	if c.Store == nil || c.Store.ID == nil {
		return "", errors.New("not logged in")
	}
	if err := ensureSocket(c, 15*time.Second); err != nil {
		return "", err
	}
	if !c.WaitForConnection(15 * time.Second) {
		return "", errors.New("timed out waiting for WhatsApp login")
	}
	groups, err := c.GetJoinedGroups(ctx)
	if err != nil {
		return "", fmt.Errorf("get joined groups: %w", err)
	}
	return encodeGroupList(groups, time.Now())
}

func encodeGroupList(groups []*types.GroupInfo, observedAt time.Time) (string, error) {
	metadata := make([]waGroupMetadata, 0, len(groups))
	for _, group := range groups {
		item, ok := groupMetadataFromGroupInfo(group, "joined_groups", observedAt)
		if !ok {
			continue
		}
		cacheGroupName(item.GroupJID, item.DisplayName)
		metadata = append(metadata, item)
	}
	sort.Slice(metadata, func(i, j int) bool {
		return metadata[i].GroupJID < metadata[j].GroupJID
	})
	payload, err := json.Marshal(groupListPayload{
		SchemaVersion: wrapperSchemaVersion,
		Groups:        metadata,
	})
	if err != nil {
		return "", fmt.Errorf("encode joined groups: %w", err)
	}
	return string(payload), nil
}

// RequestPairingCode links this device to the given phone number (full
// international format, digits only, no leading +). Returns an 8-character
// code the user types into WhatsApp > Linked Devices > Link with phone number.
// It ensures the websocket is connected first, since pairing requires it.
func RequestPairingCode(phone string) (string, error) {
	mu.Lock()
	c, ctx := client, rootCtx
	mu.Unlock()
	if c == nil {
		return "", errors.New("not started")
	}
	// Pairing happens while we're intentionally unpaired, so we only need the
	// websocket up here — NOT a logged-in session (that's what we're creating).
	if err := ensureSocket(c, 15*time.Second); err != nil {
		return "", err
	}
	code, err := c.PairPhone(ctx, phone, true, whatsmeow.PairClientChrome, "Chrome (macOS)")
	if err != nil {
		return "", fmt.Errorf("pair: %w", err)
	}
	return code, nil
}

// ensureSocket makes sure the websocket is connected and the Noise handshake has
// completed, reconnecting if necessary.
//
// Unlike Client.WaitForConnection, it does NOT require the client to be logged
// in. That distinction matters: during pairing the client is intentionally
// unpaired, so waiting for a logged-in session there would always time out.
func ensureSocket(c *whatsmeow.Client, timeout time.Duration) error {
	if !c.IsConnected() {
		if err := c.Connect(); err != nil && !errors.Is(err, whatsmeow.ErrAlreadyConnected) {
			return fmt.Errorf("connect: %w", err)
		}
	}
	deadline := time.Now().Add(timeout)
	for !c.IsConnected() {
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for WhatsApp connection")
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

// SendText sends a plain text message to a supported direct or group recipient,
// allowing whatsmeow to generate the message ID.
func SendText(recipient string, text string) error {
	return sendText(recipient, text, "")
}

// SendTextWithID sends text with a caller-stable WhatsApp message ID. The app
// uses this for confirmed jobs so a crash before the completion ACK cannot
// generate a different WhatsApp ID on retry.
func SendTextWithID(recipient string, text string, messageID string) error {
	if err := validateMessageID(messageID); err != nil {
		return err
	}
	return sendText(recipient, text, messageID)
}

func sendText(recipient string, text string, messageID string) error {
	mu.Lock()
	c, ctx := client, rootCtx
	mu.Unlock()
	if c == nil {
		return errors.New("not started")
	}
	if c.Store == nil || c.Store.ID == nil {
		return errors.New("not logged in")
	}
	// Sending requires an authenticated session, so wait for the socket and then
	// for login to complete (the paired client auto-authenticates on connect).
	if err := ensureSocket(c, 15*time.Second); err != nil {
		return err
	}
	if !c.WaitForConnection(15 * time.Second) {
		return errors.New("timed out waiting for WhatsApp login")
	}
	jid, err := resolveSendJID(ctx, c, recipient)
	if err != nil {
		return err
	}
	var resp whatsmeow.SendResponse
	if messageID == "" {
		resp, err = c.SendMessage(ctx, jid, &waE2E.Message{Conversation: proto.String(text)})
	} else {
		resp, err = c.SendMessage(
			ctx,
			jid,
			&waE2E.Message{Conversation: proto.String(text)},
			whatsmeow.SendRequestExtra{ID: types.MessageID(messageID)},
		)
	}
	if err != nil {
		return fmt.Errorf("send: %w", err)
	}
	// whatsmeow never delivers our own client's sends back as events, so echo the
	// message through the same OnMessage path used for captured messages. Without
	// this, messages sent from the app would never reach the host and so would
	// never appear in the chat. The server-assigned ID lets the host dedupe this
	// against any later history-sync copy of the same message.
	echoChatJID := canonicalJID(ctx, c, jid)
	emitSentMessage(ctx, c, echoChatJID, string(resp.ID), text, "", resp.Timestamp)
	return nil
}

func validateMessageID(messageID string) error {
	if messageID == "" {
		return errors.New("message id is required")
	}
	if strings.TrimSpace(messageID) != messageID ||
		strings.ContainsAny(messageID, " \t\r\n") ||
		len(messageID) > 128 {
		return errors.New("invalid message id")
	}
	return nil
}

// SendImageURL sends an image to a recipient (bare phone number or full JID, see
// resolveSendJID) with an optional caption. The image bytes are fetched from
// mediaURL here, inside the wrapper, rather than passed across the
// gomobile/Swift/JS bridge: that keeps large binaries off the bridge (the host
// only hands us a URL string) and lets whatsmeow encrypt + upload the raw bytes
// exactly as the protocol expects.
//
// Flow mirrors whatsmeow's documented media send: download -> Upload (encrypt +
// upload, returns keys/URL) -> build an ImageMessage from those keys -> Send.
func SendImageURL(recipient string, mediaURL string, caption string) error {
	mu.Lock()
	c, ctx := client, rootCtx
	mu.Unlock()
	if c == nil {
		return errors.New("not started")
	}
	if c.Store == nil || c.Store.ID == nil {
		return errors.New("not logged in")
	}
	if strings.TrimSpace(mediaURL) == "" {
		return errors.New("media url is required")
	}
	if err := ensureSocket(c, 15*time.Second); err != nil {
		return err
	}
	if !c.WaitForConnection(15 * time.Second) {
		return errors.New("timed out waiting for WhatsApp login")
	}

	data, mimeType, err := downloadMedia(ctx, mediaURL)
	if err != nil {
		return fmt.Errorf("download media: %w", err)
	}

	uploaded, err := c.Upload(ctx, data, whatsmeow.MediaImage)
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}

	img := &waE2E.ImageMessage{
		Mimetype:      proto.String(mimeType),
		URL:           proto.String(uploaded.URL),
		DirectPath:    proto.String(uploaded.DirectPath),
		MediaKey:      uploaded.MediaKey,
		FileEncSHA256: uploaded.FileEncSHA256,
		FileSHA256:    uploaded.FileSHA256,
		FileLength:    proto.Uint64(uploaded.FileLength),
	}
	if caption != "" {
		img.Caption = proto.String(caption)
	}

	jid, err := resolveSendJID(ctx, c, recipient)
	if err != nil {
		return err
	}
	resp, err := c.SendMessage(ctx, jid, &waE2E.Message{ImageMessage: img})
	if err != nil {
		return fmt.Errorf("send: %w", err)
	}

	// Echo the send so it surfaces in the host chat, same as SendText. Media
	// echoes don't carry the image yet, so show the caption (or a photo marker)
	// to reflect that something was sent. The server-assigned ID lets the host
	// dedupe this against any later history-sync copy. A captioned send ships
	// mediaType "image" so it renders the same as a captured captioned photo;
	// a captionless one keeps the plain-text placeholder (no mediaType, or the
	// backend would prefix the placeholder with a redundant marker).
	echoText := caption
	echoMediaType := "image"
	if echoText == "" {
		echoText = "\U0001F4F7 Photo"
		echoMediaType = ""
	}
	echoChatJID := canonicalJID(ctx, c, jid)
	emitSentMessage(ctx, c, echoChatJID, string(resp.ID), echoText, echoMediaType, resp.Timestamp)
	return nil
}

// PostStatusImageURL posts an image to the user's WhatsApp Status ("story"),
// with an optional caption. Like SendImageURL it fetches the bytes inside the
// wrapper from mediaURL (keeping large binaries off the gomobile/Swift/JS
// bridge), then uploads + sends — but the destination is the special
// status@broadcast JID instead of a 1:1 chat.
//
// whatsmeow handles the broadcast fan-out: sending to StatusBroadcastJID makes
// it resolve the user's status-privacy recipient list and encrypt the media
// message for each of them. Status posts are not echoed back as chat messages
// (they don't belong to any DM thread); the server posts the user-facing
// confirmation instead.
func PostStatusImageURL(mediaURL string, caption string) error {
	mu.Lock()
	c, ctx := client, rootCtx
	mu.Unlock()
	if c == nil {
		return errors.New("not started")
	}
	if c.Store == nil || c.Store.ID == nil {
		return errors.New("not logged in")
	}
	if strings.TrimSpace(mediaURL) == "" {
		return errors.New("media url is required")
	}
	if err := ensureSocket(c, 15*time.Second); err != nil {
		return err
	}
	if !c.WaitForConnection(15 * time.Second) {
		return errors.New("timed out waiting for WhatsApp login")
	}

	data, mimeType, err := downloadMedia(ctx, mediaURL)
	if err != nil {
		return fmt.Errorf("download media: %w", err)
	}

	uploaded, err := c.Upload(ctx, data, whatsmeow.MediaImage)
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}

	img := &waE2E.ImageMessage{
		Mimetype:      proto.String(mimeType),
		URL:           proto.String(uploaded.URL),
		DirectPath:    proto.String(uploaded.DirectPath),
		MediaKey:      uploaded.MediaKey,
		FileEncSHA256: uploaded.FileEncSHA256,
		FileSHA256:    uploaded.FileSHA256,
		FileLength:    proto.Uint64(uploaded.FileLength),
	}
	if caption != "" {
		img.Caption = proto.String(caption)
	}

	if _, err := c.SendMessage(ctx, types.StatusBroadcastJID, &waE2E.Message{ImageMessage: img}); err != nil {
		return fmt.Errorf("post status: %w", err)
	}
	return nil
}

// downloadMedia fetches bytes from a URL with a bounded size and resolves a
// usable image MIME type, preferring the server's Content-Type header and
// falling back to content sniffing. It rejects non-image and oversized bodies so
// a bad URL fails fast instead of being uploaded as a broken attachment.
func downloadMedia(ctx context.Context, url string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxOutboundMediaBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) == 0 {
		return nil, "", errors.New("empty media body")
	}
	if len(data) > maxOutboundMediaBytes {
		return nil, "", fmt.Errorf("media exceeds %d byte limit", maxOutboundMediaBytes)
	}

	mimeType := resp.Header.Get("Content-Type")
	if i := strings.IndexByte(mimeType, ';'); i >= 0 {
		mimeType = mimeType[:i]
	}
	mimeType = strings.TrimSpace(mimeType)
	if !strings.HasPrefix(mimeType, "image/") {
		// Header missing or generic (e.g. application/octet-stream from a CDN):
		// sniff the actual bytes so the recipient gets the right content type.
		mimeType = http.DetectContentType(data)
	}
	if !strings.HasPrefix(mimeType, "image/") {
		return nil, "", fmt.Errorf("unsupported media type %q", mimeType)
	}
	return data, mimeType, nil
}

// IsOnWhatsApp reports whether a phone number is registered on WhatsApp. The
// phone is digits only with no leading + (same convention as SendText); we add
// the international + prefix that whatsmeow expects. This is a server query, so
// it requires a logged-in session — callers use it to avoid sending into the
// void when a contact isn't on WhatsApp.
func IsOnWhatsApp(phone string) (bool, error) {
	mu.Lock()
	c, ctx := client, rootCtx
	mu.Unlock()
	if c == nil {
		return false, errors.New("not started")
	}
	if c.Store == nil || c.Store.ID == nil {
		return false, errors.New("not logged in")
	}
	if err := ensureSocket(c, 15*time.Second); err != nil {
		return false, err
	}
	if !c.WaitForConnection(15 * time.Second) {
		return false, errors.New("timed out waiting for WhatsApp login")
	}
	query := phone
	if !strings.HasPrefix(query, "+") {
		query = "+" + query
	}
	resp, err := c.IsOnWhatsApp(ctx, []string{query})
	if err != nil {
		return false, fmt.Errorf("is-on-whatsapp: %w", err)
	}
	if len(resp) == 0 {
		return false, nil
	}
	return resp[0].IsIn, nil
}

// Disconnect closes the websocket but keeps the session on disk.
func Disconnect() {
	mu.Lock()
	defer mu.Unlock()
	if client != nil {
		client.Disconnect()
	}
}

// Logout unlinks this device from the account and clears the local session,
// then tears down the in-memory client so the next Start builds a fresh one.
// whatsmeow can't cleanly re-pair on a client object that has already been
// logged out, so without this reset a re-link hangs on "Logging in…" until the
// app is force-quit.
func Logout() error {
	mu.Lock()
	c, ctx := client, rootCtx
	mu.Unlock()
	if c == nil {
		return errors.New("not started")
	}
	if err := c.Logout(ctx); err != nil {
		return fmt.Errorf("logout: %w", err)
	}
	resetClient()
	return nil
}

// resetClient tears down the in-memory client, store handle, and context so the
// next Start rebuilds everything from scratch: a fresh, unpaired client ready to
// pair again. Only call after the on-disk session has been cleared (e.g. after a
// successful Logout), otherwise the next Start would just resume the old device.
// resetClient tears down the in-memory client from outside an mu-locked section
// (e.g. after Logout). It is the locking wrapper around teardownLocked.
func resetClient() {
	mu.Lock()
	defer mu.Unlock()
	teardownLocked()
}

// teardownLocked cancels the root context, closes the store handle, and clears
// every cached reference so the next Start builds a fresh client. Callers must
// already hold mu (e.g. Start rebuilding a dead client in place).
func teardownLocked() {
	if cancelCtx != nil {
		cancelCtx()
	}
	if dbConn != nil {
		_ = dbConn.Close()
	}
	client = nil
	container = nil
	dbConn = nil
	rootCtx = nil
	cancelCtx = nil
	groupStateMu.Lock()
	clear(groupNameByJID)
	clear(groupRefreshInFlight)
	groupStateMu.Unlock()
}

// waMessage is the JSON shape delivered to the host for both live and
// historical messages. It is intentionally flat and text-only for now.
//
// ChatJID/SenderJID are canonical (LID-preferred, see canonicalJID) so an
// identity stays stable across the forms WhatsApp uses. Because a LID carries no
// phone number, we additionally resolve — best effort, on-device — the dialable
// phone for each JID and the user's saved address-book name for the chat
// counterparty. The host uses the phone to unify a chat with an existing contact
// and the contact name to label it, instead of surfacing the opaque LID id.
type waMessage struct {
	ChatJID       string `json:"chatJID"`
	SenderJID     string `json:"senderJID"`
	SenderAltJID  string `json:"senderAltJID,omitempty"`
	MessageID     string `json:"messageID"`
	TimestampSecs int64  `json:"timestampSecs"`
	Text          string `json:"text"`
	// Set when Text is a media caption rather than a standalone text message:
	// "image", "gif", "video", or "document" (media_caption_v1). The media
	// itself is not ingested — the caption is the retained context.
	MediaType      string `json:"mediaType,omitempty"`
	PushName       string `json:"pushName"`
	ChatType       string `json:"chatType"`
	ChatName       string `json:"chatName,omitempty"`
	IsFromMe       bool   `json:"isFromMe"`
	AddressingMode string `json:"addressingMode,omitempty"`
	// Dialable phone (digits only, no +) resolved from each JID's LID->PN
	// mapping; empty when the device knows no phone for that identity.
	SenderPhoneNumber string `json:"senderPhoneNumber"`
	ChatPhoneNumber   string `json:"chatPhoneNumber"`
	// The owner's saved address-book name for the chat counterparty, if any.
	ContactName string `json:"contactName"`
	// Reply context from ContextInfo, set when this message quotes another.
	// QuotedMessageID is the quoted message's wire ID (joinable against a
	// previously-synced MessageID); QuotedText is a short snippet of the quoted
	// content shipped inline by the protocol, so it survives even when the
	// original was never synced (pre-history media, deleted, ...).
	QuotedMessageID  string `json:"quotedMessageID,omitempty"`
	QuotedSenderJID  string `json:"quotedSenderJID,omitempty"`
	QuotedSenderName string `json:"quotedSenderName,omitempty"`
	QuotedText       string `json:"quotedText,omitempty"`
	// True when the quoted message was the owner's own — the "someone replied
	// to the user" signal. Resolved on-device, where the LID mappings live.
	QuotedIsFromMe bool `json:"quotedIsFromMe,omitempty"`
}

type historySyncPayload struct {
	Messages      []waMessage       `json:"messages"`
	GroupMetadata []waGroupMetadata `json:"groupMetadata,omitempty"`
	// PushNames carries counterparty display names from a PUSH_NAME history-sync
	// event. That event has no messages, so the names are delivered to the host
	// here (keyed by JID) and applied out-of-band rather than riding on a message.
	PushNames  []waPushName `json:"pushNames,omitempty"`
	SyncType   string       `json:"syncType"`
	ChunkOrder uint32       `json:"chunkOrder"`
	Progress   uint32       `json:"progress"`
	BatchIndex uint32       `json:"batchIndex"`
}

// waPushName is a counterparty's WhatsApp display name (pushName) delivered in a
// PUSH_NAME history-sync event. JID is canonical (matches a message's SenderJID)
// so the host/backend can tie it to an existing thread; PhoneNumber is the
// dialable phone (digits only, no +) for that JID when the device knows it.
type waPushName struct {
	JID         string `json:"jid"`
	Name        string `json:"name"`
	PhoneNumber string `json:"phoneNumber"`
}

// waGroupMetadata intentionally excludes the participant roster. MeGPT only
// uploads people observed as message senders; the group title and lifecycle
// flags are enough to identify and safely render the conversation.
type waGroupMetadata struct {
	GroupJID          string  `json:"groupJID"`
	DisplayName       string  `json:"displayName,omitempty"`
	ObservedAtSecs    int64   `json:"observedAtSecs"`
	Source            string  `json:"source"`
	ReadOnly          *bool   `json:"readOnly,omitempty"`
	IsParent          *bool   `json:"isParent,omitempty"`
	ParentGroupJID    string  `json:"parentGroupJID,omitempty"`
	IsDefaultSubgroup *bool   `json:"isDefaultSubgroup,omitempty"`
	IsAnnouncement    *bool   `json:"isAnnouncement,omitempty"`
	IsEphemeral       *bool   `json:"isEphemeral,omitempty"`
	DisappearingTimer *uint32 `json:"disappearingTimer,omitempty"`
	ParticipantCount  int     `json:"participantCount,omitempty"`
	Suspended         *bool   `json:"suspended,omitempty"`
	Deleted           *bool   `json:"deleted,omitempty"`
}

// messageText pulls plain text out of a message, covering both simple
// conversation messages and extended (link/quote) text. Media captions are
// intentionally excluded here (quoting renders them itself); the ingestion
// pipeline uses messageContent, which admits them.
func messageText(msg *waE2E.Message) string {
	if msg == nil {
		return ""
	}
	if t := msg.GetConversation(); t != "" {
		return t
	}
	if ext := msg.GetExtendedTextMessage(); ext != nil {
		return ext.GetText()
	}
	return ""
}

// messageContent extracts the ingestible text of a message along with the kind
// of media it was attached to ("" for standalone text). WhatsApp carries
// captions inline on image/video/document messages (media_caption_v1), so a
// captioned photo ships its caption as the message text instead of being
// dropped. Captionless media still yields "" and is skipped — the media bytes
// themselves are out of scope. Stickers, audio, and voice notes carry no
// caption in the protocol.
func messageContent(msg *waE2E.Message) (text string, mediaType string) {
	if msg == nil {
		return "", ""
	}
	if t := messageText(msg); t != "" {
		return t, ""
	}
	if img := msg.GetImageMessage(); img != nil {
		return img.GetCaption(), "image"
	}
	if vid := msg.GetVideoMessage(); vid != nil {
		// WhatsApp GIFs are videos with GifPlayback; naming them lets the
		// backend render "[GIF]" instead of the misleading "[video]".
		if vid.GetGifPlayback() {
			return vid.GetCaption(), "gif"
		}
		return vid.GetCaption(), "video"
	}
	if doc := msg.GetDocumentMessage(); doc != nil {
		return doc.GetCaption(), "document"
	}
	return "", ""
}

// messageContextInfo returns the ContextInfo carried by whichever admitted
// shape the message is: plain Conversation text has none; extended text and
// captioned media each carry their own. Mentions and reply context must be
// read from here — a captioned photo that replies to (or mentions) someone
// stores that context on the ImageMessage, not on an ExtendedTextMessage.
func messageContextInfo(msg *waE2E.Message) *waE2E.ContextInfo {
	if msg == nil {
		return nil
	}
	if ext := msg.GetExtendedTextMessage(); ext != nil {
		return ext.GetContextInfo()
	}
	if img := msg.GetImageMessage(); img != nil {
		return img.GetContextInfo()
	}
	if vid := msg.GetVideoMessage(); vid != nil {
		return vid.GetContextInfo()
	}
	if doc := msg.GetDocumentMessage(); doc != nil {
		return doc.GetContextInfo()
	}
	return nil
}

// quotedSnippetMaxRunes caps the inline quoted-content snippet. WhatsApp's own
// quote box renders only a preview; 300 runes is ample context and keeps a
// hostile peer from inflating every reply with a full-size quoted payload.
const quotedSnippetMaxRunes = 300

// quoteContext carries the reply-to fields extracted from a reply's
// ContextInfo. Zero value means "not a reply".
type quoteContext struct {
	messageID  string
	senderJID  string
	senderName string
	text       string
	isFromMe   bool
}

// extractQuoteContext pulls the quoted-message reference out of a reply. A
// plain Conversation message can't carry ContextInfo; extended text and
// captioned media (which can also be sent as replies) each carry their own,
// so the lookup goes through messageContextInfo and never misses a shape the
// pipeline keeps.
func extractQuoteContext(ctx context.Context, c *whatsmeow.Client, msg *waE2E.Message) quoteContext {
	info := messageContextInfo(msg)
	if info == nil {
		return quoteContext{}
	}
	stanzaID := info.GetStanzaID()
	quoted := info.GetQuotedMessage()
	if stanzaID == "" && quoted == nil {
		return quoteContext{}
	}
	q := quoteContext{
		messageID: stanzaID,
		text:      quotedMessageSnippet(ctx, c, quoted),
	}
	if raw := info.GetParticipant(); raw != "" {
		if jid, err := types.ParseJID(raw); err == nil && jid.User != "" {
			q.senderJID = canonicalJID(ctx, c, jid).String()
			q.senderName = mentionDisplayName(ctx, c, jid)
			if c != nil && c.Store != nil && c.Store.ID != nil {
				q.isFromMe = senderMatchesOwnIdentity(
					jid,
					types.JID{},
					dialablePhone(ctx, c, jid),
					c.Store.ID.ToNonAD(),
					canonicalJID(ctx, c, *c.Store.ID),
				)
			}
		}
	}
	return q
}

// unwrapQuotedMessage strips the FutureProofMessage envelopes (ephemeral,
// view-once, captioned-document) a quoted message may arrive inside, so the
// snippet reflects the actual content.
func unwrapQuotedMessage(msg *waE2E.Message) *waE2E.Message {
	for range 3 {
		switch {
		case msg.GetEphemeralMessage().GetMessage() != nil:
			msg = msg.GetEphemeralMessage().GetMessage()
		case msg.GetViewOnceMessage().GetMessage() != nil:
			msg = msg.GetViewOnceMessage().GetMessage()
		case msg.GetViewOnceMessageV2().GetMessage() != nil:
			msg = msg.GetViewOnceMessageV2().GetMessage()
		case msg.GetDocumentWithCaptionMessage().GetMessage() != nil:
			msg = msg.GetDocumentWithCaptionMessage().GetMessage()
		default:
			return msg
		}
	}
	return msg
}

// quotedMessageSnippet renders the inline copy of a quoted message as a short
// single-line snippet: the text for text quotes, the caption (else a bracketed
// kind marker) for media the text-only pipeline never stored.
func quotedMessageSnippet(ctx context.Context, c *whatsmeow.Client, msg *waE2E.Message) string {
	if msg == nil {
		return ""
	}
	msg = unwrapQuotedMessage(msg)
	if t := messageText(msg); t != "" {
		// The quoted message carries its own mention JIDs, so raw "@<digits>"
		// tokens in the snippet resolve the same way live text does.
		return normalizeQuotedSnippet(resolveMentionTokens(ctx, c, msg, t))
	}
	captionOr := func(caption string, fallback string) string {
		if caption != "" {
			return normalizeQuotedSnippet(caption)
		}
		return fallback
	}
	switch {
	case msg.GetImageMessage() != nil:
		return captionOr(msg.GetImageMessage().GetCaption(), "[photo]")
	case msg.GetVideoMessage() != nil:
		return captionOr(msg.GetVideoMessage().GetCaption(), "[video]")
	case msg.GetAudioMessage() != nil:
		if msg.GetAudioMessage().GetPTT() {
			return "[voice message]"
		}
		return "[audio]"
	case msg.GetStickerMessage() != nil:
		return "[sticker]"
	case msg.GetDocumentMessage() != nil:
		return captionOr(msg.GetDocumentMessage().GetCaption(), "[document]")
	case msg.GetLocationMessage() != nil, msg.GetLiveLocationMessage() != nil:
		return "[location]"
	case msg.GetContactMessage() != nil, msg.GetContactsArrayMessage() != nil:
		return "[contact card]"
	case msg.GetPollCreationMessage() != nil:
		return captionOr(msg.GetPollCreationMessage().GetName(), "[poll]")
	default:
		// Unknown content still ships the stanza ID, so the reply stays
		// attributable even without a snippet.
		return ""
	}
}

// normalizeQuotedSnippet collapses a quoted body onto one line and truncates
// it, so a reply renders as a single readable history line downstream.
func normalizeQuotedSnippet(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if len(runes) <= quotedSnippetMaxRunes {
		return s
	}
	return string(runes[:quotedSnippetMaxRunes]) + "…"
}

// resolveMentionTokens rewrites raw @-mention tokens into readable names.
// WhatsApp renders a mention in the wire text as "@<jid user part>" (a phone
// number or, post-LID-migration, an opaque 15-digit LID) and carries the
// mentioned JIDs in ContextInfo.MentionedJID. Downstream consumers (the
// backend's summaries and prompts) only ever see the flat text, so an
// unresolved token like "@248506975531090" hides who a message is addressed
// to. The device is the only place with the full contact store, so the
// rewrite happens here, at capture. Tokens that resolve to no name are left
// untouched rather than degraded.
func resolveMentionTokens(ctx context.Context, c *whatsmeow.Client, msg *waE2E.Message, text string) string {
	mentioned := messageContextInfo(msg).GetMentionedJID()
	if len(mentioned) == 0 || !strings.ContainsRune(text, '@') {
		return text
	}
	jids := make([]types.JID, 0, len(mentioned))
	for _, raw := range mentioned {
		jid, err := types.ParseJID(raw)
		if err != nil || jid.User == "" {
			continue
		}
		jids = append(jids, jid)
	}
	// Longest user part first so replacing "@1234" can never corrupt a
	// longer token like "@12345" that shares the prefix.
	sort.SliceStable(jids, func(i, j int) bool {
		return len(jids[i].User) > len(jids[j].User)
	})
	for _, jid := range jids {
		token := "@" + jid.User
		if !strings.Contains(text, token) {
			continue
		}
		name := mentionDisplayName(ctx, c, jid)
		if name == "" || name == jid.User {
			continue
		}
		text = strings.ReplaceAll(text, token, "@"+name)
	}
	return text
}

// mentionDisplayName resolves a mentioned JID to the best human-readable name
// the device knows: the owner's own display name when the owner is mentioned
// (the strongest signal a message is addressed to the user), then the saved
// address-book name, the contact's WhatsApp display name (pushName), and
// finally the dialable phone number.
func mentionDisplayName(ctx context.Context, c *whatsmeow.Client, jid types.JID) string {
	if c != nil && c.Store != nil && c.Store.ID != nil {
		own := c.Store.ID.ToNonAD()
		ownCanonical := canonicalJID(ctx, c, *c.Store.ID)
		candidate := jid.ToNonAD()
		if candidate.User == own.User ||
			(!ownCanonical.IsEmpty() && candidate.User == ownCanonical.User) {
			if name := strings.TrimSpace(c.Store.PushName); name != "" {
				return name
			}
		}
	}
	if name := deviceContactName(ctx, c, jid); name != "" {
		return name
	}
	if name := contactPushName(ctx, c, jid); name != "" {
		return name
	}
	return dialablePhone(ctx, c, jid)
}

// contactPushName returns the stored WhatsApp display name (pushName) for a
// 1:1 JID, looking up both the JID as observed and its resolved phone form —
// the mention-resolution counterpart of deviceContactName, which intentionally
// excludes pushName.
func contactPushName(ctx context.Context, c *whatsmeow.Client, jid types.JID) string {
	if c == nil || c.Store == nil || c.Store.Contacts == nil {
		return ""
	}
	candidates := []types.JID{jid.ToNonAD()}
	if pn := phoneJID(ctx, c, jid); !pn.IsEmpty() && pn.String() != jid.ToNonAD().String() {
		candidates = append(candidates, pn)
	}
	for _, candidate := range candidates {
		if candidate.IsEmpty() {
			continue
		}
		info, err := c.Store.Contacts.GetContact(ctx, candidate)
		if err != nil || !info.Found {
			continue
		}
		if name := strings.TrimSpace(info.PushName); name != "" {
			return name
		}
	}
	return ""
}

// toWaMessage maps a parsed whatsmeow message to the host payload shape. Only
// ordinary direct chats and message-bearing @g.us groups are admitted.
func toWaMessage(ctx context.Context, c *whatsmeow.Client, m *events.Message) (waMessage, bool) {
	if m == nil {
		return waMessage{}, false
	}
	chatType, ok := supportedChatType(m.Info)
	if !ok {
		return waMessage{}, false
	}
	// V1 group support has no expiry/edit/tombstone model, so do not retain
	// content whose lifecycle we cannot honor. Direct behavior stays unchanged.
	if chatType == "group" && (m.IsEphemeral || m.IsViewOnce || m.IsEdit) {
		return waMessage{}, false
	}
	// View-once content is designed to disappear after a single viewing;
	// retaining its caption would defeat that expectation, so it is dropped in
	// every chat type. (Before captions were admitted this was implicit:
	// view-once is always media, and media always extracted to "".)
	if m.IsViewOnce {
		return waMessage{}, false
	}
	text, mediaType := messageContent(m.Message)
	if text == "" {
		return waMessage{}, false
	}
	text = resolveMentionTokens(ctx, c, m.Message, text)
	quote := extractQuoteContext(ctx, c, m.Message)
	senderJID := canonicalParticipantJID(ctx, c, m.Info.Sender, m.Info.SenderAlt)
	senderPhone := dialablePhoneFromPair(ctx, c, m.Info.Sender, m.Info.SenderAlt)
	// whatsmeow computes IsFromMe for group messages by comparing the participant
	// against Store.ID/Store.LID only; with an absent or stale own-LID mapping the
	// device sees its own LID-addressed group messages as IsFromMe=false, and the
	// host then treats the owner's message as inbound (it gets summarized back to
	// the user as if a friend sent it). Re-derive ownership from every own-identity
	// form the device can resolve before shipping the message.
	isFromMe := m.Info.IsFromMe
	if !isFromMe && c != nil && c.Store != nil && c.Store.ID != nil {
		isFromMe = senderMatchesOwnIdentity(
			m.Info.Sender,
			m.Info.SenderAlt,
			senderPhone,
			c.Store.ID.ToNonAD(),
			canonicalJID(ctx, c, *c.Store.ID),
		)
	}
	if isFromMe && c != nil && c.Store != nil && c.Store.ID != nil {
		// An own message must never ship a third party's identity. The same stale
		// LID store that loses IsFromMe can resolve the owner's group LID to a
		// *contact's* phone, and the host then renders the owner's own message as
		// that contact (and phone-links the owner's LID row to them). The
		// account's own number is the only correct value here.
		senderPhone = c.Store.ID.User
	}
	contactTarget := m.Info.Chat
	if chatType == "group" {
		contactTarget = m.Info.Sender
	}
	contactName := ""
	// In groups the contact name is resolved against the sender; for an own
	// message that lookup can only name the owner or — through a stale LID
	// mapping — the wrong contact entirely, so skip it (direct chats resolve
	// against the counterparty chat JID and stay correct for own messages).
	if !(chatType == "group" && isFromMe) {
		contactName = deviceContactName(ctx, c, contactTarget)
		if contactName == "" && chatType == "group" && !m.Info.SenderAlt.IsEmpty() {
			contactName = deviceContactName(ctx, c, m.Info.SenderAlt)
		}
	}
	chatName := ""
	if chatType == "group" {
		chatName = cachedGroupName(m.Info.Chat)
	}
	return waMessage{
		ChatJID:           canonicalJID(ctx, c, m.Info.Chat).String(),
		SenderJID:         senderJID.String(),
		SenderAltJID:      nonADJIDString(m.Info.SenderAlt),
		MessageID:         string(m.Info.ID),
		TimestampSecs:     m.Info.Timestamp.Unix(),
		Text:              text,
		MediaType:         mediaType,
		PushName:          m.Info.PushName,
		ChatType:          chatType,
		ChatName:          chatName,
		IsFromMe:          isFromMe,
		AddressingMode:    string(m.Info.AddressingMode),
		SenderPhoneNumber: senderPhone,
		ChatPhoneNumber: func() string {
			if chatType == "group" {
				return ""
			}
			return dialablePhone(ctx, c, m.Info.Chat)
		}(),
		ContactName:      contactName,
		QuotedMessageID:  quote.messageID,
		QuotedSenderJID:  quote.senderJID,
		QuotedSenderName: quote.senderName,
		QuotedText:       quote.text,
		QuotedIsFromMe:   quote.isFromMe,
	}, true
}

// senderMatchesOwnIdentity reports whether a message's sender resolves to this
// device's own WhatsApp account, using identity forms whatsmeow's own IsFromMe
// computation does not consult: the canonical (LID-preferred, via the LID
// database rather than Store.LID) own JID, the sender's device-paired alternate
// form, and the sender's resolved dialable phone. User-part comparison matches
// upstream message.go's own convention.
func senderMatchesOwnIdentity(
	sender types.JID,
	senderAlt types.JID,
	senderPhone string,
	ownPN types.JID,
	ownCanonical types.JID,
) bool {
	if ownPN.IsEmpty() {
		return false
	}
	for _, candidate := range []types.JID{sender.ToNonAD(), senderAlt.ToNonAD()} {
		if candidate.IsEmpty() {
			continue
		}
		if candidate.User == ownPN.User {
			return true
		}
		if !ownCanonical.IsEmpty() && candidate.User == ownCanonical.User {
			return true
		}
	}
	return senderPhone != "" && senderPhone == ownPN.User
}

func supportedChatType(info types.MessageInfo) (string, bool) {
	chat := info.Chat.ToNonAD()
	switch chat.Server {
	case types.GroupServer:
		return "group", info.IsGroup && chat.User != ""
	case types.DefaultUserServer, types.HiddenUserServer:
		return "direct", !info.IsGroup && chat.User != ""
	default:
		return "", false
	}
}

func nonADJIDString(jid types.JID) string {
	if jid.IsEmpty() {
		return ""
	}
	return jid.ToNonAD().String()
}

// canonicalParticipantJID prefers an explicit LID alternate before consulting
// the store, eliminating the history-sync race where PN/LID mappings have not
// finished persisting when the event reaches the wrapper.
func canonicalParticipantJID(
	ctx context.Context,
	c *whatsmeow.Client,
	primary types.JID,
	alternate types.JID,
) types.JID {
	for _, candidate := range []types.JID{primary.ToNonAD(), alternate.ToNonAD()} {
		if candidate.Server == types.HiddenUserServer {
			return candidate
		}
	}
	return canonicalJID(ctx, c, primary)
}

func dialablePhoneFromPair(
	ctx context.Context,
	c *whatsmeow.Client,
	primary types.JID,
	alternate types.JID,
) string {
	for _, candidate := range []types.JID{primary.ToNonAD(), alternate.ToNonAD()} {
		if candidate.Server == types.DefaultUserServer {
			return candidate.User
		}
	}
	for _, candidate := range []types.JID{primary, alternate} {
		if phone := dialablePhone(ctx, c, candidate); phone != "" {
			return phone
		}
	}
	return ""
}

// canonicalJID reduces any 1:1 user JID to a single stable identity so the same
// person maps to one JID no matter how we observed them. WhatsApp exposes the
// same user under several forms — phone-number JIDs (`<pn>@s.whatsapp.net`), the
// newer privacy LID JIDs (`<id>@lid`), and per-device "AD" variants
// (`…:<device>@…`) — and they arrive inconsistently: received events carry the
// raw sender/chat, while our own sends are normalized. If we forwarded those raw
// forms, one contact (or the owner) would split into multiple users, which is
// what makes the owner's own name leak onto a thread (an owner AD/LID variant
// that doesn't match the stored account JID gets treated as the counterparty).
//
// We strip the device (ToNonAD) and prefer the LID form (matching WhatsApp's own
// direction), falling back to the phone JID when no LID mapping exists. The
// mapping is deterministic at a given time, so both the receive and self-send
// paths converge on the same value for the same identity.
func canonicalJID(ctx context.Context, c *whatsmeow.Client, jid types.JID) types.JID {
	id := jid.ToNonAD()
	if c == nil || c.Store == nil || id.Server != types.DefaultUserServer {
		return id
	}
	lid, err := c.Store.LIDs.GetLIDForPN(ctx, id)
	if err != nil || lid.IsEmpty() {
		return id
	}
	return lid.ToNonAD()
}

// phoneJID resolves a 1:1 user JID to its phone-number form
// (`<pn>@s.whatsapp.net`). A phone JID is already in that form; a LID
// (`<id>@lid`) is translated via the device's stored LID->PN mapping. Any other
// server (or an unknown LID) yields an empty JID. This is the inverse of
// canonicalJID, which prefers the LID for stable identity.
func phoneJID(ctx context.Context, c *whatsmeow.Client, jid types.JID) types.JID {
	id := jid.ToNonAD()
	switch id.Server {
	case types.DefaultUserServer:
		return id
	case types.HiddenUserServer:
		if c == nil || c.Store == nil {
			return types.JID{}
		}
		pn, err := c.Store.LIDs.GetPNForLID(ctx, id)
		if err != nil || pn.IsEmpty() {
			return types.JID{}
		}
		return pn.ToNonAD()
	default:
		return types.JID{}
	}
}

// dialablePhone returns the dialable phone number (digits only, no +) for a 1:1
// user JID, or "" when the device knows no phone for that identity (e.g. a LID
// with no stored mapping). The digits match the host's phone convention.
func dialablePhone(ctx context.Context, c *whatsmeow.Client, jid types.JID) string {
	pn := phoneJID(ctx, c, jid)
	if pn.IsEmpty() {
		return ""
	}
	return pn.User
}

// deviceContactName returns the owner's saved address-book name for a 1:1 JID
// (full name, then first name, then business name), or "" when the user isn't a
// saved contact. The contact store is keyed by phone, so we look up both the JID
// as observed and its resolved phone form. PushName is intentionally excluded
// here — it already rides along on the message as PushName.
func deviceContactName(ctx context.Context, c *whatsmeow.Client, jid types.JID) string {
	if c == nil || c.Store == nil || c.Store.Contacts == nil {
		return ""
	}
	candidates := []types.JID{jid.ToNonAD()}
	if pn := phoneJID(ctx, c, jid); !pn.IsEmpty() && pn.String() != jid.ToNonAD().String() {
		candidates = append(candidates, pn)
	}
	for _, candidate := range candidates {
		if candidate.IsEmpty() {
			continue
		}
		info, err := c.Store.Contacts.GetContact(ctx, candidate)
		if err != nil || !info.Found {
			continue
		}
		if name := firstNonEmpty(info.FullName, info.FirstName, info.BusinessName); name != "" {
			return name
		}
	}
	return ""
}

// firstNonEmpty returns the first value that is non-empty after trimming spaces.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// resolveSendJID converts a caller-provided recipient into the JID we actually
// send to. The recipient is either a bare phone number (digits, no +) — the
// historical contract used for contact-resolved sends — or a full JID string
// taken from a known thread. Because of WhatsApp's LID migration a thread's
// counterparty is often only addressable by its `<id>@lid` JID (we never learn a
// dialable phone for them server-side), so phone-only sending can't reach them.
// We resolve a LID target to its phone-number JID when the device knows the
// mapping (the most reliable delivery path); otherwise we send to the JID as
// parsed and let whatsmeow handle LID addressing.
func resolveSendJID(ctx context.Context, c *whatsmeow.Client, recipient string) (types.JID, error) {
	recipient = strings.TrimSpace(recipient)
	if recipient == "" {
		return types.JID{}, errors.New("empty recipient")
	}
	if !strings.ContainsRune(recipient, '@') {
		// Bare phone number (digits only, no +): the historical contract.
		return types.NewJID(recipient, types.DefaultUserServer), nil
	}
	jid, err := types.ParseJID(recipient)
	if err != nil {
		return types.JID{}, fmt.Errorf("parse recipient jid %q: %w", recipient, err)
	}
	jid = jid.ToNonAD()
	if !supportedSendJID(jid) {
		return types.JID{}, fmt.Errorf("unsupported recipient jid server %q", jid.Server)
	}
	if jid.Server == types.HiddenUserServer && c != nil && c.Store != nil {
		if pn, perr := c.Store.LIDs.GetPNForLID(ctx, jid); perr == nil && !pn.IsEmpty() {
			return pn.ToNonAD(), nil
		}
	}
	return jid, nil
}

func supportedSendJID(jid types.JID) bool {
	if jid.User == "" {
		return false
	}
	switch jid.Server {
	case types.DefaultUserServer, types.HiddenUserServer, types.GroupServer:
		return true
	default:
		return false
	}
}

// emitSentMessage echoes a message we just sent through the same OnMessage path
// used for received messages. whatsmeow does not deliver our own client's sends
// back as events, so this is the only way our outbound messages reach the host
// (and thus the chat). SenderJID is our own canonical JID (the same value OwnJID
// returns), so the host attributes it to the owner; it also carries the
// server-assigned id+timestamp so a later history-sync copy dedupes against it
// instead of duplicating.
func emitSentMessage(
	ctx context.Context,
	c *whatsmeow.Client,
	chat types.JID,
	id string,
	text string,
	mediaType string,
	ts time.Time,
) {
	evt := getEvt()
	if evt == nil || c.Store == nil || c.Store.ID == nil {
		return
	}
	if ts.IsZero() {
		ts = time.Now()
	}
	payload, err := json.Marshal(waMessage{
		ChatJID:       chat.String(),
		SenderJID:     canonicalJID(ctx, c, *c.Store.ID).String(),
		MessageID:     id,
		TimestampSecs: ts.Unix(),
		Text:          text,
		MediaType:     mediaType,
		PushName:      c.Store.PushName,
		ChatType: func() string {
			if chat.Server == types.GroupServer {
				return "group"
			}
			return "direct"
		}(),
		ChatName:          cachedGroupName(chat),
		IsFromMe:          true,
		SenderPhoneNumber: dialablePhone(ctx, c, *c.Store.ID),
		ChatPhoneNumber: func() string {
			if chat.Server == types.GroupServer {
				return ""
			}
			return dialablePhone(ctx, c, chat)
		}(),
		ContactName: func() string {
			if chat.Server == types.GroupServer {
				return ""
			}
			return deviceContactName(ctx, c, chat)
		}(),
	})
	if err != nil {
		evt.OnError("encode_sent", err.Error())
		return
	}
	evt.OnMessage(string(payload))
}

func dispatch(ctx context.Context, c *whatsmeow.Client, evt Events, raw interface{}) {
	switch e := raw.(type) {
	case *events.Connected:
		evt.OnConnected()
	case *events.PairSuccess:
		evt.OnPairSuccess()
	case *events.LoggedOut:
		evt.OnLoggedOut()
	case *events.Message:
		msg, ok := toWaMessage(ctx, c, e)
		if !ok {
			return
		}
		payload, err := json.Marshal(msg)
		if err != nil {
			evt.OnError("encode_message", err.Error())
			return
		}
		evt.OnMessage(string(payload))
		if msg.ChatType == "group" && msg.ChatName == "" {
			requestGroupMetadataRefresh(ctx, c, e.Info.Chat)
		}
	case *events.HistorySync:
		dispatchHistory(ctx, c, evt, e)
	case *events.JoinedGroup:
		if metadata, ok := groupMetadataFromGroupInfo(
			&e.GroupInfo,
			"joined_group",
			time.Now(),
		); ok {
			emitGroupMetadata(evt, metadata)
		}
	case *events.GroupInfo:
		dispatchGroupInfoEvent(ctx, c, evt, e)
	}
}

// dispatchHistory streams a history blob into small, text-only direct/group batches. A
// full WhatsApp history blob can be large enough to duplicate memory several
// times when marshaled through Go -> Swift -> JS, so never accumulate the whole
// blob before emitting.
func dispatchHistory(ctx context.Context, c *whatsmeow.Client, evt Events, e *events.HistorySync) {
	if c == nil || e == nil || e.Data == nil {
		return
	}
	syncType := e.Data.GetSyncType().String()
	chunkOrder := e.Data.GetChunkOrder()
	progress := e.Data.GetProgress()
	storeHistoryMappings(ctx, c, e.Data.GetPhoneNumberToLidMappings())
	// WhatsApp ships counterparty display names (pushNames) in a dedicated
	// PUSH_NAME history-sync event — a payload that carries NO messages, keyed by
	// JID. Since the LID/privacy migration the per-message PushName is left blank
	// for history, so this event is the only place these names arrive. We build
	// two views of it: a JID->name map to fill any message in *this same* event
	// that happens to lack a name, and a slice we emit to the host out-of-band
	// below. The out-of-band emit is the important one: message-bearing events and
	// the PUSH_NAME event are separate, so without it the names would be dropped
	// entirely (the host skips empty-message history batches). Key by the same
	// canonical JID we emit as SenderJID so the backend can match a thread.
	pushnameByJID := make(map[string]string)
	pushNames := make([]waPushName, 0, len(e.Data.GetPushnames()))
	for _, pn := range e.Data.GetPushnames() {
		name := strings.TrimSpace(pn.GetPushname())
		if name == "" {
			continue
		}
		jid, err := types.ParseJID(pn.GetID())
		if err != nil {
			continue
		}
		canonical := canonicalJID(ctx, c, jid).String()
		if _, seen := pushnameByJID[canonical]; seen {
			continue
		}
		pushnameByJID[canonical] = name
		pushNames = append(pushNames, waPushName{
			JID:         canonical,
			Name:        name,
			PhoneNumber: dialablePhone(ctx, c, jid),
		})
	}
	batchIndex := uint32(0)
	batch := make([]waMessage, 0, historyBridgeBatchSize)
	emitBatch := func() {
		if len(batch) == 0 {
			return
		}
		payload, err := json.Marshal(historySyncPayload{
			Messages:   batch,
			SyncType:   syncType,
			ChunkOrder: chunkOrder,
			Progress:   progress,
			BatchIndex: batchIndex,
		})
		if err != nil {
			evt.OnError("encode_history", err.Error())
			return
		}
		evt.OnHistorySync(string(payload))
		batchIndex++
		batch = make([]waMessage, 0, historyBridgeBatchSize)
	}

	groupMetadata := make([]waGroupMetadata, 0)
	for _, conv := range e.Data.GetConversations() {
		if metadata, ok := groupMetadataFromHistory(conv, time.Now()); ok {
			cacheGroupName(metadata.GroupJID, metadata.DisplayName)
			groupMetadata = append(groupMetadata, metadata)
		}
	}
	for len(groupMetadata) > 0 {
		count := min(historyBridgeBatchSize, len(groupMetadata))
		groupBatch := groupMetadata[:count]
		groupMetadata = groupMetadata[count:]
		payload, err := json.Marshal(historySyncPayload{
			Messages:      []waMessage{},
			GroupMetadata: groupBatch,
			SyncType:      syncType,
			ChunkOrder:    chunkOrder,
			Progress:      progress,
			BatchIndex:    batchIndex,
		})
		if err != nil {
			evt.OnError("encode_history_groups", err.Error())
			break
		}
		evt.OnHistorySync(string(payload))
		batchIndex++
	}

	// Deliver counterparty names first, in their own payload. WhatsApp sends them
	// in a messages-less PUSH_NAME event, so this is normally the only thing that
	// event produces; emitting it lets the backend upgrade a thread still labeled
	// by phone number on the very first sync. The log line confirms WhatsApp
	// actually handed us names for these (LID) contacts.
	if len(pushNames) > 0 {
		c.Log.Infof("history sync: emitting %d counterparty pushnames (syncType=%s)", len(pushNames), syncType)
		payload, err := json.Marshal(historySyncPayload{
			// Empty (not nil) so the host always sees a messages array on a
			// names-only batch, keeping the bridge payload shape consistent.
			Messages:   []waMessage{},
			PushNames:  pushNames,
			SyncType:   syncType,
			ChunkOrder: chunkOrder,
			Progress:   progress,
			BatchIndex: batchIndex,
		})
		if err != nil {
			evt.OnError("encode_history_pushnames", err.Error())
		} else {
			evt.OnHistorySync(string(payload))
			batchIndex++
		}
	}

	for _, conv := range e.Data.GetConversations() {
		for _, histMsg := range conv.GetMessages() {
			chatJID, _ := types.ParseJID(conv.GetID())
			parsed, err := c.ParseWebMessage(chatJID, histMsg.GetMessage())
			if err != nil {
				continue
			}
			if msg, ok := toWaMessage(ctx, c, parsed); ok {
				// History messages arrive with an empty PushName; fill it from the
				// blob-level pushnames list so the counterparty is named from the
				// first sync rather than only once they next message live.
				if msg.PushName == "" {
					if name, found := pushnameByJID[msg.SenderJID]; found {
						msg.PushName = name
					}
				}
				batch = append(batch, msg)
				if len(batch) >= historyBridgeBatchSize {
					emitBatch()
				}
			}
		}
	}
	emitBatch()
}

func storeHistoryMappings(
	ctx context.Context,
	c *whatsmeow.Client,
	mappings []*waHistorySync.PhoneNumberToLIDMapping,
) {
	if c == nil || c.Store == nil || len(mappings) == 0 {
		return
	}
	pairs := make([]store.LIDMapping, 0, len(mappings))
	for _, mapping := range mappings {
		pn, pnErr := types.ParseJID(mapping.GetPnJID())
		lid, lidErr := types.ParseJID(mapping.GetLidJID())
		if pnErr != nil || lidErr != nil {
			continue
		}
		if pn.Server == types.LegacyUserServer {
			pn.Server = types.DefaultUserServer
		}
		if pn.Server != types.DefaultUserServer || lid.Server != types.HiddenUserServer {
			continue
		}
		pairs = append(pairs, store.LIDMapping{PN: pn.ToNonAD(), LID: lid.ToNonAD()})
	}
	if len(pairs) == 0 {
		return
	}
	if err := c.Store.LIDs.PutManyLIDMappings(ctx, pairs); err != nil {
		c.Log.Warnf("Failed to synchronously store %d history PN/LID mappings: %v", len(pairs), err)
	}
}

func groupMetadataFromHistory(
	conv *waHistorySync.Conversation,
	observedAt time.Time,
) (waGroupMetadata, bool) {
	if conv == nil {
		return waGroupMetadata{}, false
	}
	jid, err := types.ParseJID(conv.GetID())
	if err != nil || jid.Server != types.GroupServer || jid.User == "" || conv.GetIsParentGroup() {
		return waGroupMetadata{}, false
	}
	readOnly := conv.GetReadOnly()
	isParent := false
	isDefaultSubgroup := conv.GetIsDefaultSubgroup()
	isEphemeral := conv.GetEphemeralExpiration() > 0
	timer := conv.GetEphemeralExpiration()
	suspended := conv.GetSuspended()
	deleted := conv.GetTerminated()
	return waGroupMetadata{
		GroupJID:          jid.ToNonAD().String(),
		DisplayName:       firstNonEmpty(conv.GetName(), conv.GetDisplayName()),
		ObservedAtSecs:    observedAt.Unix(),
		Source:            "history",
		ReadOnly:          &readOnly,
		IsParent:          &isParent,
		ParentGroupJID:    conv.GetParentGroupID(),
		IsDefaultSubgroup: &isDefaultSubgroup,
		IsEphemeral:       &isEphemeral,
		DisappearingTimer: &timer,
		ParticipantCount:  len(conv.GetParticipant()),
		Suspended:         &suspended,
		Deleted:           &deleted,
	}, true
}

func groupMetadataFromGroupInfo(
	info *types.GroupInfo,
	source string,
	observedAt time.Time,
) (waGroupMetadata, bool) {
	if info == nil || info.JID.Server != types.GroupServer || info.JID.User == "" || info.IsParent {
		return waGroupMetadata{}, false
	}
	isParent := info.IsParent
	isDefaultSubgroup := info.IsDefaultSubGroup
	isAnnouncement := info.IsAnnounce
	isEphemeral := info.IsEphemeral
	timer := info.DisappearingTimer
	participantCount := info.ParticipantCount
	if participantCount == 0 {
		participantCount = len(info.Participants)
	}
	suspended := info.Suspended
	return waGroupMetadata{
		GroupJID:          info.JID.ToNonAD().String(),
		DisplayName:       strings.TrimSpace(info.Name),
		ObservedAtSecs:    observedAt.Unix(),
		Source:            source,
		IsParent:          &isParent,
		ParentGroupJID:    nonADJIDString(info.LinkedParentJID),
		IsDefaultSubgroup: &isDefaultSubgroup,
		IsAnnouncement:    &isAnnouncement,
		IsEphemeral:       &isEphemeral,
		DisappearingTimer: &timer,
		ParticipantCount:  participantCount,
		Suspended:         &suspended,
	}, true
}

func dispatchGroupInfoEvent(
	ctx context.Context,
	c *whatsmeow.Client,
	evt Events,
	event *events.GroupInfo,
) {
	if event == nil || event.JID.Server != types.GroupServer || event.JID.User == "" {
		return
	}
	if event.Delete != nil && event.Delete.Deleted {
		deleted := true
		emitGroupMetadata(evt, waGroupMetadata{
			GroupJID:       event.JID.ToNonAD().String(),
			ObservedAtSecs: event.Timestamp.Unix(),
			Source:         "group_info_event",
			Deleted:        &deleted,
		})
		return
	}
	requestGroupMetadataRefresh(ctx, c, event.JID)
}

func requestGroupMetadataRefresh(ctx context.Context, c *whatsmeow.Client, jid types.JID) {
	jid = jid.ToNonAD()
	if c == nil || jid.Server != types.GroupServer || jid.User == "" {
		return
	}
	key := jid.String()
	groupStateMu.Lock()
	if _, exists := groupRefreshInFlight[key]; exists {
		groupStateMu.Unlock()
		return
	}
	groupRefreshInFlight[key] = struct{}{}
	groupStateMu.Unlock()

	go func() {
		defer func() {
			groupStateMu.Lock()
			delete(groupRefreshInFlight, key)
			groupStateMu.Unlock()
		}()
		info, err := c.GetGroupInfo(ctx, jid)
		if err != nil {
			c.Log.Warnf("Failed to refresh group metadata for %s: %v", jid, err)
			return
		}
		metadata, ok := groupMetadataFromGroupInfo(info, "live_refresh", time.Now())
		if !ok {
			return
		}
		if evt := getEvt(); evt != nil {
			emitGroupMetadata(evt, metadata)
		}
	}()
}

func emitGroupMetadata(evt Events, metadata waGroupMetadata) {
	cacheGroupName(metadata.GroupJID, metadata.DisplayName)
	payload, err := json.Marshal(metadata)
	if err != nil {
		evt.OnError("encode_group_info", err.Error())
		return
	}
	evt.OnGroupInfo(string(payload))
}

func cacheGroupName(groupJID string, displayName string) {
	displayName = strings.TrimSpace(displayName)
	if groupJID == "" || displayName == "" {
		return
	}
	groupStateMu.Lock()
	groupNameByJID[groupJID] = displayName
	groupStateMu.Unlock()
}

func cachedGroupName(jid types.JID) string {
	groupStateMu.Lock()
	defer groupStateMu.Unlock()
	return groupNameByJID[jid.ToNonAD().String()]
}
