INSERT INTO "user" (bridge_id, mxid, management_room, access_token)
SELECT '', mxid, management_room, ''
FROM user_old;

UPDATE "user" SET access_token=COALESCE((SELECT access_token FROM puppet_old WHERE custom_mxid="user".mxid AND access_token<>'' LIMIT 1), '');

INSERT INTO user_login (bridge_id, user_mxid, id, remote_name, space_room, metadata, remote_profile)
SELECT
    '', -- bridge_id
    mxid, -- user_mxid
    username || '@s.whatsapp.net', -- id
    '+' || username, -- remote_name
    space_room,
    -- only: postgres
    jsonb_build_object
-- only: sqlite (line commented)
--  json_object
    (
        'wa_device_id', device,
        'phone_last_seen', phone_last_seen,
        'phone_last_pinged', phone_last_pinged,
        'timezone', timezone
    ) -- metadata
FROM user_old
WHERE username<>'' AND device<>0;

INSERT INTO ghost (
    bridge_id, id, name, avatar_id, avatar_hash, avatar_mxc,
    name_set, avatar_set, contact_info_set, is_bot, identifiers, metadata
)
SELECT
    '', -- bridge_id
    username, -- id
    COALESCE(displayname, ''), -- name
    COALESCE(avatar, ''), -- avatar_id
    '', -- avatar_hash
    COALESCE(avatar_url, ''), -- avatar_mxc
    name_set,
    avatar_set,
    contact_info_set,
    false, -- is_bot
    '[]', -- identifiers
    -- only: postgres
    jsonb_build_object
    -- only: sqlite (line commented)
--  json_object
    (
        'last_sync', last_sync,
        'avatar_fetch_attempted', CASE WHEN avatar<>'' THEN json('true') ELSE json('false') END
        -- TODO name quality
    ) -- metadata
FROM puppet_old;

INSERT INTO portal (
    bridge_id, id, receiver, mxid, parent_id, parent_receiver, relay_bridge_id, relay_login_id, other_user_id,
    name, topic, avatar_id, avatar_hash, avatar_mxc, name_set, avatar_set, topic_set,
    name_is_custom, in_space, room_type, disappear_type, disappear_timer, metadata
)
SELECT
    '', -- bridge_id
    jid, -- id
    CASE WHEN receiver LIKE '%@s.whatsapp.net' THEN receiver ELSE '' END, -- receiver
    mxid,
    parent_group, -- parent_id
    '', -- parent_receiver
    CASE WHEN relay_user_id<>'' THEN '' END, -- relay_bridge_id
    (SELECT id FROM user_login WHERE user_mxid=relay_user_id), -- relay_login_id
    CASE WHEN jid LIKE '%@s.whatsapp.net' THEN replace(jid, '@s.whatsapp.net', '') ELSE '' END, -- other_user_id
    name,
    topic,
    avatar, -- avatar_id
    '', -- avatar_hash
    COALESCE(avatar_url, ''), -- avatar_mxc
    name_set,
    avatar_set,
    topic_set,
    jid NOT LIKE '%@s.whatsapp.net', -- name_is_custom
    in_space,
    CASE
        WHEN is_parent THEN 'space'
        WHEN jid LIKE '%@s.whatsapp.net' THEN 'dm'
        ELSE ''
    END, -- room_type
    CASE WHEN expiration_time>0 THEN 'after_read' END, -- disappear_type
    CASE WHEN expiration_time > 0 THEN expiration_time * 1000000000 END, -- disappear_timer TODO check multiplier
    '{}' -- metadata
FROM portal_old;

INSERT INTO user_portal (bridge_id, user_mxid, login_id, portal_id, portal_receiver, in_space, preferred, last_read)
SELECT
    '', -- bridge_id
    user_mxid,
    (SELECT id FROM user_login WHERE user_login.user_mxid=user_portal_old.user_mxid), -- login_id
    portal_jid, -- portal_id
    CASE WHEN portal_receiver LIKE '%@s.whatsapp.net' THEN portal_receiver ELSE '' END, -- portal_receiver
    in_space,
    false, -- preferred
    last_read_ts * 1000000000 -- last_read TODO check multiplier
FROM user_portal_old;

INSERT INTO message (
    bridge_id, id, part_id, mxid, room_id, room_receiver, sender_id, sender_mxid, timestamp, edit_count, metadata
)
SELECT
    '', -- bridge_id
    jid, -- id FIXME requires prefix
    '', -- part_id
    mxid,
    chat_jid, -- room_id
    CASE WHEN chat_receiver LIKE '%@s.whatsapp.net' THEN chat_receiver ELSE '' END, -- room_receiver
    sender, -- sender_id
    sender_mxid, -- sender_mxid
    timestamp * 1000000000, -- timestamp TODO check multiplier
    0, -- edit_count
    '{}' -- metadata
FROM message_old;

INSERT INTO reaction (
    bridge_id, message_id, message_part_id, sender_id, emoji_id, room_id, room_receiver, mxid, timestamp, emoji, metadata
)
SELECT
    '', -- bridge_id
    target_jid, -- message_id FIXME requires prefix
    '', -- message_part_id
    sender, -- sender_id
    '', -- emoji_id
    chat_jid, -- room_id
    CASE WHEN chat_receiver LIKE '%@s.whatsapp.net' THEN chat_receiver ELSE '' END, -- room_receiver
    mxid,
    0, -- timestamp
    '', -- emoji
    '{}' -- metadata
FROM reaction_old;

INSERT INTO disappearing_message (bridge_id, mx_room, mxid, type, timer, disappear_at)
SELECT
    '', -- bridge_id
    room_id,
    event_id,
    'after_read',
    expire_in * 1000000000, -- timer TODO check multiplier
    expire_at * 1000000000 -- disappear_at TODO check multiplier
FROM disappearing_message_old;

INSERT INTO backfill_task (
    bridge_id, portal_id, portal_receiver, user_login_id, batch_count, is_done,
    cursor, oldest_message_id, dispatched_at, completed_at, next_dispatch_min_ts
)
SELECT
    '', -- bridge_id
    portal_jid, -- portal_id
    CASE WHEN portal_receiver LIKE '%@s.whatsapp.net' THEN portal_receiver ELSE '' END, -- portal_receiver
    (SELECT id FROM user_login WHERE user_login.user_mxid=backfill_queue_old.user_mxid), -- user_login_id
    COUNT(*), -- batch_count
    COUNT(*) == COUNT(completed_at), -- is_done
    '', -- cursor
    '', -- oldest_message_id
    -- only: postgres
    EXTRACT(EPOCH FROM MAX(dispatch_time)) * 1000000000, -- dispatched_at
    -- only: sqlite (line commented)
--  unixepoch(MAX(dispatch_time)) * 1000000000,
    NULL, -- completed_at
    1 -- next_dispatch_min_ts
FROM backfill_queue_old
WHERE type IN (0, 200)
GROUP BY user_mxid, portal_jid, portal_receiver;

DROP TABLE backfill_queue_old;
DROP TABLE backfill_state_old;
DROP TABLE disappearing_message_old;
-- TODO migrate these tables
-- DROP TABLE history_sync_message_old;
-- DROP TABLE history_sync_conversation_old;
-- DROP TABLE media_backfill_requests_old;
-- DROP TABLE poll_option_id_old;
DROP TABLE user_portal_old;
DROP TABLE reaction_old;
DROP TABLE message_old;
DROP TABLE puppet_old;
DROP TABLE portal_old;
DROP TABLE user_old;
