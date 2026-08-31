package wa

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func makeTextEvent(info types.MessageInfo, text string) *events.Message {
	return &events.Message{
		Info:    info,
		Message: &waE2E.Message{Conversation: proto.String(text)},
	}
}

func resetGroupStateForTest() {
	groupStateMu.Lock()
	clear(groupNameByJID)
	clear(groupRefreshInFlight)
	groupStateMu.Unlock()
}

// The own-message fallback must recognize every own-identity form the device can
// resolve (own LID via the LID DB, phone-form alternates, resolved phone) without
// ever claiming someone else's message.
func TestSenderMatchesOwnIdentity(t *testing.T) {
	ownPN := types.NewJID("15551234567", types.DefaultUserServer)
	ownLID := types.NewJID("241000000000001", types.HiddenUserServer)
	otherLID := types.NewJID("352000000000123", types.HiddenUserServer)
	otherPN := types.NewJID("15559998888", types.DefaultUserServer)
	empty := types.JID{}

	cases := []struct {
		name         string
		sender       types.JID
		senderAlt    types.JID
		senderPhone  string
		ownPN        types.JID
		ownCanonical types.JID
		want         bool
	}{
		{"own LID sender via canonical own JID", ownLID, empty, "", ownPN, ownLID, true},
		{"own LID sender with no canonical mapping", ownLID, empty, "", ownPN, ownPN, false},
		{"phone-form alternate names the own account", otherLID, ownPN, "", ownPN, ownPN, true},
		{"resolved phone names the own account", ownLID, empty, "15551234567", ownPN, ownPN, true},
		{"someone else's LID and phone", otherLID, otherPN, "15559998888", ownPN, ownLID, false},
		{"unpaired device (empty own JID) never matches", ownLID, empty, "15551234567", empty, empty, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := senderMatchesOwnIdentity(tc.sender, tc.senderAlt, tc.senderPhone, tc.ownPN, tc.ownCanonical)
			if got != tc.want {
				t.Fatalf("senderMatchesOwnIdentity(%s, %s, %q) = %v, want %v",
					tc.sender, tc.senderAlt, tc.senderPhone, got, tc.want)
			}
		})
	}
}

// Without a client (no Store), toWaMessage must pass the upstream IsFromMe bit
// through unchanged in both directions.
func TestToWaMessagePreservesIsFromMeWithoutStore(t *testing.T) {
	resetGroupStateForTest()
	group := types.NewJID("120363000000000009", types.GroupServer)
	senderLID := types.NewJID("241000000000001", types.HiddenUserServer)

	for _, isFromMe := range []bool{true, false} {
		event := makeTextEvent(types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat:           group,
				Sender:         senderLID,
				IsGroup:        true,
				IsFromMe:       isFromMe,
				AddressingMode: types.AddressingModeLID,
			},
			ID:        "own-bit-passthrough",
			Timestamp: time.Unix(1_700_000_050, 0),
		}, "own bit")

		message, ok := toWaMessage(context.Background(), nil, event)
		if !ok {
			t.Fatal("expected group text to be accepted")
		}
		if message.IsFromMe != isFromMe {
			t.Fatalf("IsFromMe = %v, want %v", message.IsFromMe, isFromMe)
		}
	}
}

func TestToWaMessageMapsGroupSenderSeparatelyFromChat(t *testing.T) {
	resetGroupStateForTest()
	group := types.NewJID("120363000000000000", types.GroupServer)
	senderLID := types.NewJID("987654321", types.HiddenUserServer)
	senderPN := types.NewJID("15551234567", types.DefaultUserServer)
	cacheGroupName(group.String(), "Weekend Plans")

	event := makeTextEvent(types.MessageInfo{
		MessageSource: types.MessageSource{
			Chat:           group,
			Sender:         senderLID,
			SenderAlt:      senderPN,
			IsGroup:        true,
			IsFromMe:       false,
			AddressingMode: types.AddressingModeLID,
		},
		ID:        "group-message-1",
		Timestamp: time.Unix(1_700_000_000, 0),
		PushName:  "Alice",
	}, "see you at eight")

	message, ok := toWaMessage(context.Background(), nil, event)
	if !ok {
		t.Fatal("expected ordinary group text to be accepted")
	}
	if message.ChatType != "group" {
		t.Fatalf("expected group chat type, got %q", message.ChatType)
	}
	if message.ChatJID != group.String() {
		t.Fatalf("expected group chat JID %q, got %q", group, message.ChatJID)
	}
	if message.SenderJID != senderLID.String() {
		t.Fatalf("expected canonical LID sender %q, got %q", senderLID, message.SenderJID)
	}
	if message.SenderAltJID != senderPN.String() {
		t.Fatalf("expected sender alternate %q, got %q", senderPN, message.SenderAltJID)
	}
	if message.SenderPhoneNumber != senderPN.User {
		t.Fatalf("expected sender phone %q, got %q", senderPN.User, message.SenderPhoneNumber)
	}
	if message.ChatPhoneNumber != "" {
		t.Fatalf("group JID must not become a phone number, got %q", message.ChatPhoneNumber)
	}
	if message.ChatName != "Weekend Plans" {
		t.Fatalf("expected cached group title, got %q", message.ChatName)
	}
	if message.AddressingMode != string(types.AddressingModeLID) {
		t.Fatalf("expected LID addressing mode, got %q", message.AddressingMode)
	}
}

