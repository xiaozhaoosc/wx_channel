package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/fatih/color"
	"github.com/qtgolang/SunnyNet/SunnyNet"
	nf_http "github.com/qtgolang/SunnyNet/src/http"
	"github.com/qtgolang/SunnyNet/src/public"

	"wx_channel/internal/config"
	"wx_channel/internal/database"
	"wx_channel/internal/handlers"
	"wx_channel/internal/storage"
	"wx_channel/internal/utils"
	"wx_channel/pkg/argv"
	"wx_channel/pkg/certificate"
	"wx_channel/pkg/proxy"
)

//go:embed certs/SunnyRoot.cer
var cert_data []byte

//go:embed lib/FileSaver.min.js
var file_saver_js []byte

//go:embed lib/jszip.min.js
var zip_js []byte

//go:embed inject/main.js
var main_js []byte

var Sunny = SunnyNet.NewSunny()
var cfg *config.Config
var v string
var port int
var currentPageURL = "" // 存储当前页面的完整URL
var logInitMsg string

// 全局管理器
var (
	csvManager        *storage.CSVManager
	fileManager       *storage.FileManager
	apiHandler        *handlers.APIHandler
	uploadHandler     *handlers.UploadHandler
	recordHandler     *handlers.RecordHandler
	scriptHandler     *handlers.ScriptHandler
	batchHandler      *handlers.BatchHandler
	commentHandler    *handlers.CommentHandler
	consoleAPIHandler *handlers.ConsoleAPIHandler
	webSocketHandler  *handlers.WebSocketHandler
)

// downloadRecordsHeader CSV 文件的表头
var downloadRecordsHeader = []string{"ID", "标题", "视频号名称", "视频号分类", "公众号名称", "视频链接", "页面链接", "文件大小", "时长", "阅读量", "点赞量", "评论量", "收藏数", "转发数", "创建时间", "IP所在地", "下载时间", "页面来源", "搜索关键词"}

// initDownloadRecords 初始化下载记录系统
func initDownloadRecords() error {
	// 解析下载目录路径
	downloadsDir, err := utils.ResolveDownloadDir(cfg.DownloadsDir)
	if err != nil {
		return fmt.Errorf("解析下载目录失败: %v", err)
	}

	// 创建文件管理器
	fileManager, err = storage.NewFileManager(downloadsDir)
	if err != nil {
		return fmt.Errorf("创建文件管理器失败: %v", err)
	}

	// 创建CSV管理器
	csvPath := filepath.Join(downloadsDir, cfg.RecordsFile)
	csvManager, err = storage.NewCSVManager(csvPath, downloadRecordsHeader)
	if err != nil {
		return fmt.Errorf("创建CSV管理器失败: %v", err)
	}

	return nil
}

// 已废弃的辅助函数：addDownloadRecord 已移除，避免未使用告警

// saveDynamicHTML 保存动态页面的完整HTML内容，按日期和域名归档
func saveDynamicHTML(htmlContent string, parsedURL *url.URL, fullURL string, timestamp int64) {
	if fileManager == nil {
		utils.Warn("文件管理器未初始化，无法保存页面内容: %s", fullURL)
		return
	}
	if cfg == nil {
		utils.Warn("配置尚未初始化，无法保存页面内容: %s", fullURL)
		return
	}
	// 检查是否启用页面快照保存
	if !cfg.SavePageSnapshot {
		return
	}
	if htmlContent == "" {
		utils.Warn("收到空的HTML内容，跳过保存: %s", fullURL)
		return
	}
	if parsedURL == nil {
		utils.Warn("解析页面URL失败，跳过保存: %s", fullURL)
		return
	}

	if cfg.SaveDelay > 0 {
		time.Sleep(cfg.SaveDelay)
	}

	saveTime := time.Now()
	if timestamp > 0 {
		saveTime = time.Unix(0, timestamp*int64(time.Millisecond))
	}

	downloadsDir, err := utils.ResolveDownloadDir(cfg.DownloadsDir)
	if err != nil {
		utils.HandleError(err, "解析下载目录用于保存页面内容")
		return
	}

	if err := utils.EnsureDir(downloadsDir); err != nil {
		utils.HandleError(err, "创建下载目录用于保存页面内容")
		return
	}

	pagesRoot := filepath.Join(downloadsDir, "page_snapshots")
	if err := utils.EnsureDir(pagesRoot); err != nil {
		utils.HandleError(err, "创建页面保存根目录")
		return
	}

	// 去掉域名文件夹，直接使用日期目录
	dateDir := filepath.Join(pagesRoot, saveTime.Format("2006-01-02"))
	if err := utils.EnsureDir(dateDir); err != nil {
		utils.HandleError(err, "创建页面保存日期目录")
		return
	}

	var filenameParts []string
	if parsedURL.Path != "" && parsedURL.Path != "/" {
		segments := strings.Split(parsedURL.Path, "/")
		for _, segment := range segments {
			segment = strings.TrimSpace(segment)
			if segment == "" || segment == "." {
				continue
			}
			filenameParts = append(filenameParts, utils.CleanFilename(segment))
		}
	}

	if parsedURL.RawQuery != "" {
		querySegment := strings.ReplaceAll(parsedURL.RawQuery, "&", "_")
		querySegment = strings.ReplaceAll(querySegment, "=", "-")
		querySegment = utils.CleanFilename(querySegment)
		if querySegment != "" {
			filenameParts = append(filenameParts, querySegment)
		}
	}

	if len(filenameParts) == 0 {
		filenameParts = append(filenameParts, "page")
	}

	baseName := strings.Join(filenameParts, "_")
	// CleanFilename 已经处理了长度限制，这里不需要再次限制

	fileName := fmt.Sprintf("%s_%s.html", saveTime.Format("150405"), baseName)
	targetPath := utils.GenerateUniqueFilename(dateDir, fileName, 100)

	if err := os.WriteFile(targetPath, []byte(htmlContent), 0644); err != nil {
		utils.HandleError(err, "保存页面HTML内容")
		return
	}

	metaData := map[string]interface{}{
		"url":       fullURL,
		"host":      parsedURL.Host,
		"path":      parsedURL.Path,
		"query":     parsedURL.RawQuery,
		"saved_at":  saveTime.Format(time.RFC3339),
		"timestamp": timestamp,
	}

	metaBytes, err := json.MarshalIndent(metaData, "", "  ")
	if err == nil {
		metaPath := strings.TrimSuffix(targetPath, filepath.Ext(targetPath)) + ".meta.json"
		if err := os.WriteFile(metaPath, metaBytes, 0644); err != nil {
			utils.HandleError(err, "保存页面HTML元数据")
		}
	} else {
		utils.HandleError(err, "序列化页面HTML元数据")
	}

	relativePath, err := filepath.Rel(downloadsDir, targetPath)
	if err != nil {
		relativePath = targetPath
	}
	utils.Info("页面HTML已保存: %s -> %s", fullURL, relativePath)
	utils.LogInfo("[页面快照] URL=%s | 路径=%s", fullURL, relativePath)
}

