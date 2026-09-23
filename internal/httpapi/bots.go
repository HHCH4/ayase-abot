package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"Abot/internal/bot"
)

type botPayload struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Type       bot.Type `json:"type"`
	Endpoint   string   `json:"endpoint"`
	OneBotMode string   `json:"onebot_mode"`
	ListenHost string   `json:"listen_host"`
	ListenPort int      `json:"listen_port"`
	ListenPath string   `json:"listen_path"`
	// OneBotFileRoot 使用指针区分“未提交”与“明确清空”，避免旧客户端的部分更新
	// 意外覆盖已经配置好的附件宿主机目录。
	OneBotFileRoot   *string `json:"onebot_file_root"`
	GroupTriggerMode string  `json:"group_trigger_mode"`
	// AdminUserIDs is the root of trust for chat commands. It is accepted only
	// here (the WebUI), never from a chat message.
	AdminUserIDs      *[]string `json:"admin_user_ids"`
	TelegramToken     *string   `json:"telegram_token"`
	OneBotAccessToken *string   `json:"onebot_access_token"`
	Enabled           *bool     `json:"enabled"`
}

type botView struct {
	ID                      string     `json:"id"`
	Name                    string     `json:"name"`
	Type                    bot.Type   `json:"type"`
	Endpoint                string     `json:"endpoint,omitempty"`
	OneBotMode              string     `json:"onebot_mode,omitempty"`
	ListenHost              string     `json:"listen_host,omitempty"`
	ListenPort              int        `json:"listen_port,omitempty"`
	ListenPath              string     `json:"listen_path,omitempty"`
	OneBotFileRoot          string     `json:"onebot_file_root,omitempty"`
	GroupTriggerMode        string     `json:"group_trigger_mode,omitempty"`
	AdminUserIDs            []string   `json:"admin_user_ids,omitempty"`
	TelegramTokenConfigured bool       `json:"telegram_token_configured"`
	OneBotTokenConfigured   bool       `json:"onebot_access_token_configured"`
	Enabled                 bool       `json:"enabled"`
	Status                  bot.Status `json:"status"`
	StatusMessage           string     `json:"status_message,omitempty"`
	LastCheckedAt           any        `json:"last_checked_at,omitempty"`
	CreatedAt               any        `json:"created_at,omitempty"`
	UpdatedAt               any        `json:"updated_at,omitempty"`
}

func (s *Server) requireBots() (*bot.Manager, error) {
	if s.bots == nil {
		return nil, errors.New("机器人服务尚未装配")
	}
	return s.bots, nil
}

func (s *Server) listBotTypes(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{"types": []map[string]any{
		{"id": bot.TypeTelegram, "name": "Telegram", "description": "通过 getUpdates 长轮询接收消息，无需公网入口", "secret": "telegram_token"},
		{"id": bot.TypeOneBot11, "name": "OneBot 11", "description": "NapCat/Lagrange 等通过反向 WebSocket 连接", "secret": "onebot_access_token"},
	}})
}

func (s *Server) listBots(writer http.ResponseWriter, _ *http.Request) {
	service, err := s.requireBots()
	if err != nil {
		writeError(writer, err)
		return
	}
	items := service.List()
	views := make([]botView, 0, len(items))
	for _, item := range items {
		views = append(views, publicBot(item))
	}
	writeJSON(writer, http.StatusOK, map[string]any{"bots": views})
}

func (s *Server) getBot(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireBots()
	if err != nil {
		writeError(writer, err)
		return
	}
	item, err := service.Get(request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, publicBot(item))
}

func (s *Server) createBot(writer http.ResponseWriter, request *http.Request) {
	s.saveBot(writer, request, "")
}

func (s *Server) updateBot(writer http.ResponseWriter, request *http.Request) {
	s.saveBot(writer, request, request.PathValue("id"))
}

func (s *Server) saveBot(writer http.ResponseWriter, request *http.Request, pathID string) {
	service, err := s.requireBots()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload botPayload
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	if pathID != "" {
		if payload.ID != "" && payload.ID != pathID {
			writeError(writer, errors.New("路径中的机器人 ID 与请求体不一致"))
			return
		}
		payload.ID = pathID
	}
	enabled := true
	var old bot.Bot
	haveOld := false
	if pathID != "" {
		var getErr error
		old, getErr = service.Get(pathID)
		if getErr != nil {
			writeError(writer, getErr)
			return
		}
		haveOld = true
		enabled = old.Enabled
	}
	if payload.Enabled != nil {
		enabled = *payload.Enabled
	}
	if haveOld && payload.Type == bot.TypeOneBot11 && old.Type == bot.TypeOneBot11 &&
		strings.TrimSpace(payload.OneBotMode) == "" && strings.TrimSpace(payload.Endpoint) == "" &&
		strings.TrimSpace(payload.ListenHost) == "" && payload.ListenPort == 0 && strings.TrimSpace(payload.ListenPath) == "" && payload.OneBotFileRoot == nil {
		// 兼容旧客户端直接提交部分更新：未带新字段时保留原来的 OneBot 连接参数。
		payload.Endpoint = old.Endpoint
		payload.OneBotMode = old.OneBotMode
		payload.ListenHost = old.ListenHost
		payload.ListenPort = old.ListenPort
		payload.ListenPath = old.ListenPath
		payload.OneBotFileRoot = stringPointer(old.OneBotFileRoot)
	}
	if haveOld && strings.TrimSpace(payload.GroupTriggerMode) == "" {
		payload.GroupTriggerMode = old.GroupTriggerMode
	}
	adminUserIDs := []string(nil)
	if haveOld {
		// An omitted field keeps the configured administrators; an explicit
		// empty list clears them. A partial update must never silently drop the
		// only account that can manage this bot.
		adminUserIDs = append([]string(nil), old.AdminUserIDs...)
	}
	if payload.AdminUserIDs != nil {
		adminUserIDs = normalizeAdminUserIDs(*payload.AdminUserIDs)
	}
	fileRoot := ""
	if payload.OneBotFileRoot != nil {
		fileRoot = strings.TrimSpace(*payload.OneBotFileRoot)
	} else if haveOld {
		fileRoot = old.OneBotFileRoot
	}
	item, err := service.Save(request.Context(), bot.SaveRequest{
		Bot:           bot.Bot{ID: payload.ID, Name: payload.Name, Type: payload.Type, Endpoint: payload.Endpoint, OneBotMode: payload.OneBotMode, ListenHost: payload.ListenHost, ListenPort: payload.ListenPort, ListenPath: payload.ListenPath, OneBotFileRoot: fileRoot, GroupTriggerMode: payload.GroupTriggerMode, AdminUserIDs: adminUserIDs, Enabled: enabled},
		TelegramToken: payload.TelegramToken, OneBotAccessToken: payload.OneBotAccessToken,
	})
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, publicBot(item))
}