func TestToWaMessagePreservesDirectBehavior(t *testing.T) {
	friend := types.NewJID("15557654321", types.DefaultUserServer)
	event := makeTextEvent(types.MessageInfo{
		MessageSource: types.MessageSource{
			Chat:     friend,
			Sender:   friend,
			IsGroup:  false,
			IsFromMe: false,
		},
		ID:        "direct-message-1",
		Timestamp: time.Unix(1_700_000_001, 0),
	}, "hello")

	message, ok := toWaMessage(context.Background(), nil, event)
	if !ok {
		t.Fatal("expected direct text to remain accepted")
	}
	if message.ChatType != "direct" {
		t.Fatalf("expected direct chat type, got %q", message.ChatType)
	}
	if message.ChatPhoneNumber != friend.User || message.SenderPhoneNumber != friend.User {
		t.Fatalf(
			"expected direct phone enrichment %q, got chat=%q sender=%q",
			friend.User,
			message.ChatPhoneNumber,
			message.SenderPhoneNumber,
		)
	}
}

func TestToWaMessageRejectsUnsupportedAndExpiringGroupContent(t *testing.T) {
	group := types.NewJID("120363000000000001", types.GroupServer)
	sender := types.NewJID("15551230000", types.DefaultUserServer)
	baseInfo := types.MessageInfo{
		MessageSource: types.MessageSource{
			Chat:     group,
			Sender:   sender,
			IsGroup:  true,
			IsFromMe: false,
		},
		ID:        "message-1",
		Timestamp: time.Unix(1_700_000_002, 0),
	}

	ephemeral := makeTextEvent(baseInfo, "temporary")
	ephemeral.IsEphemeral = true
	if _, ok := toWaMessage(context.Background(), nil, ephemeral); ok {
		t.Fatal("expected ephemeral group text to be rejected")
	}

	viewOnce := makeTextEvent(baseInfo, "temporary")
	viewOnce.IsViewOnce = true
	if _, ok := toWaMessage(context.Background(), nil, viewOnce); ok {
		t.Fatal("expected view-once group text to be rejected")
	}

	edited := makeTextEvent(baseInfo, "edited")
	edited.IsEdit = true
	if _, ok := toWaMessage(context.Background(), nil, edited); ok {
		t.Fatal("expected edited group text to be rejected until edits are modeled")
	}

	unsupportedChats := []types.JID{
		types.StatusBroadcastJID,
		types.NewJID("12345", types.BroadcastServer),
		types.NewJID("12345", types.NewsletterServer),
	}
	for _, chat := range unsupportedChats {
		event := makeTextEvent(types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat:    chat,
				Sender:  sender,
				IsGroup: chat.Server == types.BroadcastServer,
			},
			ID:        "unsupported",
			Timestamp: time.Unix(1_700_000_003, 0),
		}, "do not ingest")
		if _, ok := toWaMessage(context.Background(), nil, event); ok {
			t.Fatalf("expected unsupported chat %q to be rejected", chat)
		}
	}
}

func makeReactionEvent(info types.MessageInfo, targetID string, emoji string) *events.Message {
	return &events.Message{
		Info: info,
		Message: &waE2E.Message{
			ReactionMessage: &waE2E.ReactionMessage{
				Key: &waCommon.MessageKey{
					RemoteJID: proto.String(info.Chat.String()),
					ID:        proto.String(targetID),
				},
				Text: proto.String(emoji),
			},
		},
	}
}

// Reactions ship as kind "reaction" with the emoji in Text and the reacted-to
// message's wire ID in TargetMessageID; an empty emoji (reaction removed) is
// still admitted, and a reaction without a target is dropped.
func TestToWaMessageExtractsReactions(t *testing.T) {
	friend := types.NewJID("15557654321", types.DefaultUserServer)
	info := types.MessageInfo{
		MessageSource: types.MessageSource{
			Chat:     friend,
			Sender:   friend,
			IsGroup:  false,
			IsFromMe: false,
		},
		ID:        "reaction-1",
		Timestamp: time.Unix(1_700_000_010, 0),
		PushName:  "Alice",
	}

	message, ok := toWaMessage(context.Background(), nil, makeReactionEvent(info, "target-42", "❤️"))
	if !ok {
		t.Fatal("expected reaction to be accepted")
	}
	if message.Kind != "reaction" {
		t.Fatalf("expected kind reaction, got %q", message.Kind)
	}
	if message.TargetMessageID != "target-42" {
		t.Fatalf("expected target message id, got %q", message.TargetMessageID)
	}
	if message.Text != "❤️" {
		t.Fatalf("expected emoji text, got %q", message.Text)
	}
	if message.MessageID != "reaction-1" {
		t.Fatalf("reaction must keep its own wire id, got %q", message.MessageID)
	}

	removal, ok := toWaMessage(context.Background(), nil, makeReactionEvent(info, "target-42", ""))
	if !ok {
		t.Fatal("expected reaction removal (empty emoji) to be accepted")
	}
	if removal.Kind != "reaction" || removal.Text != "" {
		t.Fatalf("expected empty-text reaction removal, got kind=%q text=%q", removal.Kind, removal.Text)
	}

	if _, ok := toWaMessage(context.Background(), nil, makeReactionEvent(info, "", "❤️")); ok {
		t.Fatal("expected reaction without a target id to be dropped")
	}
}

