package config

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// PersonaSchemaVersion is bumped when the persisted persona contract changes.
const PersonaSchemaVersion = 1

const (
	DefaultPersonaID           = "default"
	DefaultPersonaName         = "默认人格"
	DefaultPersonaInstruction  = "你是 Abot，一个可靠、简洁、遵守用户意图的中文 AI 助手。"
	maxPersonaIDBytes          = 64
	maxPersonaNameBytes        = 200
	maxPersonaDescriptionBytes = 2000
	maxPersonaInstructionBytes = 64 * 1024
	maxPersonaTargetIDBytes    = 200
)

// Persona is an independently versioned runtime identity. Instruction is the
// only content injected into a model request; the rest is metadata for
// selection, auditing, and UI projection.
type Persona struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Instruction string    `json:"instruction"`
	Revision    int       `json:"revision"`
	IsDefault   bool      `json:"is_default"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// PersonaRevision is an immutable audit snapshot of one persona revision.
type PersonaRevision struct {
	PersonaID   string    `json:"persona_id"`
	Number      int       `json:"revision"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Instruction string    `json:"instruction"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
}

// PersonaRepository is deliberately separate from the config profile
// repository. This lets an embedding host provide a catalog without coupling
// persona lifecycle to the profile JSON schema.
type PersonaRepository interface {
	ListPersonas(context.Context) ([]Persona, error)
	GetPersona(context.Context, string) (Persona, error)
	ListPersonaRevisions(context.Context, string) ([]PersonaRevision, error)
	SavePersona(context.Context, Persona, PersonaRevision) error
	DeletePersona(context.Context, string) error
	SetDefaultPersona(context.Context, string) error
	DefaultPersonaID(context.Context) (string, error)
	BindPersona(context.Context, BindingScope, string, string) error
	GetPersonaBinding(context.Context, BindingScope, string) (string, error)
}

// PersonaService is the sole business entry point for catalog, revision, and
// selection operations.
type PersonaService struct {
	repository PersonaRepository
	writeMu    sync.Mutex
}

func NewPersonaService(repository PersonaRepository) (*PersonaService, error) {
	if repository == nil {
		return nil, errors.New("人格 Repository 不能为空")
	}
	return &PersonaService{repository: repository}, nil
}

// EnsureDefault creates the built-in persona on a fresh store and repairs old
// stores that have personas but no default marker.
func (s *PersonaService) EnsureDefault(ctx context.Context) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	items, err := s.repository.ListPersonas(ctx)
	if err != nil {
		return fmt.Errorf("加载人格目录失败: %w", err)
	}
	if len(items) == 0 {
		now := time.Now().UTC()
		item := Persona{
			ID: DefaultPersonaID, Name: DefaultPersonaName, Instruction: DefaultPersonaInstruction,
			Revision: 1, IsDefault: true, Enabled: true, CreatedAt: now, UpdatedAt: now,
		}
		return s.repository.SavePersona(ctx, item, PersonaRevision{
			PersonaID: item.ID, Number: item.Revision, Name: item.Name,
			Instruction: item.Instruction, Enabled: item.Enabled, CreatedAt: now,
		})
	}
	for _, item := range items {
		if item.IsDefault {
			return nil
		}
	}
	sort.Slice(items, func(i, j int) bool { return strings.TrimSpace(items[i].ID) < strings.TrimSpace(items[j].ID) })
	return s.repository.SetDefaultPersona(ctx, strings.TrimSpace(items[0].ID))
}

func (s *PersonaService) ListPersonas(ctx context.Context) ([]Persona, error) {
	items, err := s.repository.ListPersonas(ctx)
	if err != nil {
		return nil, err
	}
	for i := range items {
		items[i] = normalizePersona(items[i])
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].IsDefault != items[j].IsDefault {
			return items[i].IsDefault
		}
		if items[i].Name != items[j].Name {
			return items[i].Name < items[j].Name
		}
		return items[i].ID < items[j].ID
	})
	return items, nil
}

func (s *PersonaService) GetPersona(ctx context.Context, id string) (Persona, error) {
	id = strings.TrimSpace(id)
	if err := validatePersonaID(id); err != nil {
		return Persona{}, err
	}
	item, err := s.repository.GetPersona(ctx, id)
	if err != nil {
		return Persona{}, err
	}
	return normalizePersona(item), nil
}

func (s *PersonaService) ListRevisions(ctx context.Context, id string) ([]PersonaRevision, error) {
	item, err := s.GetPersona(ctx, id)
	if err != nil {
		return nil, err
	}
	items, err := s.repository.ListPersonaRevisions(ctx, item.ID)
	if err != nil {
		return nil, err
	}
	for i := range items {
		items[i].PersonaID = item.ID
	}
	if len(items) == 0 {
		return []PersonaRevision{{
			PersonaID: item.ID, Number: item.Revision, Name: item.Name,
			Description: item.Description, Instruction: item.Instruction, Enabled: item.Enabled,
			CreatedAt: item.UpdatedAt,
		}}, nil
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Number != items[j].Number {
			return items[i].Number > items[j].Number
		}
		return items[i].CreatedAt.After(items[j].CreatedAt)
	})
	return items, nil
}

// SavePersona creates or updates a persona. A nil enabled argument preserves
// the current state on update and enables a newly-created persona.
func (s *PersonaService) SavePersona(ctx context.Context, id, name, description, instruction string, enabled *bool) (Persona, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	id = strings.TrimSpace(id)
	if id == "" {
		id = newID("persona")
	}
	if err := validatePersonaID(id); err != nil {
		return Persona{}, err
	}
	name = strings.TrimSpace(name)
	description = strings.TrimSpace(description)
	instruction = strings.TrimSpace(instruction)
	if name == "" {
		return Persona{}, fmt.Errorf("%w: 人格名称不能为空", ErrInvalidRequest)
	}
	if instruction == "" {
		return Persona{}, fmt.Errorf("%w: 人格指令不能为空", ErrInvalidRequest)
	}
	if err := validatePersonaText(name, maxPersonaNameBytes, "人格名称"); err != nil {
		return Persona{}, err
	}
	if err := validatePersonaText(description, maxPersonaDescriptionBytes, "人格描述"); err != nil {
		return Persona{}, err
	}
	if err := validatePersonaText(instruction, maxPersonaInstructionBytes, "人格指令"); err != nil {
		return Persona{}, err
	}
	old, oldErr := s.repository.GetPersona(ctx, id)
	if oldErr != nil && !errors.Is(oldErr, ErrNotFound) {
		return Persona{}, oldErr
	}
	if oldErr == nil {
		if old.IsDefault && enabled != nil && !*enabled {
			return Persona{}, fmt.Errorf("%w: 默认人格不能禁用，请先切换默认人格", ErrInvalidRequest)
		}
		if conflict, err := s.personaNameConflict(ctx, name, id); err != nil {
			return Persona{}, err
		} else if conflict {
			return Persona{}, fmt.Errorf("%w: 人格名称 %q 已存在", ErrConflict, name)
		}
	} else if conflict, err := s.personaNameConflict(ctx, name, ""); err != nil {
		return Persona{}, err
	} else if conflict {
		return Persona{}, fmt.Errorf("%w: 人格名称 %q 已存在", ErrConflict, name)
	}

	now := time.Now().UTC()
	item := Persona{ID: id, Name: name, Description: description, Instruction: instruction, Revision: 1, Enabled: true, CreatedAt: now, UpdatedAt: now}
	if oldErr == nil {
		item.Revision = old.Revision + 1
		item.IsDefault = old.IsDefault
		item.Enabled = old.Enabled
		item.CreatedAt = old.CreatedAt
		if enabled != nil {
			item.Enabled = *enabled
		}
	} else if enabled != nil {
		item.Enabled = *enabled
	}
	if err := s.repository.SavePersona(ctx, item, PersonaRevision{
		PersonaID: id, Number: item.Revision, Name: item.Name, Description: item.Description,
		Instruction: item.Instruction, Enabled: item.Enabled, CreatedAt: now,
	}); err != nil {
		return Persona{}, fmt.Errorf("保存人格失败: %w", err)
	}
	return item, nil
}

func (s *PersonaService) DeletePersona(ctx context.Context, id string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	item, err := s.GetPersona(ctx, id)
	if err != nil {
		return err
	}
	if item.IsDefault {
		return ErrDefaultPersona
	}
	return s.repository.DeletePersona(ctx, item.ID)
}

func (s *PersonaService) SetDefaultPersona(ctx context.Context, id string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	item, err := s.GetPersona(ctx, id)
	if err != nil {
		return err
	}
	if !item.Enabled {
		return fmt.Errorf("%w: 人格 %q 已禁用，不能设为默认人格", ErrInvalidRequest, item.ID)
	}
	return s.repository.SetDefaultPersona(ctx, strings.TrimSpace(id))
}

func (s *PersonaService) DefaultPersonaID(ctx context.Context) (string, error) {
	id, err := s.repository.DefaultPersonaID(ctx)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(id), nil
}

func (s *PersonaService) Bind(ctx context.Context, scope BindingScope, targetID, personaID string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if scope != BindingBot && scope != BindingConversation {
		return fmt.Errorf("%w: 人格绑定范围无效", ErrInvalidRequest)
	}
	targetID = strings.TrimSpace(targetID)
	personaID = strings.TrimSpace(personaID)
	if targetID == "" {
		return fmt.Errorf("%w: 人格绑定目标不能为空", ErrInvalidRequest)
	}
	if err := validatePersonaText(targetID, maxPersonaTargetIDBytes, "人格绑定目标"); err != nil {
		return err
	}
	if personaID != "" {
		item, err := s.GetPersona(ctx, personaID)
		if err != nil {
			return err
		}
		if !item.Enabled {
			return fmt.Errorf("%w: 人格 %q 已禁用", ErrInvalidRequest, item.ID)
		}
	}
	return s.repository.BindPersona(ctx, scope, targetID, personaID)
}

func (s *PersonaService) Binding(ctx context.Context, scope BindingScope, targetID string) (string, error) {
	if scope != BindingBot && scope != BindingConversation {
		return "", fmt.Errorf("%w: 人格绑定范围无效", ErrInvalidRequest)
	}
	if strings.TrimSpace(targetID) == "" {
		return "", fmt.Errorf("%w: 人格绑定目标不能为空", ErrInvalidRequest)
	}
	if err := validatePersonaText(strings.TrimSpace(targetID), maxPersonaTargetIDBytes, "人格绑定目标"); err != nil {
		return "", err
	}
	return s.repository.GetPersonaBinding(ctx, scope, strings.TrimSpace(targetID))
}

// ResolveSelection applies conversation > bot > profile fallback precedence.
// It returns selected=false when no explicit catalog persona was selected,
// allowing legacy profile.system_prompt behavior to remain unchanged.
func (s *PersonaService) ResolveSelection(ctx context.Context, botID, conversationID, fallbackID string) (Persona, bool, error) {
	botID = strings.TrimSpace(botID)
	conversationID = strings.TrimSpace(conversationID)
	selected := ""
	if conversationID != "" {
		bound, err := s.repository.GetPersonaBinding(ctx, BindingConversation, conversationID)
		if err != nil {
			return Persona{}, false, err
		}
		selected = strings.TrimSpace(bound)
	}
	if selected == "" && botID != "" {
		bound, err := s.repository.GetPersonaBinding(ctx, BindingBot, botID)
		if err != nil {
			return Persona{}, false, err
		}
		selected = strings.TrimSpace(bound)
	}
	if selected == "" {
		selected = strings.TrimSpace(fallbackID)
	}
	if selected == "" {
		return Persona{}, false, nil
	}
	item, err := s.GetPersona(ctx, selected)
	if err != nil {
		return Persona{}, false, err
	}
	if !item.Enabled {
		return Persona{}, false, fmt.Errorf("%w: 人格 %q 已禁用", ErrInvalidRequest, item.ID)
	}
	return item, true, nil
}

func (s *PersonaService) personaNameConflict(ctx context.Context, name, exceptID string) (bool, error) {
	name = strings.TrimSpace(name)
	items, err := s.repository.ListPersonas(ctx)
	if err != nil {
		return false, err
	}
	for _, item := range items {
		if strings.TrimSpace(item.ID) != exceptID && strings.EqualFold(strings.TrimSpace(item.Name), name) {
			return true, nil
		}
	}
	return false, nil
}

func normalizePersona(item Persona) Persona {
	item.ID = strings.TrimSpace(item.ID)
	item.Name = strings.TrimSpace(item.Name)
	item.Description = strings.TrimSpace(item.Description)
	item.Instruction = strings.TrimSpace(item.Instruction)
	return item
}

func validatePersonaID(id string) error {
	if id == "" || len(id) > maxPersonaIDBytes || id[0] < 'a' || id[0] > 'z' {
		return fmt.Errorf("%w: 人格 ID 必须以小写字母开头且不超过 %d 个字节", ErrInvalidRequest, maxPersonaIDBytes)
	}
	for _, value := range id {
		if value != '_' && value != '-' && (value < 'a' || value > 'z') && (value < '0' || value > '9') {
			return fmt.Errorf("%w: 人格 ID 只能使用小写字母、数字、_、-", ErrInvalidRequest)
		}
	}
	return nil
}

func validatePersonaText(value string, maxBytes int, label string) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("%w: %s 必须是有效 UTF-8", ErrInvalidRequest, label)
	}
	if len(value) > maxBytes {
		return fmt.Errorf("%w: %s 不能超过 %d 个字节", ErrInvalidRequest, label, maxBytes)
	}
	return nil
}
