package handlers

import (
	"encoding/json"
	"fmt"

	// "io"

	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"wx_channel/internal/config"
	"wx_channel/internal/models"
	"wx_channel/internal/storage"
	"wx_channel/internal/utils"

	"github.com/fatih/color"
	"github.com/qtgolang/SunnyNet/SunnyNet"
	nf_http "github.com/qtgolang/SunnyNet/src/http"
)

// RecordHandler 下载记录处理器
type RecordHandler struct {
	csvManager *storage.CSVManager
	currentURL string
}

// NewRecordHandler 创建记录处理器
func NewRecordHandler(cfg *config.Config, csvManager *storage.CSVManager) *RecordHandler {
	return &RecordHandler{
		csvManager: csvManager,
	}
}

// getConfig 获取当前配置（动态获取最新配置）
func (h *RecordHandler) getConfig() *config.Config {
	return config.Get()
}

// SetCurrentURL 设置当前页面URL
func (h *RecordHandler) SetCurrentURL(url string) {
	h.currentURL = url
}

// GetCurrentURL 获取当前页面URL
func (h *RecordHandler) GetCurrentURL() string {
	return h.currentURL
}

// HandleRecordDownload 处理记录下载信息请求
func (h *RecordHandler) HandleRecordDownload(Conn SunnyNet.ConnHTTP) bool {
	u, err := url.Parse(Conn.URL())
	if err != nil {
		return false
	}
	path := u.Path
	if path != "/__wx_channels_api/record_download" {
		return false
	}

	reqHeaders := nf_http.Header(Conn.GetRequestHeader())
	if h.getConfig() != nil && h.getConfig().SecretToken != "" {
		auth := reqHeaders.Get("X-Local-Auth")
		if auth != h.getConfig().SecretToken {
			h.sendErrorResponse(Conn, fmt.Errorf("unauthorized"))
			return true
		}
	}

	var data map[string]interface{}
	body := Conn.GetRequestBody()

	// 检查body是否为空
	if len(body) == 0 {
		utils.Warn("record_download请求体为空，跳过处理")
		h.sendEmptyResponse(Conn)
		return true
	}

	if err := json.Unmarshal(body, &data); err != nil {
		utils.HandleError(err, "记录下载信息")
		h.sendEmptyResponse(Conn)
		return true
	}

	// 创建下载记录
	record := &models.VideoDownloadRecord{
		ID:            fmt.Sprintf("%v", data["id"]),
		Title:         fmt.Sprintf("%v", data["title"]),
		Author:        "", // 将在后面从contact中获取
		URL:           fmt.Sprintf("%v", data["url"]),
		PageURL:       h.currentURL,
		DownloadAt:    time.Now(),
		PageSource:    "", // 将从请求数据中获取
		SearchKeyword: "", // 将从请求数据中获取
	}

	// 从正确的位置获取作者昵称
	// 优先从顶层获取（Feed页）
	if nickname, ok := data["nickname"].(string); ok && nickname != "" {
		record.Author = nickname
	} else {
		// 从 contact.nickname 获取（Home页）
		if contact, ok := data["contact"].(map[string]interface{}); ok {
			if nickname, ok := contact["nickname"].(string); ok {
				record.Author = nickname
			}
		}
	}

	// 添加可选字段
	if size, ok := data["size"].(float64); ok {
		record.FileSize = fmt.Sprintf("%.2f MB", size/(1024*1024))
	}
	if duration, ok := data["duration"].(float64); ok {
		record.Duration = utils.FormatDuration(duration)
	}

	// 添加互动数据
	if readCount, ok := data["readCount"].(float64); ok {
		record.PlayCount = utils.FormatNumber(readCount)
	}
	if likeCount, ok := data["likeCount"].(float64); ok {
		record.LikeCount = utils.FormatNumber(likeCount)
	}
	if commentCount, ok := data["commentCount"].(float64); ok {
		record.CommentCount = utils.FormatNumber(commentCount)
	}
	if favCount, ok := data["favCount"].(float64); ok {
		record.FavCount = utils.FormatNumber(favCount)
	}
	if forwardCount, ok := data["forwardCount"].(float64); ok {
		record.ForwardCount = utils.FormatNumber(forwardCount)
	}

	// 添加创建时间
	if createtime, ok := data["createtime"].(float64); ok {
		t := time.Unix(int64(createtime), 0)
		record.CreateTime = t.Format("2006-01-02 15:04:05")
	}

	// 添加视频号分类和公众号名称
	if contact, ok := data["contact"].(map[string]interface{}); ok {
		if authInfo, ok := contact["authInfo"].(map[string]interface{}); ok {
			if authProfession, ok := authInfo["authProfession"].(string); ok {
				record.AuthorType = authProfession
			}
		}

		// 尝试获取公众号名称
		if bindInfo, ok := contact["bindInfo"].([]interface{}); ok && len(bindInfo) > 0 {
			for _, bind := range bindInfo {
				if bindMap, ok := bind.(map[string]interface{}); ok {
					if bizInfo, ok := bindMap["bizInfo"].(map[string]interface{}); ok {
						if info, ok := bizInfo["info"].([]interface{}); ok && len(info) > 0 {
							if infoMap, ok := info[0].(map[string]interface{}); ok {
								if bizNickname, ok := infoMap["bizNickname"].(string); ok {
									record.OfficialName = bizNickname
									break
								}
							}
						}
					}
				}
			}
		}
	}

	// 添加IP所在地
	if ipRegionInfo, ok := data["ipRegionInfo"].(map[string]interface{}); ok {
		if regionText, ok := ipRegionInfo["regionText"].(string); ok {
			record.IPRegion = regionText
		}
	}

	// 添加页面来源标识
	if pageSource, ok := data["pageSource"].(string); ok {
		record.PageSource = pageSource
	} else {
		// 如果前端没有提供，尝试从URL推断
		record.PageSource = h.inferPageSource(h.currentURL)
	}

	// 添加搜索关键词（仅S页）
	if searchKeyword, ok := data["searchKeyword"].(string); ok {
		record.SearchKeyword = searchKeyword
	}

	// 保存记录（检查重复）
	if h.csvManager != nil {
		// 检查记录是否已存在（避免重复记录）
		if exists, err := h.csvManager.RecordExists(record.ID); err == nil && exists {
			utils.Info("[下载记录] 记录已存在，跳过保存: ID=%s, 标题=%s, 作者=%s", record.ID, record.Title, record.Author)
			h.sendEmptyResponse(Conn)
			return true
		}

		if err := h.csvManager.AddRecord(record); err != nil {
			utils.Error("[下载记录] 保存失败: ID=%s, 标题=%s, 作者=%s, 错误=%v", record.ID, record.Title, record.Author, err)
			utils.HandleError(err, "保存下载记录")
		} else {
			utils.Info("[下载记录] 已保存: ID=%s, 标题=%s, 作者=%s, 大小=%s, 时长=%s", record.ID, record.Title, record.Author, record.FileSize, record.Duration)

			// 记录到日志文件，包含页面来源和搜索关键词
			logMsg := fmt.Sprintf("[下载记录] ID=%s | 标题=%s | 作者=%s | 大小=%s | 时长=%s | 页面=%s",
				record.ID, record.Title, record.Author, record.FileSize, record.Duration, record.PageSource)
			if record.SearchKeyword != "" {
				logMsg += fmt.Sprintf(" | 搜索词=%s", record.SearchKeyword)
			}
			utils.LogInfo(logMsg)

			utils.PrintSeparator()
			color.Green("✅ 下载记录已保存")
			utils.PrintSeparator()
		}
	}

	h.sendEmptyResponse(Conn)
	return true
}