// The bare-media drop only relaxes when image bytes actually came along:
// history semantics (no media download) still drop a captionless photo.
func TestToWaMessageStillDropsCaptionlessImageWithoutBytes(t *testing.T) {
	friend := types.NewJID("15557654321", types.DefaultUserServer)
	event := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat:     friend,
				Sender:   friend,
				IsGroup:  false,
				IsFromMe: false,
			},
			ID:        "bare-photo-1",
			Timestamp: time.Unix(1_700_000_011, 0),
		},
		Message: &waE2E.Message{
			ImageMessage: &waE2E.ImageMessage{Mimetype: proto.String("image/jpeg")},
		},
	}
	if _, ok := toWaMessage(context.Background(), nil, event); ok {
		t.Fatal("expected captionless image without downloaded bytes to be dropped")
	}
	// The live path degrades identically when the download yields nothing
	// (nil client here, so downloadInboundImage returns empty).
	if _, ok := toWaMessageOpts(context.Background(), nil, event, true); ok {
		t.Fatal("expected captionless image with failed download to be dropped")
	}
}

func TestCanonicalParticipantPrefersExplicitLIDAlternate(t *testing.T) {
	pn := types.NewJID("15551234567", types.DefaultUserServer)
	lid := types.NewJID("777777777", types.HiddenUserServer)
	got := canonicalParticipantJID(context.Background(), nil, pn, lid)
	if got != lid {
		t.Fatalf("expected explicit LID %q, got %q", lid, got)
	}
	if phone := dialablePhoneFromPair(context.Background(), nil, lid, pn); phone != pn.User {
		t.Fatalf("expected alternate PN %q, got %q", pn.User, phone)
	}
}

func TestGroupMetadataFromHistory(t *testing.T) {
	readOnly := true
	isDefaultSubgroup := true
	ephemeralSeconds := uint32(86400)
	suspended := false
	terminated := false
	conversation := &waHistorySync.Conversation{
		ID:                  proto.String("120363000000000002@g.us"),
		Name:                proto.String("Family"),
		ReadOnly:            &readOnly,
		IsDefaultSubgroup:   &isDefaultSubgroup,
		ParentGroupID:       proto.String("120363000000000003@g.us"),
		EphemeralExpiration: &ephemeralSeconds,
		Suspended:           &suspended,
		Terminated:          &terminated,
	}
	observedAt := time.Unix(1_700_000_004, 0)

	metadata, ok := groupMetadataFromHistory(conversation, observedAt)
	if !ok {
		t.Fatal("expected message-bearing group history metadata")
	}
	if metadata.GroupJID != conversation.GetID() || metadata.DisplayName != "Family" {
		t.Fatalf("unexpected group identity: %#v", metadata)
	}
	if metadata.ReadOnly == nil || !*metadata.ReadOnly {
		t.Fatal("expected read-only metadata")
	}
	if metadata.IsEphemeral == nil || !*metadata.IsEphemeral {
		t.Fatal("expected ephemeral metadata")
	}
	if metadata.DisappearingTimer == nil || *metadata.DisappearingTimer != ephemeralSeconds {
		t.Fatalf("unexpected disappearing timer: %#v", metadata.DisappearingTimer)
	}

	isParent := true
	conversation.IsParentGroup = &isParent
	if _, ok := groupMetadataFromHistory(conversation, observedAt); ok {
		t.Fatal("community parent containers must not be emitted as group chats")
	}
}

func TestGroupMetadataFromGroupInfo(t *testing.T) {
	info := &types.GroupInfo{
		JID: types.NewJID("120363000000000004", types.GroupServer),
		GroupName: types.GroupName{
			Name: "Friends",
		},
		GroupAnnounce: types.GroupAnnounce{
			IsAnnounce: true,
		},
		GroupEphemeral: types.GroupEphemeral{
			IsEphemeral:       true,
			DisappearingTimer: 3600,
		},
		GroupIsDefaultSub: types.GroupIsDefaultSub{
			IsDefaultSubGroup: true,
		},
		GroupLinkedParent: types.GroupLinkedParent{
			LinkedParentJID: types.NewJID("120363000000000005", types.GroupServer),
		},
	}
	metadata, ok := groupMetadataFromGroupInfo(info, "test", time.Unix(1_700_000_005, 0))
	if !ok {
		t.Fatal("expected ordinary group info")
	}
	if metadata.DisplayName != "Friends" || metadata.IsAnnouncement == nil || !*metadata.IsAnnouncement {
		t.Fatalf("unexpected metadata: %#v", metadata)
	}

	info.IsParent = true
	if _, ok := groupMetadataFromGroupInfo(info, "test", time.Now()); ok {
		t.Fatal("community parent containers must not be emitted")
	}
}

