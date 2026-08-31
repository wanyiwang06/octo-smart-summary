package agent

import (
	"context"
	"strings"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"gorm.io/gorm"
)

// FilterChannelsForWorkspace keeps discovery inside the authenticated space.
// Group/thread rows carry a space id directly. DM rows do not, so their peer
// must be an active member of the current space before the channel is exposed
// to the workspace Agent or granted to its mutable read allowlist.
func FilterChannelsForWorkspace(ctx context.Context, actorID string, imDB *gorm.DB, channels []pipeline.ChannelInfo) ([]pipeline.ChannelInfo, error) {
	spaceID := strings.TrimSpace(WorkspaceSpaceID(ctx))
	if spaceID == "" {
		return channels, nil
	}

	peerIDs := make([]string, 0)
	seenPeers := make(map[string]struct{})
	for _, channel := range channels {
		if channel.ChannelType != model.ChannelTypeDM {
			continue
		}
		peerID := strings.TrimSpace(channel.PeerUID)
		if peerID == "" {
			peerID = dmPeerID(channel.ChannelID, actorID)
		}
		if peerID == "" || peerID == actorID {
			continue
		}
		if _, exists := seenPeers[peerID]; !exists {
			seenPeers[peerID] = struct{}{}
			peerIDs = append(peerIDs, peerID)
		}
	}

	activePeers := make(map[string]struct{}, len(peerIDs))
	if len(peerIDs) > 0 {
		var rows []struct {
			UID string `gorm:"column:uid"`
		}
		if err := imDB.WithContext(ctx).Table("space_member").
			Select("DISTINCT uid").
			Where("space_id = ? AND uid IN ? AND status = 1", spaceID, peerIDs).
			Scan(&rows).Error; err != nil {
			return nil, err
		}
		for _, row := range rows {
			activePeers[row.UID] = struct{}{}
		}
	}

	filtered := make([]pipeline.ChannelInfo, 0, len(channels))
	for _, channel := range channels {
		switch channel.ChannelType {
		case model.ChannelTypeGroup, model.ChannelTypeThread:
			if channel.SpaceID != spaceID {
				continue
			}
		case model.ChannelTypeDM:
			peerID := strings.TrimSpace(channel.PeerUID)
			if peerID == "" {
				peerID = dmPeerID(channel.ChannelID, actorID)
			}
			if _, ok := activePeers[peerID]; !ok {
				continue
			}
		default:
			continue
		}
		filtered = append(filtered, channel)
	}
	return filtered, nil
}

func dmPeerID(channelID, actorID string) string {
	parts := strings.Split(channelID, "@")
	if len(parts) != 2 {
		return ""
	}
	if parts[0] == actorID && parts[1] != actorID {
		return parts[1]
	}
	if parts[1] == actorID && parts[0] != actorID {
		return parts[0]
	}
	return ""
}