// saveSearchData 保存搜索页面的结构化数据（账号信息、直播数据、动态数据）
func saveSearchData(fullURL string, parsedURL *url.URL, keyword string, profiles, liveResults, feedResults []map[string]interface{}, timestamp int64) {
	if fileManager == nil {
		utils.Warn("文件管理器未初始化，无法保存搜索数据: %s", fullURL)
		return
	}
	if cfg == nil {
		utils.Warn("配置尚未初始化，无法保存搜索数据: %s", fullURL)
		return
	}
	// 检查是否启用搜索数据保存
	if !cfg.SaveSearchData {
		return
	}
	if parsedURL == nil {
		utils.Warn("解析搜索页面URL失败，跳过保存: %s", fullURL)
		return
	}

	if cfg.SaveDelay > 0 {
		time.Sleep(cfg.SaveDelay)
	}

	saveTime := time.Now()
	if timestamp > 0 {
		saveTime = time.Unix(0, timestamp*int64(time.Millisecond))
	}

	downloadsDir, err := utils.ResolveDownloadDir(cfg.DownloadsDir)
	if err != nil {
		utils.HandleError(err, "解析下载目录用于保存搜索数据")
		return
	}

	if err := utils.EnsureDir(downloadsDir); err != nil {
		utils.HandleError(err, "创建下载目录用于保存搜索数据")
		return
	}

	searchDataRoot := filepath.Join(downloadsDir, "search_data")
	if err := utils.EnsureDir(searchDataRoot); err != nil {
		utils.HandleError(err, "创建搜索数据根目录")
		return
	}

	// 去掉域名文件夹，直接使用日期目录
	dateDir := filepath.Join(searchDataRoot, saveTime.Format("2006-01-02"))
	if err := utils.EnsureDir(dateDir); err != nil {
		utils.HandleError(err, "创建搜索数据日期目录")
		return
	}

	// 构建文件名
	sanitizedKeyword := utils.CleanFilename(keyword)
	if sanitizedKeyword == "" {
		sanitizedKeyword = "search"
	}
	// CleanFilename 已经处理了长度限制（100字符），这里不需要再次限制

	fileName := fmt.Sprintf("%s_%s.json", saveTime.Format("150405"), sanitizedKeyword)
	targetPath := utils.GenerateUniqueFilename(dateDir, fileName, 100)

	// 构建数据结构
	searchData := map[string]interface{}{
		"url":          fullURL,
		"host":         parsedURL.Host,
		"path":         parsedURL.Path,
		"query":        parsedURL.RawQuery,
		"keyword":      keyword,
		"profiles":     profiles,
		"liveResults":  liveResults,
		"feedResults":  feedResults,
		"profileCount": len(profiles),
		"liveCount":    len(liveResults),
		"feedCount":    len(feedResults),
		"saved_at":     saveTime.Format(time.RFC3339),
		"timestamp":    timestamp,
	}

	// 保存JSON数据
	dataBytes, err := json.MarshalIndent(searchData, "", "  ")
	if err != nil {
		utils.HandleError(err, "序列化搜索数据")
		return
	}

	if err := os.WriteFile(targetPath, dataBytes, 0644); err != nil {
		utils.HandleError(err, "保存搜索数据")
		return
	}

	relativePath, err := filepath.Rel(downloadsDir, targetPath)
	if err != nil {
		relativePath = targetPath
	}
	utils.Info("搜索数据已保存: 关键词=%s, 账号=%d, 直播=%d, 动态=%d -> %s",
		keyword, len(profiles), len(liveResults), len(feedResults), relativePath)
	utils.LogInfo("[搜索数据] 关键词=%s | 账号=%d | 直播=%d | 动态=%d | 路径=%s",
		keyword, len(profiles), len(liveResults), len(feedResults), relativePath)
}