func TestCapabilitiesAdvertiseOnlyImplementedFeatures(t *testing.T) {
	var capabilities wrapperCapabilities
	if err := json.Unmarshal([]byte(Capabilities()), &capabilities); err != nil {
		t.Fatalf("capabilities must be valid JSON: %v", err)
	}
	if capabilities.SchemaVersion != wrapperSchemaVersion {
		t.Fatalf(
			"expected schema version %d, got %d",
			wrapperSchemaVersion,
			capabilities.SchemaVersion,
		)
	}
	expected := []string{
		"group_metadata_v1",
		"groups_v1",
		"inbound_media_image_v1",
		"list_groups_v1",
		"media_caption_v1",
		"reactions_v1",
		"reply_context_v1",
		"stable_send_id",
	}
	if strings.Join(capabilities.Features, ",") != strings.Join(expected, ",") {
		t.Fatalf("unexpected capabilities: %#v", capabilities.Features)
	}
}

func TestEncodeGroupListIsMetadataOnlyAndDeterministic(t *testing.T) {
	parent := &types.GroupInfo{
		JID:         types.NewJID("120363000000000009", types.GroupServer),
		GroupParent: types.GroupParent{IsParent: true},
		GroupName:   types.GroupName{Name: "Community container"},
	}
	second := &types.GroupInfo{
		JID:              types.NewJID("120363000000000008", types.GroupServer),
		GroupName:        types.GroupName{Name: "Second"},
		ParticipantCount: 8,
		Participants: []types.GroupParticipant{
			{JID: types.NewJID("15550000001", types.DefaultUserServer)},
		},
	}
	first := &types.GroupInfo{
		JID:       types.NewJID("120363000000000007", types.GroupServer),
		GroupName: types.GroupName{Name: "First"},
		Participants: []types.GroupParticipant{
			{JID: types.NewJID("15550000002", types.DefaultUserServer)},
			{JID: types.NewJID("15550000003", types.DefaultUserServer)},
		},
	}

	payload, err := encodeGroupList(
		[]*types.GroupInfo{second, parent, first},
		time.Unix(1_700_000_006, 0),
	)
	if err != nil {
		t.Fatalf("encode group list: %v", err)
	}
	var decoded groupListPayload
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("decode group list: %v", err)
	}
	if len(decoded.Groups) != 2 {
		t.Fatalf("expected two message-bearing groups, got %d", len(decoded.Groups))
	}
	if decoded.Groups[0].DisplayName != "First" || decoded.Groups[1].DisplayName != "Second" {
		t.Fatalf("groups must be sorted by JID: %#v", decoded.Groups)
	}
	if decoded.Groups[0].ParticipantCount != 2 {
		t.Fatalf("expected participant count fallback, got %d", decoded.Groups[0].ParticipantCount)
	}
	if decoded.Groups[1].ParticipantCount != 8 {
		t.Fatalf("expected explicit participant count, got %d", decoded.Groups[1].ParticipantCount)
	}

	var raw struct {
		Groups []map[string]any `json:"groups"`
	}
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		t.Fatalf("decode raw group list: %v", err)
	}
	for _, group := range raw.Groups {
		if _, leakedRoster := group["participants"]; leakedRoster {
			t.Fatal("group list must not expose participant rosters")
		}
	}
}

func TestResolveSendJIDAllowlist(t *testing.T) {
	ctx := context.Background()
	allowed := []string{
		"15551234567",
		"15551234567@s.whatsapp.net",
		"777777777@lid",
		"120363000000000006@g.us",
	}
	for _, recipient := range allowed {
		if _, err := resolveSendJID(ctx, nil, recipient); err != nil {
			t.Fatalf("expected %q to be allowed: %v", recipient, err)
		}
	}

	rejected := []string{
		"status@broadcast",
		"12345@broadcast",
		"12345@newsletter",
		"12345@bot",
	}
	for _, recipient := range rejected {
		if _, err := resolveSendJID(ctx, nil, recipient); err == nil {
			t.Fatalf("expected %q to be rejected", recipient)
		}
	}
}

func TestValidateMessageID(t *testing.T) {
	if err := validateMessageID("3EB0AABBCCDDEEFF"); err != nil {
		t.Fatalf("expected stable message ID to be accepted: %v", err)
	}
	for _, value := range []string{"", " leading", "trailing ", "has\nnewline", strings.Repeat("x", 129)} {
		if err := validateMessageID(value); err == nil {
			t.Fatalf("expected invalid message ID %q to be rejected", value)
		}
	}
}

