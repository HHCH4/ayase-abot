package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"Abot/internal/bot"
	"Abot/internal/schedule"
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
	AdminUserIDs      *[]string             `json:"admin_user_ids"`
	RuntimeConfig     *bot.BotRuntimeConfig `json:"runtime_config"`
	TelegramToken     *string               `json:"telegram_token"`
	OneBotAccessToken *string               `json:"onebot_access_token"`
	Enabled           *bool                 `json:"enabled"`
}

type botView struct {
	ID                      string               `json:"id"`
	Name                    string               `json:"name"`
	Type                    bot.Type             `json:"type"`
	Endpoint                string               `json:"endpoint,omitempty"`
	OneBotMode              string               `json:"onebot_mode,omitempty"`
	ListenHost              string               `json:"listen_host,omitempty"`
	ListenPort              int                  `json:"listen_port,omitempty"`
	ListenPath              string               `json:"listen_path,omitempty"`
	OneBotFileRoot          string               `json:"onebot_file_root,omitempty"`
	GroupTriggerMode        string               `json:"group_trigger_mode,omitempty"`
	AdminUserIDs            []string             `json:"admin_user_ids,omitempty"`
	RuntimeConfig           bot.BotRuntimeConfig `json:"runtime_config"`
	TelegramTokenConfigured bool                 `json:"telegram_token_configured"`
	OneBotTokenConfigured   bool                 `json:"onebot_access_token_configured"`
	Enabled                 bool                 `json:"enabled"`
	Status                  bot.Status           `json:"status"`
	StatusMessage           string               `json:"status_message,omitempty"`
	LastCheckedAt           any                  `json:"last_checked_at,omitempty"`
	CreatedAt               any                  `json:"created_at,omitempty"`
	UpdatedAt               any                  `json:"updated_at,omitempty"`
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
	runtimeConfig := bot.BotRuntimeConfig{}
	if haveOld {
		// An omitted field keeps the configured administrators; an explicit
		// empty list clears them. A partial update must never silently drop the
		// only account that can manage this bot.
		adminUserIDs = append([]string(nil), old.AdminUserIDs...)
		runtimeConfig = old.RuntimeConfig
	}
	if payload.AdminUserIDs != nil {
		adminUserIDs = normalizeAdminUserIDs(*payload.AdminUserIDs)
	}
	if payload.RuntimeConfig != nil {
		runtimeConfig = payload.RuntimeConfig.NormalizeRuntimeConfig()
	}
	fileRoot := ""
	if payload.OneBotFileRoot != nil {
		fileRoot = strings.TrimSpace(*payload.OneBotFileRoot)
	} else if haveOld {
		fileRoot = old.OneBotFileRoot
	}
	item, err := service.Save(request.Context(), bot.SaveRequest{
		Bot:           bot.Bot{ID: payload.ID, Name: payload.Name, Type: payload.Type, Endpoint: payload.Endpoint, OneBotMode: payload.OneBotMode, ListenHost: payload.ListenHost, ListenPort: payload.ListenPort, ListenPath: payload.ListenPath, OneBotFileRoot: fileRoot, GroupTriggerMode: payload.GroupTriggerMode, AdminUserIDs: adminUserIDs, RuntimeConfig: runtimeConfig, Enabled: enabled},
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

// getBotRuntimeOverview 返回 Bot Runtime 的有界只读投影，供 WebUI 展示来源积压、
// 行为原因码和最近动作；该接口不会触发模型、工具或平台动作。
func (s *Server) getBotRuntimeOverview(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireBots()
	if err != nil {
		writeError(writer, err)
		return
	}
	limit := 20
	if raw := strings.TrimSpace(request.URL.Query().Get("limit")); raw != "" {
		if parsed, parseErr := strconv.Atoi(raw); parseErr == nil && parsed > 0 {
			limit = parsed
		}
	}
	overview, err := service.GetRuntimeOverview(request.Context(), request.PathValue("id"), limit)
	if err != nil {
		writeError(writer, err)
		return
	}

	// 后续任务复用持久化调度器，按机器人过滤后和运行态一起返回，确保重启后仍能在 WebUI 看到任务。
	followUps := make([]schedule.Task, 0)
	if s.schedules != nil {
		tasks, listErr := s.schedules.List(request.Context(), "")
		if listErr != nil {
			writeError(writer, listErr)
			return
		}
		for _, task := range tasks {
			if task.AdapterID != request.PathValue("id") {
				continue
			}
			followUps = append(followUps, task)
			if len(followUps) >= limit {
				break
			}
		}
	}

	// 使用显式响应对象保持 RuntimeOverview 原有字段兼容，同时增加 Follow-up 投影。
	writeJSON(writer, http.StatusOK, map[string]any{
		"bot_id":     overview.BotID,
		"instance":   overview.Instance,
		"sources":    overview.Sources,
		"decisions":  overview.Decisions,
		"actions":    overview.Actions,
		"relations":  overview.Relations,
		"follow_ups": followUps,
	})
}

// runtimeRelationPayload 是关系管理接口的稳定输入模型；revision 用于 CAS，
// 防止 WebUI 打开的旧页面覆盖运行时刚刚写入的事实。
type runtimeRelationPayload struct {
	PreferredName     string     `json:"preferred_name"`
	StablePreferences string     `json:"stable_preferences"`
	InteractionStyle  string     `json:"interaction_style"`
	TrustLevel        string     `json:"trust_level"`
	RecentTopics      string     `json:"recent_topics"`
	Commitments       string     `json:"commitments"`
	LastInteractionAt *time.Time `json:"last_interaction_at,omitempty"`
	Revision          int64      `json:"revision"`
}

// listBotRuntimeRelations 返回完整但有界的关系目录，供 WebUI 分页或刷新编辑。
func (s *Server) listBotRuntimeRelations(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireBots()
	if err != nil {
		writeError(writer, err)
		return
	}
	botID := request.PathValue("id")
	if _, err := service.Get(botID); err != nil {
		writeError(writer, err)
		return
	}
	limit := 100
	if raw := strings.TrimSpace(request.URL.Query().Get("limit")); raw != "" {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil || parsed <= 0 {
			writeError(writer, fmt.Errorf("limit 必须是正整数"))
			return
		}
		limit = parsed
	}
	if limit > 500 {
		limit = 500
	}
	relations, err := service.ListRuntimeRelations(request.Context(), botID, limit)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"bot_id": botID, "relations": relations})
}

// saveBotRuntimeRelation 保存关系的可解释字段；Bot Runtime 的消息入口仍可同时
// 更新关系，revision 冲突时返回 409 让前端重新读取，而不是静默丢失事实。
func (s *Server) saveBotRuntimeRelation(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireBots()
	if err != nil {
		writeError(writer, err)
		return
	}
	botID := strings.TrimSpace(request.PathValue("id"))
	if _, err := service.Get(botID); err != nil {
		writeError(writer, err)
		return
	}
	var payload runtimeRelationPayload
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, err)
		return
	}
	scopeType := strings.TrimSpace(request.PathValue("scope_type"))
	scopeID := strings.TrimSpace(request.PathValue("scope_id"))
	if scopeType == "" || scopeID == "" || payload.Revision < 0 {
		writeError(writer, fmt.Errorf("关系范围和 revision 无效"))
		return
	}
	state := bot.RelationState{
		BotID: botID, ScopeType: scopeType, ScopeID: scopeID,
		PreferredName: payload.PreferredName, StablePreferences: payload.StablePreferences,
		InteractionStyle: payload.InteractionStyle, TrustLevel: payload.TrustLevel,
		RecentTopics: payload.RecentTopics, Commitments: payload.Commitments,
		Revision: payload.Revision,
	}
	if payload.LastInteractionAt != nil {
		state.LastInteractionAt = payload.LastInteractionAt.UTC()
	}
	if err := service.SaveRuntimeRelation(request.Context(), state, payload.Revision); err != nil {
		writeError(writer, err)
		return
	}
	state.Revision = payload.Revision + 1
	if state.LastInteractionAt.IsZero() {
		state.LastInteractionAt = time.Now().UTC()
	}
	writeJSON(writer, http.StatusOK, state)
}

