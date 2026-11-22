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

// extractTargetsFromInput 从多行/混排文本中自动提取 IP、域名与端口，生成独立测试目标
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
				if domainWithPortRe.MatchString(tok) {
					addTarget(host, port, country)
					continue
				}
			}

			// 2. 纯 IP（IPv4 或 IPv6）
			cleanIP := strings.Trim(tok, "[]")
			if parsedIP := net.ParseIP(cleanIP); parsedIP != nil {
				addTarget(parsedIP.String(), "", country)
				continue
			}

			// 3. 域名（带或不带端口）
			if domainWithPortRe.MatchString(tok) {
				if colonIdx := strings.LastIndex(tok, ":"); colonIdx > 0 {
					addTarget(tok[:colonIdx], tok[colonIdx+1:], country)
				} else {
					addTarget(tok, "", country)
				}
				continue
			}

			// 4. 兜底交给原有 ParseTargetLine 处理
			ip, port, parsedCountry := filter.ParseTargetLine(tok)
			if country == "" {
				country = parsedCountry
			}
			if ip != "" {
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
		}