// HandleExportVideoList 处理批量导出视频链接请求
func (h *RecordHandler) HandleExportVideoList(Conn SunnyNet.ConnHTTP) bool {
	u, err := url.Parse(Conn.URL())
	if err != nil {
		return false
	}
	path := u.Path
	if path != "/__wx_channels_api/export_video_list" {
		return false
	}

	reqHeaders := nf_http.Header(Conn.GetRequestHeader())
	if h.getConfig() != nil && h.getConfig().SecretToken != "" {
		auth := reqHeaders.Get("X-Local-Auth")
		if auth != h.getConfig().SecretToken {
			h.sendErrorResponse(Conn, fmt.Errorf("unauthorized"))
			return true
		}
	}

	var requestData struct {
		Videos []map[string]interface{} `json:"videos"`
	}

	body := Conn.GetRequestBody()

	if err := json.Unmarshal(body, &requestData); err != nil {
		utils.HandleError(err, "解析批量导出请求")
		h.sendErrorResponse(Conn, err)
		return true
	}

	// 生成视频链接列表
	var videoList []string
	for i, video := range requestData.Videos {
		title := fmt.Sprintf("%v", video["title"])
		videoId := fmt.Sprintf("%v", video["id"])
		url := fmt.Sprintf("%v", video["url"])

		videoList = append(videoList, fmt.Sprintf("%d. %s\n   ID: %s\n   URL: %s\n",
			i+1, title, videoId, url))
	}

	content := fmt.Sprintf("主页页面视频列表导出\n生成时间: %s\n总计: %d 个视频\n\n%s",
		time.Now().Format("2006-01-02 15:04:05"),
		len(requestData.Videos),
		strings.Join(videoList, "\n"))

	// 保存到文件
	baseDir, err := utils.GetBaseDir()
	if err == nil {
		exportDir := filepath.Join(baseDir, h.getConfig().DownloadsDir)
		if err := utils.EnsureDir(exportDir); err == nil {
			exportFile := filepath.Join(exportDir, fmt.Sprintf("profile_videos_export_%s.txt",
				time.Now().Format("20060102_150405")))
			err = os.WriteFile(exportFile, []byte(content), 0644)
			if err == nil {
				utils.PrintSeparator()
				color.Green("📄 视频列表已导出")
				utils.PrintSeparator()
				utils.PrintLabelValue("📁", "导出文件", exportFile)
				utils.PrintLabelValue("📊", "视频数量", len(requestData.Videos))
				utils.PrintSeparator()

				// 记录导出操作
				utils.LogInfo("[导出动态] 格式=TXT | 视频数=%d | 路径=%s", len(requestData.Videos), exportFile)
			} else {
				utils.HandleError(err, "保存导出文件")
			}
		}
	}

	h.sendEmptyResponse(Conn)
	return true
}

