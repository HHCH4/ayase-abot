package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"Abot/internal/agent"
)

const (
	MaxRuntimeConfigDirectoryRebindDestinations = MaxRuntimeConfigDirectoryFanoutDestinations
	MaxRuntimeConfigDirectoryRebindID           = 240
	MaxRuntimeConfigDirectoryRebindInvocationID = MaxRuntimeConfigDeliveryInvocationLength
	MaxRuntimeConfigDirectoryRebindFanoutID     = MaxRuntimeConfigDirectoryFanoutID
)

var (
	ErrInvalidRuntimeConfigDirectoryRebind     = errors.New("Runtime 配置目录 rebind 计划无效")
	ErrRuntimeConfigDirectoryRebindUnavailable = errors.New("Runtime 配置目录 rebind 能力不可用")
	ErrRuntimeConfigDirectoryRebindNotEligible = errors.New("Invocation 当前状态不允许 rebind")
	ErrRuntimeConfigDirectoryRebindCapability  = errors.New("目标 Runtime 不支持 rebind 能力")
	ErrRuntimeConfigDirectoryRebindSnapshot    = errors.New("Invocation 配置快照不匹配")
	ErrRuntimeConfigDirectoryRebindConflict    = errors.New("Runtime 配置目录 rebind 计划冲突")
)

// RuntimeConfigDirectoryRebindStatus is the local lifecycle of an explicit
// rebind plan. The first implementation only creates validated plans; apply
// and cancellation are intentionally reserved for a later receipt-backed
// protocol.
type RuntimeConfigDirectoryRebindStatus string

const (
	RuntimeConfigDirectoryRebindValidated RuntimeConfigDirectoryRebindStatus = "validated"
	RuntimeConfigDirectoryRebindCancelled RuntimeConfigDirectoryRebindStatus = "cancelled"
)

func (s RuntimeConfigDirectoryRebindStatus) valid() bool {
	return s == RuntimeConfigDirectoryRebindValidated || s == RuntimeConfigDirectoryRebindCancelled
}

func (s RuntimeConfigDirectoryRebindStatus) ValidForHTTP() bool {
	return s.valid()
}

type RuntimeConfigDirectoryRebindTargetStatus string

const (
	RuntimeConfigDirectoryRebindTargetReady RuntimeConfigDirectoryRebindTargetStatus = "ready"
)

func (s RuntimeConfigDirectoryRebindTargetStatus) valid() bool {
	return s == RuntimeConfigDirectoryRebindTargetReady
}

// RuntimeConfigDirectoryRebindCapability is a route catalog snapshot. The
// source Runtime, rather than the HTTP caller, supplies it through an
// explicitly configured catalog so a client cannot self-assert remote support.
type RuntimeConfigDirectoryRebindCapability struct {
	Destination                   string `json:"destination"`
	Revision                      int64  `json:"revision"`
	SupportsRebind                bool   `json:"supports_rebind"`
	SupportsCheckpoint            bool   `json:"supports_checkpoint"`
	SupportsResume                bool   `json:"supports_resume"`
	SupportsConfigMaterialization bool   `json:"supports_config_materialization"`
}

func (c RuntimeConfigDirectoryRebindCapability) normalize() (RuntimeConfigDirectoryRebindCapability, error) {
	c.Destination = strings.TrimSpace(c.Destination)
	if c.Destination == "" || len(c.Destination) > MaxRuntimeConfigDirectoryTargetLength || c.Revision <= 0 {
		return RuntimeConfigDirectoryRebindCapability{}, fmt.Errorf("%w: route capability metadata 无效", ErrInvalidRuntimeConfigDirectoryRebind)
	}
	return c, nil
}

func (c RuntimeConfigDirectoryRebindCapability) ready(requireConfig bool) bool {
	return c.SupportsRebind && c.SupportsCheckpoint && c.SupportsResume && (!requireConfig || c.SupportsConfigMaterialization)
}

type RuntimeConfigDirectoryRebindTarget struct {
	Destination                   string                                   `json:"destination"`
	CapabilityRevision            int64                                    `json:"capability_revision"`
	SupportsRebind                bool                                     `json:"supports_rebind"`
	SupportsCheckpoint            bool                                     `json:"supports_checkpoint"`
	SupportsResume                bool                                     `json:"supports_resume"`
	SupportsConfigMaterialization bool                                     `json:"supports_config_materialization"`
	Status                        RuntimeConfigDirectoryRebindTargetStatus `json:"status"`
}

