//go:build windows

package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"akitafrp/internal/tunnel"
	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"github.com/lxn/win"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func Run(mode string) error {
	server := mode == "server"
	mutexName := `Global\AkitaFrp.Server.SingleInstance`
	if !server {
		mutexName = `Global\AkitaFrp.Client.SingleInstance`
	}
	mutexNamePtr, err := windows.UTF16PtrFromString(mutexName)
	if err != nil {
		return err
	}
	instanceMutex, mutexErr := windows.CreateMutex(nil, false, mutexNamePtr)
	if mutexErr != nil && !errors.Is(mutexErr, windows.ERROR_ALREADY_EXISTS) {
		return mutexErr
	}
	if errors.Is(mutexErr, windows.ERROR_ALREADY_EXISTS) {
		_ = windows.CloseHandle(instanceMutex)
		if !server {
			text, _ := windows.UTF16PtrFromString("AkitaFrp Client 已经运行，每台电脑只能启动一个 Client 实例。")
			caption, _ := windows.UTF16PtrFromString("AkitaFrp Client")
			win.MessageBox(0, text, caption, win.MB_ICONINFORMATION|win.MB_OK)
		}
		return nil
	}
	defer windows.CloseHandle(instanceMutex)
	title, file := "AkitaFrp Client", "client.json"
	if server {
		title, file = "AkitaFrp Server", "server.json"
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	configPath := filepath.Join(filepath.Dir(exe), file)
	logDir := filepath.Join(filepath.Dir(exe), "logs")
	if e := os.MkdirAll(logDir, 0755); e != nil {
		return fmt.Errorf("创建日志目录失败：%w", e)
	}
	logPath := filepath.Join(logDir, "启动时间_"+time.Now().Format("20060102_150405")+".txt")
	logFile, logOpenErr := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if logOpenErr != nil {
		return fmt.Errorf("创建日志文件失败：%w", logOpenErr)
	}
	defer logFile.Close()
	serverCfg, clientCfg := tunnel.DefaultServer(), tunnel.DefaultClient()
	loadErr := error(nil)
	if b, e := os.ReadFile(configPath); e == nil {
		if server {
			loadErr = json.Unmarshal(b, &serverCfg)
		} else {
			loadErr = json.Unmarshal(b, &clientCfg)
			if loadErr == nil {
				if clientCfg.CloseAction == "" {
					clientCfg.CloseAction = "tray"
				}
			}
		}
		if loadErr != nil {
			if server {
				serverCfg = tunnel.DefaultServer()
			} else {
				clientCfg = tunnel.DefaultClient()
			}
		}
	} else if os.IsNotExist(e) {
		var b []byte
		if server {
			b, _ = json.MarshalIndent(serverCfg, "", "  ")
		} else {
			b, _ = json.MarshalIndent(clientCfg, "", "  ")
		}
		if writeErr := os.WriteFile(configPath, append(b, '\n'), 0644); writeErr != nil {
			loadErr = writeErr
		}
	} else {
		loadErr = e
	}
	if server && len(serverCfg.Mappings) == 0 {
		serverCfg.Mappings = []tunnel.Mapping{}
	}
	if !server && len(clientCfg.Mappings) == 0 {
		clientCfg.Mappings = []tunnel.LocalMapping{}
	}
	if !server && clientCfg.ReconnectAttempts <= 0 {
		clientCfg.ReconnectAttempts = 5
	}

	var mw *walk.MainWindow
	var status *walk.Label
	var logView *walk.TextEdit
	var mapModel *mappingTableModel
	var logMu sync.Mutex
	var startBtn, stopBtn, reconnectBtn *walk.PushButton
	var reconnect func()
	var resumeLastConnection *walk.CheckBox
	var trayStatusAction, trayConnectAction, trayDisconnectAction, trayReconnectAction *walk.Action
	trayStatusText := "已停止"
	var runCancel context.CancelFunc
	var runDone chan struct{}
	var reconnecting bool
	var reconnectSeq uint64
	var runSeq uint64
	clientConnected, serverListening := false, false
	var lock sync.Mutex
	appendLog := func(message string) {
		now := time.Now()
		clean := strings.ReplaceAll(strings.ReplaceAll(message, "\r", " "), "\n", " ")
		level := "信息"
		if strings.Contains(clean, "拒绝") || strings.Contains(clean, "失败") || strings.Contains(clean, "异常") || strings.Contains(clean, "错误") || strings.Contains(clean, "超时") || strings.Contains(clean, "冲突") {
			level = "错误"
		} else if strings.Contains(clean, "断开") || strings.Contains(clean, "中断") || strings.Contains(clean, "重试") || strings.Contains(clean, "重启") {
			level = "警告"
		}
		line := now.Format("2006-01-02 15:04:05.000") + "  [" + level + "] " + clean + "\r\n"
		logMu.Lock()
		_, _ = logFile.WriteString(line[:len(line)-2] + "\n")
		logMu.Unlock()
		if mw != nil && logView != nil {
			mw.Synchronize(func() {
				logView.AppendText(line)
				end := logView.TextLength()
				logView.SetTextSelection(end, end)
				logView.ScrollToCaret()
			})
		}
	}
	setStatus := func(s string) {
		appendLog(s)
		if server {
			switch {
			case s == "已停止":
				trayStatusText = "已停止"
			case s == "正在启动…":
				trayStatusText = "正在启动"
			case strings.HasPrefix(s, "控制端口监听于"):
				trayStatusText = "服务运行中"
			case strings.HasPrefix(s, "Server 异常："):
				trayStatusText = "服务异常，正在恢复"
			case strings.HasPrefix(s, "启动失败："):
				trayStatusText = "启动失败"
			}
		} else if s == "已停止" || s == "正在启动…" || s == "正在断开并重新连接…" || strings.HasPrefix(s, "已连接服务器") || strings.HasPrefix(s, "连接中断：") || strings.HasPrefix(s, "启动失败：") {
			trayStatusText = s
		}
		if mw != nil {
			lock.Lock()
			active := runCancel != nil || reconnecting
			lock.Unlock()
			mw.Synchronize(func() {
				if server {
					if strings.HasPrefix(s, "控制端口监听于") {
						serverListening = true
					}
					if s == "已停止" || s == "正在启动…" || strings.HasPrefix(s, "Server 异常：") || strings.HasPrefix(s, "启动失败：") {
						serverListening = false
					}
				} else {
					if strings.HasPrefix(s, "已连接服务器") {
						clientConnected = true
					}
					if s == "已停止" || s == "正在启动…" || s == "正在断开并重新连接…" || strings.HasPrefix(s, "连接中断：") || strings.HasPrefix(s, "启动失败：") {
						clientConnected = false
					}
				}
				if mapModel != nil {
					mapModel.SetRuntimeState(clientConnected, serverListening)
				}
				status.SetText("状态：" + s)
				if startBtn != nil {
					startBtn.SetEnabled(!active)
				}
				if stopBtn != nil {
					stopBtn.SetEnabled(active)
				}
				if reconnectBtn != nil {
					reconnectBtn.SetEnabled(true)
				}
				if trayStatusAction != nil {
					_ = trayStatusAction.SetText("状态：" + trayStatusText)
				}
				if trayConnectAction != nil {
					_ = trayConnectAction.SetEnabled(!active)
				}
				if trayDisconnectAction != nil {
					_ = trayDisconnectAction.SetEnabled(active)
				}
				if trayReconnectAction != nil {
					_ = trayReconnectAction.SetEnabled(true)
				}
			})
		}
	}
	serverEvent := func(message string) {
		setStatus(message)
	}
	applyAuto := func(enabled bool) {
		key, e := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.SET_VALUE)
		if e != nil {
			key, _, e = registry.CreateKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.SET_VALUE)
		}
		if e != nil {
			setStatus("设置自启动失败：" + e.Error())
			return
		}
		defer key.Close()
		if enabled {
			e = key.SetStringValue("AkitaFrpClient", fmt.Sprintf("\"%s\"", exe))
		} else {
			e = key.DeleteValue("AkitaFrpClient")
		}
		if e != nil && enabled {
			setStatus("设置自启动失败：" + e.Error())
		}
	}

	var controlAddress, controlPort, serverAddress, serverPort, token, clientID *walk.LineEdit
	var auto *walk.CheckBox
	var closeBox *walk.ComboBox
	var mapTable *walk.TableView
	var editSelectedButton *walk.PushButton
	var toggleMappingButton *walk.PushButton
	var deleteSelectedButton, copyKeyButton *walk.PushButton
	var restartCurrentService func()
	serviceActive := func() bool { lock.Lock(); defer lock.Unlock(); return runCancel != nil || reconnecting }
	mapModel = &mappingTableModel{server: server}
	var mappingsS = append([]tunnel.Mapping(nil), serverCfg.Mappings...)
	var mappingsC = append([]tunnel.LocalMapping(nil), clientCfg.Mappings...)
	serverKeysUpdated := false
	if server {
		seenKeys := make(map[string]struct{}, len(mappingsS))
		for i := range mappingsS {
			if mappingsS[i].Key == "" {
				mappingsS[i].Key = tunnel.NewMappingKey()
				serverKeysUpdated = true
			}
			if _, duplicate := seenKeys[mappingsS[i].Key]; duplicate {
				mappingsS[i].Key = tunnel.NewMappingKey()
				serverKeysUpdated = true
			}
			seenKeys[mappingsS[i].Key] = struct{}{}
		}
		if serverKeysUpdated {
			serverCfg.Mappings = append([]tunnel.Mapping(nil), mappingsS...)
		}
	}
	mappingSwitches := tunnel.NewMappingSwitches()
	for _, m := range mappingsS {
		mappingSwitches.SetEnabled(tunnel.ServerMappingSwitchKey(m), !m.Disabled)
	}
	for _, m := range mappingsC {
		mappingSwitches.SetEnabled(tunnel.ClientMappingSwitchKey(m), !m.Disabled)
	}
	icon, _ := walk.NewIconFromResourceId(1)
	closeMode := 0
	if clientCfg.CloseAction == "exit" {
		closeMode = 1
	}
	refreshList := func(selectIndex int) {
		if mapTable == nil {
			return
		}
		mapModel.Reset(mappingsS, mappingsC)
		if selectIndex >= 0 {
			_ = mapTable.SetCurrentIndex(selectIndex)
		} else {
			_ = mapTable.SetCurrentIndex(-1)
		}
	}
	writeConfig := func() error {
		var b []byte
		var e error
		if server {
			b, e = json.MarshalIndent(serverCfg, "", "  ")
		} else {
			b, e = json.MarshalIndent(clientCfg, "", "  ")
		}
		if e != nil {
			return e
		}
		return os.WriteFile(configPath, append(b, '\n'), 0644)
	}
	if serverKeysUpdated {
		if e := writeConfig(); e != nil {
			loadErr = fmt.Errorf("保存映射 Key 失败：%w", e)
		}
	}
	collect := func() error {
		if server {
			serverCfg.ControlAddress = strings.TrimSpace(controlAddress.Text())
			serverCfg.Token = strings.TrimSpace(token.Text())
			p, e := strconv.Atoi(strings.TrimSpace(controlPort.Text()))
			if e != nil || p < 1 || p > 65535 {
				return fmt.Errorf("控制端口需要填写 1 到 65535 之间的数字")
			}
			serverCfg.ControlPort = p
			if len(serverCfg.Token) < 16 {
				return fmt.Errorf("Token 至少需要 16 个字符")
			}
			usedKeys := make(map[string]struct{}, len(mappingsS))
			for _, m := range mappingsS {
				if strings.TrimSpace(m.Name) == "" || strings.TrimSpace(m.Key) == "" || strings.TrimSpace(m.ClientID) == "" || m.ListenPort < 1 || m.ListenPort > 65535 || !validProtocol(m.Protocol) {
					return fmt.Errorf("请检查端口映射名称、客户端 ID 和端口")
				}
				key := tunnel.ServerMappingSwitchKey(m)
				if _, exists := usedKeys[key]; exists {
					return fmt.Errorf("映射 Key 重复，请重新编辑对应映射")
				}
				usedKeys[key] = struct{}{}
			}
			usedPorts := make(map[int]string, len(mappingsS))
			for _, m := range mappingsS {
				if existing, ok := usedPorts[m.ListenPort]; ok {
					return fmt.Errorf("公网端口 %d 已同时分配给映射“%s”和“%s”；一个公网端口只能配置一条映射", m.ListenPort, existing, m.Name)
				}
				usedPorts[m.ListenPort] = m.Name
			}
		} else {
			clientCfg.ServerAddress = strings.TrimSpace(serverAddress.Text())
			clientCfg.Token = strings.TrimSpace(token.Text())
			clientCfg.ClientID = strings.TrimSpace(clientID.Text())
			p, e := strconv.Atoi(strings.TrimSpace(serverPort.Text()))
			if e != nil || p < 1 || p > 65535 {
				return fmt.Errorf("服务器端口需要填写 1 到 65535 之间的数字")
			}
			clientCfg.ServerPort = p
			if clientCfg.ServerAddress == "" || clientCfg.ClientID == "" {
				return fmt.Errorf("请填写服务器地址和客户端 ID")
			}
			if len(clientCfg.Token) < 16 {
				return fmt.Errorf("Token 至少需要 16 个字符")
			}
			clientCfg.AutoStart = auto.Checked()
			clientCfg.ResumeLastConnection = resumeLastConnection.Checked()
			clientCfg.CloseAction = "tray"
			if closeBox.CurrentIndex() == 1 {
				clientCfg.CloseAction = "exit"
			}
			usedKeys := make(map[string]struct{}, len(mappingsC))
			for _, m := range mappingsC {
				if strings.TrimSpace(m.Name) == "" || strings.TrimSpace(m.LocalHost) == "" || m.LocalPort < 1 || m.LocalPort > 65535 || !validProtocol(m.Protocol) {
					return fmt.Errorf("请检查映射名称、本地地址和本地端口")
				}
				key := tunnel.ClientMappingSwitchKey(m)
				if _, exists := usedKeys[key]; exists {
					return fmt.Errorf("映射 Key 重复，请检查 Client 映射")
				}
				usedKeys[key] = struct{}{}
			}
		}
		if server {
			serverCfg.Mappings = append([]tunnel.Mapping(nil), mappingsS...)
		} else {
			clientCfg.Mappings = append([]tunnel.LocalMapping(nil), mappingsC...)
		}
		return writeConfig()
	}
	openMappingDialog := func(index int) {
		nameValue, clientValue, addressValue, portValue := "", "default", "0.0.0.0", ""
		keyValue := ""
		localHostValue, localPortValue, remotePortValue, protocolValue := "127.0.0.1", "", "", 0
		if server && (index < 0 || index >= len(mappingsS)) {
			keyValue = tunnel.NewMappingKey()
		}
		if server && index >= 0 && index < len(mappingsS) {
			m := mappingsS[index]
			nameValue, clientValue, addressValue, portValue = m.Name, m.ClientID, m.ListenAddress, strconv.Itoa(m.ListenPort)
			keyValue = m.Key
			protocolValue = protocolIndex(m.Protocol)
		} else if !server && index >= 0 && index < len(mappingsC) {
			m := mappingsC[index]
			nameValue, localHostValue, localPortValue, remotePortValue = m.Name, m.LocalHost, strconv.Itoa(m.LocalPort), strconv.Itoa(m.RemotePort)
			keyValue = m.Key
			protocolValue = protocolIndex(m.Protocol)
		}
		var dlg *walk.Dialog
		var nameEdit, keyEdit, clientEdit, addressEdit, portEdit, hostEdit, localPortEdit, remotePortEdit *walk.LineEdit
		var protocolEdit *walk.ComboBox
		var saveButton, cancelButton *walk.PushButton
		save := func() {
			name := strings.TrimSpace(nameEdit.Text())
			if name == "" {
				walk.MsgBox(dlg, "信息不完整", "请填写映射名称。", walk.MsgBoxIconWarning)
				return
			}
			p := selectedProtocol(protocolEdit)
			if server {
				port, err := strconv.Atoi(strings.TrimSpace(portEdit.Text()))
				m := tunnel.Mapping{Name: name, Key: strings.TrimSpace(keyEdit.Text()), Protocol: p, ClientID: strings.TrimSpace(clientEdit.Text()), ListenAddress: strings.TrimSpace(addressEdit.Text()), ListenPort: port}
				if err != nil || port < 1 || port > 65535 || m.ClientID == "" || m.ListenAddress == "" || m.Key == "" {
					walk.MsgBox(dlg, "信息不完整", "请填写客户端 ID、监听地址和 1 到 65535 之间的公网端口。", walk.MsgBoxIconWarning)
					return
				}
				if index >= 0 && index < len(mappingsS) {
					m.Disabled = mappingsS[index].Disabled
				}
				for row, existing := range mappingsS {
					if row != index && existing.Key == m.Key {
						walk.MsgBox(dlg, "映射 Key 重复", "请关闭窗口重新生成 Key。", walk.MsgBoxIconWarning)
						return
					}
					if row != index && existing.ListenPort == m.ListenPort {
						walk.MsgBox(dlg, "公网端口已占用", fmt.Sprintf("公网端口 %d 已由映射“%s”使用。", m.ListenPort, existing.Name), walk.MsgBoxIconWarning)
						return
					}
				}
				if index >= 0 && index < len(mappingsS) {
					mappingsS[index] = m
				} else {
					mappingsS = append(mappingsS, m)
					index = len(mappingsS) - 1
				}
			} else {
				port, err := strconv.Atoi(strings.TrimSpace(localPortEdit.Text()))
				remotePort, remoteErr := strconv.Atoi(strings.TrimSpace(remotePortEdit.Text()))
				m := tunnel.LocalMapping{Name: name, Key: strings.TrimSpace(keyEdit.Text()), Protocol: p, LocalHost: strings.TrimSpace(hostEdit.Text()), LocalPort: port, RemotePort: remotePort}
				if err != nil || remoteErr != nil || port < 1 || port > 65535 || remotePort < 1 || remotePort > 65535 || m.LocalHost == "" || m.Key == "" {
					walk.MsgBox(dlg, "信息不完整", "请填写映射 Key、本地地址、本地端口和 Server 公网端口（均为 1 到 65535）。", walk.MsgBoxIconWarning)
					return
				}
				if index >= 0 && index < len(mappingsC) {
					m.Disabled = mappingsC[index].Disabled
				}
				for row, existing := range mappingsC {
					if row != index && existing.Key == m.Key {
						walk.MsgBox(dlg, "映射 Key 重复", "一个 Client 不能重复绑定同一个 Server 映射 Key。", walk.MsgBoxIconWarning)
						return
					}
				}
				if index >= 0 && index < len(mappingsC) {
					mappingsC[index] = m
				} else {
					mappingsC = append(mappingsC, m)
					index = len(mappingsC) - 1
				}
			}
			if server {
				serverCfg.Mappings = append([]tunnel.Mapping(nil), mappingsS...)
				if index >= 0 && index < len(mappingsS) {
					mappingSwitches.SetEnabled(tunnel.ServerMappingSwitchKey(mappingsS[index]), !mappingsS[index].Disabled)
				}
			} else {
				clientCfg.Mappings = append([]tunnel.LocalMapping(nil), mappingsC...)
				if index >= 0 && index < len(mappingsC) {
					mappingSwitches.SetEnabled(tunnel.ClientMappingSwitchKey(mappingsC[index]), !mappingsC[index].Disabled)
				}
			}
			if err := writeConfig(); err != nil {
				walk.MsgBox(dlg, "保存映射失败", err.Error(), walk.MsgBoxIconWarning)
				return
			}
			refreshList(index)
			if server {
				m := mappingsS[index]
				setStatus(fmt.Sprintf("映射已保存：名称=%s，协议=%s，监听=%s:%d，ClientID=%s，Key=%s", m.Name, strings.ToUpper(m.Protocol), m.ListenAddress, m.ListenPort, m.ClientID, tunnelKeySummary(m.Key)))
			} else {
				m := mappingsC[index]
				setStatus(fmt.Sprintf("映射已保存：名称=%s，协议=%s，本地=%s:%d，Server 公网端口=%d，标识=%s", m.Name, strings.ToUpper(m.Protocol), m.LocalHost, m.LocalPort, m.RemotePort, tunnelKeySummary(m.Key)))
			}
			needsRestart := serviceActive()
			dlg.Accept()
			if needsRestart && restartCurrentService != nil {
				restartCurrentService()
			}
		}
		children := []Widget{}
		if server {
			children = append(children,
				Composite{Layout: Grid{Columns: 2, Spacing: 12}, Children: []Widget{Label{Text: "映射名称（备注）"}, LineEdit{AssignTo: &nameEdit, Text: nameValue, CueBanner: "例如 MC"}, Label{Text: "映射标识（复制到 Client）"}, LineEdit{AssignTo: &keyEdit, Text: keyValue, ReadOnly: true}, Label{Text: "协议"}, ComboBox{AssignTo: &protocolEdit, Model: []string{"TCP", "UDP", "TCP/UDP"}, CurrentIndex: protocolValue}, Label{Text: "Client ID（代理名称前缀）"}, LineEdit{AssignTo: &clientEdit, Text: clientValue}, Label{Text: "监听地址"}, LineEdit{AssignTo: &addressEdit, Text: addressValue}, Label{Text: "公网端口"}, LineEdit{AssignTo: &portEdit, Text: portValue, CueBanner: "1-65535"}}},
				Composite{Layout: HBox{Spacing: 12}, Children: []Widget{Label{Text: "Key 创建后固定不变，可在列表下方复制。"}, PushButton{Text: "复制 Key", OnClicked: func() {
					if err := walk.Clipboard().SetText(keyValue); err != nil {
						walk.MsgBox(dlg, "复制失败", err.Error(), walk.MsgBoxIconWarning)
					} else {
						setStatus("映射 Key 已复制到剪贴板")
					}
				}}}},
			)
		} else {
			children = append(children,
				Composite{Layout: Grid{Columns: 2, Spacing: 12}, Children: []Widget{Label{Text: "映射名称（备注）"}, LineEdit{AssignTo: &nameEdit, Text: nameValue, CueBanner: "例如 MC"}, Label{Text: "映射标识（与 Server 一致）"}, LineEdit{AssignTo: &keyEdit, Text: keyValue, CueBanner: "复制 Server 上的标识"}, Label{Text: "协议"}, ComboBox{AssignTo: &protocolEdit, Model: []string{"TCP", "UDP", "TCP/UDP"}, CurrentIndex: protocolValue}, Label{Text: "本地服务地址"}, LineEdit{AssignTo: &hostEdit, Text: localHostValue}, Label{Text: "本地端口"}, LineEdit{AssignTo: &localPortEdit, Text: localPortValue, CueBanner: "1-65535"}, Label{Text: "Server 公网端口"}, LineEdit{AssignTo: &remotePortEdit, Text: remotePortValue, CueBanner: "与 Server 映射一致"}}},
			)
		}
		children = append(children, Composite{Layout: HBox{Spacing: 12, Alignment: AlignHFarVCenter}, Children: []Widget{PushButton{AssignTo: &saveButton, Text: "保存", MinSize: Size{Width: 88, Height: 32}, OnClicked: func() { save() }}, PushButton{AssignTo: &cancelButton, Text: "取消", MinSize: Size{Width: 88, Height: 32}, OnClicked: func() { dlg.Cancel() }}}})
		title := "新建端口映射"
		if index >= 0 {
			title = "编辑端口映射"
		}
		// Scale both editors with the current desktop and let users resize them.
		screenWidth := int(win.GetSystemMetrics(win.SM_CXSCREEN))
		screenHeight := int(win.GetSystemMetrics(win.SM_CYSCREEN))
		if screenWidth <= 0 {
			screenWidth = 1280
		}
		if screenHeight <= 0 {
			screenHeight = 800
		}
		dialogWidth, dialogHeight := screenWidth*3/4, screenHeight*3/4
		if dialogWidth < 900 {
			dialogWidth = 900
		}
		if dialogHeight < 560 {
			dialogHeight = 560
		}
		if dialogWidth > screenWidth-40 {
			dialogWidth = screenWidth - 40
		}
		if dialogHeight > screenHeight-80 {
			dialogHeight = screenHeight - 80
		}
		minWidth, minHeight := 760, 480
		if minWidth > dialogWidth {
			minWidth = dialogWidth
		}
		if minHeight > dialogHeight {
			minHeight = dialogHeight
		}
		decl := Dialog{AssignTo: &dlg, Title: title, Icon: icon, Size: Size{Width: dialogWidth, Height: dialogHeight}, MinSize: Size{Width: minWidth, Height: minHeight}, FixedSize: false, DefaultButton: &saveButton, CancelButton: &cancelButton, Layout: VBox{Margins: Margins{26, 24, 26, 22}, Spacing: 18}, Children: children}
		if err := decl.Create(mw); err != nil {
			walk.MsgBox(mw, "打开映射窗口失败", err.Error(), walk.MsgBoxIconWarning)
			return
		}
		dlg.Run()
	}
	deleteMapping := func() {
		i := mapTable.CurrentIndex()
		if i < 0 {
			return
		}
		deletedName := ""
		if server && i < len(mappingsS) {
			deletedName = mappingsS[i].Name
			mappingSwitches.SetEnabled(tunnel.ServerMappingSwitchKey(mappingsS[i]), false)
			mappingsS = append(mappingsS[:i], mappingsS[i+1:]...)
			serverCfg.Mappings = append([]tunnel.Mapping(nil), mappingsS...)
		} else if !server && i < len(mappingsC) {
			deletedName = mappingsC[i].Name
			mappingSwitches.SetEnabled(tunnel.ClientMappingSwitchKey(mappingsC[i]), false)
			mappingsC = append(mappingsC[:i], mappingsC[i+1:]...)
			clientCfg.Mappings = append([]tunnel.LocalMapping(nil), mappingsC...)
		}
		if e := writeConfig(); e != nil {
			walk.MsgBox(mw, "删除映射失败", e.Error(), walk.MsgBoxIconWarning)
			return
		}
		refreshList(-1)
		setStatus(fmt.Sprintf("已删除映射：%s", deletedName))
		if serviceActive() && restartCurrentService != nil {
			restartCurrentService()
		}
	}
	toggleMapping := func() {
		i := mapTable.CurrentIndex()
		if i < 0 {
			return
		}
		var name, switchKey string
		var enabled bool
		if server && i < len(mappingsS) {
			mappingsS[i].Disabled = !mappingsS[i].Disabled
			name, enabled = mappingsS[i].Name, !mappingsS[i].Disabled
			switchKey = tunnel.ServerMappingSwitchKey(mappingsS[i])
			serverCfg.Mappings = append([]tunnel.Mapping(nil), mappingsS...)
		} else if !server && i < len(mappingsC) {
			mappingsC[i].Disabled = !mappingsC[i].Disabled
			name, enabled = mappingsC[i].Name, !mappingsC[i].Disabled
			switchKey = tunnel.ClientMappingSwitchKey(mappingsC[i])
			clientCfg.Mappings = append([]tunnel.LocalMapping(nil), mappingsC...)
		} else {
			return
		}
		mappingSwitches.SetEnabled(switchKey, enabled)
		if err := writeConfig(); err != nil {
			walk.MsgBox(mw, "保存映射状态失败", err.Error(), walk.MsgBoxIconWarning)
		}
		refreshList(i)
		buttonText := "断开映射"
		if !enabled {
			buttonText = "连接映射"
		}
		_ = toggleMappingButton.SetText(buttonText)
		stateText := "已断开"
		if enabled {
			stateText = "已启用"
		}
		setStatus(fmt.Sprintf("映射状态已更新：名称=%s，状态=%s", name, stateText))
		// The FRP client applies proxy additions/removals through its runtime
		// config API. Keep the control session and unrelated proxies untouched.
		if !server && serviceActive() {
			appendLog(fmt.Sprintf("Client 映射状态已变更：%s，正在单独更新此映射", name))
		}
	}
	updateMappingActions := func() {
		i := mapTable.CurrentIndex()
		var disabled bool
		if server && i >= 0 && i < len(mappingsS) {
			disabled = mappingsS[i].Disabled
		} else if !server && i >= 0 && i < len(mappingsC) {
			disabled = mappingsC[i].Disabled
		} else {
			editSelectedButton.SetEnabled(false)
			toggleMappingButton.SetEnabled(false)
			deleteSelectedButton.SetEnabled(false)
			if copyKeyButton != nil {
				copyKeyButton.SetEnabled(false)
			}
			return
		}
		editSelectedButton.SetEnabled(true)
		toggleMappingButton.SetEnabled(true)
		deleteSelectedButton.SetEnabled(true)
		if copyKeyButton != nil {
			copyKeyButton.SetEnabled(server)
		}
		text := "断开映射"
		if disabled {
			text = "连接映射"
		}
		_ = toggleMappingButton.SetText(text)
	}
	start := func() {
		if e := collect(); e != nil {
			walk.MsgBox(mw, "请检查配置", e.Error(), walk.MsgBoxIconWarning)
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		serverToRun, clientToRun := serverCfg, clientCfg
		if server {
			appendLog(fmt.Sprintf("启动 Server：控制监听=%s:%d，映射数=%d", serverToRun.ControlAddress, serverToRun.ControlPort, len(serverToRun.Mappings)))
		} else {
			enabled := 0
			for _, m := range clientToRun.Mappings {
				if !m.Disabled {
					enabled++
				}
			}
			appendLog(fmt.Sprintf("启动 Client：Server=%s:%d，ClientID=%s，映射数=%d（启用=%d），TCP 由系统路由转发", clientToRun.ServerAddress, clientToRun.ServerPort, clientToRun.ClientID, len(clientToRun.Mappings), enabled))
		}
		lock.Lock()
		if runCancel != nil || reconnecting {
			lock.Unlock()
			cancel()
			return
		}
		runCancel = cancel
		if !server {
			clientCfg.LastConnected = true
			if e := writeConfig(); e != nil {
				appendLog("保存上次连接状态失败：" + e.Error())
			}
		}
		done := make(chan struct{})
		runDone = done
		runSeq++
		seq := runSeq
		lock.Unlock()
		startBtn.SetEnabled(false)
		stopBtn.SetEnabled(true)
		setStatus("正在启动…")
		go func() {
			defer close(done)
			var e error
			if server {
				e = superviseServer(ctx, serverToRun, mappingSwitches, serverEvent)
			} else {
				e = runClientSafely(ctx, clientToRun, mappingSwitches, setStatus)
			}
			lock.Lock()
			current := runSeq == seq
			if current {
				runCancel = nil
				runDone = nil
			}
			lock.Unlock()
			if current {
				if ctx.Err() == nil && e != nil {
					setStatus("启动失败：" + e.Error())
					if !server {
						mw.Synchronize(func() {
							walk.MsgBox(mw, "FRP 连接失败", e.Error(), walk.MsgBoxIconWarning)
						})
					}
				} else {
					setStatus("已停止")
				}
			}
		}()
	}
	stop := func() {
		appendLog("收到停止服务/连接请求")
		lock.Lock()
		cancel := runCancel
		runCancel = nil
		reconnecting = false
		reconnectSeq++
		lock.Unlock()
		if cancel != nil {
			cancel()
		}
		if !server {
			clientCfg.LastConnected = false
			if e := writeConfig(); e != nil {
				appendLog("保存断开状态失败：" + e.Error())
			}
		}
		startBtn.SetEnabled(true)
		stopBtn.SetEnabled(false)
		setStatus("已停止")
	}
	reconnect = func() {
		appendLog("收到主动重连请求")
		lock.Lock()
		cancel, done := runCancel, runDone
		if cancel == nil {
			lock.Unlock()
			start()
			return
		}
		reconnecting = true
		reconnectSeq++
		seq := reconnectSeq
		lock.Unlock()
		cancel()
		setStatus("正在断开并重新连接…")
		go func() {
			if done != nil {
				<-done
			}
			mw.Synchronize(func() {
				lock.Lock()
				if reconnectSeq != seq || !reconnecting {
					lock.Unlock()
					return
				}
				reconnecting = false
				lock.Unlock()
				start()
			})
		}()
	}
	restartCurrentService = reconnect

	mapModel.Reset(mappingsS, mappingsC)
	mapColumns := []TableViewColumn{{Title: "序号", Width: 48}, {Title: "映射备注", Width: 110}, {Title: "状态", Width: 70}, {Title: "协议", Width: 55}, {Title: "公网监听地址", Width: 135}, {Title: "端口", Width: 55}, {Title: "客户端 ID", Width: 90}, {Title: "映射 Key", Width: 205}}
	if !server {
		mapColumns = []TableViewColumn{{Title: "序号", Width: 48}, {Title: "映射备注", Width: 120}, {Title: "状态", Width: 70}, {Title: "协议", Width: 55}, {Title: "本地服务地址", Width: 160}, {Title: "本地端口", Width: 70}, {Title: "Server 公网端口", Width: 95}, {Title: "映射标识", Width: 205}}
	}
	lastLeftIndex := -1
	mapListWidget := TableView{AssignTo: &mapTable, Model: mapModel, Columns: mapColumns, AlternatingRowBG: false, LastColumnStretched: true, CustomRowHeight: 28, MinSize: Size{Height: 150}, OnItemActivated: func() {
		i := mapTable.CurrentIndex()
		if i < 0 {
			i = lastLeftIndex
			_ = mapTable.SetCurrentIndex(i)
		}
		if i >= 0 {
			openMappingDialog(i)
		}
	}, OnMouseUp: func(x, y int, button walk.MouseButton) {
		if button != walk.LeftButton {
			return
		}
		hitIndex := mapTable.IndexAt(10, y)
		// Walk raises this callback inside the list-view's mouse message. Defer
		// bookkeeping until the native control has finished updating. Clicking
		// the selected row does not toggle it; only clicking empty space clears.
		time.AfterFunc(10*time.Millisecond, func() {
			mw.Synchronize(func() {
				if hitIndex < 0 {
					lastLeftIndex = -1
					_ = mapTable.SetCurrentIndex(-1)
					return
				}
				lastLeftIndex = hitIndex
				_ = mapTable.SetCurrentIndex(hitIndex)
			})
		})
	}}
	mapButtonWidgets := []Widget{PushButton{Text: "新建映射", OnClicked: func() { openMappingDialog(-1) }}, PushButton{AssignTo: &toggleMappingButton, Text: "连接映射", Enabled: false, OnClicked: func() { toggleMapping() }}, PushButton{AssignTo: &editSelectedButton, Text: "编辑映射", Enabled: false, OnClicked: func() {
		if i := mapTable.CurrentIndex(); i >= 0 {
			openMappingDialog(i)
		}
	}}, PushButton{AssignTo: &deleteSelectedButton, Text: "删除映射", Enabled: false, OnClicked: func() { deleteMapping() }}}
	if server {
		mapButtonWidgets = append(mapButtonWidgets, PushButton{AssignTo: &copyKeyButton, Text: "复制映射 Key", Enabled: false, OnClicked: func() {
			i := mapTable.CurrentIndex()
			if i >= 0 && i < len(mappingsS) {
				if err := walk.Clipboard().SetText(mappingsS[i].Key); err != nil {
					setStatus("复制映射 Key 失败：" + err.Error())
				} else {
					setStatus("映射 Key 已复制到剪贴板")
				}
			}
		}})
	}
	mapButtonWidgets = append(mapButtonWidgets, Label{Text: "单击行选中，单击空白处取消选择。"})
	mapButtons := Composite{Layout: HBox{Spacing: 8}, Children: mapButtonWidgets}
	logPanel := GroupBox{Title: "运行日志", StretchFactor: 1, Background: SolidColorBrush{Color: walk.RGB(255, 255, 255)}, Layout: VBox{Margins: Margins{8, 6, 8, 8}}, Children: []Widget{TextEdit{AssignTo: &logView, ReadOnly: true, VScroll: true, MaxLength: 1000000, MinSize: Size{Height: 130}, StretchFactor: 1, Background: SolidColorBrush{Color: walk.RGB(12, 16, 20)}, TextColor: walk.RGB(132, 220, 150), Font: Font{Family: "Consolas", PointSize: 9}}}}
	serverControls := Composite{Layout: HBox{Spacing: 8}, Children: []Widget{PushButton{Text: "保存配置", OnClicked: func() {
		if e := collect(); e != nil {
			walk.MsgBox(mw, "请检查配置", e.Error(), walk.MsgBoxIconWarning)
		} else {
			setStatus("服务端配置已保存：" + configPath)
		}
	}}, PushButton{AssignTo: &startBtn, Text: "启动服务", OnClicked: func() { start() }}, PushButton{AssignTo: &stopBtn, Text: "停止服务", Enabled: false, OnClicked: func() { stop() }}}}
	clientControls := Composite{Layout: HBox{Spacing: 8}, Children: []Widget{PushButton{Text: "保存配置", OnClicked: func() {
		if e := collect(); e != nil {
			walk.MsgBox(mw, "请检查配置", e.Error(), walk.MsgBoxIconWarning)
		} else {
			setStatus("客户端配置已保存：" + configPath)
		}
	}}, PushButton{AssignTo: &startBtn, Text: "连接服务器", OnClicked: func() { start() }}, PushButton{AssignTo: &stopBtn, Text: "断开连接", Enabled: false, OnClicked: func() { stop() }}, PushButton{AssignTo: &reconnectBtn, Text: "重新连接", OnClicked: func() { reconnect() }}}}
	var widgets []Widget
	if server {
		widgets = []Widget{
			Label{Text: "AkitaFrp Server", Font: Font{PointSize: 21, Bold: true}, TextColor: walk.RGB(22, 76, 91)}, Label{Text: "管理公网 TCP/UDP 入口，将访客流量转发给已连接的 Client。"},
			GroupBox{Title: "Client 接入设置", Background: SolidColorBrush{Color: walk.RGB(255, 255, 255)}, Layout: VBox{Margins: Margins{12, 8, 12, 10}, Spacing: 7}, Children: []Widget{Label{Text: "此地址和端口供 Client 连接 Server 使用；访客访问的公网端口请在下方“公网端口映射”中设置。"}, Composite{Layout: Grid{Columns: 4, Spacing: 8}, Children: []Widget{Label{Text: "接入地址"}, LineEdit{AssignTo: &controlAddress, Text: serverCfg.ControlAddress}, Label{Text: "接入端口"}, LineEdit{AssignTo: &controlPort, Text: strconv.Itoa(serverCfg.ControlPort)}}}, Composite{Layout: HBox{Spacing: 8}, Children: []Widget{Label{Text: "共享 Token"}, LineEdit{AssignTo: &token, Text: serverCfg.Token, StretchFactor: 1}}}, serverControls}},
			GroupBox{Title: "公网端口映射", Background: SolidColorBrush{Color: walk.RGB(255, 255, 255)}, Layout: VBox{Margins: Margins{12, 8, 12, 10}, Spacing: 7}, Children: []Widget{Label{Text: "这里配置访客访问的公网端口和转发目标。单击行选中，单击列表空白处取消选择；双击行可编辑。"}, mapListWidget, mapButtons}},
			logPanel, HSeparator{}, Label{AssignTo: &status, Text: "状态：已停止"},
		}
	} else {
		widgets = []Widget{
			Label{Text: "AkitaFrp Client", Font: Font{PointSize: 21, Bold: true}, TextColor: walk.RGB(22, 76, 91)}, Label{Text: "填入服务器信息和本机服务地址，建立反向隧道。"},
			GroupBox{Title: "服务器连接", Background: SolidColorBrush{Color: walk.RGB(255, 255, 255)}, Layout: VBox{Margins: Margins{12, 8, 12, 10}, Spacing: 7}, Children: []Widget{Composite{Layout: Grid{Columns: 4, Spacing: 8}, Children: []Widget{Label{Text: "服务器地址"}, LineEdit{AssignTo: &serverAddress, Text: clientCfg.ServerAddress, CueBanner: "公网 IP 或域名"}, Label{Text: "服务器端口"}, LineEdit{AssignTo: &serverPort, Text: strconv.Itoa(clientCfg.ServerPort)}}}, Composite{Layout: Grid{Columns: 4, Spacing: 8}, Children: []Widget{Label{Text: "共享 Token"}, LineEdit{AssignTo: &token, Text: clientCfg.Token}, Label{Text: "客户端 ID"}, LineEdit{AssignTo: &clientID, Text: clientCfg.ClientID}}}, Label{Text: "断线后由 FRP 自动重连。"}, CheckBox{AssignTo: &resumeLastConnection, Text: "上次退出时处于连接状态，下次启动自动连接", Checked: clientCfg.ResumeLastConnection}, clientControls}},
			GroupBox{Title: "本地服务映射 · FRP 代理", Background: SolidColorBrush{Color: walk.RGB(255, 255, 255)}, Layout: VBox{Margins: Margins{12, 8, 12, 10}, Spacing: 7}, Children: []Widget{Label{Text: "填写与 Server 一致的公网端口和协议；映射标识用于生成 FRP 代理名称。单击行选中，单击列表空白处取消选择；双击行可编辑。"}, mapListWidget, mapButtons}},
			GroupBox{Title: "启动设置", Background: SolidColorBrush{Color: walk.RGB(255, 255, 255)}, Layout: VBox{Margins: Margins{12, 8, 12, 10}, Spacing: 6}, Children: []Widget{Composite{Layout: HBox{Spacing: 8}, Children: []Widget{Label{Text: "关闭窗口时"}, ComboBox{AssignTo: &closeBox, Model: []string{"最小化到系统托盘", "退出程序"}, CurrentIndex: closeMode}, CheckBox{AssignTo: &auto, Text: "随 Windows 登录启动", Checked: clientCfg.AutoStart}}}, Label{Text: "连接使用系统默认 TCP 路由。"}}},
			logPanel, HSeparator{}, Label{AssignTo: &status, Text: "状态：已停止"},
		}
	}
	windowSize, minWindowSize, maxWindowSize, initialScreenW, initialScreenH := responsiveWindowSizes()
	decl := MainWindow{AssignTo: &mw, Title: title, Size: windowSize, MinSize: minWindowSize, MaxSize: maxWindowSize, Layout: VBox{Margins: Margins{18, 16, 18, 12}, Spacing: 10}, Children: widgets, Icon: icon, Background: SolidColorBrush{Color: walk.RGB(246, 249, 250)}}
	if err = decl.Create(); err != nil {
		return err
	}
	mapTable.CurrentIndexChanged().Attach(func() { updateMappingActions() })
	appendLog("程序启动，日志文件：" + logPath)
	appendLog(fmt.Sprintf("配置文件：%s；运行模式：%s", configPath, map[bool]string{true: "Server", false: "Client"}[server]))
	if server {
		appendLog(fmt.Sprintf("Server 配置摘要：控制监听=%s:%d，映射总数=%d", serverCfg.ControlAddress, serverCfg.ControlPort, len(mappingsS)))
		for _, m := range mappingsS {
			appendLog(fmt.Sprintf("Server 映射：名称=%s，协议=%s，监听=%s:%d，ClientID=%s，Key=%s，状态=%s", m.Name, strings.ToUpper(m.Protocol), m.ListenAddress, m.ListenPort, m.ClientID, tunnelKeySummary(m.Key), map[bool]string{true: "已断开", false: "启用"}[m.Disabled]))
		}
	} else {
		appendLog(fmt.Sprintf("Client 配置摘要：Server=%s:%d，ClientID=%s，映射总数=%d，网络=%s，随 Windows 登录启动=%t，恢复上次连接=%t，上次运行状态为连接=%t", clientCfg.ServerAddress, clientCfg.ServerPort, clientCfg.ClientID, len(mappingsC), "系统默认 TCP 路由", clientCfg.AutoStart, clientCfg.ResumeLastConnection, clientCfg.LastConnected))
		for _, m := range mappingsC {
			appendLog(fmt.Sprintf("Client 映射：名称=%s，协议=%s，本地=%s:%d，Server 公网端口=%d，标识=%s，状态=%s", m.Name, strings.ToUpper(m.Protocol), m.LocalHost, m.LocalPort, m.RemotePort, tunnelKeySummary(m.Key), map[bool]string{true: "已断开", false: "启用"}[m.Disabled]))
		}
	}
	if loadErr != nil {
		if server {
			setStatus("配置读取失败，已加载默认值：" + loadErr.Error())
		} else {
			walk.MsgBox(mw, "配置文件读取失败", "将显示默认值；检查后点击保存。\n\n"+loadErr.Error(), walk.MsgBoxIconWarning)
		}
	}
	if !server {
		if clientCfg.AutoStart {
			applyAuto(true)
		}
	}
	var tray *walk.NotifyIcon
	var exitFromTray bool
	tray, err = walk.NewNotifyIcon(mw)
	if err != nil {
		return err
	}
	defer tray.Dispose()
	_ = tray.SetIcon(icon)
	trayTitle := "AkitaFrp Client"
	connectText, disconnectText, reconnectText := "连接服务器", "断开连接", "重新连接"
	if server {
		trayTitle = "AkitaFrp Server"
		connectText, disconnectText, reconnectText = "启动服务", "停止服务", "重启服务"
	}
	_ = tray.SetToolTip(trayTitle)
	restore := func() { _ = tray.SetVisible(false); mw.SetVisible(true); _ = mw.SetFocus() }
	tray.MouseDown().Attach(func(_, _ int, button walk.MouseButton) {
		if button == walk.LeftButton {
			restore()
		}
	})
	trayStatusAction = walk.NewAction()
	_ = trayStatusAction.SetText("状态：" + trayStatusText)
	_ = trayStatusAction.SetEnabled(false)
	_ = tray.ContextMenu().Actions().Add(trayStatusAction)
	_ = tray.ContextMenu().Actions().Add(walk.NewSeparatorAction())
	trayConnectAction = walk.NewAction()
	_ = trayConnectAction.SetText(connectText)
	trayConnectAction.Triggered().Attach(func() { start() })
	_ = tray.ContextMenu().Actions().Add(trayConnectAction)
	trayDisconnectAction = walk.NewAction()
	_ = trayDisconnectAction.SetText(disconnectText)
	_ = trayDisconnectAction.SetEnabled(false)
	trayDisconnectAction.Triggered().Attach(func() { stop() })
	_ = tray.ContextMenu().Actions().Add(trayDisconnectAction)
	trayReconnectAction = walk.NewAction()
	_ = trayReconnectAction.SetText(reconnectText)
	trayReconnectAction.Triggered().Attach(func() { reconnect() })
	_ = tray.ContextMenu().Actions().Add(trayReconnectAction)
	_ = tray.ContextMenu().Actions().Add(walk.NewSeparatorAction())
	restoreAction := walk.NewAction()
	_ = restoreAction.SetText("打开 " + trayTitle)
	restoreAction.Triggered().Attach(restore)
	_ = tray.ContextMenu().Actions().Add(restoreAction)
	exitAction := walk.NewAction()
	_ = exitAction.SetText("退出")
	exitAction.Triggered().Attach(func() {
		exitFromTray = true
		_ = tray.SetVisible(false)
		lock.Lock()
		cancel := runCancel
		lock.Unlock()
		if cancel != nil {
			cancel()
		}
		walk.App().Exit(0)
	})
	_ = tray.ContextMenu().Actions().Add(exitAction)
	mw.Closing().Attach(func(canceled *bool, _ walk.CloseReason) {
		if !exitFromTray && (server || closeBox.CurrentIndex() == 0) {
			*canceled = true
			mw.SetVisible(false)
			_ = tray.SetVisible(true)
		}
	})
	if !server {
		closeBox.CurrentIndexChanged().Attach(func() {
			clientCfg.CloseAction = "tray"
			if closeBox.CurrentIndex() == 1 {
				clientCfg.CloseAction = "exit"
			}
			_ = writeConfig()
		})
		auto.CheckedChanged().Attach(func() { clientCfg.AutoStart = auto.Checked(); applyAuto(clientCfg.AutoStart); _ = writeConfig() })
		resumeLastConnection.CheckedChanged().Attach(func() {
			clientCfg.ResumeLastConnection = resumeLastConnection.Checked()
			if e := writeConfig(); e != nil {
				appendLog("保存自动恢复连接选项失败：" + e.Error())
			}
		})
	}
	displayWatchDone := make(chan struct{})
	go watchDisplayChanges(mw, displayWatchDone, initialScreenW, initialScreenH)
	if !server && clientCfg.ResumeLastConnection && clientCfg.LastConnected {
		go func() {
			time.Sleep(500 * time.Millisecond)
			mw.Synchronize(func() {
				appendLog("检测到上次退出时 Client 处于连接状态，按设置自动连接")
				start()
			})
		}()
	}
	mw.Run()
	close(displayWatchDone)
	lock.Lock()
	cancel := runCancel
	lock.Unlock()
	if cancel != nil {
		cancel()
	}
	return err
}

