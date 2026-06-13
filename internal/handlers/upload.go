package handlers

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"wx_channel/internal/config"
	"wx_channel/internal/storage"
	"wx_channel/internal/utils"
	"wx_channel/pkg/util"

	"github.com/fatih/color"
	"github.com/qtgolang/SunnyNet/SunnyNet"
	nf_http "github.com/qtgolang/SunnyNet/src/http"
)

// UploadHandler 文件上传处理器
type UploadHandler struct {
	csvManager *storage.CSVManager
	chunkSem   chan struct{}
	mergeSem   chan struct{}
}

// NewUploadHandler 创建上传处理器
func NewUploadHandler(cfg *config.Config, csvManager *storage.CSVManager) *UploadHandler {
	ch := cfg.UploadChunkConcurrency
	if ch <= 0 {
		ch = 4
	}
	mg := cfg.UploadMergeConcurrency
	if mg <= 0 {
		mg = 1
	}
	return &UploadHandler{
		csvManager: csvManager,
		chunkSem:   make(chan struct{}, ch),
		mergeSem:   make(chan struct{}, mg),
	}
}

// getConfig 获取当前配置（动态获取最新配置）
func (h *UploadHandler) getConfig() *config.Config {
	return config.Get()
}

// getDownloadsDir 获取解析后的下载目录
func (h *UploadHandler) getDownloadsDir() (string, error) {
	cfg := h.getConfig()
	return cfg.GetResolvedDownloadsDir()
}

// HandleInitUpload 处理分片上传初始化请求
func (h *UploadHandler) HandleInitUpload(Conn SunnyNet.ConnHTTP) bool {
	u, err := url.Parse(Conn.URL())
	if err != nil {
		return false
	}
	path := u.Path
	if path != "/__wx_channels_api/init_upload" {
		return false
	}

	reqHeaders := nf_http.Header(Conn.GetRequestHeader())
	if h.getConfig() != nil && h.getConfig().SecretToken != "" {
		auth := reqHeaders.Get("X-Local-Auth")
		if auth != h.getConfig().SecretToken {
			h.sendJSONResponse(Conn, 401, []byte(`{"success":false,"error":"unauthorized"}`))
			return true
		}
	}
	if h.getConfig() != nil && len(h.getConfig().AllowedOrigins) > 0 {
		origin := reqHeaders.Get("Origin")
		if origin != "" {
			allowed := false
			for _, o := range h.getConfig().AllowedOrigins {
				if o == origin {
					allowed = true
					break
				}
			}
			if !allowed {
				h.sendJSONResponse(Conn, 403, []byte(`{"success":false,"error":"forbidden_origin"}`))
				return true
			}
		}
	}

	// 获取下载目录
	downloadsDir, err := h.getDownloadsDir()
	if err != nil {
		utils.HandleError(err, "获取下载目录")
		h.sendErrorResponse(Conn, err)
		return true
	}

	uploadsRoot := filepath.Join(downloadsDir, ".uploads")
	if err := utils.EnsureDir(uploadsRoot); err != nil {
		utils.HandleError(err, "创建上传目录")
	}

	// 生成 uploadId
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		utils.HandleError(err, "生成随机数")
		h.sendErrorResponse(Conn, err)
		return true
	}
	uploadId := fmt.Sprintf("%x", b)
	utils.Info("🔄 init_upload: 生成 uploadId = %s", uploadId)

	// 创建临时目录
	upDir := filepath.Join(uploadsRoot, uploadId)
	if err := os.MkdirAll(upDir, 0755); err != nil {
		utils.HandleError(err, "创建上传目录")
		utils.LogUploadInit(uploadId, false)
		h.sendErrorResponse(Conn, err)
		return true
	}

	// 记录上传初始化成功
	utils.LogUploadInit(uploadId, true)

	// 使用 JSON 编码确保正确转义
	responseData := map[string]interface{}{
		"success":  true,
		"uploadId": uploadId,
	}
	responseBytes, err := json.Marshal(responseData)
	if err != nil {
		utils.HandleError(err, "生成响应JSON")
		h.sendErrorResponse(Conn, err)
		return true
	}

	utils.Info("✅ init_upload: 返回响应: %s", string(responseBytes))
	h.sendJSONResponse(Conn, 200, responseBytes)
	return true
}

