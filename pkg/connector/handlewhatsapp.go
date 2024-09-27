package connector

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"maunium.net/go/mautrix/bridge/status"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/event"

	"maunium.net/go/mautrix-whatsapp/pkg/waid"
)

const (
	WANotLoggedIn      status.BridgeStateErrorCode = "wa-not-logged-in"
	WALoggedOut        status.BridgeStateErrorCode = "wa-logged-out"
	WAMainDeviceGone   status.BridgeStateErrorCode = "wa-main-device-gone"
	WAUnknownLogout    status.BridgeStateErrorCode = "wa-unknown-logout"
	WANotConnected     status.BridgeStateErrorCode = "wa-not-connected"
	WAConnecting       status.BridgeStateErrorCode = "wa-connecting"
	WAKeepaliveTimeout status.BridgeStateErrorCode = "wa-keepalive-timeout"
	WAPhoneOffline     status.BridgeStateErrorCode = "wa-phone-offline"
	WAConnectionFailed status.BridgeStateErrorCode = "wa-connection-failed"
	WADisconnected     status.BridgeStateErrorCode = "wa-transient-disconnect"
	WAStreamReplaced   status.BridgeStateErrorCode = "wa-stream-replaced"
	WAStreamError      status.BridgeStateErrorCode = "wa-stream-error"
	WAClientOutdated   status.BridgeStateErrorCode = "wa-client-outdated"
	WATemporaryBan     status.BridgeStateErrorCode = "wa-temporary-ban"
)

func init() {
	status.BridgeStateHumanErrors.Update(status.BridgeStateErrorMap{
		WALoggedOut:        "You were logged out from another device. Relogin to continue using the bridge.",
		WANotLoggedIn:      "You're not logged into WhatsApp. Relogin to continue using the bridge.",
		WAMainDeviceGone:   "Your phone was logged out from WhatsApp. Relogin to continue using the bridge.",
		WAUnknownLogout:    "You were logged out for an unknown reason. Relogin to continue using the bridge.",
		WANotConnected:     "You're not connected to WhatsApp",
		WAConnecting:       "Reconnecting to WhatsApp...",
		WAKeepaliveTimeout: "The WhatsApp web servers are not responding. The bridge will try to reconnect.",
		WAPhoneOffline:     "Your phone hasn't been seen in over 12 days. The bridge is currently connected, but will get disconnected if you don't open the app soon.",
		WAConnectionFailed: "Connecting to the WhatsApp web servers failed.",
		WADisconnected:     "Disconnected from WhatsApp. Trying to reconnect.",
		WAClientOutdated:   "Connect failure: 405 client outdated. Bridge must be updated.",
		WAStreamReplaced:   "Stream replaced: the bridge was started in another location.",
	})
}