func mapTableHitIndex(table *walk.TableView, x, y int) int {
	return table.IndexAt(x, y)
}

func tunnelKeySummary(key string) string {
	key = strings.TrimSpace(key)
	if len(key) > 8 {
		return key[:8] + "…"
	}
	if key == "" {
		return "(空)"
	}
	return key
}

func responsiveWindowSizes() (windowSize, minSize, maxSize Size, screenWidth, screenHeight int32) {
	screenWidth = win.GetSystemMetrics(win.SM_CXSCREEN)
	screenHeight = win.GetSystemMetrics(win.SM_CYSCREEN)
	if screenWidth <= 0 {
		screenWidth = 1280
	}
	if screenHeight <= 0 {
		screenHeight = 800
	}
	maxWidth := int(screenWidth) - 40
	maxHeight := int(screenHeight) - 80
	if maxWidth < 320 {
		maxWidth = int(screenWidth) - 12
	}
	if maxHeight < 360 {
		maxHeight = int(screenHeight) - 48
	}
	if maxWidth < 240 {
		maxWidth = 240
	}
	if maxHeight < 240 {
		maxHeight = 240
	}
	windowWidth, windowHeight := 860, 760
	if windowWidth > maxWidth {
		windowWidth = maxWidth
	}
	if windowHeight > maxHeight {
		windowHeight = maxHeight
	}
	minWidth, minHeight := 780, 680
	if minWidth > maxWidth {
		minWidth = maxWidth
	}
	if minHeight > maxHeight {
		minHeight = maxHeight
	}
	return Size{Width: windowWidth, Height: windowHeight}, Size{Width: minWidth, Height: minHeight}, Size{Width: maxWidth, Height: maxHeight}, screenWidth, screenHeight
}