// HandleUploadChunk 处理分片上传请求
func (h *UploadHandler) HandleUploadChunk(Conn SunnyNet.ConnHTTP) bool {
	// 并发限流（分片）
	if h.chunkSem != nil {
		h.chunkSem <- struct{}{}
		defer func() { <-h.chunkSem }()
	}
	u, err := url.Parse(Conn.URL())
	if err != nil {
		return false
	}
	path := u.Path
	if path != "/__wx_channels_api/upload_chunk" {
		return false
	}

	reqHeaders := nf_http.Header(Conn.GetRequestHeader())
	if h.getConfig() != nil && h.getConfig().SecretToken != "" {
		auth := reqHeaders.Get("X-Local-Auth")
		if auth != h.getConfig().SecretToken {
			h.sendJSONResponse(Conn, 401, []byte(`{"success":false,"error":"unauthorized"}`))
			return true
		}
	}
	if h.getConfig() != nil && len(h.getConfig().AllowedOrigins) > 0 {
		origin := reqHeaders.Get("Origin")
		if origin != "" {
			allowed := false
			for _, o := range h.getConfig().AllowedOrigins {
				if o == origin {
					allowed = true
					break
				}
			}
			if !allowed {
				h.sendJSONResponse(Conn, 403, []byte(`{"success":false,"error":"forbidden_origin"}`))
				return true
			}
		}
	}

	// 构建 mock request 来解析 multipart form
	// 创建一个新的 http.Request
	bodyBytes := Conn.GetRequestBody()
	mockReq, err := http.NewRequest("POST", Conn.URL(), bytes.NewReader(bodyBytes))
	if err != nil {
		utils.HandleError(err, "创建mock请求")
		h.sendErrorResponse(Conn, err)
		return true
	}
	// 设置 Content-Type
	if ct := reqHeaders.Get("Content-Type"); ct != "" {
		mockReq.Header.Set("Content-Type", ct)
	}

	// 解析multipart表单
	err = mockReq.ParseMultipartForm(h.getConfig().MaxUploadSize)
	if err != nil {
		utils.HandleError(err, "解析multipart表单")
		h.sendErrorResponse(Conn, err)
		return true
	}

	uploadId := mockReq.FormValue("uploadId")
	indexStr := mockReq.FormValue("index")
	totalStr := mockReq.FormValue("total")

	if uploadId == "" || indexStr == "" || totalStr == "" {
		h.sendErrorResponse(Conn, fmt.Errorf("missing fields"))
		return true
	}

	index, err := strconv.Atoi(indexStr)
	if err != nil {
		utils.HandleError(err, "解析索引")
		h.sendErrorResponse(Conn, err)
		return true
	}

	total, err := strconv.Atoi(totalStr)
	if err != nil {
		utils.HandleError(err, "解析总数")
		h.sendErrorResponse(Conn, err)
		return true
	}

	utils.Info("[分片上传] 接收分片: uploadId=%s, 分片索引=%d/%d", uploadId, index+1, total)

	file, _, err := mockReq.FormFile("chunk")
	if err != nil {
		utils.HandleError(err, "获取分片文件")
		h.sendErrorResponse(Conn, err)
		return true
	}
	defer file.Close()

	checksum := mockReq.FormValue("checksum")
	algo := strings.ToLower(mockReq.FormValue("algo"))
	if algo == "" {
		algo = "md5"
	}
	var expectedSize int64 = -1
	if sz := mockReq.FormValue("size"); sz != "" {
		if v, convErr := strconv.ParseInt(sz, 10, 64); convErr == nil {
			expectedSize = v
		}
	}

	downloadsDir, err := h.getDownloadsDir()
	if err != nil {
		utils.HandleError(err, "获取下载目录")
		h.sendErrorResponse(Conn, err)
		return true
	}

	uploadsRoot := filepath.Join(downloadsDir, ".uploads")
	upDir := filepath.Join(uploadsRoot, uploadId)

	if _, err := os.Stat(upDir); os.IsNotExist(err) {
		h.sendErrorResponse(Conn, fmt.Errorf("uploadId not found"))
		return true
	}

	partPath := filepath.Join(upDir, fmt.Sprintf("%06d.part", index))
	out, err := os.Create(partPath)
	if err != nil {
		utils.HandleError(err, "创建分片文件")
		h.sendErrorResponse(Conn, err)
		return true
	}
	defer out.Close()

	var written int64
	if checksum != "" {
		switch algo {
		case "md5":
			hsh := md5.New()
			n, err := io.Copy(io.MultiWriter(out, hsh), file)
			if err != nil {
				utils.HandleError(err, "写入分片数据")
				h.sendErrorResponse(Conn, err)
				return true
			}
			sum := fmt.Sprintf("%x", hsh.Sum(nil))
			if !strings.EqualFold(sum, checksum) {
				_ = out.Close()
				_ = os.Remove(partPath)
				utils.Error("[分片上传] 校验失败: uploadId=%s, 分片索引=%d, 算法=%s, 期望=%s, 实际=%s", uploadId, index, algo, checksum, sum)
				h.sendErrorResponse(Conn, fmt.Errorf("checksum_mismatch"))
				return true
			}
			written = n
			utils.Info("[分片上传] 校验通过: uploadId=%s, 分片索引=%d, 算法=%s, 大小=%.2fMB", uploadId, index, algo, float64(written)/(1024*1024))
		case "sha256":
			hsh := sha256.New()
			n, err := io.Copy(io.MultiWriter(out, hsh), file)
			if err != nil {
				utils.HandleError(err, "写入分片数据")
				h.sendErrorResponse(Conn, err)
				return true
			}
			sum := fmt.Sprintf("%x", hsh.Sum(nil))
			if !strings.EqualFold(sum, checksum) {
				_ = out.Close()
				_ = os.Remove(partPath)
				utils.Error("[分片上传] 校验失败: uploadId=%s, 分片索引=%d, 算法=%s, 期望=%s, 实际=%s", uploadId, index, algo, checksum, sum)
				h.sendErrorResponse(Conn, fmt.Errorf("checksum_mismatch"))
				return true
			}
			written = n
			utils.Info("[分片上传] 校验通过: uploadId=%s, 分片索引=%d, 算法=%s, 大小=%.2fMB", uploadId, index, algo, float64(written)/(1024*1024))
		default:
			h.sendErrorResponse(Conn, fmt.Errorf("unsupported_algo"))
			return true
		}
	} else {
		n, err := io.Copy(out, file)
		if err != nil {
			utils.HandleError(err, "写入分片数据")
			h.sendErrorResponse(Conn, err)
			return true
		}
		written = n
	}

	// 尺寸校验（可选字段 + 上限保护）
	if expectedSize >= 0 && written != expectedSize {
		_ = out.Close()
		_ = os.Remove(partPath)
		utils.Error("[分片上传] 尺寸不匹配: uploadId=%s, 分片索引=%d, 期望=%d, 实际=%d", uploadId, index, expectedSize, written)
		h.sendErrorResponse(Conn, fmt.Errorf("size_mismatch"))
		return true
	}
	if h.getConfig() != nil && h.getConfig().ChunkSize > 0 && written > h.getConfig().ChunkSize*2 { // 容忍放宽至2倍
		_ = out.Close()
		_ = os.Remove(partPath)
		utils.Error("[分片上传] 分片过大: uploadId=%s, 分片索引=%d, 大小=%d, 限制=%d", uploadId, index, written, h.getConfig().ChunkSize*2)
		h.sendErrorResponse(Conn, fmt.Errorf("chunk_too_large"))
		return true
	}
	if err != nil {
		utils.HandleError(err, "写入分片数据")
		h.sendErrorResponse(Conn, err)
		return true
	}

	sizeMB := float64(written) / (1024 * 1024)
	utils.Info("[分片上传] 分片已保存: uploadId=%s, 分片索引=%d/%d, 大小=%.2fMB, 路径=%s", uploadId, index+1, total, sizeMB, partPath)

	// 记录分片上传成功
	utils.LogUploadChunk(uploadId, index, total, sizeMB, true)

	h.sendSuccessResponse(Conn)
	return true
}

