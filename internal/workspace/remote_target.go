package workspace

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
)

var (
	ErrRemoteTargetNotFound    = errors.New("远程主机不存在")
	ErrRemoteTargetInvalid     = errors.New("远程主机配置无效")
	ErrRemoteTargetInUse       = errors.New("远程主机仍被项目引用，不能删除")
	ErrRemoteTargetUnconfirmed = errors.New("远程主机指纹尚未确认")
	ErrRemoteTargetDisabled    = errors.New("远程主机已禁用")
)

type RemoteTransport string

const (
	RemoteTransportSSH RemoteTransport = "ssh"
)

type RemoteTargetStatus string

const (
	RemoteTargetConfigured RemoteTargetStatus = "configured"
	RemoteTargetReady      RemoteTargetStatus = "ready"
	RemoteTargetError      RemoteTargetStatus = "error"
)

// RemoteTarget 是可被多个项目复用的远程连接资源。密码和私钥路径只在服务端内部使用。
type RemoteTarget struct {
	ID                 string             `json:"id"`
	Name               string             `json:"name"`
	Transport          RemoteTransport    `json:"transport"`
	Host               string             `json:"host"`
	Port               int                `json:"port"`
	User               string             `json:"user"`
	AuthType           AuthType           `json:"auth_type"`
	KeyPath            string             `json:"-"`
	Password           string             `json:"-"`
	PasswordConfigured bool               `json:"password_configured,omitempty"`
	HostKeyFingerprint string             `json:"host_key_fingerprint,omitempty"`
	Enabled            bool               `json:"enabled"`
	Status             RemoteTargetStatus `json:"status"`
	StatusMessage      string             `json:"status_message,omitempty"`
	LastCheckedAt      *time.Time         `json:"last_checked_at,omitempty"`
	CreatedAt          time.Time          `json:"created_at"`
	UpdatedAt          time.Time          `json:"updated_at"`
}

// RemoteTargetSaveRequest 用指针区分“保留旧密码”和“清空密码”。
type RemoteTargetSaveRequest struct {
	Target   RemoteTarget
	Password *string
}

// RemoteTargetRepository 是远程主机配置的持久化边界。
type RemoteTargetRepository interface {
	List(context.Context) ([]RemoteTarget, error)
	Get(context.Context, string) (RemoteTarget, error)
	Save(context.Context, RemoteTarget) error
	Delete(context.Context, string) error
}

// WorkspaceUsageCounter 防止项目仍引用远程主机时误删连接资源。
type WorkspaceUsageCounter func(context.Context, string) (int, error)

// RemoteTargetService 管理远程主机、SSH 指纹确认和远程目录选择。
type RemoteTargetService struct {
	repository   RemoteTargetRepository
	usageCounter WorkspaceUsageCounter
}

func NewRemoteTargetService(repository RemoteTargetRepository) (*RemoteTargetService, error) {
	if repository == nil {
		return nil, errors.New("远程主机 Repository 不能为空")
	}
	return &RemoteTargetService{repository: repository}, nil
}

func (s *RemoteTargetService) SetWorkspaceUsageCounter(counter WorkspaceUsageCounter) {
	s.usageCounter = counter
}

func (s *RemoteTargetService) List(ctx context.Context) ([]RemoteTarget, error) {
	items, err := s.repository.List(ctx)
	if err != nil {
		return nil, err
	}
	for index := range items {
		items[index] = normalizeRemoteTarget(items[index])
	}
	return items, nil
}

func (s *RemoteTargetService) Get(ctx context.Context, id string) (RemoteTarget, error) {
	item, err := s.repository.Get(ctx, strings.TrimSpace(id))
	if err != nil {
		return RemoteTarget{}, err
	}
	return normalizeRemoteTarget(item), nil
}

// Save 保存远程连接配置；没有指纹时允许先保存为待确认，项目和目录操作仍会被拒绝。
func (s *RemoteTargetService) Save(ctx context.Context, request RemoteTargetSaveRequest) (RemoteTarget, error) {
	item := normalizeRemoteTarget(request.Target)
	if item.ID == "" {
		item.ID = newID("remote")
	}
	old, oldErr := s.repository.Get(ctx, item.ID)
	if oldErr != nil && !errors.Is(oldErr, ErrRemoteTargetNotFound) {
		return RemoteTarget{}, oldErr
	}
	if oldErr == nil {
		item.CreatedAt = old.CreatedAt
		if request.Password == nil {
			item.Password = old.Password
		}
	} else {
		item.CreatedAt = time.Now().UTC()
	}
	if request.Password != nil {
		item.Password = strings.TrimSpace(*request.Password)
	}
	if err := item.Validate(); err != nil {
		return RemoteTarget{}, err
	}
	item.PasswordConfigured = item.Password != ""
	item.Status = RemoteTargetConfigured
	item.StatusMessage = "尚未完成连接测试"
	item.LastCheckedAt = nil
	item.UpdatedAt = time.Now().UTC()
	if err := s.repository.Save(ctx, item); err != nil {
		return RemoteTarget{}, fmt.Errorf("保存远程主机失败: %w", err)
	}
	return normalizeRemoteTarget(item), nil
}