func watchDisplayChanges(mw *walk.MainWindow, done <-chan struct{}, initialWidth, initialHeight int32) {
	lastWidth, lastHeight := initialWidth, initialHeight
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			width, height := win.GetSystemMetrics(win.SM_CXSCREEN), win.GetSystemMetrics(win.SM_CYSCREEN)
			if width <= 0 || height <= 0 || (width == lastWidth && height == lastHeight) {
				continue
			}
			lastWidth, lastHeight = width, height
			_, minSize, maxSize, _, _ := responsiveWindowSizes()
			mw.Synchronize(func() {
				min := walk.Size{Width: minSize.Width, Height: minSize.Height}
				max := walk.Size{Width: maxSize.Width, Height: maxSize.Height}
				_ = mw.SetMinMaxSize(min, max)
				current := mw.Size()
				newWidth, newHeight := current.Width, current.Height
				if newWidth > max.Width {
					newWidth = max.Width
				}
				if newHeight > max.Height {
					newHeight = max.Height
				}
				if newWidth < min.Width {
					newWidth = min.Width
				}
				if newHeight < min.Height {
					newHeight = min.Height
				}
				bounds := mw.Bounds()
				if bounds.X < 0 || bounds.Y < 0 || bounds.X+newWidth > max.Width || bounds.Y+newHeight > max.Height {
					_ = mw.SetBounds(walk.Rectangle{X: 20, Y: 20, Width: newWidth, Height: newHeight})
				} else if newWidth != current.Width || newHeight != current.Height {
					_ = mw.SetSize(walk.Size{Width: newWidth, Height: newHeight})
				}
			})
		}
	}
}