// HandleCompleteUpload 处理分片上传完成请求
func (h *UploadHandler) HandleCompleteUpload(Conn SunnyNet.ConnHTTP) bool {
	// 并发限流（合并）
	if h.mergeSem != nil {
		h.mergeSem <- struct{}{}
		defer func() { <-h.mergeSem }()
	}
	u, err := url.Parse(Conn.URL())
	if err != nil {
		return false
	}
	path := u.Path
	if path != "/__wx_channels_api/complete_upload" {
		return false
	}

	if h.getConfig() != nil && h.getConfig().SecretToken != "" {
		auth := nf_http.Header(Conn.GetRequestHeader()).Get("X-Local-Auth")
		if auth != h.getConfig().SecretToken {
			headers := make(nf_http.Header)
			headers.Set("Content-Type", "application/json")
			headers.Set("X-Content-Type-Options", "nosniff")
			Conn.StopRequest(401, `{"success":false,"error":"unauthorized"}`, headers)
			return true
		}
	}
	if h.getConfig() != nil && len(h.getConfig().AllowedOrigins) > 0 {
		origin := nf_http.Header(Conn.GetRequestHeader()).Get("Origin")
		if origin != "" {
			allowed := false
			for _, o := range h.getConfig().AllowedOrigins {
				if o == origin {
					allowed = true
					break
				}
			}
			if !allowed {
				headers := make(nf_http.Header)
				headers.Set("Content-Type", "application/json")
				headers.Set("X-Content-Type-Options", "nosniff")
				Conn.StopRequest(403, `{"success":false,"error":"forbidden_origin"}`, headers)
				return true
			}
		}
	}

	body := Conn.GetRequestBody()

	var req struct {
		UploadId   string `json:"uploadId"`
		Total      int    `json:"total"`
		Filename   string `json:"filename"`
		AuthorName string `json:"authorName"`
	}

	if err := json.Unmarshal(body, &req); err != nil {
		utils.HandleError(err, "解析complete_upload JSON")
		h.sendErrorResponse(Conn, err)
		return true
	}

	if req.UploadId == "" || req.Total <= 0 || req.Filename == "" {
		utils.Error("[分片合并] 缺少必要字段: uploadId=%s, total=%d, filename=%s", req.UploadId, req.Total, req.Filename)
		h.sendErrorResponse(Conn, fmt.Errorf("missing fields"))
		return true
	}
	utils.Info("[分片合并] 开始合并: uploadId=%s, 文件名=%s, 作者=%s, 分片数=%d", req.UploadId, req.Filename, req.AuthorName, req.Total)

	downloadsDir, err := h.getDownloadsDir()
	if err != nil {
		utils.HandleError(err, "获取下载目录")
		h.sendErrorResponse(Conn, err)
		return true
	}

	uploadsRoot := filepath.Join(downloadsDir, ".uploads")
	upDir := filepath.Join(uploadsRoot, req.UploadId)

	// 目标作者目录
	authorFolder := utils.CleanFolderName(req.AuthorName)
	savePath := filepath.Join(downloadsDir, authorFolder)

	if err := utils.EnsureDir(savePath); err != nil {
		utils.HandleError(err, "创建作者目录")
		h.sendErrorResponse(Conn, err)
		return true
	}

	// 清理文件名
	cleanFilename := utils.CleanFilename(req.Filename)
	cleanFilename = utils.EnsureExtension(cleanFilename, ".mp4")

	// 冲突处理
	base := filepath.Base(cleanFilename)
	ext := filepath.Ext(cleanFilename)
	baseName := strings.TrimSuffix(base, ext)
	finalPath := filepath.Join(savePath, cleanFilename)
	if _, err := os.Stat(finalPath); err == nil {
		// 文件已存在，生成唯一文件名
		for i := 1; i < 1000; i++ {
			candidate := filepath.Join(savePath, fmt.Sprintf("%s(%d)%s", baseName, i, ext))
			if _, existsErr := os.Stat(candidate); os.IsNotExist(existsErr) {
				finalPath = candidate
				break
			}
		}
	}

	// 合并分片
	out, err := os.Create(finalPath)
	if err != nil {
		utils.HandleError(err, "创建目标文件")
		h.sendErrorResponse(Conn, err)
		return true
	}
	defer out.Close()

	// 基本存在性与数量校验
	for i := 0; i < req.Total; i++ {
		partPath := filepath.Join(upDir, fmt.Sprintf("%06d.part", i))
		if _, err := os.Stat(partPath); err != nil {
			utils.Error("[分片合并] 分片缺失: uploadId=%s, 分片索引=%d, 路径=%s", req.UploadId, i, partPath)
			h.sendErrorResponse(Conn, fmt.Errorf("missing_part_%06d", i))
			return true
		}
	}

	var totalWritten int64
	for i := 0; i < req.Total; i++ {
		partPath := filepath.Join(upDir, fmt.Sprintf("%06d.part", i))
		in, err := os.Open(partPath)
		if err != nil {
			utils.HandleError(err, "打开分片文件")
			h.sendErrorResponse(Conn, err)
			return true
		}

		n, err := io.Copy(out, in)
		in.Close()
		if err != nil {
			utils.HandleError(err, "合并分片数据")
			h.sendErrorResponse(Conn, err)
			return true
		}
		totalWritten += n
	}

	// 清理临时目录
	os.RemoveAll(upDir)

	fileSize := float64(totalWritten) / (1024 * 1024)
	utils.Info("[分片合并] 合并完成: uploadId=%s, 文件名=%s, 作者=%s, 路径=%s, 大小=%.2fMB, 分片数=%d", req.UploadId, req.Filename, req.AuthorName, finalPath, fileSize, req.Total)
	color.Green("✓ 分片视频已保存: %s (%.2f MB)", finalPath, fileSize)

	// 记录分片合并成功
	utils.LogUploadMerge(req.UploadId, req.Filename, req.AuthorName, req.Total, fileSize, true)

	responseData := map[string]interface{}{
		"success": true,
		"path":    finalPath,
		"size":    fileSize,
	}
	responseBytes, err := json.Marshal(responseData)
	if err != nil {
		utils.HandleError(err, "生成响应JSON")
		h.sendErrorResponse(Conn, err)
		return true
	}

	utils.Info("✅ complete_upload: 返回响应: %s", string(responseBytes))
	h.sendJSONResponse(Conn, 200, responseBytes)
	return true
}