func makeExtendedTextEvent(info types.MessageInfo, text string, mentionedJIDs []string) *events.Message {
	return &events.Message{
		Info: info,
		Message: &waE2E.Message{
			ExtendedTextMessage: &waE2E.ExtendedTextMessage{
				Text:        proto.String(text),
				ContextInfo: &waE2E.ContextInfo{MentionedJID: mentionedJIDs},
			},
		},
	}
}

// fakeContactStore serves GetContact from a map; every other ContactStore
// method panics via the embedded nil interface, which no tested path calls.
type fakeContactStore struct {
	store.ContactStore
	contacts map[string]types.ContactInfo
}

func (f *fakeContactStore) GetContact(_ context.Context, user types.JID) (types.ContactInfo, error) {
	return f.contacts[user.String()], nil
}

// fakeLIDStore serves the two lookup methods from maps (zero JID = no
// mapping); every other LIDStore method panics via the embedded nil interface.
type fakeLIDStore struct {
	store.LIDStore
	pnByLID map[string]types.JID
	lidByPN map[string]types.JID
}

func (f *fakeLIDStore) GetPNForLID(_ context.Context, lid types.JID) (types.JID, error) {
	return f.pnByLID[lid.String()], nil
}

func (f *fakeLIDStore) GetLIDForPN(_ context.Context, pn types.JID) (types.JID, error) {
	return f.lidByPN[pn.String()], nil
}

func makeMentionResolutionClient(
	ownJID *types.JID,
	ownPushName string,
	contacts map[string]types.ContactInfo,
) *whatsmeow.Client {
	return &whatsmeow.Client{Store: &store.Device{
		ID:       ownJID,
		PushName: ownPushName,
		Contacts: &fakeContactStore{contacts: contacts},
		LIDs:     &fakeLIDStore{},
	}}
}

// Mention tokens must surface as names when the device knows them: the wire
// text only carries "@<jid user part>", which is meaningless to everything
// downstream of the device.
func TestResolveMentionTokensResolvesKnownNames(t *testing.T) {
	ctx := context.Background()
	bolajiLID := types.NewJID("248506975531090", types.HiddenUserServer)
	andrewLID := types.NewJID("170090066657309", types.HiddenUserServer)
	client := makeMentionResolutionClient(nil, "", map[string]types.ContactInfo{
		// Known only by WhatsApp display name (pushName).
		bolajiLID.String(): {Found: true, PushName: "Bolaji"},
		// Saved in the address book, which wins over pushName.
		andrewLID.String(): {Found: true, FullName: "Andrew Zhang", PushName: "andz"},
	})

	msg := &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String("@248506975531090, for @170090066657309, any housing groups in tech treks?"),
			ContextInfo: &waE2E.ContextInfo{
				MentionedJID: []string{bolajiLID.String(), andrewLID.String()},
			},
		},
	}
	got := resolveMentionTokens(ctx, client, msg, messageText(msg))
	want := "@Bolaji, for @Andrew Zhang, any housing groups in tech treks?"
	if got != want {
		t.Fatalf("resolved text = %q, want %q", got, want)
	}
}

// A mention the device cannot name must stay raw instead of degrading the text.
func TestResolveMentionTokensLeavesUnknownMentionsRaw(t *testing.T) {
	ctx := context.Background()
	client := makeMentionResolutionClient(nil, "", map[string]types.ContactInfo{})
	msg := &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text:        proto.String("ping @999000111222333"),
			ContextInfo: &waE2E.ContextInfo{MentionedJID: []string{"999000111222333@lid"}},
		},
	}
	if got := resolveMentionTokens(ctx, client, msg, messageText(msg)); got != "ping @999000111222333" {
		t.Fatalf("unknown mention must stay raw, got %q", got)
	}
}

// A mention of the owner must resolve to the owner's own display name — it is
// the strongest "this message is addressed to the user" signal downstream.
func TestResolveMentionTokensResolvesOwnerMention(t *testing.T) {
	ctx := context.Background()
	ownPN := types.NewJID("15551234567", types.DefaultUserServer)
	client := makeMentionResolutionClient(&ownPN, "Benji", map[string]types.ContactInfo{})
	msg := &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text:        proto.String("@15551234567 you seeing this?"),
			ContextInfo: &waE2E.ContextInfo{MentionedJID: []string{ownPN.String()}},
		},
	}
	if got := resolveMentionTokens(ctx, client, msg, messageText(msg)); got != "@Benji you seeing this?" {
		t.Fatalf("owner mention must use own push name, got %q", got)
	}
}