func superviseServer(ctx context.Context, config tunnel.ServerConfig, switches *tunnel.MappingSwitches, log func(string)) error {
	backoff := time.Second
	for ctx.Err() == nil {
		err := runServerSafely(ctx, config, switches, log)
		if ctx.Err() != nil {
			return nil
		}
		if err == nil {
			err = fmt.Errorf("服务意外停止")
		}
		log(fmt.Sprintf("Server 异常：%v；%s 后自动重启", err, backoff))
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
	return nil
}

func runServerSafely(ctx context.Context, config tunnel.ServerConfig, switches *tunnel.MappingSwitches, log func(string)) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log(fmt.Sprintf("Server 捕获到未处理异常：%v", recovered))
			for _, line := range strings.Split(strings.TrimSpace(string(debug.Stack())), "\n") {
				if strings.TrimSpace(line) != "" {
					log("调用栈：" + strings.TrimSpace(line))
				}
			}
			err = fmt.Errorf("内部异常：%v", recovered)
		}
	}()
	return tunnel.RunServerManaged(ctx, config, switches, log)
}

func runClientSafely(ctx context.Context, config tunnel.ClientConfig, switches *tunnel.MappingSwitches, log func(string)) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log(fmt.Sprintf("Client 捕获到未处理异常：%v", recovered))
			for _, line := range strings.Split(strings.TrimSpace(string(debug.Stack())), "\n") {
				if strings.TrimSpace(line) != "" {
					log("调用栈：" + strings.TrimSpace(line))
				}
			}
			err = fmt.Errorf("内部异常：%v", recovered)
		}
	}()
	return tunnel.RunClientManaged(ctx, config, switches, log)
}