// deleteBotRuntimeRelation 只删除关系投影，不删除 Conversation、来源上下文或附件。
func (s *Server) deleteBotRuntimeRelation(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireBots()
	if err != nil {
		writeError(writer, err)
		return
	}
	botID := strings.TrimSpace(request.PathValue("id"))
	if _, err := service.Get(botID); err != nil {
		writeError(writer, err)
		return
	}
	rawRevision := strings.TrimSpace(request.URL.Query().Get("revision"))
	revision, parseErr := strconv.ParseInt(rawRevision, 10, 64)
	if parseErr != nil || revision <= 0 {
		writeError(writer, fmt.Errorf("删除关系必须提供正 revision"))
		return
	}
	if err := service.DeleteRuntimeRelation(request.Context(), botID, request.PathValue("scope_type"), request.PathValue("scope_id"), revision); err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusNoContent, map[string]any{"deleted": true})
}

func publicBot(item bot.Bot) botView {
	return botView{
		ID: item.ID, Name: item.Name, Type: item.Type, Endpoint: item.Endpoint, OneBotMode: item.OneBotMode, ListenHost: item.ListenHost, ListenPort: item.ListenPort, ListenPath: item.ListenPath, OneBotFileRoot: item.OneBotFileRoot, GroupTriggerMode: item.GroupTriggerMode, AdminUserIDs: item.AdminUserIDs, RuntimeConfig: item.RuntimeConfig.NormalizeRuntimeConfig(),
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