// printDownloadRecordInfo 打印下载记录信息
func printDownloadRecordInfo() {
	utils.PrintSeparator()
	color.Blue("📋 下载记录信息")
	utils.PrintSeparator()

	downloadsDir, err := utils.ResolveDownloadDir(cfg.DownloadsDir)
	if err != nil {
		utils.HandleError(err, "解析下载目录")
		return
	}

	recordsPath := filepath.Join(downloadsDir, cfg.RecordsFile)
	utils.PrintLabelValue("📁", "记录文件", recordsPath)
	utils.PrintLabelValue("✏️", "记录格式", "CSV表格格式")
	utils.PrintLabelValue("📊", "记录字段", strings.Join(downloadRecordsHeader, ", "))
	utils.PrintSeparator()
}

// printEnvConfig 打印环境变量配置信息（只要设置了任何环境变量就显示所有相关配置）
func printEnvConfig() {
	// 检查是否有任何环境变量被设置
	hasAnyConfig := os.Getenv("WX_CHANNEL_TOKEN") != "" ||
		os.Getenv("WX_CHANNEL_ALLOWED_ORIGINS") != "" ||
		os.Getenv("WX_CHANNEL_LOG_FILE") != "" ||
		os.Getenv("WX_CHANNEL_LOG_MAX_MB") != "" ||
		os.Getenv("WX_CHANNEL_SAVE_PAGE_SNAPSHOT") != "" ||
		os.Getenv("WX_CHANNEL_SAVE_SEARCH_DATA") != "" ||
		os.Getenv("WX_CHANNEL_SAVE_PAGE_JS") != "" ||
		os.Getenv("WX_CHANNEL_SHOW_LOG_BUTTON") != "" ||
		os.Getenv("WX_CHANNEL_UPLOAD_CHUNK_CONCURRENCY") != "" ||
		os.Getenv("WX_CHANNEL_UPLOAD_MERGE_CONCURRENCY") != "" ||
		os.Getenv("WX_CHANNEL_DOWNLOAD_CONCURRENCY") != ""

	// 只有设置了任何环境变量时才显示
	if hasAnyConfig {
		utils.PrintSeparator()
		color.Blue("⚙️  环境变量配置信息")
		utils.PrintSeparator()

		// 安全配置
		if cfg.SecretToken != "" {
			utils.PrintLabelValue("🔐", "安全令牌", "已设置")
		}
		if len(cfg.AllowedOrigins) > 0 {
			utils.PrintLabelValue("🌐", "允许的Origin", strings.Join(cfg.AllowedOrigins, ", "))
		}

		// 日志配置
		if cfg.LogFile != "" {
			utils.PrintLabelValue("📝", "日志文件", cfg.LogFile)
		}
		if cfg.MaxLogSizeMB > 0 {
			utils.PrintLabelValue("📊", "日志最大大小", fmt.Sprintf("%d MB", cfg.MaxLogSizeMB))
		}

		// 保存功能开关
		utils.PrintLabelValue("💾", "保存页面快照", fmt.Sprintf("%v", cfg.SavePageSnapshot))
		utils.PrintLabelValue("🔍", "保存搜索数据", fmt.Sprintf("%v", cfg.SaveSearchData))
		utils.PrintLabelValue("📄", "保存JS文件", fmt.Sprintf("%v", cfg.SavePageJS))

		// UI功能开关
		utils.PrintLabelValue("🖼️", "显示日志按钮", fmt.Sprintf("%v", cfg.ShowLogButton))

		// 并发配置
		utils.PrintLabelValue("📤", "分片上传并发", cfg.UploadChunkConcurrency)
		utils.PrintLabelValue("🔀", "分片合并并发", cfg.UploadMergeConcurrency)
		utils.PrintLabelValue("📥", "批量下载并发", cfg.DownloadConcurrency)

		utils.PrintSeparator()
	}
}

// 打印帮助信息
func print_usage() {
	fmt.Printf("Usage: wx_video_download [OPTION...]\n")
	fmt.Printf("Download WeChat video.\n\n")
	fmt.Printf("      --help                 display this help and exit\n")
	fmt.Printf("  -v, --version              output version information and exit\n")
	fmt.Printf("  -p, --port                 set proxy server network port\n")
	fmt.Printf("  -d, --dev                  set proxy server network device\n")
	fmt.Printf("      --uninstall            uninstall root certificate and exit\n")
	os.Exit(0)
}

