package wa

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
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
		"list_groups_v1",
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