func (s *RemoteTargetService) Delete(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if _, err := s.Get(ctx, id); err != nil {
		return err
	}
	if s.usageCounter != nil {
		count, err := s.usageCounter(ctx, id)
		if err != nil {
			return err
		}
		if count > 0 {
			return ErrRemoteTargetInUse
		}
	}
	return s.repository.Delete(ctx, id)
}

// TestPreview 测试尚未保存的远程主机；未知指纹只返回结果，不自动建立信任。
func (s *RemoteTargetService) TestPreview(ctx context.Context, request RemoteTargetSaveRequest) (TestResult, error) {
	item := normalizeRemoteTarget(request.Target)
	if item.ID == "" {
		item.ID = "preview"
	}
	if request.Password != nil {
		item.Password = strings.TrimSpace(*request.Password)
	}
	if request.Password == nil && item.ID != "preview" {
		if old, err := s.repository.Get(ctx, item.ID); err == nil {
			item.Password = old.Password
		}
	}
	if err := item.Validate(); err != nil {
		return TestResult{}, err
	}
	return testRemoteTarget(ctx, item)
}

// Test 测试已保存的主机，并只有在指纹已确认时才进入 ready 状态。
func (s *RemoteTargetService) Test(ctx context.Context, id string) (TestResult, error) {
	item, err := s.Get(ctx, id)
	if err != nil {
		return TestResult{}, err
	}
	result, testErr := testRemoteTarget(ctx, item)
	now := time.Now().UTC()
	item.LastCheckedAt = &now
	if testErr != nil {
		if errors.Is(testErr, ErrRemoteTargetUnconfirmed) {
			item.Status = RemoteTargetConfigured
			item.StatusMessage = result.Message
		} else {
			item.Status = RemoteTargetError
			item.StatusMessage = trimRemoteError(testErr)
		}
	} else {
		item.Status = RemoteTargetReady
		item.StatusMessage = result.Message
	}
	if saveErr := s.repository.Save(ctx, item); saveErr != nil {
		// 无论探测成功还是失败，健康状态都必须和探测结果一起可靠落库。
		if testErr == nil {
			return TestResult{}, fmt.Errorf("保存远程主机状态失败: %w", saveErr)
		}
		return result, errors.Join(testErr, fmt.Errorf("保存远程主机状态失败: %w", saveErr))
	}
	return result, testErr
}

// ListDirectories 通过已经确认的 SSH 连接浏览主机上的目录，只返回目录名称和路径。
func (s *RemoteTargetService) ListDirectories(ctx context.Context, id, requested string) (DirectoryListing, error) {
	item, err := s.Get(ctx, id)
	if err != nil {
		return DirectoryListing{}, err
	}
	if !item.Enabled {
		return DirectoryListing{}, ErrRemoteTargetDisabled
	}
	if strings.TrimSpace(item.HostKeyFingerprint) == "" || item.Status != RemoteTargetReady {
		return DirectoryListing{}, ErrRemoteTargetUnconfirmed
	}
	requested = strings.TrimSpace(strings.ReplaceAll(requested, "\\", "/"))
	if requested == "" {
		requested = "/"
	}
	if !strings.HasPrefix(requested, "/") {
		return DirectoryListing{}, fmt.Errorf("%w: 远程目录必须是绝对路径", ErrRemoteTargetInvalid)
	}
	connection, _, err := dialSSH(ctx, item.asWorkspace(), false)
	if err != nil {
		return DirectoryListing{}, err
	}
	defer connection.Close()
	command := "base=$(realpath -- " + shellQuote(path.Clean(requested)) + ") || exit 2; [ -d \"$base\" ] || exit 3; printf 'PATH\\t%s\\n' \"$base\"; " +
		"for p in \"$base\"/* \"$base\"/.[!.]* \"$base\"/..?*; do [ -d \"$p\" ] || continue; printf 'DIR\\t%s\\t%s\\n' \"$p\" \"${p##*/}\"; done"
	result, err := runSSH(ctx, connection.client, command, "", 20*time.Second)
	if err != nil {
		return DirectoryListing{}, err
	}
	listing := DirectoryListing{Path: path.Clean(requested), Parent: path.Dir(path.Clean(requested)), Directories: make([]DirectoryEntry, 0)}
	for _, line := range strings.Split(result.Output, "\n") {
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) == 2 && parts[0] == "PATH" {
			listing.Path = parts[1]
			listing.Parent = path.Dir(parts[1])
		}
		if len(parts) == 3 && parts[0] == "DIR" && parts[1] != "" && parts[2] != "" {
			listing.Directories = append(listing.Directories, DirectoryEntry{Name: parts[2], Path: parts[1], IsDir: true})
		}
	}
	return listing, nil
}