// 卸载证书
func uninstall_certificate() {
	color.Yellow("正在卸载根证书...\n")

	// 检查证书是否存在
	existing, err := certificate.CheckCertificate("SunnyNet")
	if err != nil {
		color.Red("检查证书时发生错误: %v\n", err.Error())
		color.Yellow("请手动检查证书是否已安装。\n")
		os.Exit(1)
	}

	if !existing {
		color.Green("✓ 证书未安装，无需卸载。\n")
		os.Exit(0)
	}

	// 尝试卸载证书
	err = certificate.RemoveCertificate("SunnyNet")
	if err != nil {
		color.Red("卸载证书失败: %v\n", err.Error())
		color.Yellow("请尝试以管理员身份运行此命令。\n")
		os.Exit(1)
	}

	color.Green("✓ 证书卸载成功！\n")
	color.Yellow("注意：如果程序仍在运行，请重启浏览器以确保更改生效。\n")
	os.Exit(0)
}

// printTitle 打印标题
func printTitle() {
	color.Set(color.FgCyan)
	fmt.Println("")
	fmt.Println(" ██╗    ██╗██╗  ██╗     ██████╗██╗  ██╗ █████╗ ███╗   ██╗███╗   ██╗███████╗██╗     ")
	fmt.Println(" ██║    ██║╚██╗██╔╝    ██╔════╝██║  ██║██╔══██╗████╗  ██║████╗  ██║██╔════╝██║     ")
	fmt.Println(" ██║ █╗ ██║ ╚███╔╝     ██║     ███████║███████║██╔██╗ ██║██╔██╗ ██║█████╗  ██║     ")
	fmt.Println(" ██║███╗██║ ██╔██╗     ██║     ██╔══██║██╔══██║██║╚██╗██║██║╚██╗██║██╔══╝  ██║     ")
	fmt.Println(" ╚███╔███╔╝██╔╝ ██╗    ╚██████╗██║  ██║██║  ██║██║ ╚████║██║ ╚████║███████╗███████╗")
	fmt.Println("  ╚══╝╚══╝ ╚═╝  ╚═╝     ╚═════╝╚═╝  ╚═╝╚═╝  ╚═╝╚═╝  ╚═══╝╚═╝  ╚═══╝╚══════╝╚══════╝")
	color.Unset()

	color.Yellow("    微信视频号下载助手 v%s", cfg.Version)
	color.Yellow("    项目地址：https://github.com/nobiyou/wx_channel")
	color.Green("    v5.2.10 更新要点：")
	color.Green("    • 解除批量下载数量限制，优化批量下载UI")
	color.Green("    • 增加视频和直播回放样式区别")
	color.Green("    • web控制台增加批量下载相关操作")
	color.Green("    • 增加取消下载，继续下载及清除任务功能")
	fmt.Println()
}