func (wa *WhatsAppClient) handleWAEvent(rawEvt any) {
	log := wa.UserLogin.Log

	switch evt := rawEvt.(type) {
	case *events.Message:
		wa.handleWAMessage(evt)
	case *events.Receipt:
		wa.handleWAReceipt(evt)
	case *events.ChatPresence:
		wa.handleWAChatPresence(evt)
	case *events.UndecryptableMessage:
		wa.handleWAUndecryptableMessage(evt)

	case *events.CallOffer:
		wa.handleWACallStart(evt.CallCreator, evt.CallID, "", evt.Timestamp)
	case *events.CallOfferNotice:
		wa.handleWACallStart(evt.CallCreator, evt.CallID, evt.Type, evt.Timestamp)
	case *events.CallTerminate, *events.CallRelayLatency, *events.CallAccept, *events.UnknownCallEvent:
		// ignore
	case *events.IdentityChange:
		wa.handleWAIdentityChange(evt)
	case *events.MarkChatAsRead:
		wa.handleWAMarkChatAsRead(evt)
	case *events.DeleteForMe:
		wa.handleWADeleteForMe(evt)
	case *events.DeleteChat:
		wa.handleWADeleteChat(evt)
	case *events.Mute:
		wa.handleWAMute(evt)
	case *events.Archive:
		wa.handleWAArchive(evt)
	case *events.Pin:
		wa.handleWAPin(evt)

	case *events.HistorySync:
		if wa.Main.Bridge.Config.Backfill.Enabled {
			wa.historySyncs <- evt.Data
		}
	case *events.MediaRetry:
		// TODO

	case *events.GroupInfo:
		wa.handleWAGroupInfoChange(evt)
	case *events.JoinedGroup:
		wa.handleWAJoinedGroup(evt)
	case *events.NewsletterJoin:
		// TODO
	case *events.NewsletterLeave:
		// TODO
	case *events.Picture:
		go wa.handleWAPictureUpdate(evt)

	case *events.AppStateSyncComplete:
		if len(wa.Client.Store.PushName) > 0 && evt.Name == appstate.WAPatchCriticalBlock {
			err := wa.Client.SendPresence(types.PresenceUnavailable)
			if err != nil {
				log.Warn().Err(err).Msg("Failed to send presence after app state sync")
			}
		} else if evt.Name == appstate.WAPatchCriticalUnblockLow {
			go wa.resyncContacts(false)
		}
	case *events.AppState:
		// Intentionally ignored
	case *events.PushNameSetting:
		// Send presence available when connecting and when the pushname is changed.
		// This makes sure that outgoing messages always have the right pushname.
		err := wa.Client.SendPresence(types.PresenceUnavailable)
		if err != nil {
			log.Warn().Err(err).Msg("Failed to send presence after push name update")
		}
		_, _, err = wa.Client.Store.Contacts.PutPushName(wa.JID.ToNonAD(), evt.Action.GetName())
		if err != nil {
			log.Err(err).Msg("Failed to update push name in store")
		}
		go wa.syncGhost(wa.JID.ToNonAD(), "push name setting", "")
	case *events.Contact:
		go wa.syncGhost(evt.JID, "contact event", "")
	case *events.PushName:
		go wa.syncGhost(evt.JID, "push name event", "")
	case *events.BusinessName:
		go wa.syncGhost(evt.JID, "business name event", "")

	case *events.Connected:
		log.Debug().Msg("Connected to WhatsApp socket")
		wa.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnected})
		if len(wa.Client.Store.PushName) > 0 {
			go func() {
				err := wa.Client.SendPresence(types.PresenceUnavailable)
				if err != nil {
					log.Warn().Err(err).Msg("Failed to send initial presence after connecting")
				}
			}()
		}
	case *events.OfflineSyncPreview:
		log.Info().
			Int("message_count", evt.Messages).
			Int("receipt_count", evt.Receipts).
			Int("notification_count", evt.Notifications).
			Int("app_data_change_count", evt.AppDataChanges).
			Msg("Server sent number of events that were missed during downtime")
	case *events.OfflineSyncCompleted:
		if !wa.PhoneRecentlySeen(true) {
			log.Info().
				Time("phone_last_seen", wa.UserLogin.Metadata.(*waid.UserLoginMetadata).PhoneLastSeen.Time).
				Msg("Offline sync completed, but phone last seen date is still old - sending phone offline bridge status")
			wa.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateTransientDisconnect, Error: WAPhoneOffline})
		} else {
			log.Info().Msg("Offline sync completed")
		}
	case *events.LoggedOut:
		wa.handleWALogout(evt.Reason, evt.OnConnect)
	case *events.Disconnected:
		// Don't send the normal transient disconnect state if we're already in a different transient disconnect state.
		// TODO remove this if/when the phone offline state is moved to a sub-state of CONNECTED
		if wa.UserLogin.BridgeState.GetPrev().Error != WAPhoneOffline && wa.PhoneRecentlySeen(false) {
			wa.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateTransientDisconnect, Error: WADisconnected})
		}
	case *events.StreamError:
		var message string
		if evt.Code != "" {
			message = fmt.Sprintf("Unknown stream error with code %s", evt.Code)
		} else if children := evt.Raw.GetChildren(); len(children) > 0 {
			message = fmt.Sprintf("Unknown stream error (contains %s node)", children[0].Tag)
		} else {
			message = "Unknown stream error"
		}
		wa.UserLogin.BridgeState.Send(status.BridgeState{
			StateEvent: status.StateUnknownError,
			Error:      WAStreamError,
			Message:    message,
		})
	case *events.StreamReplaced:
		wa.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateUnknownError, Error: WAStreamReplaced})
	case *events.KeepAliveTimeout:
		wa.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateTransientDisconnect, Error: WAKeepaliveTimeout})
	case *events.KeepAliveRestored:
		log.Info().Msg("Keepalive restored after timeouts, sending connected event")
		wa.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnected})
	case *events.ConnectFailure:
		wa.UserLogin.BridgeState.Send(status.BridgeState{
			StateEvent: status.StateUnknownError,
			Error:      status.BridgeStateErrorCode(fmt.Sprintf("wa-connect-failure-%d", evt.Reason)),
			Message:    fmt.Sprintf("Unknown connection failure: %s (%s)", evt.Reason, evt.Message),
		})
	case *events.ClientOutdated:
		wa.UserLogin.Log.Error().Msg("Got a client outdated connect failure. The bridge is likely out of date, please update immediately.")
		wa.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateUnknownError, Error: WAClientOutdated})
	case *events.TemporaryBan:
		wa.UserLogin.BridgeState.Send(status.BridgeState{
			StateEvent: status.StateBadCredentials,
			Error:      WATemporaryBan,
			Message:    evt.String(),
		})
	default:
		log.Debug().Type("event_type", rawEvt).Msg("Unhandled WhatsApp event")
	}
}