// HandleSaveVideo 处理直接保存视频文件请求
func (h *UploadHandler) HandleSaveVideo(Conn SunnyNet.ConnHTTP) bool {
	u, err := url.Parse(Conn.URL())
	if err != nil {
		return false
	}
	path := u.Path
	if path != "/__wx_channels_api/save_video" {
		return false
	}

	// 解析请求头
	reqHeaders := nf_http.Header(Conn.GetRequestHeader())

	if h.getConfig() != nil && h.getConfig().SecretToken != "" {
		auth := reqHeaders.Get("X-Local-Auth")
		if auth != h.getConfig().SecretToken {
			h.sendJSONResponse(Conn, 401, []byte(`{"success":false,"error":"unauthorized"}`))
			return true
		}
	}
	if h.getConfig() != nil && len(h.getConfig().AllowedOrigins) > 0 {
		origin := reqHeaders.Get("Origin")
		if origin != "" {
			allowed := false
			for _, o := range h.getConfig().AllowedOrigins {
				if o == origin {
					allowed = true
					break
				}
			}
			if !allowed {
				h.sendJSONResponse(Conn, 403, []byte(`{"success":false,"error":"forbidden_origin"}`))
				return true
			}
		}
	}

	utils.Info("🔄 save_video: 开始处理请求")

	// 构建 mock request 来解析 multipart form
	bodyBytes := Conn.GetRequestBody()
	mockReq, err := http.NewRequest("POST", Conn.URL(), bytes.NewReader(bodyBytes))
	if err != nil {
		utils.HandleError(err, "创建mock请求")
		h.sendErrorResponse(Conn, err)
		return true
	}
	// 设置 Content-Type
	if ct := reqHeaders.Get("Content-Type"); ct != "" {
		mockReq.Header.Set("Content-Type", ct)
	}

	// 解析multipart表单
	err = mockReq.ParseMultipartForm(h.getConfig().MaxUploadSize)
	if err != nil {
		utils.HandleError(err, "解析表单数据")
		h.sendErrorResponse(Conn, err)
		return true
	}

	utils.Info("✅ save_video: 表单解析成功")

	file, header, err := mockReq.FormFile("video")
	if err != nil {
		utils.HandleError(err, "获取视频文件")
		h.sendErrorResponse(Conn, err)
		return true
	}
	defer file.Close()

	utils.Info("接收上传: %s, 报告大小: %d bytes", header.Filename, header.Size)

	filename := mockReq.FormValue("filename")
	authorName := mockReq.FormValue("authorName")
	isEncrypted := mockReq.FormValue("isEncrypted") == "true"

	// 创建作者文件夹路径
	authorFolder := utils.CleanFolderName(authorName)

	downloadsDir, err := h.getDownloadsDir()
	if err != nil {
		utils.HandleError(err, "获取下载目录")
		h.sendErrorResponse(Conn, err)
		return true
	}
	savePath := filepath.Join(downloadsDir, authorFolder)

	utils.Info("保存目录: %s", savePath)
	if err := utils.EnsureDir(savePath); err != nil {
		utils.HandleError(err, "创建文件夹")
		h.sendErrorResponse(Conn, err)
		return true
	}

	// 清理文件名
	cleanFilename := utils.CleanFilename(filename)
	cleanFilename = utils.EnsureExtension(cleanFilename, ".mp4")

	// 生成唯一文件名
	filePath := filepath.Join(savePath, cleanFilename)
	if _, statErr := os.Stat(filePath); statErr == nil {
		base := strings.TrimSuffix(cleanFilename, filepath.Ext(cleanFilename))
		ext := filepath.Ext(cleanFilename)
		for i := 1; i < 1000; i++ {
			candidate := filepath.Join(savePath, fmt.Sprintf("%s(%d)%s", base, i, ext))
			if _, existsErr := os.Stat(candidate); os.IsNotExist(existsErr) {
				filePath = candidate
				break
			}
		}
	}

	// 保存文件
	out, err := os.Create(filePath)
	if err != nil {
		utils.HandleError(err, "创建目标文件")
		h.sendErrorResponse(Conn, err)
		return true
	}
	defer out.Close()

	// 流式拷贝
	if seeker, ok := file.(io.Seeker); ok {
		_, _ = seeker.Seek(0, io.SeekStart)
	}

	written, err := io.Copy(out, file)
	if err != nil {
		utils.HandleError(err, "写入视频数据")
		h.sendErrorResponse(Conn, err)
		return true
	}

	fileSize := float64(written) / (1024 * 1024)
	statusMsg := ""
	if isEncrypted {
		statusMsg = " [已解密]"
	}
	utils.Info("✓ 视频已保存: %s (%.2f MB)%s", filePath, fileSize, statusMsg)

	// 记录直接上传成功
	utils.LogDirectUpload(filename, authorName, fileSize, isEncrypted, true)

	responseData := map[string]interface{}{
		"success": true,
		"path":    filePath,
		"size":    fileSize,
	}
	responseBytes, err := json.Marshal(responseData)
	if err != nil {
		utils.HandleError(err, "生成响应JSON")
		h.sendErrorResponse(Conn, err)
		return true
	}

	utils.Info("✅ save_video: 返回响应: %s", string(responseBytes))
	h.sendJSONResponse(Conn, 200, responseBytes)
	return true
}