// 格式化视频时长为时分秒
// formatDuration 和 formatNumber 已移至 internal/utils/output.go
func main() {
	// 初始化配置
	cfg = config.Load()
	// 记录配置加载
	utils.LogConfigLoad("config.yaml", true)

	// 初始化日志（可选滚动）
	if cfg.LogFile != "" {
		_ = utils.InitLoggerWithRotation(utils.INFO, cfg.LogFile, cfg.MaxLogSizeMB)
		logInitMsg = fmt.Sprintf("日志已初始化: %s (最大 %dMB)", cfg.LogFile, cfg.MaxLogSizeMB)
	}
	port = cfg.Port
	v = "?t=" + cfg.Version

	os_env := runtime.GOOS
	args := argv.ArgsToMap(os.Args) // 分解参数列表为Map
	if _, ok := args["help"]; ok {
		print_usage()
	} // 存在help则输出帮助信息并退出主程序
	if v, ok := args["v"]; ok { // 存在v则输出版本信息并退出主程序
		fmt.Printf("v%s %.0s\n", cfg.Version, v)
		os.Exit(0)
	}
	if v, ok := args["version"]; ok { // 存在version则输出版本信息并退出主程序
		fmt.Printf("v%s %.0s\n", cfg.Version, v)
		os.Exit(0)
	}
	if _, ok := args["uninstall"]; ok { // 存在uninstall则卸载证书并退出主程序
		uninstall_certificate()
	}
	// 设置参数默认值
	args["dev"] = argv.ArgsValue(args, "", "d", "dev")
	args["port"] = argv.ArgsValue(args, "", "p", "port")

	iport, errstr := strconv.Atoi(args["port"])
	if errstr != nil {
		args["port"] = strconv.Itoa(cfg.DefaultPort) // 用户自定义值解析失败则使用默认端口
	} else {
		port = iport
		cfg.SetPort(port)
	}

	delete(args, "p") // 删除冗余的参数p
	delete(args, "d") // 删除冗余的参数d

	signalChan := make(chan os.Signal, 1)
	// Notify the signal channel on SIGINT (Ctrl+C) and SIGTERM
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-signalChan
		color.Red("\n正在关闭服务...%v\n\n", sig)
		// 记录系统关闭
		utils.LogSystemShutdown(fmt.Sprintf("收到信号: %v", sig))
		// 关闭数据库连接
		database.Close()
		if os_env == "darwin" {
			proxy.DisableProxyInMacOS(proxy.ProxySettings{
				Device:   args["dev"],
				Hostname: "127.0.0.1",
				Port:     args["port"],
			})
		} else if os_env == "windows" {
			// 在 Windows 上关闭系统代理和驱动
			utils.Info("正在关闭系统代理和驱动...")
			Sunny.OpenDrive(0) // 关闭驱动
			Sunny.Close()      // 关闭代理并尝试还原 IE 代理
			utils.Info("✓ 已关闭系统代理和驱动")
		}
		os.Exit(0)
	}()

	// 打印标题和程序信息
	printTitle()

	// 初始化下载记录系统
	if err := initDownloadRecords(); err != nil {
		utils.HandleError(err, "初始化下载记录系统")
	} else {
		printDownloadRecordInfo()
		if logInitMsg != "" {
			utils.Info(logInitMsg)
			logInitMsg = ""
		}
	}

	// 打印并发配置信息
	printEnvConfig()

	// 初始化API处理器
	apiHandler = handlers.NewAPIHandler(cfg)

	// 初始化上传处理器（需要在csvManager初始化之后）
	if csvManager != nil {
		uploadHandler = handlers.NewUploadHandler(cfg, csvManager)
		// 初始化记录处理器
		recordHandler = handlers.NewRecordHandler(cfg, csvManager)
	}

	// 初始化脚本处理器
	scriptHandler = handlers.NewScriptHandler(cfg, main_js, zip_js, file_saver_js, v)

	// 初始化批量下载处理器
	if csvManager != nil {
		batchHandler = handlers.NewBatchHandler(cfg, csvManager)
	}

	// 初始化评论处理器
	commentHandler = handlers.NewCommentHandler(cfg)

	// 初始化数据库（用于Web控制台API）
	downloadsDir, err := utils.ResolveDownloadDir(cfg.DownloadsDir)
	if err != nil {
		utils.HandleError(err, "解析下载目录用于数据库初始化")
	} else {
		dbPath := filepath.Join(downloadsDir, "console.db")
		if err := database.Initialize(&database.Config{DBPath: dbPath}); err != nil {
			utils.HandleError(err, "初始化数据库")
			utils.Warn("Web控制台功能可能受限")
		} else {
			utils.Info("✓ 数据库已初始化: %s", dbPath)

			// 设置数据库配置加载器
			settingsRepo := database.NewSettingsRepository()
			config.SetDatabaseLoader(settingsRepo)

			// 重新加载配置以应用数据库中的设置
			cfg = config.Reload()
			utils.Info("✓ 配置已从数据库重新加载")

			// 重新初始化下载记录系统（使用更新后的配置）
			if err := initDownloadRecords(); err != nil {
				utils.HandleError(err, "重新初始化下载记录系统")
			} else {
				utils.Info("✓ 下载记录系统已使用新配置重新初始化")

				// 重新初始化需要csvManager的处理器
				if csvManager != nil {
					uploadHandler = handlers.NewUploadHandler(cfg, csvManager)
					recordHandler = handlers.NewRecordHandler(cfg, csvManager)
					batchHandler = handlers.NewBatchHandler(cfg, csvManager)
					utils.Info("✓ 处理器已使用新配置重新初始化")
				}
			}
		}
	}

	// 初始化Web控制台API处理器
	consoleAPIHandler = handlers.NewConsoleAPIHandler(cfg)

	// 初始化WebSocket处理器
	webSocketHandler = handlers.NewWebSocketHandler()

	existing, err1 := certificate.CheckCertificate("SunnyNet")
	if err1 != nil {
		utils.HandleError(err1, "检查证书")
		utils.Warn("程序将继续运行，但HTTPS功能可能受限...")
		existing = false // 假设证书未安装
	} else if !existing {
		utils.Info("正在安装证书...")
		err := certificate.InstallCertificate(cert_data)
		time.Sleep(cfg.CertInstallDelay)
		if err != nil {
			utils.HandleError(err, "证书安装")
			utils.Warn("程序将继续运行，但HTTPS功能可能受限。")
			utils.Warn("如需完整功能，请手动安装证书或以管理员身份运行程序。")

			// 保存证书文件到 downloads 目录，方便用户手动安装
			if fileManager != nil {
				downloadsDir, err := utils.ResolveDownloadDir(cfg.DownloadsDir)
				if err == nil {
					certPath := filepath.Join(downloadsDir, cfg.CertFile)
					if err := utils.EnsureDir(downloadsDir); err == nil {
						if err := os.WriteFile(certPath, cert_data, 0644); err == nil {
							utils.Info("证书文件已保存到: %s", certPath)
							utils.Info("您可以双击此文件手动安装证书。")
						} else {
							utils.HandleError(err, "保存证书文件")
						}
					}
				}
			}
		} else {
			utils.Info("✓ 证书安装成功！")
		}
	} else {
		utils.Info("✓ 证书已存在，无需重新安装。")
	}
	Sunny.SetPort(port)
	Sunny.SetGoCallback(HttpCallback, nil, nil, nil)
	sunnyErr := Sunny.Start().Error
	if sunnyErr != nil {
		utils.HandleError(sunnyErr, "启动代理服务")
		utils.Warn("按 Ctrl+C 退出...")
		select {}
	}
	proxy_server := fmt.Sprintf("127.0.0.1:%v", port)
	client := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(&url.URL{
				Scheme: "http",
				Host:   proxy_server,
			}),
		},
	}
	_, err3 := client.Get("https://sunny.io/")
	if err3 == nil {
		Sunny.ProcessAddName("WeChatAppEx.exe")
		// 尝试开启驱动以捕获进程流量
		Sunny.OpenDrive(1)
		// 启用系统代理作为备份，确保微信浏览器流量能正确进入代理
		Sunny.SetIEProxy()
		utils.Info("✓ 已启用进程代理(WeChatAppEx.exe)和系统代理驱动")

		// 打印服务状态信息
		utils.PrintSeparator()
		color.Blue("📡 服务状态信息")
		utils.PrintSeparator()

		utils.PrintLabelValue("⏳", "服务状态", "已启动")
		utils.PrintLabelValue("🔌", "代理端口", port)
		utils.PrintLabelValue("📱", "支持平台", "微信视频号")

		// 记录系统启动
		proxyMode := "进程代理"
		if os_env != "windows" {
			proxyMode = "系统代理"
		}
		utils.LogSystemStart(port, proxyMode)

		// 启动WebSocket服务器（使用代理端口+1）
		// Requirements: 14.5 - WebSocket endpoint for real-time updates
		wsPort := port + 1
		go startWebSocketServer(wsPort)

		utils.Info("🔍 请打开需要下载的视频号页面进行下载")
	} else {
		utils.PrintSeparator()
		utils.Warn("⚠️ 您还未安装证书，请在浏览器打开 http://%v 并根据说明安装证书", proxy_server)
		utils.Warn("⚠️ 在安装完成后重新启动此程序即可")
		utils.PrintSeparator()
	}
	utils.Info("💡 服务正在运行，按 Ctrl+C 退出...")
	select {}
}

