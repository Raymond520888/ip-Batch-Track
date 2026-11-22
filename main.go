package main

import (
	"context"
	"fmt"
	"image/color"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"ip-batch-track/internal/config"
	"ip-batch-track/internal/filter"
	"ip-batch-track/internal/tracer"
	"ip-batch-track/internal/worker"
)

const (
	bannerTopRight = "Make by Raymond"
	maxConcurrency = 128
	defaultConc    = 128

	prefKeyConcurrency = "concurrency"
	prefKeyDarkMode    = "dark_mode"
)

var (
	version = "V3.0"
	commit  = "unknown"
)

var domainWithPortRe = regexp.MustCompile(`^[A-Za-z0-9.-]+\.[A-Za-z]{2,}(?::(\d{1,5}))?$`)

func isValidDomainToken(s string) bool {
	if s == "" || !strings.Contains(s, ".") {
		return false
	}
	for _, r := range s {
		if r > 127 {
			return false
		}
	}
	return domainWithPortRe.MatchString(s)
}

// extractTargetsFromInput 从多行/混排文本中自动提取 IP、域名与端口，忽略中文线路标题/描述
func extractTargetsFromInput(rawText string) []worker.Target {
	var targets []worker.Target
	seen := make(map[string]bool)

	addTarget := func(ip, port, country string) {
		ip = strings.TrimSpace(ip)
		port = strings.TrimSpace(port)
		if ip == "" {
			return
		}
		key := ip + ":" + port
		if seen[key] {
			return
		}
		seen[key] = true
		targets = append(targets, worker.Target{
			IP:      ip,
			Port:    port,
			Country: country,
		})
	}

	for _, line := range strings.Split(rawText, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// 先按空白、逗号、分号拆开，避免同一行混入多个 IP 或域名导致整段被当成域名查询
		tokens := strings.FieldsFunc(line, func(r rune) bool {
			return r == ' ' || r == '\t' || r == ',' || r == ';'
		})

		for _, tok := range tokens {
			tok = strings.TrimSpace(tok)
			if tok == "" || strings.HasPrefix(tok, "#") {
				continue
			}

			// 提取可选的 #国家/备注
			country := ""
			if idx := strings.Index(tok, "#"); idx >= 0 {
				country = strings.TrimSpace(tok[idx+1:])
				tok = strings.TrimSpace(tok[:idx])
			}
			if tok == "" {
				continue
			}

			// 1. 尝试直接按 host:port 拆分（兼容 IPv4:port、[IPv6]:port、domain:port）
			if host, port, err := net.SplitHostPort(tok); err == nil {
				if parsedIP := net.ParseIP(strings.Trim(host, "[]")); parsedIP != nil {
					addTarget(parsedIP.String(), port, country)
					continue
				}
				if isValidDomainToken(tok) {
					addTarget(host, port, country)
					continue
				}
				continue
			}

			// 2. 纯 IP（IPv4 或 IPv6）
			cleanIP := strings.Trim(tok, "[]")
			if parsedIP := net.ParseIP(cleanIP); parsedIP != nil {
				addTarget(parsedIP.String(), "", country)
				continue
			}

			// 3. 域名（带或不带端口）
			if isValidDomainToken(tok) {
				if colonIdx := strings.LastIndex(tok, ":"); colonIdx > 0 {
					addTarget(tok[:colonIdx], tok[colonIdx+1:], country)
				} else {
					addTarget(tok, "", country)
				}
				continue
			}

			// 4. 仅当非纯中文描述时，才尝试兜底 ParseTargetLine
			hasNonASCII := false
			for _, r := range tok {
				if r > 127 {
					hasNonASCII = true
					break
				}
			}
			if hasNonASCII {
				continue
			}

			ip, port, parsedCountry := filter.ParseTargetLine(tok)
			if country == "" {
				country = parsedCountry
			}
			if ip != "" && net.ParseIP(ip) != nil {
				addTarget(ip, port, country)
			}
		}
	}

	return targets
}

// customTheme 支援日間淺綠風格與夜間黑夜模式切換
type customTheme struct {
	dark bool
}