type mappingTableModel struct {
	walk.TableModelBase
	server          bool
	clientConnected bool
	serverListening bool
	serverRows      []tunnel.Mapping
	clientRows      []tunnel.LocalMapping
}

func (m *mappingTableModel) Reset(serverRows []tunnel.Mapping, clientRows []tunnel.LocalMapping) {
	m.serverRows = append(m.serverRows[:0], serverRows...)
	m.clientRows = append(m.clientRows[:0], clientRows...)
	m.PublishRowsReset()
}
func (m *mappingTableModel) RowCount() int {
	if m.server {
		return len(m.serverRows)
	}
	return len(m.clientRows)
}
func (m *mappingTableModel) SetRuntimeState(clientConnected, serverListening bool) {
	if m.clientConnected == clientConnected && m.serverListening == serverListening {
		return
	}
	m.clientConnected, m.serverListening = clientConnected, serverListening
	for row := 0; row < m.RowCount(); row++ {
		m.PublishRowChanged(row)
	}
}
func (m *mappingTableModel) Value(row, col int) interface{} {
	if m.server {
		x := m.serverRows[row]
		switch col {
		case 0:
			return row + 1
		case 1:
			return x.Name
		case 2:
			if x.Disabled {
				return "已断开"
			}
			if m.serverListening {
				return "监听中"
			}
			return "待启动"
		case 3:
			return protocolLabel(x.Protocol)
		case 4:
			return x.ListenAddress
		case 5:
			return x.ListenPort
		case 6:
			return x.ClientID
		case 7:
			return x.Key
		}
	} else {
		x := m.clientRows[row]
		switch col {
		case 0:
			return row + 1
		case 1:
			return x.Name
		case 2:
			if x.Disabled {
				return "已断开"
			}
			if m.clientConnected {
				return "已连接"
			}
			return "待连接"
		case 3:
			return protocolLabel(x.Protocol)
		case 4:
			return x.LocalHost
		case 5:
			return x.LocalPort
		case 6:
			return x.RemotePort
		case 7:
			return x.Key
		}
	}
	return ""
}

func validProtocol(p string) bool {
	return p == "" || strings.EqualFold(p, "tcp") || strings.EqualFold(p, "udp") || strings.EqualFold(p, "tcp+udp") || strings.EqualFold(p, "tcp/udp")
}
func protocolIndex(p string) int {
	if strings.EqualFold(p, "udp") {
		return 1
	}
	if strings.EqualFold(p, "tcp+udp") || strings.EqualFold(p, "tcp/udp") {
		return 2
	}
	return 0
}
func selectedProtocol(c *walk.ComboBox) string {
	if c.CurrentIndex() == 1 {
		return "udp"
	}
	if c.CurrentIndex() == 2 {
		return "tcp+udp"
	}
	return "tcp"
}
func protocolLabel(p string) string {
	if strings.EqualFold(p, "udp") {
		return "UDP"
	}
	if strings.EqualFold(p, "tcp+udp") || strings.EqualFold(p, "tcp/udp") {
		return "TCP/UDP"
	}
	return "TCP"
}
