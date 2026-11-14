package main

import (
	"context"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// updateGroupName overwrites the cached chat name. GetChatName never refreshes
// a name once stored, so renames must be pushed here. Only existing rows are
// touched: a group with no stored messages gets its name on first message.
func (store *MessageStore) updateGroupName(jid, name string) (bool, error) {
	if name == "" {
		return false, nil
	}
	res, err := store.db.Exec("UPDATE chats SET name = ? WHERE jid = ? AND name IS NOT ?", name, jid, name)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func handleGroupInfo(store *MessageStore, evt *events.GroupInfo, logger waLog.Logger) {
	if evt.Name == nil {
		return
	}
	if changed, err := store.updateGroupName(evt.JID.String(), evt.Name.Name); err != nil {
		logger.Warnf("Failed to update group name for %s: %v", evt.JID, err)
	} else if changed {
		logger.Infof("Group renamed: %s -> %s", evt.JID, evt.Name.Name)
	}
}

// syncGroupNames catches renames that happened while the bridge was offline
// (or before handleGroupInfo existed).
func syncGroupNames(client *whatsmeow.Client, store *MessageStore, logger waLog.Logger) {
	groups, err := client.GetJoinedGroups(context.Background())
	if err != nil {
		logger.Warnf("Group name sync: %v", err)
		return
	}
	updated := 0
	for _, g := range groups {
		if changed, err := store.updateGroupName(g.JID.String(), g.Name); err != nil {
			logger.Warnf("Group name sync %s: %v", g.JID, err)
		} else if changed {
			updated++
		}
	}
	logger.Infof("Group name sync: %d of %d group(s) updated", updated, len(groups))
}