func (t *customTheme) Color(name fyne.ThemeColorName, variant fyne.ThemeVariant) color.Color {
	if t.dark {
		switch name {
		case theme.ColorNameBackground:
			return color.NRGBA{R: 24, G: 28, B: 26, A: 255}
		case theme.ColorNameInputBackground:
			return color.NRGBA{R: 34, G: 44, B: 38, A: 255}
		case theme.ColorNameButton:
			return color.NRGBA{R: 46, G: 68, B: 54, A: 255}
		}
		return theme.DefaultTheme().Color(name, theme.VariantDark)
	}

	switch name {
	case theme.ColorNameBackground:
		return color.NRGBA{R: 236, G: 242, B: 235, A: 255}
	case theme.ColorNameInputBackground:
		return color.NRGBA{R: 204, G: 228, B: 198, A: 255}
	case theme.ColorNameButton:
		return color.NRGBA{R: 176, G: 214, B: 174, A: 255}
	case theme.ColorNameForeground:
		return color.NRGBA{R: 20, G: 28, B: 22, A: 255}
	}
	return theme.DefaultTheme().Color(name, theme.VariantLight)
}

func (t *customTheme) Font(style fyne.TextStyle) fyne.Resource {
	return theme.DefaultTheme().Font(style)
}

func (t *customTheme) Icon(name fyne.ThemeIconName) fyne.Resource {
	return theme.DefaultTheme().Icon(name)
}

func (t *customTheme) Size(name fyne.ThemeSizeName) float32 {
	if name == theme.SizeNamePadding {
		return 2
	}
	return theme.DefaultTheme().Size(name)
}

// compactBoxLayout 讓控制項按指定寬高緊湊排布，避免被 HBox 撐寬
type compactBoxLayout struct {
	width  float32
	height float32
}

func (l *compactBoxLayout) MinSize(objects []fyne.CanvasObject) fyne.Size {
	h := l.height
	if h <= 0 && len(objects) > 0 {
		h = objects[0].MinSize().Height
	}
	return fyne.NewSize(l.width, h)
}

func (l *compactBoxLayout) Layout(objects []fyne.CanvasObject, size fyne.Size) {
	w := l.width
	if w <= 0 {
		w = size.Width
	}
	h := size.Height
	if l.height > 0 && l.height < h {
		h = l.height
	}
	for _, o := range objects {
		o.Resize(fyne.NewSize(w, h))
		o.Move(fyne.NewPos(0, (size.Height-h)/2))
	}
}