func (wa *WhatsAppClient) handleWAMessage(evt *events.Message) {
	wa.UserLogin.Log.Trace().
		Any("info", evt.Info).
		Any("payload", evt.Message).
		Msg("Received WhatsApp message")
	if evt.Info.Chat.Server == types.HiddenUserServer || evt.Info.Sender.Server == types.HiddenUserServer {
		return
	}
	parsedMessageType := getMessageType(evt.Message)
	if parsedMessageType == "ignore" || strings.HasPrefix(parsedMessageType, "unknown_protocol_") {
		return
	}
	wa.Main.Bridge.QueueRemoteEvent(wa.UserLogin, &WAMessageEvent{
		MessageInfoWrapper: &MessageInfoWrapper{
			Info: evt.Info,
			wa:   wa,
		},
		Message:  evt.Message,
		MsgEvent: evt,

		parsedMessageType: parsedMessageType,
	})
}

func (wa *WhatsAppClient) handleWAUndecryptableMessage(evt *events.UndecryptableMessage) {
	wa.UserLogin.Log.Debug().
		Any("info", evt.Info).
		Bool("unavailable", evt.IsUnavailable).
		Str("decrypt_fail", string(evt.DecryptFailMode)).
		Msg("Received undecryptable WhatsApp message")
	if evt.DecryptFailMode == events.DecryptFailHide || evt.Info.Chat.Server == types.HiddenUserServer || evt.Info.Sender.Server == types.HiddenUserServer {
		return
	}
	wa.Main.Bridge.QueueRemoteEvent(wa.UserLogin, &WAUndecryptableMessage{
		MessageInfoWrapper: &MessageInfoWrapper{
			Info: evt.Info,
			wa:   wa,
		},
	})
}

func (wa *WhatsAppClient) handleWAReceipt(evt *events.Receipt) {
	var evtType bridgev2.RemoteEventType
	switch evt.Type {
	case types.ReceiptTypeRead, types.ReceiptTypeReadSelf:
		evtType = bridgev2.RemoteEventReadReceipt
	case types.ReceiptTypeDelivered:
		evtType = bridgev2.RemoteEventDeliveryReceipt
	case types.ReceiptTypeSender:
		fallthrough
	default:
		return
	}
	targets := make([]networkid.MessageID, len(evt.MessageIDs))
	for i, id := range evt.MessageIDs {
		targets[i] = waid.MakeMessageID(evt.Chat, *wa.Device.ID, id)
	}
	wa.Main.Bridge.QueueRemoteEvent(wa.UserLogin, &simplevent.Receipt{
		EventMeta: simplevent.EventMeta{
			Type:      evtType,
			PortalKey: wa.makeWAPortalKey(evt.Chat),
			Sender:    wa.makeEventSender(evt.Sender),
			Timestamp: evt.Timestamp,
		},
		Targets: targets,
	})
}