func (t RuntimeConfigDirectoryRebindTarget) normalize() (RuntimeConfigDirectoryRebindTarget, error) {
	t.Destination = strings.TrimSpace(t.Destination)
	t.Status = RuntimeConfigDirectoryRebindTargetStatus(strings.TrimSpace(strings.ToLower(string(t.Status))))
	if t.Destination == "" || len(t.Destination) > MaxRuntimeConfigDirectoryTargetLength || t.CapabilityRevision <= 0 || !t.Status.valid() {
		return RuntimeConfigDirectoryRebindTarget{}, fmt.Errorf("%w: rebind target metadata 无效", ErrInvalidRuntimeConfigDirectoryRebind)
	}
	return t, nil
}

func (t RuntimeConfigDirectoryRebindTarget) ready(requireConfig bool) bool {
	return t.SupportsRebind && t.SupportsCheckpoint && t.SupportsResume && (!requireConfig || t.SupportsConfigMaterialization)
}

func targetFromCapability(capability RuntimeConfigDirectoryRebindCapability) (RuntimeConfigDirectoryRebindTarget, error) {
	normalized, err := capability.normalize()
	if err != nil {
		return RuntimeConfigDirectoryRebindTarget{}, err
	}
	return RuntimeConfigDirectoryRebindTarget{
		Destination: normalized.Destination, CapabilityRevision: normalized.Revision,
		SupportsRebind: normalized.SupportsRebind, SupportsCheckpoint: normalized.SupportsCheckpoint,
		SupportsResume: normalized.SupportsResume, SupportsConfigMaterialization: normalized.SupportsConfigMaterialization,
		Status: RuntimeConfigDirectoryRebindTargetReady,
	}, nil
}

// RuntimeConfigDirectoryRebindPlan is metadata-only. It records the
// invocation boundary and route capability snapshots, but never stores the
// invocation message, session contents, attachments, tool arguments or
// credentials.
type RuntimeConfigDirectoryRebindPlan struct {
	ID                   string                               `json:"id"`
	Source               string                               `json:"source"`
	InvocationID         string                               `json:"invocation_id"`
	BoundaryStatus       InvocationStatus                     `json:"boundary_status"`
	ConfigSnapshotDigest string                               `json:"config_snapshot_digest"`
	ConfigFanoutID       string                               `json:"config_fanout_id,omitempty"`
	Destinations         []string                             `json:"destinations"`
	Targets              []RuntimeConfigDirectoryRebindTarget `json:"targets"`
	CapabilitiesDigest   string                               `json:"capabilities_digest"`
	Status               RuntimeConfigDirectoryRebindStatus   `json:"status"`
	Revision             int64                                `json:"revision"`
	IdempotencyKey       string                               `json:"-"`
	CreatedAt            time.Time                            `json:"created_at"`
	UpdatedAt            time.Time                            `json:"updated_at"`
}

type runtimeConfigDirectoryRebindRoute struct {
	destination string
	target      RuntimeConfigDirectoryRebindTarget
}

func normalizeRuntimeConfigDirectoryRebindRoutes(destinations []string, targets []RuntimeConfigDirectoryRebindTarget) ([]string, []RuntimeConfigDirectoryRebindTarget, error) {
	if len(destinations) == 0 || len(destinations) > MaxRuntimeConfigDirectoryRebindDestinations || len(destinations) != len(targets) {
		return nil, nil, fmt.Errorf("%w: rebind destination 数量必须是 1-%d 且与 target 一致", ErrInvalidRuntimeConfigDirectoryRebind, MaxRuntimeConfigDirectoryRebindDestinations)
	}
	routes := make([]runtimeConfigDirectoryRebindRoute, len(destinations))
	for index, destination := range destinations {
		destination = strings.TrimSpace(destination)
		target, err := targets[index].normalize()
		if err != nil {
			return nil, nil, err
		}
		if destination == "" || len(destination) > MaxRuntimeConfigDirectoryTargetLength || target.Destination != destination {
			return nil, nil, fmt.Errorf("%w: rebind destination/target 不一致", ErrInvalidRuntimeConfigDirectoryRebind)
		}
		routes[index] = runtimeConfigDirectoryRebindRoute{destination: destination, target: target}
	}
	sort.Slice(routes, func(left, right int) bool { return routes[left].destination < routes[right].destination })
	normalizedDestinations := make([]string, len(routes))
	normalizedTargets := make([]RuntimeConfigDirectoryRebindTarget, len(routes))
	for index, route := range routes {
		if index > 0 && routes[index-1].destination == route.destination {
			return nil, nil, fmt.Errorf("%w: rebind destination 不能重复", ErrInvalidRuntimeConfigDirectoryRebind)
		}
		normalizedDestinations[index] = route.destination
		normalizedTargets[index] = route.target
	}
	return normalizedDestinations, normalizedTargets, nil
}