// Replacing a shorter token first would corrupt a longer token sharing its
// prefix, so resolution must go longest-first.
func TestResolveMentionTokensHandlesPrefixCollisions(t *testing.T) {
	ctx := context.Background()
	shortPN := types.NewJID("1234", types.DefaultUserServer)
	longPN := types.NewJID("12345", types.DefaultUserServer)
	client := makeMentionResolutionClient(nil, "", map[string]types.ContactInfo{
		shortPN.String(): {Found: true, FullName: "Short Num"},
		longPN.String():  {Found: true, FullName: "Long Num"},
	})
	msg := &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text:        proto.String("hey @12345 and @1234"),
			ContextInfo: &waE2E.ContextInfo{MentionedJID: []string{shortPN.String(), longPN.String()}},
		},
	}
	got := resolveMentionTokens(ctx, client, msg, messageText(msg))
	if got != "hey @Long Num and @Short Num" {
		t.Fatalf("prefix collision mishandled: %q", got)
	}
}

// Without a client there is nothing to resolve against; the text must pass
// through untouched (in particular, a phone token must not be "replaced" with
// the same digits).
func TestResolveMentionTokensWithoutClient(t *testing.T) {
	ctx := context.Background()
	for _, text := range []string{"@248506975531090 hello", "@15551234567 hello"} {
		msg := &waE2E.Message{
			ExtendedTextMessage: &waE2E.ExtendedTextMessage{
				Text: proto.String(text),
				ContextInfo: &waE2E.ContextInfo{
					MentionedJID: []string{"248506975531090@lid", "15551234567@s.whatsapp.net"},
				},
			},
		}
		if got := resolveMentionTokens(ctx, nil, msg, messageText(msg)); got != text {
			t.Fatalf("nil-client resolution must be a no-op, got %q from %q", got, text)
		}
	}
}

// A reply must ship who/what it quotes: the stanza ID (joinable against the
// stored original), the quoted sender, and the inline snippet the protocol
// carries for rendering the quote box.
func TestToWaMessageExtractsQuoteContext(t *testing.T) {
	resetGroupStateForTest()
	group := types.NewJID("120363000000000011", types.GroupServer)
	senderLID := types.NewJID("352000000000123", types.HiddenUserServer)
	quotedPN := types.NewJID("15557654321", types.DefaultUserServer)
	client := makeMentionResolutionClient(nil, "", map[string]types.ContactInfo{
		quotedPN.String(): {Found: true, FullName: "Alice Doe"},
	})

	event := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat:    group,
				Sender:  senderLID,
				IsGroup: true,
			},
			ID:        "reply-message-1",
			Timestamp: time.Unix(1_700_000_070, 0),
		},
		Message: &waE2E.Message{
			ExtendedTextMessage: &waE2E.ExtendedTextMessage{
				Text: proto.String("yeah let's do it"),
				ContextInfo: &waE2E.ContextInfo{
					StanzaID:    proto.String("original-message-9"),
					Participant: proto.String(quotedPN.String()),
					QuotedMessage: &waE2E.Message{
						Conversation: proto.String("dinner at 8 on\nfriday?"),
					},
				},
			},
		},
	}

	message, ok := toWaMessage(context.Background(), client, event)
	if !ok {
		t.Fatal("expected reply text to be accepted")
	}
	if message.Text != "yeah let's do it" {
		t.Fatalf("reply body must stay the message text, got %q", message.Text)
	}
	if message.QuotedMessageID != "original-message-9" {
		t.Fatalf("expected quoted stanza ID, got %q", message.QuotedMessageID)
	}
	if message.QuotedSenderJID != quotedPN.String() {
		t.Fatalf("expected quoted sender %q, got %q", quotedPN, message.QuotedSenderJID)
	}
	if message.QuotedSenderName != "Alice Doe" {
		t.Fatalf("expected contact-resolved quoted sender name, got %q", message.QuotedSenderName)
	}
	if message.QuotedText != "dinner at 8 on friday?" {
		t.Fatalf("expected single-line quoted snippet, got %q", message.QuotedText)
	}
	if message.QuotedIsFromMe {
		t.Fatal("someone else's quoted message must not be marked own")
	}
}

// Quoting the owner's own message is the "someone replied to the user" signal;
// the device is the only place that can resolve it reliably.
func TestExtractQuoteContextMarksOwnQuotedMessage(t *testing.T) {
	ctx := context.Background()
	ownPN := types.NewJID("15551234567", types.DefaultUserServer)
	client := makeMentionResolutionClient(&ownPN, "Benji", map[string]types.ContactInfo{})

	msg := &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String("sounds good"),
			ContextInfo: &waE2E.ContextInfo{
				StanzaID:      proto.String("owners-message-1"),
				Participant:   proto.String(ownPN.String()),
				QuotedMessage: &waE2E.Message{Conversation: proto.String("free tonight?")},
			},
		},
	}
	quote := extractQuoteContext(ctx, client, msg)
	if !quote.isFromMe {
		t.Fatal("quote of the owner's message must be marked own")
	}
	if quote.senderName != "Benji" {
		t.Fatalf("owner quote must resolve to own push name, got %q", quote.senderName)
	}
}

