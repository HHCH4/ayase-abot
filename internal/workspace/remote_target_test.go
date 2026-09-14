package workspace

import (
	"context"
	"errors"
	"testing"
)

func TestRemoteTargetSaveRequiresValidAuthentication(t *testing.T) {
	service, err := NewRemoteTargetService(newRemoteTargetMemoryRepository())
	if err != nil {
		t.Fatal(err)
	}

	_, err = service.Save(context.Background(), RemoteTargetSaveRequest{Target: RemoteTarget{
		ID: "home", Name: "家庭开发机", Host: "192.168.1.20", User: "dev", AuthType: AuthPassword,
	}})
	if !errors.Is(err, ErrRemoteTargetInvalid) {
		t.Fatalf("缺少 SSH 密码时错误 = %v，期望 ErrRemoteTargetInvalid", err)
	}

	password := "secret"
	saved, err := service.Save(context.Background(), RemoteTargetSaveRequest{
		Target:   RemoteTarget{ID: "home", Name: "家庭开发机", Host: "192.168.1.20", User: "dev", AuthType: AuthPassword},
		Password: &password,
	})
	if err != nil {
		t.Fatalf("保存合法远程主机失败: %v", err)
	}
	if !saved.PasswordConfigured || saved.Password != password {
		t.Fatalf("远程主机凭据状态不正确: %#v", saved)
	}
}

func TestRemoteTargetDeleteChecksProjectReferences(t *testing.T) {
	repository := newRemoteTargetMemoryRepository()
	service, err := NewRemoteTargetService(repository)
	if err != nil {
		t.Fatal(err)
	}
	service.SetWorkspaceUsageCounter(func(context.Context, string) (int, error) { return 1, nil })

	password := "secret"
	if _, err := service.Save(context.Background(), RemoteTargetSaveRequest{
		Target: RemoteTarget{ID: "home", Name: "家庭开发机", Host: "192.168.1.20", User: "dev", AuthType: AuthPassword}, Password: &password,
	}); err != nil {
		t.Fatalf("保存远程主机失败: %v", err)
	}
	if err := service.Delete(context.Background(), "home"); !errors.Is(err, ErrRemoteTargetInUse) {
		t.Fatalf("删除被项目引用的远程主机错误 = %v，期望 ErrRemoteTargetInUse", err)
	}
	if _, err := repository.Get(context.Background(), "home"); err != nil {
		t.Fatalf("被引用的远程主机不应被删除: %v", err)
	}
}

type remoteTargetMemoryRepository struct {
	items map[string]RemoteTarget
}

func newRemoteTargetMemoryRepository() *remoteTargetMemoryRepository {
	return &remoteTargetMemoryRepository{items: map[string]RemoteTarget{}}
}

func (r *remoteTargetMemoryRepository) List(context.Context) ([]RemoteTarget, error) {
	items := make([]RemoteTarget, 0, len(r.items))
	for _, item := range r.items {
		items = append(items, item)
	}
	return items, nil
}

func (r *remoteTargetMemoryRepository) Get(_ context.Context, id string) (RemoteTarget, error) {
	item, ok := r.items[id]
	if !ok {
		return RemoteTarget{}, ErrRemoteTargetNotFound
	}
	return item, nil
}

func (r *remoteTargetMemoryRepository) Save(_ context.Context, item RemoteTarget) error {
	r.items[item.ID] = item
	return nil
}

func (r *remoteTargetMemoryRepository) Delete(_ context.Context, id string) error {
	if _, ok := r.items[id]; !ok {
		return ErrRemoteTargetNotFound
	}
	delete(r.items, id)
	return nil
}