func (t RemoteTarget) Validate() error {
	t.ID = strings.TrimSpace(t.ID)
	t.Name = strings.TrimSpace(t.Name)
	t.Transport = RemoteTransport(strings.TrimSpace(string(t.Transport)))
	t.Host = strings.TrimSpace(t.Host)
	t.User = strings.TrimSpace(t.User)
	t.AuthType = AuthType(strings.TrimSpace(string(t.AuthType)))
	t.KeyPath = strings.TrimSpace(t.KeyPath)
	if t.ID == "" || !regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`).MatchString(t.ID) {
		return fmt.Errorf("%w: 远程主机 ID 必须以小写字母开头，并且只能包含小写字母、数字、_、-", ErrRemoteTargetInvalid)
	}
	if t.Name == "" {
		return fmt.Errorf("%w: 远程主机名称不能为空", ErrRemoteTargetInvalid)
	}
	if t.Transport == "" {
		t.Transport = RemoteTransportSSH
	}
	if t.Transport != RemoteTransportSSH {
		return fmt.Errorf("%w: 暂不支持 %q 传输方式", ErrRemoteTargetInvalid, t.Transport)
	}
	if t.Host == "" || strings.ContainsAny(t.Host, "/?#") {
		return fmt.Errorf("%w: SSH 主机地址无效", ErrRemoteTargetInvalid)
	}
	if t.User == "" {
		return fmt.Errorf("%w: SSH 用户不能为空", ErrRemoteTargetInvalid)
	}
	if t.Port == 0 {
		t.Port = 22
	}
	if t.Port < 1 || t.Port > 65535 {
		return fmt.Errorf("%w: SSH 端口必须在 1-65535 之间", ErrRemoteTargetInvalid)
	}
	if t.AuthType == "" {
		t.AuthType = AuthAgent
	}
	switch t.AuthType {
	case AuthAgent:
	case AuthKeyFile:
		if t.KeyPath == "" {
			return fmt.Errorf("%w: 密钥认证必须填写本机私钥路径", ErrRemoteTargetInvalid)
		}
	case AuthPassword:
		if strings.TrimSpace(t.Password) == "" {
			return fmt.Errorf("%w: 密码认证必须填写 SSH 密码", ErrRemoteTargetInvalid)
		}
	default:
		return fmt.Errorf("%w: SSH 认证方式无效", ErrRemoteTargetInvalid)
	}
	return nil
}

func (t RemoteTarget) asWorkspace() Workspace {
	return Workspace{
		ID: "remote-target-probe", Name: t.Name, Type: TypeSSH, RootPath: "/",
		Host: t.Host, Port: t.Port, User: t.User, AuthType: t.AuthType,
		KeyPath: t.KeyPath, Password: t.Password, HostKeyFingerprint: t.HostKeyFingerprint, Enabled: true,
	}
}

func testRemoteTarget(ctx context.Context, item RemoteTarget) (TestResult, error) {
	connection, fingerprint, err := dialSSH(ctx, item.asWorkspace(), true)
	if err != nil {
		return TestResult{Fingerprint: fingerprint, Message: trimRemoteError(err)}, err
	}
	defer connection.Close()
	if _, err := runSSH(ctx, connection.client, "printf ready", "", 15*time.Second); err != nil {
		return TestResult{Fingerprint: fingerprint, Message: trimRemoteError(err)}, err
	}
	if strings.TrimSpace(item.HostKeyFingerprint) == "" {
		return TestResult{Fingerprint: fingerprint, Message: "SSH 连接正常，请确认主机指纹后保存"}, ErrRemoteTargetUnconfirmed
	}
	return TestResult{OK: true, Fingerprint: fingerprint, Message: "SSH 连接和主机指纹验证正常"}, nil
}

func normalizeRemoteTarget(item RemoteTarget) RemoteTarget {
	item.ID = strings.TrimSpace(item.ID)
	item.Name = strings.TrimSpace(item.Name)
	item.Host = strings.TrimSpace(item.Host)
	item.User = strings.TrimSpace(item.User)
	item.KeyPath = strings.TrimSpace(item.KeyPath)
	item.HostKeyFingerprint = strings.TrimSpace(item.HostKeyFingerprint)
	item.Transport = RemoteTransport(strings.TrimSpace(string(item.Transport)))
	item.AuthType = AuthType(strings.TrimSpace(string(item.AuthType)))
	if item.Transport == "" {
		item.Transport = RemoteTransportSSH
	}
	if item.Port == 0 {
		item.Port = 22
	}
	if item.AuthType == "" {
		item.AuthType = AuthAgent
	}
	item.PasswordConfigured = strings.TrimSpace(item.Password) != ""
	if item.Status == "" {
		item.Status = RemoteTargetConfigured
	}
	return item
}

func trimRemoteError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.TrimSpace(err.Error())
	if len(message) > 1000 {
		return message[:1000]
	}
	return message
}