// HandleExportVideoListJSON 处理批量导出视频链接（JSON）
func (h *RecordHandler) HandleExportVideoListJSON(Conn SunnyNet.ConnHTTP) bool {
	u, err := url.Parse(Conn.URL())
	if err != nil {
		return false
	}
	path := u.Path
	if path != "/__wx_channels_api/export_video_list_json" {
		return false
	}

	reqHeaders := nf_http.Header(Conn.GetRequestHeader())
	if h.getConfig() != nil && h.getConfig().SecretToken != "" {
		auth := reqHeaders.Get("X-Local-Auth")
		if auth != h.getConfig().SecretToken {
			h.sendErrorResponse(Conn, fmt.Errorf("unauthorized"))
			return true
		}
	}

	var requestData struct {
		Videos []map[string]interface{} `json:"videos"`
	}

	body := Conn.GetRequestBody()

	if err := json.Unmarshal(body, &requestData); err != nil {
		utils.HandleError(err, "解析批量导出JSON请求")
		h.sendErrorResponse(Conn, err)
		return true
	}

	payload := map[string]interface{}{
		"generated_at": time.Now().Format("2006-01-02 15:04:05"),
		"count":        len(requestData.Videos),
		"videos":       requestData.Videos,
	}

	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		h.sendErrorResponse(Conn, err)
		return true
	}

	baseDir, err := utils.GetBaseDir()
	if err == nil {
		exportDir := filepath.Join(baseDir, h.getConfig().DownloadsDir)
		if err := utils.EnsureDir(exportDir); err == nil {
			exportFile := filepath.Join(exportDir, fmt.Sprintf("profile_videos_export_%s.json",
				time.Now().Format("20060102_150405")))
			if err := os.WriteFile(exportFile, b, 0644); err == nil {
				utils.PrintSeparator()
				color.Green("📄 视频列表已导出(JSON)")
				utils.PrintSeparator()
				utils.PrintLabelValue("📁", "导出文件", exportFile)
				utils.PrintLabelValue("📊", "视频数量", len(requestData.Videos))
				utils.PrintSeparator()

				// 记录导出操作
				utils.LogInfo("[导出动态] 格式=JSON | 视频数=%d | 路径=%s", len(requestData.Videos), exportFile)
			} else {
				utils.HandleError(err, "保存JSON导出文件")
			}
		}
	}

	h.sendEmptyResponse(Conn)
	return true
}

