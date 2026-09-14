package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"Abot/internal/agent"
	agentruntime "Abot/internal/agent/runtime"
	"gorm.io/gorm"
)

// CommitRuntimeConfigMigrationWithDelivery atomically changes the local
// Invocation snapshot and records both the migration audit event and the
// metadata-only remote delivery cursor. A successful return therefore always
// has a durable retry path for the destination lock.
func (r *runtimeRepository) CommitRuntimeConfigMigrationWithDelivery(ctx context.Context, commit agentruntime.RuntimeConfigMigrationCommit, source, destination string) (agentruntime.Invocation, agentruntime.AgentEvent, agentruntime.RuntimeConfigDeliveryOutbox, error) {
	source = strings.TrimSpace(source)
	destination = strings.TrimSpace(destination)
	if source == "" || destination == "" {
		return agentruntime.Invocation{}, agentruntime.AgentEvent{}, agentruntime.RuntimeConfigDeliveryOutbox{}, fmt.Errorf("%w: config delivery source/destination 不完整", agentruntime.ErrInvalidRuntimeConfigDelivery)
	}
	now := time.Now().UTC()
	// Keep the durable cursor aligned with the migration event's logical clock
	// when one is supplied.  This makes replay/tests deterministic and avoids a
	// queued delivery being scheduled after a caller's dispatch timestamp.
	if !commit.Event.Timestamp.IsZero() {
		now = commit.Event.Timestamp.UTC()
	}
	normalized, newSnapshot, newDigest, err := agentruntime.NormalizeRuntimeConfigMigrationCommit(commit, now)
	if err != nil {
		return agentruntime.Invocation{}, agentruntime.AgentEvent{}, agentruntime.RuntimeConfigDeliveryOutbox{}, err
	}
	delivery, err := agentruntime.NewRuntimeConfigDeliveryOutbox(source, destination, normalized.InvocationID, normalized.ExpectedDigest, normalized.IdempotencyKey, newSnapshot, now)
	if err != nil {
		return agentruntime.Invocation{}, agentruntime.AgentEvent{}, agentruntime.RuntimeConfigDeliveryOutbox{}, err
	}
	var savedInvocation agentruntime.Invocation
	var savedEvent agentruntime.AgentEvent
	var savedDelivery agentruntime.RuntimeConfigDeliveryOutbox
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row invocationRow
		if findErr := tx.Where("id = ?", normalized.InvocationID).First(&row).Error; findErr != nil {
			if errors.Is(findErr, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return findErr
		}
		status := agentruntime.InvocationStatus(row.Status)
		if !agentruntime.IsRuntimeConfigMigrationWaitingStatus(status) {
			return agentruntime.ErrConflict
		}
		storedDigest := agent.RuntimeConfigSnapshotDigest(row.ConfigSnapshot)
		var existingEvent agentEventRow
		findEventErr := tx.Where("id = ?", normalized.Event.ID).First(&existingEvent).Error
		if findEventErr == nil {
			event := eventFromRow(existingEvent)
			if event.InvocationID != normalized.InvocationID || event.Type != agentruntime.EventRuntimeConfigMigrated || !agentruntime.RuntimeConfigMigrationEventMatches(event, normalized.ExpectedDigest, newDigest, normalized.IdempotencyKey) || storedDigest != newDigest {
				return agentruntime.ErrConflict
			}
			var existingDeliveryRow runtimeConfigDeliveryOutboxRow
			if findErr := tx.Where("id = ?", delivery.ID).First(&existingDeliveryRow).Error; findErr != nil {
				if errors.Is(findErr, gorm.ErrRecordNotFound) {
					return agentruntime.ErrConflict
				}
				return findErr
			}
			storedDelivery, decodeErr := runtimeConfigDeliveryOutboxFromRow(existingDeliveryRow)
			if decodeErr != nil {
				return decodeErr
			}
			if !storedDelivery.Matches(delivery) {
				return agentruntime.ErrConflict
			}
			eventOutbox, eventOutboxErr := agentruntime.NewRuntimeEventOutbox(event)
			if eventOutboxErr != nil {
				return eventOutboxErr
			}
			deliveryGroup, groupErr := agentruntime.NewRuntimeDeliveryGroup(source, destination, normalized.InvocationID, []agentruntime.RuntimeDeliveryGroupMember{
				{Kind: agentruntime.RuntimeDeliveryKindEvent, OutboxID: eventOutbox.ID, DeliveryID: event.ID},
				{Kind: agentruntime.RuntimeDeliveryKindConfig, OutboxID: storedDelivery.ID, DeliveryID: storedDelivery.DeliveryID},
			}, event.Timestamp)
			if groupErr != nil {
				return groupErr
			}
			var existingGroupRow runtimeDeliveryGroupRow
			groupFindErr := tx.Where("id = ?", deliveryGroup.ID).First(&existingGroupRow).Error
			if groupFindErr == nil {
				existingGroup, decodeErr := runtimeDeliveryGroupFromRow(existingGroupRow)
				if decodeErr != nil {
					return decodeErr
				}
				if !existingGroup.MatchesIdentity(deliveryGroup) {
					return agentruntime.ErrConflict
				}
			} else if errors.Is(groupFindErr, gorm.ErrRecordNotFound) {
				groupRow, groupRowErr := runtimeDeliveryGroupToRow(deliveryGroup)
				if groupRowErr != nil {
					return groupRowErr
				}
				if createErr := tx.Create(groupRow).Error; createErr != nil {
					return createErr
				}
			} else {
				return groupFindErr
			}
			if updateErr := tx.Model(&runtimeEventOutboxRow{}).Where("id = ?", eventOutbox.ID).Update("group_id", deliveryGroup.ID).Error; updateErr != nil {
				return updateErr
			}
			if updateErr := tx.Model(&runtimeConfigDeliveryOutboxRow{}).Where("id = ?", storedDelivery.ID).Update("group_id", deliveryGroup.ID).Error; updateErr != nil {
				return updateErr
			}
			storedDelivery.GroupID = deliveryGroup.ID
			savedInvocation = invocationFromRow(row)
			savedEvent = event
			savedDelivery = storedDelivery
			return nil
		}
		if !errors.Is(findEventErr, gorm.ErrRecordNotFound) {
			return findEventErr
		}
		if storedDigest == "" || storedDigest != normalized.ExpectedDigest || newDigest == storedDigest {
			return agentruntime.ErrConflict
		}
		previousSnapshot, previousErr := agent.ParseRuntimeConfigSnapshot(row.ConfigSnapshot)
		if previousErr != nil {
			return agentruntime.ErrConflict
		}
		data, dataErr := agentruntime.RuntimeConfigMigrationEventData(normalized.InvocationID, storedDigest, newDigest, normalized.IdempotencyKey, previousSnapshot, newSnapshot)
		if dataErr != nil {
			return dataErr
		}
		event := normalized.Event
		event.InvocationID = normalized.InvocationID
		event.Type = agentruntime.EventRuntimeConfigMigrated
		event.Data = data
		var last agentEventRow
		lastErr := tx.Where("invocation_id = ?", normalized.InvocationID).Order("sequence DESC").First(&last).Error
		if lastErr != nil && !errors.Is(lastErr, gorm.ErrRecordNotFound) {
			return lastErr
		}
		event.Sequence = last.Sequence + 1
		encodedData, marshalErr := json.Marshal(event.Data)
		if marshalErr != nil {
			return marshalErr
		}
		if createErr := tx.Create(&agentEventRow{ID: event.ID, InvocationID: event.InvocationID, Sequence: event.Sequence, Type: event.Type, Timestamp: event.Timestamp, DataJSON: string(encodedData)}).Error; createErr != nil {
			return createErr
		}
		eventOutbox, outboxErr := agentruntime.NewRuntimeEventOutbox(event)
		if outboxErr != nil {
			return outboxErr
		}
		if createErr := tx.Create(runtimeEventOutboxToRow(eventOutbox)).Error; createErr != nil {
			return createErr
		}
		deliveryRow, rowErr := runtimeConfigDeliveryOutboxToRow(delivery)
		if rowErr != nil {
			return rowErr
		}
		if createErr := tx.Create(deliveryRow).Error; createErr != nil {
			return createErr
		}
		deliveryGroup, groupErr := agentruntime.NewRuntimeDeliveryGroup(source, destination, normalized.InvocationID, []agentruntime.RuntimeDeliveryGroupMember{
			{Kind: agentruntime.RuntimeDeliveryKindEvent, OutboxID: eventOutbox.ID, DeliveryID: event.ID},
			{Kind: agentruntime.RuntimeDeliveryKindConfig, OutboxID: delivery.ID, DeliveryID: delivery.DeliveryID},
		}, now)
		if groupErr != nil {
			return groupErr
		}
		groupRow, groupRowErr := runtimeDeliveryGroupToRow(deliveryGroup)
		if groupRowErr != nil {
			return groupRowErr
		}
		if createErr := tx.Create(groupRow).Error; createErr != nil {
			return createErr
		}
		if updateErr := tx.Model(&runtimeEventOutboxRow{}).Where("id = ?", eventOutbox.ID).Update("group_id", deliveryGroup.ID).Error; updateErr != nil {
			return updateErr
		}
		if updateErr := tx.Model(&runtimeConfigDeliveryOutboxRow{}).Where("id = ?", delivery.ID).Update("group_id", deliveryGroup.ID).Error; updateErr != nil {
			return updateErr
		}
		delivery.GroupID = deliveryGroup.ID
		updated := tx.Model(&invocationRow{}).Where("id = ? AND config_snapshot = ?", normalized.InvocationID, row.ConfigSnapshot).Updates(map[string]any{
			"config_snapshot": normalized.NewSnapshot, "updated_at": now,
		})
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return agentruntime.ErrConflict
		}
		row.ConfigSnapshot = normalized.NewSnapshot
		row.UpdatedAt = now
		savedInvocation = invocationFromRow(row)
		savedEvent = event
		savedDelivery = delivery
		return nil
	})
	if err != nil {
		return agentruntime.Invocation{}, agentruntime.AgentEvent{}, agentruntime.RuntimeConfigDeliveryOutbox{}, err
	}
	return savedInvocation, savedEvent, savedDelivery, nil
}