func (wa *WhatsAppClient) handleWAChatPresence(evt *events.ChatPresence) {
	typingType := bridgev2.TypingTypeText
	timeout := 15 * time.Second
	if evt.Media == types.ChatPresenceMediaAudio {
		typingType = bridgev2.TypingTypeRecordingMedia
	}
	if evt.State == types.ChatPresencePaused {
		timeout = 0
	}

	wa.Main.Bridge.QueueRemoteEvent(wa.UserLogin, &simplevent.Typing{
		EventMeta: simplevent.EventMeta{
			Type:       bridgev2.RemoteEventTyping,
			LogContext: nil,
			PortalKey:  wa.makeWAPortalKey(evt.Chat),
			Sender:     wa.makeEventSender(evt.Sender),
			Timestamp:  time.Now(),
		},
		Timeout: timeout,
		Type:    typingType,
	})
}

func (wa *WhatsAppClient) handleWALogout(reason events.ConnectFailureReason, onConnect bool) {
	errorCode := WAUnknownLogout
	if reason == events.ConnectFailureLoggedOut {
		errorCode = WALoggedOut
	} else if reason == events.ConnectFailureMainDeviceGone {
		errorCode = WAMainDeviceGone
	}
	wa.Client.Disconnect()
	wa.Client = nil
	wa.JID = types.EmptyJID
	wa.UserLogin.Metadata.(*waid.UserLoginMetadata).WADeviceID = 0
	wa.UserLogin.BridgeState.Send(status.BridgeState{
		StateEvent: status.StateBadCredentials,
		Error:      errorCode,
	})
}

func (wa *WhatsAppClient) handleWACallStart(sender types.JID, id, callType string, ts time.Time) {
	if !wa.Main.Config.CallStartNotices {
		return
	}
	wa.UserLogin.QueueRemoteEvent(&simplevent.Message[string]{
		EventMeta: simplevent.EventMeta{
			Type:         bridgev2.RemoteEventMessage,
			LogContext:   nil,
			PortalKey:    wa.makeWAPortalKey(sender),
			Sender:       wa.makeEventSender(sender),
			CreatePortal: true,
			Timestamp:    ts,
		},
		Data:               callType,
		ID:                 waid.MakeFakeMessageID(sender, sender, "call-"+id),
		ConvertMessageFunc: convertCallStart,
	})
}

func convertCallStart(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, callType string) (*bridgev2.ConvertedMessage, error) {
	text := "Incoming call. Use the WhatsApp app to answer."
	if callType != "" {
		text = fmt.Sprintf("Incoming %s call. Use the WhatsApp app to answer.", callType)
	}
	return &bridgev2.ConvertedMessage{
		Parts: []*bridgev2.ConvertedMessagePart{{
			Type: event.EventMessage,
			Content: &event.MessageEventContent{
				MsgType: event.MsgText,
				Body:    text,
			},
		}},
	}, nil
}

func (wa *WhatsAppClient) handleWAIdentityChange(evt *events.IdentityChange) {
	if !wa.Main.Config.IdentityChangeNotices {
		return
	}
	wa.UserLogin.QueueRemoteEvent(&simplevent.Message[*events.IdentityChange]{
		EventMeta: simplevent.EventMeta{
			Type:         bridgev2.RemoteEventMessage,
			LogContext:   nil,
			PortalKey:    wa.makeWAPortalKey(evt.JID),
			Sender:       wa.makeEventSender(evt.JID),
			CreatePortal: false,
			Timestamp:    evt.Timestamp,
		},
		Data:               evt,
		ID:                 waid.MakeFakeMessageID(evt.JID, evt.JID, "idchange-"+strconv.FormatInt(evt.Timestamp.UnixMilli(), 10)),
		ConvertMessageFunc: convertIdentityChange,
	})
}