type ChannelProfile struct {
	Title string `json:"title"`
}
type FrontendTip struct {
	Msg string `json:"msg"`
}

// SunnyNetResponseWriter adapts SunnyNet connection to http.ResponseWriter
type SunnyNetResponseWriter struct {
	conn       SunnyNet.ConnHTTP
	headers    http.Header
	statusCode int
	body       bytes.Buffer
}

func NewSunnyNetResponseWriter(conn SunnyNet.ConnHTTP) *SunnyNetResponseWriter {
	return &SunnyNetResponseWriter{
		conn:       conn,
		headers:    make(http.Header),
		statusCode: http.StatusOK,
	}
}

func (w *SunnyNetResponseWriter) Header() http.Header {
	return w.headers
}

func (w *SunnyNetResponseWriter) Write(data []byte) (int, error) {
	return w.body.Write(data)
}

func (w *SunnyNetResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
}

func (w *SunnyNetResponseWriter) Flush() {
	w.conn.StopRequest(w.statusCode, w.body.String(), nf_http.Header(w.headers))
}

// handleConsoleAPI bridges SunnyNet to the console API handler
func handleConsoleAPI(Conn SunnyNet.ConnHTTP) {
	w := NewSunnyNetResponseWriter(Conn)
	// Construct http.Request from Conn
	u, _ := url.Parse(Conn.URL())

	// Create a dummy body that is closed as IO NopCloser
	bodyBytes := Conn.GetRequestBody()
	body := io.NopCloser(bytes.NewBuffer(bodyBytes))

	req := &http.Request{
		Method: Conn.Method(),
		URL:    u,
		Header: http.Header(Conn.GetRequestHeader()),
		Body:   body,
	}

	consoleAPIHandler.HandleAPIRequest(w, req)
	w.Flush()
}

// startWebSocketServer starts a separate HTTP server for WebSocket connections
// WebSocket requires a real HTTP connection that can be hijacked, which SunnyNet proxy doesn't support
// Requirements: 14.5 - WebSocket endpoint for real-time updates
func startWebSocketServer(wsPort int) {
	mux := http.NewServeMux()

	// WebSocket endpoint
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		// Set CORS headers for WebSocket upgrade
		origin := r.Header.Get("Origin")
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
		}
		handlers.ServeWs(w, r)
	})

	// Health check for WebSocket server
	mux.HandleFunc("/ws/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		hub := handlers.GetWebSocketHub()
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "ok",
			"clients": hub.ClientCount(),
		})
	})

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", wsPort),
		Handler: mux,
	}

	utils.Info("🔌 WebSocket服务已启动，端口: %d", wsPort)
	utils.Info("   WebSocket地址: ws://127.0.0.1:%d/ws", wsPort)

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		utils.Warn("WebSocket服务启动失败: %v", err)
	}
}

