package runtime

import (
	"context"
	"strings"
)

func runtimeConfigDirectoryKey(kind RuntimeConfigDirectoryKind, id string) string {
	return string(kind) + "\x00" + strings.TrimSpace(id)
}

func (r *MemoryRepository) AcceptRuntimeConfigDirectory(_ context.Context, envelope RuntimeConfigDirectoryEnvelope, entry RuntimeConfigDirectoryEntry) (bool, error) {
	normalizedEnvelope, normalizedEntry, err := normalizeRuntimeConfigDirectoryPair(envelope, entry)
	if err != nil {
		return false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.configDirectory == nil {
		r.configDirectory = make(map[string]RuntimeConfigDirectoryRecord)
	}
	key := runtimeConfigDirectoryKey(normalizedEnvelope.Kind, normalizedEnvelope.EntryID)
	existing, exists := r.configDirectory[key]
	if !exists {
		var record RuntimeConfigDirectoryRecord
		duplicate, acceptErr := AcceptRuntimeConfigDirectoryRecord(&record, normalizedEnvelope, normalizedEntry, nowUTC())
		if acceptErr != nil {
			return false, acceptErr
		}
		r.configDirectory[key] = cloneRuntimeConfigDirectoryRecord(record)
		return duplicate, nil
	}
	duplicate, err := AcceptRuntimeConfigDirectoryRecord(&existing, normalizedEnvelope, normalizedEntry, nowUTC())
	if err != nil {
		return false, err
	}
	if !duplicate {
		r.configDirectory[key] = cloneRuntimeConfigDirectoryRecord(existing)
	}
	return duplicate, nil
}

func (r *MemoryRepository) GetRuntimeConfigDirectory(_ context.Context, kind RuntimeConfigDirectoryKind, id string) (RuntimeConfigDirectoryRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.configDirectory[runtimeConfigDirectoryKey(RuntimeConfigDirectoryKind(strings.TrimSpace(strings.ToLower(string(kind)))), id)]
	if !ok {
		return RuntimeConfigDirectoryRecord{}, ErrNotFound
	}
	return cloneRuntimeConfigDirectoryRecord(record), nil
}

var _ RuntimeConfigDirectoryRepository = (*MemoryRepository)(nil)