// HandleSaveCover 处理保存封面图片请求
func (h *UploadHandler) HandleSaveCover(Conn SunnyNet.ConnHTTP) bool {
	u, err := url.Parse(Conn.URL())
	if err != nil {
		return false
	}
	path := u.Path
	if path != "/__wx_channels_api/save_cover" {
		return false
	}

	// 授权校验
	if h.getConfig() != nil && h.getConfig().SecretToken != "" {
		auth := nf_http.Header(Conn.GetRequestHeader()).Get("X-Local-Auth")
		if auth != h.getConfig().SecretToken {
			headers := make(nf_http.Header)
			headers.Set("Content-Type", "application/json")
			headers.Set("X-Content-Type-Options", "nosniff")
			Conn.StopRequest(401, `{"success":false,"error":"unauthorized"}`, headers)
			return true
		}
	}
	if h.getConfig() != nil && len(h.getConfig().AllowedOrigins) > 0 {
		origin := nf_http.Header(Conn.GetRequestHeader()).Get("Origin")
		if origin != "" {
			allowed := false
			for _, o := range h.getConfig().AllowedOrigins {
				if o == origin {
					allowed = true
					break
				}
			}
			if !allowed {
				headers := make(nf_http.Header)
				headers.Set("Content-Type", "application/json")
				headers.Set("X-Content-Type-Options", "nosniff")
				Conn.StopRequest(403, `{"success":false,"error":"forbidden_origin"}`, headers)
				return true
			}
		}
	}

	// 只处理 POST 请求
	if Conn.Method() != "POST" {
		h.sendErrorResponse(Conn, fmt.Errorf("method not allowed: %s", Conn.Method()))
		return true
	}

	body := Conn.GetRequestBody()

	var req struct {
		CoverURL  string `json:"coverUrl"`
		VideoID   string `json:"videoId"`
		Title     string `json:"title"`
		Author    string `json:"author"`
		ForceSave bool   `json:"forceSave"` // 是否强制保存（即使文件已存在）
	}

	if err := json.Unmarshal(body, &req); err != nil {
		utils.HandleError(err, "解析save_cover JSON")
		h.sendErrorResponse(Conn, err)
		return true
	}

	if req.CoverURL == "" {
		h.sendErrorResponse(Conn, fmt.Errorf("封面URL不能为空"))
		return true
	}

	// 创建作者目录
	authorFolder := utils.CleanFolderName(req.Author)
	if authorFolder == "" {
		authorFolder = "未知作者"
	}

	downloadsDir, err := h.getDownloadsDir()
	if err != nil {
		utils.HandleError(err, "获取下载目录")
		h.sendErrorResponse(Conn, err)
		return true
	}
	savePath := filepath.Join(downloadsDir, authorFolder)

	if err := utils.EnsureDir(savePath); err != nil {
		utils.HandleError(err, "创建作者目录")
		h.sendErrorResponse(Conn, err)
		return true
	}

	// 生成文件名：使用视频标题，如果没有则使用视频ID
	var filename string
	if req.Title != "" {
		filename = utils.CleanFilename(req.Title)
	} else if req.VideoID != "" {
		filename = "cover_" + req.VideoID
	} else {
		filename = "cover_" + fmt.Sprintf("%d", time.Now().Unix())
	}

	// 确保文件扩展名
	filename = utils.EnsureExtension(filename, ".jpg")
	coverPath := filepath.Join(savePath, filename)

	// 检查文件是否已存在
	if !req.ForceSave {
		if _, err := os.Stat(coverPath); err == nil {
			utils.Info("⏭️ [封面下载] 文件已存在，跳过: %s", filename)
			relativePath, _ := filepath.Rel(downloadsDir, coverPath)
			responseData := map[string]interface{}{
				"success":      true,
				"path":         coverPath,
				"message":      "文件已存在",
				"relativePath": relativePath,
			}
			responseBytes, _ := json.Marshal(responseData)
			h.sendJSONResponse(Conn, 200, responseBytes)
			return true
		}
	}

	// 下载封面图片
	client := &http.Client{
		Timeout: 30 * time.Second,
	}
	resp, err := client.Get(req.CoverURL)
	if err != nil {
		utils.HandleError(err, "下载封面图片")
		h.sendErrorResponse(Conn, fmt.Errorf("下载封面失败: %v", err))
		return true
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		h.sendErrorResponse(Conn, fmt.Errorf("下载封面失败: HTTP %d", resp.StatusCode))
		return true
	}

	// 保存文件
	out, err := os.Create(coverPath)
	if err != nil {
		utils.HandleError(err, "创建封面文件")
		h.sendErrorResponse(Conn, err)
		return true
	}
	defer out.Close()

	written, err := io.Copy(out, resp.Body)
	if err != nil {
		utils.HandleError(err, "写入封面数据")
		h.sendErrorResponse(Conn, err)
		return true
	}

	fileSize := float64(written) / 1024 // KB
	relativePath, _ := filepath.Rel(downloadsDir, coverPath)

	utils.Info("✓ [封面下载] 封面已保存: %s (%.2f KB)", relativePath, fileSize)

	responseData := map[string]interface{}{
		"success":      true,
		"path":         coverPath,
		"relativePath": relativePath,
		"size":         fileSize,
	}
	responseBytes, err := json.Marshal(responseData)
	if err != nil {
		utils.HandleError(err, "生成响应JSON")
		h.sendErrorResponse(Conn, err)
		return true
	}
	h.sendJSONResponse(Conn, 200, responseBytes)
	return true
}