func convertIdentityChange(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, data *events.IdentityChange) (*bridgev2.ConvertedMessage, error) {
	ghost, err := portal.Bridge.GetGhostByID(ctx, waid.MakeUserID(data.JID))
	if err != nil {
		return nil, err
	}
	text := fmt.Sprintf("Your security code with %s changed.", ghost.Name)
	if data.Implicit {
		text = fmt.Sprintf("Your security code with %s (device #%d) changed.", ghost.Name, data.JID.Device)
	}
	return &bridgev2.ConvertedMessage{
		Parts: []*bridgev2.ConvertedMessagePart{{
			Type: event.EventMessage,
			Content: &event.MessageEventContent{
				MsgType: event.MsgNotice,
				Body:    text,
			},
		}},
	}, nil
}

func (wa *WhatsAppClient) handleWADeleteChat(evt *events.DeleteChat) {
	wa.UserLogin.QueueRemoteEvent(&simplevent.ChatDelete{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventChatDelete,
			PortalKey: wa.makeWAPortalKey(evt.JID),
			Timestamp: evt.Timestamp,
		},
		OnlyForMe: true,
	})
}

func (wa *WhatsAppClient) handleWADeleteForMe(evt *events.DeleteForMe) {
	wa.UserLogin.QueueRemoteEvent(&simplevent.MessageRemove{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventMessageRemove,
			PortalKey: wa.makeWAPortalKey(evt.ChatJID),
			Timestamp: evt.Timestamp,
		},
		TargetMessage: waid.MakeMessageID(evt.ChatJID, evt.SenderJID, evt.MessageID),
		OnlyForMe:     true,
	})
}

func (wa *WhatsAppClient) handleWAMarkChatAsRead(evt *events.MarkChatAsRead) {
	wa.UserLogin.QueueRemoteEvent(&simplevent.Receipt{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventReadReceipt,
			PortalKey: wa.makeWAPortalKey(evt.JID),
			Sender:    wa.makeEventSender(wa.JID),
			Timestamp: evt.Timestamp,
		},
		ReadUpTo: evt.Timestamp,
	})
}

func (wa *WhatsAppClient) syncGhost(jid types.JID, reason, pictureID string) {
	log := wa.UserLogin.Log.With().
		Str("action", "sync ghost").
		Str("reason", reason).
		Str("picture_id", pictureID).
		Stringer("jid", jid).
		Logger()
	ctx := log.WithContext(context.Background())
	ghost, err := wa.Main.Bridge.GetGhostByID(ctx, waid.MakeUserID(jid))
	if err != nil {
		log.Err(err).Msg("Failed to get ghost")
		return
	}
	if pictureID != "" && ghost.AvatarID == networkid.AvatarID(pictureID) {
		return
	}
	userInfo, err := wa.getUserInfo(ctx, jid, pictureID != "")
	if err != nil {
		log.Err(err).Msg("Failed to get user info")
	} else {
		ghost.UpdateInfo(ctx, userInfo)
		log.Debug().Msg("Synced ghost info")
	}
}

func (wa *WhatsAppClient) handleWAPictureUpdate(evt *events.Picture) {
	if evt.JID.Server == types.DefaultUserServer {
		wa.syncGhost(evt.JID, "picture event", evt.PictureID)
	} else {
		var changes bridgev2.ChatInfo
		if evt.Remove {
			changes.Avatar = &bridgev2.Avatar{Remove: true, ID: "remove"}
		} else {
			changes.ExtraUpdates = wa.makePortalAvatarFetcher(evt.PictureID, evt.JID, evt.Timestamp)
		}
		wa.UserLogin.QueueRemoteEvent(&simplevent.ChatInfoChange{
			EventMeta: simplevent.EventMeta{
				Type: bridgev2.RemoteEventChatInfoChange,
				LogContext: func(c zerolog.Context) zerolog.Context {
					return c.
						Str("wa_event_type", "picture").
						Stringer("picture_author", evt.Author).
						Str("new_picture_id", evt.PictureID).
						Bool("remove_picture", evt.Remove)
				},
				PortalKey: wa.makeWAPortalKey(evt.JID),
				Sender:    wa.makeEventSender(evt.Author),
				Timestamp: evt.Timestamp,
			},
			ChatInfoChange: &bridgev2.ChatInfoChange{
				ChatInfo: &changes,
			},
		})
	}
}