// HandleExportVideoListMarkdown 处理批量导出视频链接（Markdown）
func (h *RecordHandler) HandleExportVideoListMarkdown(Conn SunnyNet.ConnHTTP) bool {
	u, err := url.Parse(Conn.URL())
	if err != nil {
		return false
	}
	path := u.Path
	if path != "/__wx_channels_api/export_video_list_md" {
		return false
	}

	reqHeaders := nf_http.Header(Conn.GetRequestHeader())
	if h.getConfig() != nil && h.getConfig().SecretToken != "" {
		auth := reqHeaders.Get("X-Local-Auth")
		if auth != h.getConfig().SecretToken {
			h.sendErrorResponse(Conn, fmt.Errorf("unauthorized"))
			return true
		}
	}

	var requestData struct {
		Videos []map[string]interface{} `json:"videos"`
	}

	body := Conn.GetRequestBody()

	if err := json.Unmarshal(body, &requestData); err != nil {
		utils.HandleError(err, "解析批量导出MD请求")
		h.sendErrorResponse(Conn, err)
		return true
	}

	var sb strings.Builder
	sb.WriteString("# 主页页面视频列表导出\n\n")
	sb.WriteString(fmt.Sprintf("生成时间: %s\\n\n", time.Now().Format("2006-01-02 15:04:05")))
	sb.WriteString(fmt.Sprintf("总计: %d 个视频\\n\n", len(requestData.Videos)))
	for i, v := range requestData.Videos {
		title := fmt.Sprintf("%v", v["title"])
		videoId := fmt.Sprintf("%v", v["id"])
		url := fmt.Sprintf("%v", v["url"])
		sb.WriteString(fmt.Sprintf("%d. [%s](%s)  ", i+1, title, url))
		sb.WriteString(fmt.Sprintf("ID: `%s`\\n\n", videoId))
	}

	baseDir, err := utils.GetBaseDir()
	if err == nil {
		exportDir := filepath.Join(baseDir, h.getConfig().DownloadsDir)
		if err := utils.EnsureDir(exportDir); err == nil {
			exportFile := filepath.Join(exportDir, fmt.Sprintf("profile_videos_export_%s.md",
				time.Now().Format("20060102_150405")))
			if err := os.WriteFile(exportFile, []byte(sb.String()), 0644); err == nil {
				utils.PrintSeparator()
				color.Green("📄 视频列表已导出(Markdown)")
				utils.PrintLabelValue("📁", "导出文件", exportFile)
				utils.PrintLabelValue("📊", "视频数量", len(requestData.Videos))
				utils.PrintSeparator()

				// 记录导出操作
				utils.LogInfo("[导出动态] 格式=Markdown | 视频数=%d | 路径=%s", len(requestData.Videos), exportFile)
			} else {
				utils.HandleError(err, "保存Markdown导出文件")
			}
		}
	}

	h.sendEmptyResponse(Conn)
	return true
}