// HandleDownloadVideo 处理从URL下载视频请求
// HandleDownloadVideo 处理从URL下载视频请求
func (h *UploadHandler) HandleDownloadVideo(Conn SunnyNet.ConnHTTP) bool {
	u, err := url.Parse(Conn.URL())
	if err != nil {
		return false
	}
	path := u.Path
	if path != "/__wx_channels_api/download_video" {
		return false
	}

	// 授权校验
	if h.getConfig() != nil && h.getConfig().SecretToken != "" {
		auth := nf_http.Header(Conn.GetRequestHeader()).Get("X-Local-Auth")
		if auth != h.getConfig().SecretToken {
			headers := make(nf_http.Header)
			headers.Set("Content-Type", "application/json")
			headers.Set("X-Content-Type-Options", "nosniff")
			Conn.StopRequest(401, `{"success":false,"error":"unauthorized"}`, headers)
			return true
		}
	}
	if h.getConfig() != nil && len(h.getConfig().AllowedOrigins) > 0 {
		origin := nf_http.Header(Conn.GetRequestHeader()).Get("Origin")
		if origin != "" {
			allowed := false
			for _, o := range h.getConfig().AllowedOrigins {
				if o == origin {
					allowed = true
					break
				}
			}
			if !allowed {
				headers := make(nf_http.Header)
				headers.Set("Content-Type", "application/json")
				headers.Set("X-Content-Type-Options", "nosniff")
				Conn.StopRequest(403, `{"success":false,"error":"forbidden_origin"}`, headers)
				return true
			}
		}
	}

	// 只处理 POST 请求
	if Conn.Method() != "POST" {
		h.sendErrorResponse(Conn, fmt.Errorf("method not allowed: %s", Conn.Method()))
		return true
	}

	body := Conn.GetRequestBody()

	var req struct {
		VideoURL   string `json:"videoUrl"`
		VideoID    string `json:"videoId"`
		Title      string `json:"title"`
		Author     string `json:"author"`
		Key        string `json:"key"`        // 解密key（可选）
		ForceSave  bool   `json:"forceSave"`  // 是否强制保存（即使文件已存在）
		Resolution string `json:"resolution"` // 分辨率字符串（如 "1080x1920" 或 "1080p"）
		Width      int    `json:"width"`      // 视频宽度（可选）
		Height     int    `json:"height"`     // 视频高度（可选）
		FileFormat string `json:"fileFormat"` // 文件格式（如 "hd", "sd" 等）
	}

	if err := json.Unmarshal(body, &req); err != nil {
		h.sendErrorResponse(Conn, err)
		return true
	}

	if req.VideoURL == "" {
		h.sendErrorResponse(Conn, fmt.Errorf("missing_url"))
		return true
	}

	// 异步下载
	go func() {
		downloadsDir, err := h.getDownloadsDir()
		if err != nil {
			utils.LogError("获取下载目录失败: %v", err)
			return
		}

		// 创建作者目录
		authorFolder := utils.CleanFolderName(req.Author)
		if authorFolder == "" {
			authorFolder = "未知作者"
		}
		savePath := filepath.Join(downloadsDir, authorFolder)
		if err := utils.EnsureDir(savePath); err != nil {
			utils.LogError("创建作者目录失败: %v", err)
			return
		}

		// Generate filename from Title and VideoID
		filename := utils.GenerateVideoFilename(req.Title, req.VideoID)

		// 确保文件名安全
		filename = utils.CleanFilename(filename)
		if !strings.HasSuffix(filename, ".mp4") {
			filename += ".mp4"
		}

		// 创建下载请求
		resp, err := http.Get(req.VideoURL)
		if err != nil {
			utils.LogError("下载请求失败: %v", err)
			return
		}
		defer resp.Body.Close()

		// 创建文件
		finalPath := filepath.Join(savePath, filename)
		out, err := os.Create(finalPath)
		if err != nil {
			utils.LogError("创建文件失败: %v", err)
			return
		}
		defer out.Close()

		// 写入文件
		_, err = io.Copy(out, resp.Body)
		if err != nil {
			utils.LogError("写入文件失败: %v", err)
			return
		}

		utils.Info("✅ 下载完成: %s", filename)
	}()

	h.sendJSONResponse(Conn, 200, []byte(`{"success":true}`))
	return true
}