func HttpCallback(Conn SunnyNet.ConnHTTP) {
	host := ""
	path := ""
	urlStr := Conn.URL()
	if u, err := url.Parse(urlStr); err == nil {
		host = u.Hostname()
		path = u.Path
	}

	// 记录所有流量到控制台，用于确认代理是否生效
	// 如果流量过大，建议正式版中改回 LogInfo
	utils.Info("[拦截流量] Host=%s | Path=%s", host, path)

	if Conn.Type() == public.HttpSendRequest {
		// Conn.Request.Header.Set("Cache-Control", "no-cache")
		// Conn.Request.Header.Del("Accept-Encoding")

		// 处理静态文件请求
		if handlers.HandleStaticFiles(Conn, zip_js, file_saver_js) {
			return
		}

		// 处理API请求
		if apiHandler != nil {
			// 处理profile请求
			if apiHandler.HandleProfile(Conn) {
				return
			}
			// 处理tip请求
			if apiHandler.HandleTip(Conn) {
				return
			}
			// 处理page_url请求
			if apiHandler.HandlePageURL(Conn) {
				currentPageURL = apiHandler.GetCurrentURL() // 同步URL
				// 同步URL到recordHandler
				if recordHandler != nil {
					recordHandler.SetCurrentURL(currentPageURL)
				}
				return
			}
		}

		// 处理上传相关API请求
		if uploadHandler != nil {
			// 处理分片上传初始化
			if uploadHandler.HandleInitUpload(Conn) {
				return
			}
			// 处理分片上传
			if uploadHandler.HandleUploadChunk(Conn) {
				return
			}
			// 处理分片上传完成
			if uploadHandler.HandleCompleteUpload(Conn) {
				return
			}
			// 查询已上传分片
			if uploadHandler.HandleUploadStatus(Conn) {
				return
			}
			// 处理直接保存视频
			if uploadHandler.HandleSaveVideo(Conn) {
				return
			}
			// 处理保存封面图片
			if uploadHandler.HandleSaveCover(Conn) {
				return
			}
			// 处理从URL下载视频
			if uploadHandler.HandleDownloadVideo(Conn) {
				return
			}
		}

		// 处理记录相关API请求
		if recordHandler != nil {
			// 处理记录下载信息
			if recordHandler.HandleRecordDownload(Conn) {
				return
			}
			// 处理导出视频列表
			if recordHandler.HandleExportVideoList(Conn) {
				return
			}
			// 处理导出视频列表(JSON)
			if recordHandler.HandleExportVideoListJSON(Conn) {
				return
			}
			// 处理导出视频列表(Markdown)
			if recordHandler.HandleExportVideoListMarkdown(Conn) {
				return
			}
			// 处理批量下载状态
			if recordHandler.HandleBatchDownloadStatus(Conn) {
				return
			}
		}

		// 处理批量下载相关API请求
		if batchHandler != nil {
			if batchHandler.HandleBatchStart(Conn) {
				return
			}
			if batchHandler.HandleBatchProgress(Conn) {
				return
			}
			if batchHandler.HandleBatchCancel(Conn) {
				return
			}
			if batchHandler.HandleBatchResume(Conn) {
				return
			}
			if batchHandler.HandleBatchClear(Conn) {
				return
			}
			if batchHandler.HandleBatchFailed(Conn) {
				return
			}
		}

		// 处理评论数据保存请求
		if commentHandler != nil {
			if commentHandler.HandleSaveCommentData(Conn) {
				return
			}
		}

		// 提供 Web 控制台
		if path == "/console" || path == "/console/" {
			consoleHTML, err := os.ReadFile("web/console.html")
			if err != nil {
				utils.Warn("无法读取 web/console.html: %v", err)
				Conn.StopRequest(404, "Console not found", make(nf_http.Header))
				return
			}
			headers := make(nf_http.Header)
			headers.Set("Content-Type", "text/html; charset=utf-8")
			Conn.StopRequest(200, string(consoleHTML), headers)
			return
		}

		// 提供 Web 控制台静态资源 (js/, css/, docs/, 图片等)
		// 先检查是否是微信资源路径，如果是则跳过（让请求转发到微信服务器）
		isWeixinResource := strings.Contains(path, "pic_blank.gif") ||
			strings.Contains(path, "we-emoji") ||
			strings.Contains(path, "Expression") ||
			strings.Contains(path, "auth_icon") ||
			strings.Contains(path, "weixin/checkresupdate") ||
			strings.Contains(path, "fed_upload") ||
			strings.HasPrefix(path, "/a/") ||
			strings.HasPrefix(path, "/weixin/")

		// 只有Web控制台的资源才从本地读取，微信资源直接跳过
		// 注意：如果路径匹配静态文件模式但不是微信资源，且文件不存在，也不输出警告
		// 因为这些可能是微信服务器的资源，应该让请求继续转发
		if !isWeixinResource && (strings.HasPrefix(path, "/js/") || strings.HasPrefix(path, "/css/") || strings.HasPrefix(path, "/docs/") ||
			strings.HasSuffix(path, ".png") || strings.HasSuffix(path, ".jpg") ||
			strings.HasSuffix(path, ".jpeg") || strings.HasSuffix(path, ".gif") ||
			strings.HasSuffix(path, ".svg") || strings.HasSuffix(path, ".ico") ||
			strings.HasSuffix(path, ".md")) {
			filePath := "web" + path
			content, err := os.ReadFile(filePath)
			if err != nil {
				// 文件不存在时，直接跳过（不拦截），让请求继续转发到微信服务器
				// 不输出警告，不返回404，这样可以避免对微信服务器资源的误报警告
				// 同时让微信服务器的资源能够正常加载
				return
			}
			headers := make(nf_http.Header)
			if strings.HasSuffix(path, ".js") {
				headers.Set("Content-Type", "application/javascript; charset=utf-8")
			} else if strings.HasSuffix(path, ".css") {
				headers.Set("Content-Type", "text/css; charset=utf-8")
			} else if strings.HasSuffix(path, ".md") {
				headers.Set("Content-Type", "text/markdown; charset=utf-8")
			} else if strings.HasSuffix(path, ".png") {
				headers.Set("Content-Type", "image/png")
			} else if strings.HasSuffix(path, ".jpg") || strings.HasSuffix(path, ".jpeg") {
				headers.Set("Content-Type", "image/jpeg")
			} else if strings.HasSuffix(path, ".gif") {
				headers.Set("Content-Type", "image/gif")
			} else if strings.HasSuffix(path, ".svg") {
				headers.Set("Content-Type", "image/svg+xml")
			} else if strings.HasSuffix(path, ".ico") {
				headers.Set("Content-Type", "image/x-icon")
			}
			Conn.StopRequest(200, string(content), headers)
			return
		}

		// 处理Web控制台REST API请求
		if strings.HasPrefix(path, "/api/") && consoleAPIHandler != nil {
			handleConsoleAPI(Conn)
			return
		}

		// 处理预检请求（CORS）
		if strings.HasPrefix(path, "/__wx_channels_api/") && (Conn.Method() == "OPTIONS" || Conn.Method() == "POST") {
			// Note: Accessing Method via Method() assuming it exists. If not, we might be blind.
			if Conn.Method() == "OPTIONS" {
				headers := make(nf_http.Header)
				headers.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
				headers.Set("Access-Control-Allow-Headers", "Content-Type, X-Local-Auth")
				// 若配置了允许的 Origin 且来路匹配，回显 origin
				if cfg != nil && len(cfg.AllowedOrigins) > 0 {
					origin := nf_http.Header(Conn.GetRequestHeader()).Get("Origin")
					if origin != "" {
						for _, o := range cfg.AllowedOrigins {
							if o == origin {
								headers.Set("Access-Control-Allow-Origin", origin)
								headers.Set("Vary", "Origin")
								break
							}
						}
					}
				}
				Conn.StopRequest(204, "", headers)
				return
			}
		}

		// 保存页面完整内容的API端点（用于测试，保留在main.go中）
		if path == "/__wx_channels_api/save_page_content" {
			var contentData struct {
				URL       string `json:"url"`
				HTML      string `json:"html"`
				Timestamp int64  `json:"timestamp"`
			}
			body := Conn.GetRequestBody()
			if len(body) == 0 {
				// utils.HandleError(err, "读取save_page_content请求体")
				return
			}

			err := json.Unmarshal(body, &contentData)
			if err != nil {
				utils.HandleError(err, "解析页面内容数据")
			} else {
				parsedURL, err := url.Parse(contentData.URL)
				if err != nil {
					utils.HandleError(err, "解析页面内容URL")
				} else {
					saveDynamicHTML(contentData.HTML, parsedURL, contentData.URL, contentData.Timestamp)
				}
			}
			headers := make(nf_http.Header)
			headers.Set("Content-Type", "application/json")
			headers.Set("__debug", "fake_resp")
			Conn.StopRequest(200, "{}", headers)
			return
		}

		// 保存搜索页面结构化数据的API端点
		if path == "/__wx_channels_api/save_search_data" {
			var searchData struct {
				URL         string                   `json:"url"`
				Keyword     string                   `json:"keyword"`
				Profiles    []map[string]interface{} `json:"profiles"`    // 账号信息
				LiveResults []map[string]interface{} `json:"liveResults"` // 直播数据
				FeedResults []map[string]interface{} `json:"feedResults"` // 动态数据
				Timestamp   int64                    `json:"timestamp"`
			}
			body := Conn.GetRequestBody()
			if len(body) == 0 {
				// utils.HandleError(err, "读取save_search_data请求体")
				return
			}

			err := json.Unmarshal(body, &searchData)
			if err != nil {
				utils.HandleError(err, "解析搜索数据")
			} else {
				parsedURL, err := url.Parse(searchData.URL)
				if err != nil {
					utils.HandleError(err, "解析搜索页面URL")
				} else {
					saveSearchData(searchData.URL, parsedURL, searchData.Keyword, searchData.Profiles, searchData.LiveResults, searchData.FeedResults, searchData.Timestamp)
				}
			}
			headers := make(nf_http.Header)
			headers.Set("Content-Type", "application/json")
			headers.Set("__debug", "fake_resp")
			Conn.StopRequest(200, "{}", headers)
			return
		}
	}
	if Conn.Type() == public.HttpResponseOK {
		Body := Conn.GetResponseBody()
		// 记录JS文件请求（调试用）
		if strings.Contains(path, ".js") {
			contentType := strings.ToLower(Conn.GetResponseHeader().Get("Content-Type"))
			utils.LogInfo("[响应] Path=%s | ContentType=%s", path, contentType)
		}

		// 使用ScriptHandler处理HTML响应
		if scriptHandler != nil {
			if scriptHandler.HandleHTMLResponse(Conn, host, path, Body) {
				return
			}
		}

		// 使用ScriptHandler处理JavaScript响应
		if scriptHandler != nil {
			if scriptHandler.HandleJavaScriptResponse(Conn, host, path, Body) {
				return
			}
		}

		// 如果没有被ScriptHandler处理，SetResponseBody
		Conn.SetResponseBody(Body)

	}
	if Conn.Type() == public.HttpRequestFail {
		// 请求错误处理
		if strings.Contains(host, "weixin.qq.com") {
			utils.Warn("[请求失败] Host=%s | Path=%s", host, path)
		}
	}
}