func (s *Server) deleteBot(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireBots()
	if err != nil {
		writeError(writer, err)
		return
	}
	if err := service.Delete(request.Context(), request.PathValue("id")); err != nil {
		writeError(writer, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (s *Server) testBot(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireBots()
	if err != nil {
		writeError(writer, err)
		return
	}
	result, err := service.Test(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (s *Server) previewBotTest(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireBots()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload botPayload
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	item := bot.Bot{ID: payload.ID, Name: payload.Name, Type: payload.Type, Endpoint: payload.Endpoint, OneBotMode: payload.OneBotMode, ListenHost: payload.ListenHost, ListenPort: payload.ListenPort, ListenPath: payload.ListenPath, GroupTriggerMode: payload.GroupTriggerMode}
	if payload.OneBotFileRoot != nil {
		item.OneBotFileRoot = strings.TrimSpace(*payload.OneBotFileRoot)
	}
	if payload.TelegramToken != nil {
		item.TelegramToken = strings.TrimSpace(*payload.TelegramToken)
	}
	if payload.OneBotAccessToken != nil {
		item.OneBotAccessToken = strings.TrimSpace(*payload.OneBotAccessToken)
	}
	if strings.TrimSpace(payload.ID) != "" {
		// 编辑已有机器人时，表单留空的秘密字段只代表复用内存中的旧值，仍然不向前端回显。
		stored, getErr := service.Get(payload.ID)
		if getErr == nil {
			if payload.TelegramToken == nil {
				item.TelegramToken = stored.TelegramToken
			}
			if payload.OneBotAccessToken == nil {
				item.OneBotAccessToken = stored.OneBotAccessToken
			}
		} else if !errors.Is(getErr, bot.ErrNotFound) {
			writeError(writer, getErr)
			return
		}
	}
	result, err := service.TestPreview(request.Context(), item)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (s *Server) startBot(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireBots()
	if err != nil {
		writeError(writer, err)
		return
	}
	if err := service.StartBot(request.PathValue("id")); err != nil {
		writeError(writer, err)
		return
	}
	item, err := service.Get(request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, publicBot(item))
}

func (s *Server) stopBot(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireBots()
	if err != nil {
		writeError(writer, err)
		return
	}
	if err := service.StopBot(request.PathValue("id")); err != nil {
		writeError(writer, err)
		return
	}
	item, err := service.Get(request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, publicBot(item))
}

func (s *Server) restartBot(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireBots()
	if err != nil {
		writeError(writer, err)
		return
	}
	if err := service.RestartBot(request.PathValue("id")); err != nil {
		writeError(writer, err)
		return
	}
	item, err := service.Get(request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, publicBot(item))
}

func publicBot(item bot.Bot) botView {
	return botView{
		ID: item.ID, Name: item.Name, Type: item.Type, Endpoint: item.Endpoint, OneBotMode: item.OneBotMode, ListenHost: item.ListenHost, ListenPort: item.ListenPort, ListenPath: item.ListenPath, OneBotFileRoot: item.OneBotFileRoot, GroupTriggerMode: item.GroupTriggerMode, AdminUserIDs: item.AdminUserIDs,
		TelegramTokenConfigured: strings.TrimSpace(item.TelegramToken) != "",
		OneBotTokenConfigured:   strings.TrimSpace(item.OneBotAccessToken) != "",
		Enabled:                 item.Enabled, Status: item.Status, StatusMessage: item.StatusMessage,
		LastCheckedAt: item.LastCheckedAt, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
	}
}

// stringPointer 将旧配置复制到兼容性更新字段，明确表达“继续使用原值”。
func stringPointer(value string) *string {
	return &value
}

// normalizeAdminUserIDs keeps the administrator list bounded, deduplicated and
// sorted so a WebUI round-trip is predictable.
func normalizeAdminUserIDs(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > 64 || strings.ContainsAny(value, " \t\r\n\x00") {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	if len(result) == 0 {
		return nil
	}
	return result
}