func runtimeConfigDirectoryRebindCapabilitiesDigest(targets []RuntimeConfigDirectoryRebindTarget) (string, error) {
	canonical := make([]RuntimeConfigDirectoryRebindTarget, len(targets))
	copy(canonical, targets)
	for index := range canonical {
		normalized, err := canonical[index].normalize()
		if err != nil {
			return "", err
		}
		canonical[index] = normalized
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("%w: route capability 编码失败", ErrInvalidRuntimeConfigDirectoryRebind)
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func RuntimeConfigDirectoryRebindID(source, invocationID, configSnapshotDigest, configFanoutID string, destinations []string, capabilitiesDigest, idempotencyKey string) string {
	routes := append([]string(nil), destinations...)
	for index := range routes {
		routes[index] = strings.TrimSpace(routes[index])
	}
	sort.Strings(routes)
	material := strings.Join(append([]string{strings.TrimSpace(source), strings.TrimSpace(invocationID), strings.TrimSpace(strings.ToLower(configSnapshotDigest)), strings.TrimSpace(configFanoutID), strings.TrimSpace(capabilitiesDigest), strings.TrimSpace(idempotencyKey)}, routes...), "\x00")
	sum := sha256.Sum256([]byte(material))
	return "runtime-config-directory-rebind-" + hex.EncodeToString(sum[:16])
}

func (p RuntimeConfigDirectoryRebindPlan) normalize(now time.Time) (RuntimeConfigDirectoryRebindPlan, error) {
	p.ID = strings.TrimSpace(p.ID)
	p.Source = strings.TrimSpace(p.Source)
	p.InvocationID = strings.TrimSpace(p.InvocationID)
	p.ConfigSnapshotDigest = strings.TrimSpace(strings.ToLower(p.ConfigSnapshotDigest))
	p.ConfigFanoutID = strings.TrimSpace(p.ConfigFanoutID)
	p.IdempotencyKey = strings.TrimSpace(p.IdempotencyKey)
	p.Status = RuntimeConfigDirectoryRebindStatus(strings.TrimSpace(strings.ToLower(string(p.Status))))
	if p.Source == "" || len(p.Source) > MaxRuntimeConfigDirectorySourceLength || p.InvocationID == "" || len(p.InvocationID) > MaxRuntimeConfigDirectoryRebindInvocationID || !runtimeConfigSnapshotDigestValid(p.ConfigSnapshotDigest) {
		return RuntimeConfigDirectoryRebindPlan{}, fmt.Errorf("%w: rebind immutable metadata 不完整", ErrInvalidRuntimeConfigDirectoryRebind)
	}
	if len(p.ID) == 0 || len(p.ID) > MaxRuntimeConfigDirectoryRebindID || len(p.ConfigFanoutID) > MaxRuntimeConfigDirectoryRebindFanoutID || len(p.IdempotencyKey) > MaxRuntimeConfigDirectoryIdempotency {
		return RuntimeConfigDirectoryRebindPlan{}, fmt.Errorf("%w: rebind metadata 超出长度限制", ErrInvalidRuntimeConfigDirectoryRebind)
	}
	if p.BoundaryStatus != InvocationWaitingTool && p.BoundaryStatus != InvocationWaitingApproval && p.BoundaryStatus != InvocationWaitingUser {
		return RuntimeConfigDirectoryRebindPlan{}, fmt.Errorf("%w: Invocation boundary 不受支持", ErrRuntimeConfigDirectoryRebindNotEligible)
	}
	destinations, targets, err := normalizeRuntimeConfigDirectoryRebindRoutes(p.Destinations, p.Targets)
	if err != nil {
		return RuntimeConfigDirectoryRebindPlan{}, err
	}
	p.Destinations, p.Targets = destinations, targets
	requireConfigMaterialization := p.ConfigFanoutID != ""
	for _, target := range p.Targets {
		if !target.ready(requireConfigMaterialization) {
			return RuntimeConfigDirectoryRebindPlan{}, fmt.Errorf("%w: target capability 不满足 rebind 要求", ErrRuntimeConfigDirectoryRebindCapability)
		}
	}
	capabilitiesDigest, err := runtimeConfigDirectoryRebindCapabilitiesDigest(targets)
	if err != nil {
		return RuntimeConfigDirectoryRebindPlan{}, err
	}
	if p.CapabilitiesDigest != capabilitiesDigest {
		return RuntimeConfigDirectoryRebindPlan{}, fmt.Errorf("%w: route capability digest 不一致", ErrRuntimeConfigDirectoryRebindCapability)
	}
	expectedID := RuntimeConfigDirectoryRebindID(p.Source, p.InvocationID, p.ConfigSnapshotDigest, p.ConfigFanoutID, p.Destinations, p.CapabilitiesDigest, p.IdempotencyKey)
	if p.ID != expectedID {
		return RuntimeConfigDirectoryRebindPlan{}, fmt.Errorf("%w: rebind plan identity 不一致", ErrRuntimeConfigDirectoryRebindConflict)
	}
	if p.Status == "" {
		p.Status = RuntimeConfigDirectoryRebindValidated
	}
	if !p.Status.valid() {
		return RuntimeConfigDirectoryRebindPlan{}, fmt.Errorf("%w: rebind status 不受支持", ErrInvalidRuntimeConfigDirectoryRebind)
	}
	if p.Revision <= 0 {
		p.Revision = 1
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	} else {
		p.CreatedAt = p.CreatedAt.UTC()
	}
	if p.UpdatedAt.IsZero() {
		p.UpdatedAt = p.CreatedAt
	} else {
		p.UpdatedAt = p.UpdatedAt.UTC()
	}
	return p, nil
}

func (p RuntimeConfigDirectoryRebindPlan) Normalize(now time.Time) (RuntimeConfigDirectoryRebindPlan, error) {
	return p.normalize(now)
}

func cloneRuntimeConfigDirectoryRebindPlan(plan RuntimeConfigDirectoryRebindPlan) RuntimeConfigDirectoryRebindPlan {
	plan.Destinations = append([]string(nil), plan.Destinations...)
	plan.Targets = append([]RuntimeConfigDirectoryRebindTarget(nil), plan.Targets...)
	return plan
}

func (p RuntimeConfigDirectoryRebindPlan) Matches(other RuntimeConfigDirectoryRebindPlan) bool {
	left, leftErr := p.normalize(time.Time{})
	right, rightErr := other.normalize(time.Time{})
	if leftErr != nil || rightErr != nil {
		return false
	}
	return left.ID == right.ID && left.Source == right.Source && left.InvocationID == right.InvocationID && left.BoundaryStatus == right.BoundaryStatus && left.ConfigSnapshotDigest == right.ConfigSnapshotDigest && left.ConfigFanoutID == right.ConfigFanoutID && left.CapabilitiesDigest == right.CapabilitiesDigest && left.IdempotencyKey == right.IdempotencyKey && equalRuntimeConfigDirectoryStrings(left.Destinations, right.Destinations) && equalRuntimeConfigDirectoryRebindTargets(left.Targets, right.Targets)
}

func equalRuntimeConfigDirectoryRebindTargets(left, right []RuntimeConfigDirectoryRebindTarget) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// NewRuntimeConfigDirectoryRebindPlan validates the immutable invocation
// boundary and authoritative route capability snapshots before deriving the
// stable plan identity. It never contacts a remote Runtime.
func NewRuntimeConfigDirectoryRebindPlan(source string, invocation Invocation, destinations []string, capabilities []RuntimeConfigDirectoryRebindCapability, configFanoutID, expectedSnapshotDigest, idempotencyKey string, now time.Time) (RuntimeConfigDirectoryRebindPlan, error) {
	source = strings.TrimSpace(source)
	invocationID := strings.TrimSpace(invocation.ID)
	expectedSnapshotDigest = strings.TrimSpace(strings.ToLower(expectedSnapshotDigest))
	if source == "" || len(source) > MaxRuntimeConfigDirectorySourceLength {
		return RuntimeConfigDirectoryRebindPlan{}, fmt.Errorf("%w: source 无效", ErrInvalidRuntimeConfigDirectoryRebind)
	}
	if invocationID == "" || strings.TrimSpace(invocation.SessionID) == "" {
		return RuntimeConfigDirectoryRebindPlan{}, fmt.Errorf("%w: Invocation identity 不完整", ErrRuntimeConfigDirectoryRebindNotEligible)
	}
	if invocation.Status != InvocationWaitingTool && invocation.Status != InvocationWaitingApproval && invocation.Status != InvocationWaitingUser {
		return RuntimeConfigDirectoryRebindPlan{}, fmt.Errorf("%w: Invocation status=%s", ErrRuntimeConfigDirectoryRebindNotEligible, invocation.Status)
	}
	if strings.TrimSpace(invocation.LeaseOwner) != "" || invocation.LeaseExpiresAt != nil {
		return RuntimeConfigDirectoryRebindPlan{}, fmt.Errorf("%w: Invocation 仍持有执行租约", ErrRuntimeConfigDirectoryRebindNotEligible)
	}
	storedDigest := strings.TrimSpace(strings.ToLower(invocation.ConfigSnapshotDigest))
	if !runtimeConfigSnapshotDigestValid(storedDigest) {
		return RuntimeConfigDirectoryRebindPlan{}, fmt.Errorf("%w: Invocation 没有有效配置快照", ErrRuntimeConfigDirectoryRebindSnapshot)
	}
	if !runtimeConfigSnapshotDigestValid(expectedSnapshotDigest) || storedDigest != expectedSnapshotDigest {
		return RuntimeConfigDirectoryRebindPlan{}, fmt.Errorf("%w: expected digest 与当前快照不一致", ErrRuntimeConfigDirectoryRebindSnapshot)
	}
	if strings.TrimSpace(invocation.ConfigSnapshot) != "" && agent.RuntimeConfigSnapshotDigest(invocation.ConfigSnapshot) != storedDigest {
		return RuntimeConfigDirectoryRebindPlan{}, fmt.Errorf("%w: Invocation 隐藏配置快照 digest 不一致", ErrRuntimeConfigDirectoryRebindSnapshot)
	}
	if len(capabilities) != len(destinations) {
		return RuntimeConfigDirectoryRebindPlan{}, fmt.Errorf("%w: route capability 数量不一致", ErrRuntimeConfigDirectoryRebindCapability)
	}
	targets := make([]RuntimeConfigDirectoryRebindTarget, len(capabilities))
	for index, capability := range capabilities {
		target, err := targetFromCapability(capability)
		if err != nil {
			return RuntimeConfigDirectoryRebindPlan{}, err
		}
		requireConfig := strings.TrimSpace(configFanoutID) != ""
		if !capability.ready(requireConfig) {
			return RuntimeConfigDirectoryRebindPlan{}, fmt.Errorf("%w: destination=%s revision=%d", ErrRuntimeConfigDirectoryRebindCapability, capability.Destination, capability.Revision)
		}
		targets[index] = target
	}
	normalizedDestinations, normalizedTargets, err := normalizeRuntimeConfigDirectoryRebindRoutes(destinations, targets)
	if err != nil {
		return RuntimeConfigDirectoryRebindPlan{}, err
	}
	capabilitiesDigest, err := runtimeConfigDirectoryRebindCapabilitiesDigest(normalizedTargets)
	if err != nil {
		return RuntimeConfigDirectoryRebindPlan{}, err
	}
	configFanoutID = strings.TrimSpace(configFanoutID)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	plan := RuntimeConfigDirectoryRebindPlan{
		ID:     RuntimeConfigDirectoryRebindID(source, invocationID, storedDigest, configFanoutID, normalizedDestinations, capabilitiesDigest, idempotencyKey),
		Source: source, InvocationID: invocationID, BoundaryStatus: invocation.Status,
		ConfigSnapshotDigest: storedDigest, ConfigFanoutID: configFanoutID, Destinations: normalizedDestinations,
		Targets: normalizedTargets, CapabilitiesDigest: capabilitiesDigest, Status: RuntimeConfigDirectoryRebindValidated,
		Revision: 1, IdempotencyKey: idempotencyKey,
	}
	return plan.normalize(now)
}

// RuntimeConfigDirectoryRebindRouteCatalog is the authoritative source-side
// capability lookup. Implementations must not trust capabilities supplied by
// an HTTP caller.
type RuntimeConfigDirectoryRebindRouteCatalog interface {
	ResolveRuntimeConfigDirectoryRebindCapability(context.Context, string, string) (RuntimeConfigDirectoryRebindCapability, error)
}

// MemoryRuntimeConfigDirectoryRebindRouteCatalog is a small explicit catalog
// useful for local hosts and deterministic tests. Production hosts can inject
// a catalog backed by their route registry or control plane.
type MemoryRuntimeConfigDirectoryRebindRouteCatalog struct {
	mu     sync.RWMutex
	routes map[string]RuntimeConfigDirectoryRebindCapability
}

func NewMemoryRuntimeConfigDirectoryRebindRouteCatalog() *MemoryRuntimeConfigDirectoryRebindRouteCatalog {
	return &MemoryRuntimeConfigDirectoryRebindRouteCatalog{routes: make(map[string]RuntimeConfigDirectoryRebindCapability)}
}

func runtimeConfigDirectoryRebindRouteKey(source, destination string) string {
	return strings.TrimSpace(source) + "\x00" + strings.TrimSpace(destination)
}

func (c *MemoryRuntimeConfigDirectoryRebindRouteCatalog) Set(source string, capability RuntimeConfigDirectoryRebindCapability) error {
	if c == nil {
		return ErrRuntimeConfigDirectoryRebindUnavailable
	}
	normalized, err := capability.normalize()
	if err != nil {
		return err
	}
	source = strings.TrimSpace(source)
	if source == "" || len(source) > MaxRuntimeConfigDirectorySourceLength {
		return fmt.Errorf("%w: source 无效", ErrInvalidRuntimeConfigDirectoryRebind)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.routes == nil {
		c.routes = make(map[string]RuntimeConfigDirectoryRebindCapability)
	}
	c.routes[runtimeConfigDirectoryRebindRouteKey(source, normalized.Destination)] = normalized
	return nil
}

func (c *MemoryRuntimeConfigDirectoryRebindRouteCatalog) ResolveRuntimeConfigDirectoryRebindCapability(_ context.Context, source, destination string) (RuntimeConfigDirectoryRebindCapability, error) {
	if c == nil {
		return RuntimeConfigDirectoryRebindCapability{}, ErrRuntimeConfigDirectoryRebindUnavailable
	}
	c.mu.RLock()
	capability, ok := c.routes[runtimeConfigDirectoryRebindRouteKey(source, destination)]
	c.mu.RUnlock()
	if !ok {
		return RuntimeConfigDirectoryRebindCapability{}, ErrNotFound
	}
	return capability, nil
}

type RuntimeConfigDirectoryRebindPlanRepository interface {
	EnqueueRuntimeConfigDirectoryRebindPlan(context.Context, RuntimeConfigDirectoryRebindPlan) (RuntimeConfigDirectoryRebindPlan, error)
	GetRuntimeConfigDirectoryRebindPlan(context.Context, string) (RuntimeConfigDirectoryRebindPlan, error)
	ListRuntimeConfigDirectoryRebindPlans(context.Context, string, string, RuntimeConfigDirectoryRebindStatus, int) ([]RuntimeConfigDirectoryRebindPlan, error)
}

func (c *Coordinator) SetRuntimeConfigDirectoryRebindRouteCatalog(catalog RuntimeConfigDirectoryRebindRouteCatalog) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.rebindRouteCatalog = catalog
	c.mu.Unlock()
}

func (c *Coordinator) rebindRouteCatalogConfig() RuntimeConfigDirectoryRebindRouteCatalog {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rebindRouteCatalog
}

// CreateRuntimeConfigDirectoryRebindPlan creates only a local validated plan.
// It does not mutate the Invocation or call a destination Runtime.
func (c *Coordinator) CreateRuntimeConfigDirectoryRebindPlan(ctx context.Context, source, invocationID string, destinations []string, expectedSnapshotDigest, configFanoutID, idempotencyKey string, now time.Time) (RuntimeConfigDirectoryRebindPlan, error) {
	if c == nil || c.repo == nil {
		return RuntimeConfigDirectoryRebindPlan{}, ErrRuntimeConfigDirectoryRebindUnavailable
	}
	repo, ok := c.repo.(RuntimeConfigDirectoryRebindPlanRepository)
	if !ok {
		return RuntimeConfigDirectoryRebindPlan{}, ErrRuntimeConfigDirectoryRebindUnavailable
	}
	catalog := c.rebindRouteCatalogConfig()
	if catalog == nil {
		return RuntimeConfigDirectoryRebindPlan{}, ErrRuntimeConfigDirectoryRebindUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	invocation, err := c.repo.GetInvocation(ctx, strings.TrimSpace(invocationID))
	if err != nil {
		return RuntimeConfigDirectoryRebindPlan{}, err
	}
	if strings.TrimSpace(configFanoutID) != "" {
		fanoutRepo, fanoutOK := c.repo.(RuntimeConfigDirectoryFanoutRepository)
		if !fanoutOK {
			return RuntimeConfigDirectoryRebindPlan{}, ErrRuntimeConfigDirectoryRebindUnavailable
		}
		fanout, fanoutErr := fanoutRepo.GetRuntimeConfigDirectoryFanout(ctx, configFanoutID)
		if fanoutErr != nil {
			return RuntimeConfigDirectoryRebindPlan{}, fanoutErr
		}
		if fanout.Status != RuntimeConfigDirectoryFanoutCompleted || fanout.Source != strings.TrimSpace(source) || !equalStringSet(fanout.Destinations, destinations) {
			return RuntimeConfigDirectoryRebindPlan{}, fmt.Errorf("%w: config fanout 未完成或 route 不一致", ErrRuntimeConfigDirectoryRebindConflict)
		}
	}
	capabilities := make([]RuntimeConfigDirectoryRebindCapability, len(destinations))
	for index, destination := range destinations {
		capability, capabilityErr := catalog.ResolveRuntimeConfigDirectoryRebindCapability(ctx, strings.TrimSpace(source), strings.TrimSpace(destination))
		if capabilityErr != nil {
			if errors.Is(capabilityErr, ErrNotFound) {
				return RuntimeConfigDirectoryRebindPlan{}, fmt.Errorf("%w: destination=%s", ErrRuntimeConfigDirectoryRebindCapability, strings.TrimSpace(destination))
			}
			return RuntimeConfigDirectoryRebindPlan{}, capabilityErr
		}
		capabilities[index] = capability
	}
	plan, err := NewRuntimeConfigDirectoryRebindPlan(source, invocation, destinations, capabilities, configFanoutID, expectedSnapshotDigest, idempotencyKey, now)
	if err != nil {
		return RuntimeConfigDirectoryRebindPlan{}, err
	}
	return repo.EnqueueRuntimeConfigDirectoryRebindPlan(ctx, plan)
}

func equalStringSet(left, right []string) bool {
	leftCopy, rightCopy := append([]string(nil), left...), append([]string(nil), right...)
	for index := range leftCopy {
		leftCopy[index] = strings.TrimSpace(leftCopy[index])
	}
	for index := range rightCopy {
		rightCopy[index] = strings.TrimSpace(rightCopy[index])
	}
	sort.Strings(leftCopy)
	sort.Strings(rightCopy)
	return equalRuntimeConfigDirectoryStrings(leftCopy, rightCopy)
}

func (c *Coordinator) GetRuntimeConfigDirectoryRebindPlan(ctx context.Context, id string) (RuntimeConfigDirectoryRebindPlan, error) {
	if c == nil || c.repo == nil {
		return RuntimeConfigDirectoryRebindPlan{}, ErrRuntimeConfigDirectoryRebindUnavailable
	}
	repo, ok := c.repo.(RuntimeConfigDirectoryRebindPlanRepository)
	if !ok {
		return RuntimeConfigDirectoryRebindPlan{}, ErrRuntimeConfigDirectoryRebindUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return repo.GetRuntimeConfigDirectoryRebindPlan(ctx, id)
}

func (c *Coordinator) ListRuntimeConfigDirectoryRebindPlans(ctx context.Context, source, invocationID string, status RuntimeConfigDirectoryRebindStatus, limit int) ([]RuntimeConfigDirectoryRebindPlan, error) {
	if c == nil || c.repo == nil {
		return nil, ErrRuntimeConfigDirectoryRebindUnavailable
	}
	repo, ok := c.repo.(RuntimeConfigDirectoryRebindPlanRepository)
	if !ok {
		return nil, ErrRuntimeConfigDirectoryRebindUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return repo.ListRuntimeConfigDirectoryRebindPlans(ctx, source, invocationID, status, limit)
}