func (wa *WhatsAppClient) handleWAGroupInfoChange(evt *events.GroupInfo) {
	eventMeta := simplevent.EventMeta{
		Type:         bridgev2.RemoteEventChatInfoChange,
		LogContext:   nil,
		PortalKey:    wa.makeWAPortalKey(evt.JID),
		CreatePortal: true,
		Timestamp:    evt.Timestamp,
	}
	if evt.Sender != nil {
		eventMeta.Sender = wa.makeEventSender(*evt.Sender)
	}
	if evt.Delete != nil {
		eventMeta.Type = bridgev2.RemoteEventChatDelete
		wa.UserLogin.QueueRemoteEvent(&simplevent.ChatDelete{EventMeta: eventMeta})
	} else {
		wa.UserLogin.QueueRemoteEvent(&simplevent.ChatInfoChange{
			EventMeta:      eventMeta,
			ChatInfoChange: wa.wrapGroupInfoChange(evt),
		})
	}
}

func (wa *WhatsAppClient) handleWAJoinedGroup(evt *events.JoinedGroup) {
	wa.Main.Bridge.QueueRemoteEvent(wa.UserLogin, &simplevent.ChatResync{
		EventMeta: simplevent.EventMeta{
			Type:         bridgev2.RemoteEventChatResync,
			LogContext:   nil,
			PortalKey:    wa.makeWAPortalKey(evt.JID),
			CreatePortal: true,
		},
		ChatInfo: wa.wrapGroupInfo(&evt.GroupInfo),
	})
}

func (wa *WhatsAppClient) handleWAUserLocalPortalInfo(chatJID types.JID, ts time.Time, info *bridgev2.UserLocalPortalInfo) {
	wa.UserLogin.QueueRemoteEvent(&simplevent.ChatInfoChange{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventChatInfoChange,
			PortalKey: wa.makeWAPortalKey(chatJID),
			Timestamp: ts,
		},
		ChatInfoChange: &bridgev2.ChatInfoChange{
			ChatInfo: &bridgev2.ChatInfo{
				UserLocal: info,
			},
		},
	})
}

func (wa *WhatsAppClient) handleWAMute(evt *events.Mute) {
	var mutedUntil time.Time
	if evt.Action.GetMuted() {
		mutedUntil = event.MutedForever
		if evt.Action.GetMuteEndTimestamp() != 0 {
			mutedUntil = time.Unix(evt.Action.GetMuteEndTimestamp(), 0)
		}
	} else {
		mutedUntil = bridgev2.Unmuted
	}
	wa.handleWAUserLocalPortalInfo(evt.JID, evt.Timestamp, &bridgev2.UserLocalPortalInfo{
		MutedUntil: &mutedUntil,
	})
}

func (wa *WhatsAppClient) handleWAArchive(evt *events.Archive) {
	var tag event.RoomTag
	if evt.Action.GetArchived() {
		tag = event.RoomTagLowPriority
	}
	wa.handleWAUserLocalPortalInfo(evt.JID, evt.Timestamp, &bridgev2.UserLocalPortalInfo{
		Tag: &tag,
	})
}

func (wa *WhatsAppClient) handleWAPin(evt *events.Pin) {
	var tag event.RoomTag
	if evt.Action.GetPinned() {
		tag = event.RoomTagFavourite
	}
	wa.handleWAUserLocalPortalInfo(evt.JID, evt.Timestamp, &bridgev2.UserLocalPortalInfo{
		Tag: &tag,
	})
}