// A message with no ContextInfo (or context without a quote) is not a reply.
func TestExtractQuoteContextIgnoresNonReplies(t *testing.T) {
	ctx := context.Background()
	plain := &waE2E.Message{Conversation: proto.String("hello")}
	if quote := extractQuoteContext(ctx, nil, plain); quote != (quoteContext{}) {
		t.Fatalf("plain message must not produce quote context: %#v", quote)
	}
	mentionOnly := &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text:        proto.String("@15551234567 hi"),
			ContextInfo: &waE2E.ContextInfo{MentionedJID: []string{"15551234567@s.whatsapp.net"}},
		},
	}
	if quote := extractQuoteContext(ctx, nil, mentionOnly); quote != (quoteContext{}) {
		t.Fatalf("mention-only context must not produce quote context: %#v", quote)
	}
}

// Media quotes never made it into the text-only pipeline, so their snippet is
// the caption when there is one and a bracketed kind marker otherwise.
func TestQuotedMessageSnippetForMediaAndLength(t *testing.T) {
	ctx := context.Background()
	captioned := &waE2E.Message{
		ImageMessage: &waE2E.ImageMessage{Caption: proto.String("us at the beach")},
	}
	if got := quotedMessageSnippet(ctx, nil, captioned); got != "us at the beach" {
		t.Fatalf("captioned photo snippet = %q", got)
	}
	bare := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{}}
	if got := quotedMessageSnippet(ctx, nil, bare); got != "[photo]" {
		t.Fatalf("bare photo snippet = %q", got)
	}
	voice := &waE2E.Message{AudioMessage: &waE2E.AudioMessage{PTT: proto.Bool(true)}}
	if got := quotedMessageSnippet(ctx, nil, voice); got != "[voice message]" {
		t.Fatalf("voice snippet = %q", got)
	}
	ephemeralText := &waE2E.Message{
		EphemeralMessage: &waE2E.FutureProofMessage{
			Message: &waE2E.Message{Conversation: proto.String("wrapped text")},
		},
	}
	if got := quotedMessageSnippet(ctx, nil, ephemeralText); got != "wrapped text" {
		t.Fatalf("ephemeral-wrapped snippet = %q", got)
	}
	long := &waE2E.Message{Conversation: proto.String(strings.Repeat("a", 400))}
	got := quotedMessageSnippet(ctx, nil, long)
	if len([]rune(got)) != quotedSnippetMaxRunes+1 || !strings.HasSuffix(got, "…") {
		t.Fatalf("long snippet must truncate to %d runes + ellipsis, got %d runes", quotedSnippetMaxRunes, len([]rune(got)))
	}
}

// Captioned media ships its caption as the message text with the media kind
// alongside; captionless media stays dropped (the caption is the retained
// context, the bytes are out of scope).
func TestToWaMessageIngestsMediaCaptions(t *testing.T) {
	resetGroupStateForTest()
	friend := types.NewJID("15557654321", types.DefaultUserServer)
	directInfo := types.MessageInfo{
		MessageSource: types.MessageSource{
			Chat:     friend,
			Sender:   friend,
			IsGroup:  false,
			IsFromMe: false,
		},
		ID:        "media-caption-1",
		Timestamp: time.Unix(1_700_000_080, 0),
	}

	cases := []struct {
		name          string
		message       *waE2E.Message
		wantText      string
		wantMediaType string
	}{
		{
			"captioned photo",
			&waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("us at the beach")}},
			"us at the beach",
			"image",
		},
		{
			"captioned video",
			&waE2E.Message{VideoMessage: &waE2E.VideoMessage{Caption: proto.String("wait for it")}},
			"wait for it",
			"video",
		},
		{
			"captioned gif",
			&waE2E.Message{VideoMessage: &waE2E.VideoMessage{
				Caption:     proto.String("mood"),
				GifPlayback: proto.Bool(true),
			}},
			"mood",
			"gif",
		},
		{
			"captioned document",
			&waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{Caption: proto.String("signed lease")}},
			"signed lease",
			"document",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			event := &events.Message{Info: directInfo, Message: tc.message}
			message, ok := toWaMessage(context.Background(), nil, event)
			if !ok {
				t.Fatal("expected captioned media to be accepted")
			}
			if message.Text != tc.wantText {
				t.Fatalf("Text = %q, want %q", message.Text, tc.wantText)
			}
			if message.MediaType != tc.wantMediaType {
				t.Fatalf("MediaType = %q, want %q", message.MediaType, tc.wantMediaType)
			}
		})
	}

	// Plain text must not gain a media type.
	plain, ok := toWaMessage(context.Background(), nil, makeTextEvent(directInfo, "hello"))
	if !ok || plain.MediaType != "" {
		t.Fatalf("plain text must have no media type, got %q (ok=%v)", plain.MediaType, ok)
	}

	dropped := []struct {
		name  string
		event *events.Message
	}{
		{
			"captionless photo",
			&events.Message{Info: directInfo, Message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{}}},
		},
		{
			"sticker",
			&events.Message{Info: directInfo, Message: &waE2E.Message{StickerMessage: &waE2E.StickerMessage{}}},
		},
		{
			"voice note",
			&events.Message{Info: directInfo, Message: &waE2E.Message{AudioMessage: &waE2E.AudioMessage{PTT: proto.Bool(true)}}},
		},
	}
	for _, tc := range dropped {
		if _, ok := toWaMessage(context.Background(), nil, tc.event); ok {
			t.Fatalf("expected %s to be dropped", tc.name)
		}
	}
}