// abs 返回 int64 的绝对值
func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// downloadVideoWithRetry 执行一次视频下载尝试（支持重试和断点续传）
func (h *UploadHandler) downloadVideoWithRetry(ctx context.Context, client *http.Client, httpReq *http.Request, req struct {
	VideoURL  string
	VideoID   string
	Title     string
	Author    string
	Key       string
	ForceSave bool
}, videoPath string, needDecrypt bool, resumeOffset int64, written *int64, expectedTotalSize *int64) error {
	tmpPath := videoPath + ".tmp"
	prefixLen := int64(131072) // 128KB 加密前缀长度

	// 发送请求
	resp, err := client.Do(httpReq)
	if err != nil {
		// 确保错误时也尝试关闭响应体（如果存在）
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		return fmt.Errorf("请求失败: %v", err)
	}

	// 确保响应体总是被关闭
	defer func() {
		if resp != nil && resp.Body != nil {
			// 尝试完全读取并关闭，避免连接泄漏
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()

	// 检查响应状态（支持 200 和 206 Partial Content）
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		// 读取并丢弃响应体，确保连接可以复用
		io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	// 如果服务器不支持 Range，重新下载
	if resumeOffset > 0 && resp.StatusCode != http.StatusPartialContent {
		utils.Warn("⚠️ [视频下载] 服务器不支持断点续传，需要重新下载")
		return fmt.Errorf("服务器不支持断点续传")
	}

	// 计算期望的总文件大小
	if resp.StatusCode == http.StatusPartialContent {
		// 断点续传：总大小 = 已下载 + Content-Length
		if resp.ContentLength > 0 {
			*expectedTotalSize = resumeOffset + resp.ContentLength
		} else {
			*expectedTotalSize = -1 // 未知大小
		}
	} else {
		// 完整下载：总大小 = Content-Length
		*expectedTotalSize = resp.ContentLength
	}

	if *expectedTotalSize > 0 {
		sizeMB := float64(*expectedTotalSize) / (1024 * 1024)
		utils.Info("📦 [视频下载] 期望文件大小: %.2f MB", sizeMB)
	}

	// 打开/创建文件（断点续传时追加，否则创建新文件）
	var out *os.File
	if resumeOffset > 0 {
		out, err = os.OpenFile(tmpPath, os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return fmt.Errorf("打开文件失败（断点续传）: %v", err)
		}
	} else {
		out, err = os.Create(tmpPath)
		if err != nil {
			return fmt.Errorf("创建文件失败: %v", err)
		}
	}
	defer out.Close()

	*written = 0

	if needDecrypt {
		if resumeOffset >= prefixLen {
			// 断点续传：已下载部分 >= 128KB，直接复制剩余数据（未加密）
			utils.Info("🔐 [视频下载] 加密视频断点续传，复制剩余数据（未加密部分）...")
			n, err := io.Copy(out, resp.Body)
			if err != nil {
				return fmt.Errorf("写入视频数据失败: %v", err)
			}
			*written = n
		} else {
			// 从头开始下载：需要解密前128KB
			utils.Info("🔐 [视频下载] 开始解密下载...")

			// 解析 key 为 uint64
			seed, err := parseKey(req.Key)
			if err != nil {
				return fmt.Errorf("解析密钥失败: %v", err)
			}

			// 生成 128KB 解密数组
			decryptorPrefix := util.GenerateDecryptorArray(seed, int(prefixLen))
			utils.Info("🔑 [视频下载] 从 key 生成解密数组，长度: %d bytes", len(decryptorPrefix))

			// 读取前缀数据
			prefixData := make([]byte, prefixLen)
			n, err := io.ReadFull(resp.Body, prefixData)
			if err != nil && err != io.ErrUnexpectedEOF {
				return fmt.Errorf("读取前缀失败: %v", err)
			}
			prefixData = prefixData[:n]

			utils.Info("📖 [视频下载] 读取前缀: %d bytes", n)

			// 解密前缀
			decryptedPrefix := util.XorDecrypt(prefixData, decryptorPrefix)

			// 写入解密后的前缀
			nw, err := out.Write(decryptedPrefix)
			if err != nil {
				return fmt.Errorf("写入解密前缀失败: %v", err)
			}
			*written += int64(nw)

			utils.Info("✓ [视频下载] 前缀解密完成")

			// 复制剩余数据（未加密）
			buf := make([]byte, 32*1024)
			for {
				select {
				case <-ctx.Done():
					return fmt.Errorf("下载已取消")
				default:
				}

				nr, er := resp.Body.Read(buf)
				if nr > 0 {
					nw, ew := out.Write(buf[0:nr])
					if ew != nil {
						return fmt.Errorf("写入视频数据失败: %v", ew)
					}
					*written += int64(nw)
					if nr != nw {
						return fmt.Errorf("写入不完整: 期望 %d, 实际 %d", nr, nw)
					}
				}
				if er != nil {
					if er != io.EOF {
						return fmt.Errorf("读取视频数据失败: %v", er)
					}
					break
				}
			}
		}
	} else {
		// 普通下载（非加密）
		utils.Info("📥 [视频下载] 开始下载...")
		n, err := io.Copy(out, resp.Body)
		if err != nil {
			return fmt.Errorf("写入视频数据失败: %v", err)
		}
		*written = n
	}

	// 关闭文件
	if err := out.Close(); err != nil {
		return fmt.Errorf("关闭文件失败: %v", err)
	}

	// 验证文件大小
	if *written == 0 {
		os.Remove(tmpPath)
		return fmt.Errorf("下载的文件为空")
	}

	// 验证实际写入的大小（对于断点续传，需要加上已下载的部分）
	actualWritten := *written
	if resumeOffset > 0 {
		actualWritten = resumeOffset + *written
	}

	// 如果知道期望大小，验证是否匹配
	if *expectedTotalSize > 0 {
		if actualWritten != *expectedTotalSize {
			diff := abs(actualWritten - *expectedTotalSize)
			diffPercent := float64(diff) / float64(*expectedTotalSize) * 100
			// 如果差异超过1%，认为下载不完整
			if diffPercent > 1.0 {
				return fmt.Errorf("下载不完整: 期望 %d bytes (%.2f MB), 实际 %d bytes (%.2f MB), 差异 %.2f%%",
					*expectedTotalSize, float64(*expectedTotalSize)/(1024*1024),
					actualWritten, float64(actualWritten)/(1024*1024),
					diffPercent)
			}
			// 差异在1%以内，记录警告但继续
			if diff > 0 {
				utils.Warn("⚠️ [视频下载] 文件大小略有差异: 期望 %.2f MB, 实际 %.2f MB, 差异 %.2f%%",
					float64(*expectedTotalSize)/(1024*1024),
					float64(actualWritten)/(1024*1024),
					diffPercent)
			}
		} else {
			utils.Info("✓ [视频下载] 下载大小验证通过: %.2f MB", float64(actualWritten)/(1024*1024))
		}
	}

	return nil
}

// HandleUploadStatus 查询已上传的分片列表
func (h *UploadHandler) HandleUploadStatus(Conn SunnyNet.ConnHTTP) bool {
	u, err := url.Parse(Conn.URL())
	if err != nil {
		return false
	}
	path := u.Path
	if path != "/__wx_channels_api/upload_status" {
		return false
	}

	if h.getConfig() != nil && h.getConfig().SecretToken != "" {
		auth := nf_http.Header(Conn.GetRequestHeader()).Get("X-Local-Auth")
		if auth != h.getConfig().SecretToken {
			headers := make(nf_http.Header)
			headers.Set("Content-Type", "application/json")
			headers.Set("X-Content-Type-Options", "nosniff")
			Conn.StopRequest(401, `{"success":false,"error":"unauthorized"}`, headers)
			return true
		}
	}
	if h.getConfig() != nil && len(h.getConfig().AllowedOrigins) > 0 {
		origin := nf_http.Header(Conn.GetRequestHeader()).Get("Origin")
		if origin != "" {
			allowed := false
			for _, o := range h.getConfig().AllowedOrigins {
				if o == origin {
					allowed = true
					break
				}
			}
			if !allowed {
				headers := make(nf_http.Header)
				headers.Set("Content-Type", "application/json")
				headers.Set("X-Content-Type-Options", "nosniff")
				Conn.StopRequest(403, `{"success":false,"error":"forbidden_origin"}`, headers)
				return true
			}
		}
	}

	body := Conn.GetRequestBody()

	var req struct {
		UploadId string `json:"uploadId"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendErrorResponse(Conn, err)
		return true
	}
	if req.UploadId == "" {
		h.sendErrorResponse(Conn, fmt.Errorf("missing_uploadId"))
		return true
	}

	downloadsDir, err := h.getDownloadsDir()
	if err != nil {
		h.sendErrorResponse(Conn, err)
		return true
	}
	upDir := filepath.Join(downloadsDir, ".uploads", req.UploadId)
	entries, err := os.ReadDir(upDir)
	if err != nil {
		h.sendErrorResponse(Conn, err)
		return true
	}

	parts := []int{}
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".part") && len(name) >= 10 {
			idxStr := strings.TrimSuffix(name, ".part")
			if n, convErr := strconv.Atoi(strings.TrimLeft(idxStr, "0")); convErr == nil {
				parts = append(parts, n)
			} else if idxStr == "000000" { // 0 特判
				parts = append(parts, 0)
			}
		}
	}

	resp := map[string]interface{}{"success": true, "parts": parts}
	b, _ := json.Marshal(resp)
	h.sendJSONResponse(Conn, 200, b)
	return true
}

// sendSuccessResponse 发送成功响应
func (h *UploadHandler) sendSuccessResponse(Conn SunnyNet.ConnHTTP) {
	headers := make(nf_http.Header)
	headers.Set("Content-Type", "application/json")
	headers.Set("Cache-Control", "no-cache, no-store, must-revalidate")
	headers.Set("Pragma", "no-cache")
	headers.Set("Expires", "0")
	headers.Set("X-Content-Type-Options", "nosniff")
	if h.getConfig() != nil && len(h.getConfig().AllowedOrigins) > 0 {
		origin := nf_http.Header(Conn.GetRequestHeader()).Get("Origin")
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
	Conn.StopRequest(200, `{"success":true}`, headers)
}

// sendJSONResponse 发送JSON响应
func (h *UploadHandler) sendJSONResponse(Conn SunnyNet.ConnHTTP, statusCode int, data []byte) {
	headers := make(nf_http.Header)
	headers.Set("Content-Type", "application/json")
	headers.Set("X-Content-Type-Options", "nosniff")
	if h.getConfig() != nil && len(h.getConfig().AllowedOrigins) > 0 {
		origin := nf_http.Header(Conn.GetRequestHeader()).Get("Origin")
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
	Conn.StopRequest(statusCode, string(data), headers)
}

// sendErrorResponse 发送错误响应
func (h *UploadHandler) sendErrorResponse(Conn SunnyNet.ConnHTTP, err error) {
	headers := make(nf_http.Header)
	headers.Set("Content-Type", "application/json")
	headers.Set("X-Content-Type-Options", "nosniff")
	if h.getConfig() != nil && len(h.getConfig().AllowedOrigins) > 0 {
		origin := nf_http.Header(Conn.GetRequestHeader()).Get("Origin")
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
	errorMsg := fmt.Sprintf(`{"success":false,"error":"%s"}`, err.Error())
	Conn.StopRequest(500, errorMsg, headers)
}

// 注意：saveDownloadRecord 方法已被移除
// 原因：该方法创建的下载记录使用未格式化的文件名（包含 ？ 字符），
// 而队列系统的 CompleteDownload() 方法使用格式化的文件名（？ 替换为 _），
// 导致出现重复记录且文件名格式不一致。
// 现在统一使用队列系统的 CompleteDownload() 方法来创建下载记录。