func main() {
	a := app.NewWithID("ip-batch-track-gui")
	prefs := a.Preferences()

	savedConc := prefs.IntWithFallback(prefKeyConcurrency, defaultConc)
	if savedConc <= 0 {
		savedConc = 1
	} else if savedConc > maxConcurrency {
		savedConc = maxConcurrency
	}

	isDark := prefs.BoolWithFallback(prefKeyDarkMode, false)
	appTheme := &customTheme{dark: isDark}
	a.Settings().SetTheme(appTheme)

	w := a.NewWindow(fmt.Sprintf("ip路由批量测试 %s", version))
	w.Resize(fyne.NewSize(960, 720))

	// 右上角作者標識
	topRightLabel := canvas.NewText(bannerTopRight, color.NRGBA{R: 180, G: 40, B: 40, A: 255})
	topRightLabel.TextSize = 12
	topRightLabel.TextStyle = fyne.TextStyle{Bold: true}
	topHeader := container.NewHBox(layout.NewSpacer(), topRightLabel)

	// 1. 目標輸入區
	targetHint := widget.NewLabel("输入目标（支持换行/空格/逗号分隔，自动提取 IP:端口 与域名）：")
	targetEntry := widget.NewMultiLineEntry()
	targetEntry.SetPlaceHolder("例如：\n1.1.1.1:443#US\nexample.com:8443#JP\nopenai.com 172.64.154.211 104.18.33.45")

	// 2. 控制欄元件（純數字輸入框 + 居中 + 無上下按鈕 + 無滾動條 + 無勾號 + 上限 128）
	concEntry := widget.NewEntry()
	concEntry.SetText(strconv.Itoa(savedConc))
	concEntry.Wrapping = fyne.TextTruncate
	concEntry.Validator = nil

	var adjustingConc bool
	concEntry.OnChanged = func(s string) {
		if adjustingConc {
			return
		}
		trimmed := strings.TrimSpace(s)
		if trimmed == "" {
			return
		}
		v, err := strconv.Atoi(trimmed)
		if err != nil {
			return
		}
		if v < 1 {
			v = 1
			adjustingConc = true
			concEntry.SetText("1")
			adjustingConc = false
		} else if v > maxConcurrency {
			v = maxConcurrency
			adjustingConc = true
			concEntry.SetText(strconv.Itoa(maxConcurrency))
			adjustingConc = false
		}
		prefs.SetInt(prefKeyConcurrency, v)
	}

	ctrlHeight := concEntry.MinSize().Height
	concInputWrap := container.New(&compactBoxLayout{width: 58, height: ctrlHeight}, concEntry)
	concLabel := container.NewCenter(widget.NewLabel("并发:"))
	concBox := container.NewHBox(concLabel, concInputWrap)

	enablePingCheck := widget.NewCheck("启用延迟测试", nil)
	statusLabel := widget.NewLabel("就绪")

	// 3. 執行日誌區
	logHint := widget.NewLabel("运行日志：")
	var (
		logMu      sync.Mutex
		logBuf     strings.Builder
		cancel     context.CancelFunc
		running    bool
		updatingDB bool
	)

	logEntry := widget.NewMultiLineEntry()
	logEntry.Wrapping = fyne.TextWrapWord

	appendLog := func(line string) {
		ts := time.Now().Format("15:04:05")
		msg := fmt.Sprintf("[%s] %s\n", ts, line)
		fyne.Do(func() {
			logMu.Lock()
			logBuf.WriteString(msg)
			current := logBuf.String()
			logMu.Unlock()
			logEntry.SetText(current)
			logEntry.CursorRow = len(strings.Split(current, "\n")) - 1
		})
	}

	clearLogBtn := widget.NewButton("清空日志", func() {
		logMu.Lock()
		logBuf.Reset()
		logMu.Unlock()
		logEntry.SetText("")
	})

	clearTargetBtn := widget.NewButton("清空目标", func() {
		targetEntry.SetText("")
		fyne.Do(func() { statusLabel.SetText("已清空目标") })
	})

	var updateDBBtn, startBtn, stopBtn, themeBtn *widget.Button

	setBusy := func(busy bool, statusText string) {
		running = busy
		fyne.Do(func() {
			statusLabel.SetText(statusText)
			if busy {
				startBtn.Disable()
				updateDBBtn.Disable()
				clearTargetBtn.Disable()
				stopBtn.Enable()
			} else {
				startBtn.Enable()
				if !updatingDB {
					updateDBBtn.Enable()
				}
				clearTargetBtn.Enable()
				stopBtn.Disable()
			}
		})
	}

	runPrefixUpdate := func(auto bool) {
		logMu.Lock()
		if updatingDB {
			logMu.Unlock()
			if !auto {
				appendLog("[!] 前缀表正在更新中，请稍候...")
			}
			return
		}
		updatingDB = true
		logMu.Unlock()

		fyne.Do(func() {
			updateDBBtn.Disable()
			if !running {
				statusLabel.SetText("更新前缀表中...")
			}
		})

		if auto {
			appendLog("[+] 启动自动更新前缀表 (asn_prefixes_v4 + asn_prefixes_v6)...")
		} else {
			appendLog("[+] 正在后台更新前缀表 (asn_prefixes_v4 + asn_prefixes_v6)，请稍候...")
		}

		go func() {
			defer func() {
				logMu.Lock()
				updatingDB = false
				logMu.Unlock()
				fyne.Do(func() {
					if !running {
						updateDBBtn.Enable()
						statusLabel.SetText("就绪")
					}
				})
			}()

			if err := filter.UpdatePrefixDB(); err != nil {
				if auto {
					appendLog(fmt.Sprintf("[-] 启动自动更新前缀表失败（可稍后手动点击更新）: %v", err))
				} else {
					appendLog(fmt.Sprintf("[-] 前缀表更新失败: %v", err))
					fyne.Do(func() { dialog.ShowError(err, w) })
				}
				return
			}

			appendLog("[+] 前缀表 (asn_prefixes_v4 + asn_prefixes_v6) 更新完成")
		}()
	}

	updateDBBtn = widget.NewButton("更新前缀表 (IPv4+IPv6)", func() {
		if running {
			return
		}
		runPrefixUpdate(false)
	})

	stopBtn = widget.NewButton("终止测试", func() {
		if cancel != nil {
			cancel()
			appendLog("[!] 用户请求终止测试，正在清理任务...")
			fyne.Do(func() { statusLabel.SetText("正在终止...") })
		}
	})
	stopBtn.Disable()

	startBtn = widget.NewButton("开始测试", func() {
		if running {
			return
		}
		conc, err := strconv.Atoi(strings.TrimSpace(concEntry.Text))
		if err != nil || conc <= 0 || conc > maxConcurrency {
			dialog.ShowError(fmt.Errorf("并发数必须是 1 到 %d 之间的整数", maxConcurrency), w)
			return
		}
		prefs.SetInt(prefKeyConcurrency, conc)

		targets := extractTargetsFromInput(targetEntry.Text)
		if len(targets) == 0 {
			dialog.ShowInformation("提示", "请先在上方输入至少一个有效的 IP 或域名。", w)
			return
		}

		enablePing := enablePingCheck.Checked
		runCtx, runCancel := context.WithCancel(context.Background())
		cancel = runCancel

		setBusy(true, "测试中...")
		appendLog(fmt.Sprintf("[+] 已提取并加载 %d 个目标，并发数: %d，启用延迟测试: %v", len(targets), conc, enablePing))

		go func() {
			defer func() {
				cancel = nil
				setBusy(false, "就绪")
			}()

			if _, err := os.Stat(config.DBV4Path); os.IsNotExist(err) {
				appendLog("[+] 未找到本地前缀表，正在自动拉取...")
				if err := filter.UpdatePrefixDB(); err != nil {
					appendLog(fmt.Sprintf("[-] 前缀表拉取失败: %v", err))
					return
				}
				appendLog("[+] 前缀表拉取完成")
			}

			matcher, err := filter.LoadMatcher()
			if err != nil {
				appendLog(fmt.Sprintf("[-] 加载前缀匹配器失败: %v", err))
				return
			}

			cfg := config.Config{
				Concurrency: conc,
				Timeout:     60 * time.Second,
				MaxHops:     30,
				EnablePing:  enablePing,
			}

			for _, t := range targets {
				appendLog(fmt.Sprintf("  > 正在探测 %s:%s ...", t.IP, t.Port))
			}

			tr := tracer.NewTracer(cfg.Timeout, cfg.MaxHops)
			startTime := time.Now()
			results := worker.Run(runCtx, targets, tr, matcher, cfg.Concurrency, cfg.EnablePing, func(r worker.Result) {
				if r.Err != nil {
					errMsg := r.Err.Error()
					if strings.Contains(errMsg, "域名解析失败") || strings.Contains(errMsg, "no such host") {
						return
					}
					appendLog(fmt.Sprintf("  - %s:%s | 异常: %v", r.IP, r.Port, r.Err))
				} else {
					appendLog(fmt.Sprintf("  - %s:%s | 运营商: %s | 线路: %s | ASN: %s | 延迟: %s",
						r.IP, r.Port, r.ISP, r.LineType, r.ASNStr, r.Delay))
				}
			})
			elapsed := time.Since(startTime).Round(time.Second)

			appendLog(fmt.Sprintf("[+] 测试结束，耗时 %v，共取得 %d 条结果", elapsed, len(results)))
		}()
	})

	initialThemeLabel := "🌙 黑夜模式"
	if isDark {
		initialThemeLabel = "☀️ 日间模式"
	}

	themeBtn = widget.NewButton(initialThemeLabel, func() {
		isDark = !isDark
		appTheme.dark = isDark
		a.Settings().SetTheme(appTheme)
		prefs.SetBool(prefKeyDarkMode, isDark)
		if isDark {
			themeBtn.SetText("☀️ 日间模式")
		} else {
			themeBtn.SetText("🌙 黑夜模式")
		}
		canvas.Refresh(w.Content())
	})

	// 版面配置：上方目標輸入 + 中間控制欄 + 下方執行日誌
	controlBar := container.NewHBox(
		concBox,
		enablePingCheck,
		clearLogBtn,
		updateDBBtn,
		startBtn,
		stopBtn,
		themeBtn,
		layout.NewSpacer(),
		statusLabel,
	)

	topSection := container.NewBorder(
		container.NewVBox(topHeader, targetHint),
		nil,
		nil,
		nil,
		targetEntry,
	)

	logHeader := container.NewHBox(logHint, layout.NewSpacer(), clearTargetBtn)
	bottomSection := container.NewBorder(
		logHeader,
		nil,
		nil,
		nil,
		logEntry,
	)

	split := container.NewVSplit(topSection, bottomSection)
	split.SetOffset(0.45)

	mainLayout := container.NewBorder(
		nil,
		controlBar,
		nil,
		nil,
		split,
	)

	w.SetContent(mainLayout)
	appendLog(fmt.Sprintf("[+] ip路由批量测试 %s 已启动", version))
	runPrefixUpdate(true)
	w.ShowAndRun()
}