// View-once content is designed to disappear after one viewing; its caption
// must never be retained, in direct chats as well as groups.
func TestToWaMessageRejectsViewOnceCaptionedMedia(t *testing.T) {
	friend := types.NewJID("15557654321", types.DefaultUserServer)
	event := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat:     friend,
				Sender:   friend,
				IsGroup:  false,
				IsFromMe: false,
			},
			ID:        "view-once-1",
			Timestamp: time.Unix(1_700_000_081, 0),
		},
		Message: &waE2E.Message{
			ImageMessage: &waE2E.ImageMessage{Caption: proto.String("delete after reading")},
		},
	}
	event.IsViewOnce = true
	if _, ok := toWaMessage(context.Background(), nil, event); ok {
		t.Fatal("expected view-once captioned media to be rejected in a direct chat")
	}
}

// A captioned photo carries mention and reply context on the ImageMessage's
// own ContextInfo (there is no ExtendedTextMessage wrapper); both must resolve
// exactly as they do for text replies.
func TestToWaMessageResolvesCaptionMentionsAndQuotes(t *testing.T) {
	resetGroupStateForTest()
	group := types.NewJID("120363000000000012", types.GroupServer)
	senderLID := types.NewJID("352000000000123", types.HiddenUserServer)
	bolajiLID := types.NewJID("248506975531090", types.HiddenUserServer)
	quotedPN := types.NewJID("15557654321", types.DefaultUserServer)
	client := makeMentionResolutionClient(nil, "", map[string]types.ContactInfo{
		bolajiLID.String(): {Found: true, PushName: "Bolaji"},
		quotedPN.String():  {Found: true, FullName: "Alice Doe"},
	})

	event := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat:    group,
				Sender:  senderLID,
				IsGroup: true,
			},
			ID:        "caption-context-1",
			Timestamp: time.Unix(1_700_000_082, 0),
		},
		Message: &waE2E.Message{
			ImageMessage: &waE2E.ImageMessage{
				Caption: proto.String("@248506975531090 this is the spot"),
				ContextInfo: &waE2E.ContextInfo{
					MentionedJID: []string{bolajiLID.String()},
					StanzaID:     proto.String("original-message-12"),
					Participant:  proto.String(quotedPN.String()),
					QuotedMessage: &waE2E.Message{
						Conversation: proto.String("where should we meet?"),
					},
				},
			},
		},
	}

	message, ok := toWaMessage(context.Background(), client, event)
	if !ok {
		t.Fatal("expected captioned media reply to be accepted")
	}
	if message.Text != "@Bolaji this is the spot" {
		t.Fatalf("expected resolved mention in caption, got %q", message.Text)
	}
	if message.MediaType != "image" {
		t.Fatalf("MediaType = %q, want image", message.MediaType)
	}
	if message.QuotedMessageID != "original-message-12" {
		t.Fatalf("expected quoted stanza ID, got %q", message.QuotedMessageID)
	}
	if message.QuotedSenderName != "Alice Doe" {
		t.Fatalf("expected resolved quoted sender name, got %q", message.QuotedSenderName)
	}
	if message.QuotedText != "where should we meet?" {
		t.Fatalf("expected quoted snippet, got %q", message.QuotedText)
	}
}

// End to end through toWaMessage: the shipped payload text carries the
// resolved mention.
func TestToWaMessageResolvesMentionTokens(t *testing.T) {
	resetGroupStateForTest()
	group := types.NewJID("120363000000000010", types.GroupServer)
	senderLID := types.NewJID("352000000000123", types.HiddenUserServer)
	bolajiLID := types.NewJID("248506975531090", types.HiddenUserServer)
	client := makeMentionResolutionClient(nil, "", map[string]types.ContactInfo{
		bolajiLID.String(): {Found: true, PushName: "Bolaji"},
	})

	event := makeExtendedTextEvent(types.MessageInfo{
		MessageSource: types.MessageSource{
			Chat:           group,
			Sender:         senderLID,
			IsGroup:        true,
			IsFromMe:       false,
			AddressingMode: types.AddressingModeLID,
		},
		ID:        "mention-message-1",
		Timestamp: time.Unix(1_700_000_060, 0),
		PushName:  "Mike",
	}, "@248506975531090 are there any housing groups?", []string{bolajiLID.String()})

	message, ok := toWaMessage(context.Background(), client, event)
	if !ok {
		t.Fatal("expected mention-bearing group text to be accepted")
	}
	if message.Text != "@Bolaji are there any housing groups?" {
		t.Fatalf("expected resolved mention in payload text, got %q", message.Text)
	}
}