// HandleBatchDownloadStatus 处理批量下载状态查询请求
func (h *RecordHandler) HandleBatchDownloadStatus(Conn SunnyNet.ConnHTTP) bool {
	u, err := url.Parse(Conn.URL())
	if err != nil {
		return false
	}
	path := u.Path
	if path != "/__wx_channels_api/batch_download_status" {
		return false
	}

	// 授权校验
	reqHeaders := nf_http.Header(Conn.GetRequestHeader())
	if h.getConfig() != nil && h.getConfig().SecretToken != "" {
		auth := reqHeaders.Get("X-Local-Auth")
		if auth != h.getConfig().SecretToken {
			h.sendJSONResponse(Conn, 401, []byte(`{"success":false,"error":"unauthorized"}`))
			return true
		}
	}

	var statusData struct {
		Current int    `json:"current"`
		Total   int    `json:"total"`
		Status  string `json:"status"`
	}

	body := Conn.GetRequestBody()

	if err := json.Unmarshal(body, &statusData); err != nil {
		utils.HandleError(err, "解析批量下载状态")
		h.sendErrorResponse(Conn, err)
		return true
	}

	// 显示批量下载进度
	if statusData.Total > 0 {
		percentage := float64(statusData.Current) / float64(statusData.Total) * 100
		utils.PrintSeparator()
		color.Blue("📥 批量下载进度")
		utils.PrintSeparator()
		utils.PrintLabelValue("📊", "进度", fmt.Sprintf("%d/%d (%.1f%%)",
			statusData.Current, statusData.Total, percentage))
		utils.PrintLabelValue("🔄", "状态", statusData.Status)
		utils.PrintSeparator()
	}

	h.sendEmptyResponse(Conn)
	return true
}

// inferPageSource 从URL推断页面来源
func (h *RecordHandler) inferPageSource(url string) string {
	if strings.Contains(url, "/pages/feed") {
		return "feed"
	} else if strings.Contains(url, "/pages/home") {
		return "home"
	} else if strings.Contains(url, "/pages/profile") {
		return "profile"
	} else if strings.Contains(url, "/pages/s") {
		return "search"
	}
	return "unknown"
}

// sendEmptyResponse 发送空JSON响应
func (h *RecordHandler) sendEmptyResponse(Conn SunnyNet.ConnHTTP) {
	headers := make(nf_http.Header)
	headers.Set("Content-Type", "application/json")
	headers.Set("X-Content-Type-Options", "nosniff")
	// CORS
	reqHeaders := nf_http.Header(Conn.GetRequestHeader())
	if h.getConfig() != nil && len(h.getConfig().AllowedOrigins) > 0 {
		origin := reqHeaders.Get("Origin")
		if origin != "" {
			for _, o := range h.getConfig().AllowedOrigins {
				if o == origin {
					headers.Set("Access-Control-Allow-Origin", origin)
					headers.Set("Vary", "Origin")
					headers.Set("Access-Control-Allow-Headers", "Content-Type, X-Local-Auth")
					headers.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
					break
				}
			}
		}
	}
	headers.Set("__debug", "fake_resp")
	Conn.StopRequest(200, "{}", headers)
}

// sendErrorResponse 发送错误响应
func (h *RecordHandler) sendErrorResponse(Conn SunnyNet.ConnHTTP, err error) {
	errorMsg := fmt.Sprintf(`{"success":false,"error":"%s"}`, err.Error())
	h.sendJSONResponse(Conn, 500, []byte(errorMsg))
}

// sendJSONResponse 发送JSON响应
func (h *RecordHandler) sendJSONResponse(Conn SunnyNet.ConnHTTP, code int, body []byte) {
	headers := make(nf_http.Header)
	headers.Set("Content-Type", "application/json")
	headers.Set("X-Content-Type-Options", "nosniff")
	// CORS
	reqHeaders := nf_http.Header(Conn.GetRequestHeader())
	if h.getConfig() != nil && len(h.getConfig().AllowedOrigins) > 0 {
		origin := reqHeaders.Get("Origin")
		if origin != "" {
			for _, o := range h.getConfig().AllowedOrigins {
				if o == origin {
					headers.Set("Access-Control-Allow-Origin", origin)
					headers.Set("Vary", "Origin")
					headers.Set("Access-Control-Allow-Headers", "Content-Type, X-Local-Auth")
					headers.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
					break
				}
			}
		}
	}
	headers.Set("__debug", "fake_resp")
	Conn.StopRequest(code, string(body), headers)
}
