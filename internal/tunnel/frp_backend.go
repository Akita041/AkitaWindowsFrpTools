package tunnel

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	frpclient "github.com/fatedier/frp/client"
	v1 "github.com/fatedier/frp/pkg/config/v1"
	frpmsg "github.com/fatedier/frp/pkg/msg"
	frpplugin "github.com/fatedier/frp/pkg/plugin/server"
	frplog "github.com/fatedier/frp/pkg/util/log"
	frpserver "github.com/fatedier/frp/server"
	golog "github.com/fatedier/golib/log"
)

var frpLogOnce sync.Once

type frpLogWriter struct{ write func(string) }

func (w frpLogWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimSpace(string(p)), "\n") {
		if line != "" && w.write != nil {
			line = strings.TrimSpace(line)
			w.write("[FRP] " + line)
			if strings.Contains(strings.ToLower(line), "login to server success") {
				w.write("已连接服务器：FRP 登录成功")
			} else if strings.Contains(strings.ToLower(line), "connect to server error:") {
				w.write("连接中断：FRP 正在自动重连")
			}
		}
	}
	return len(p), nil
}

func initFRPLogger(write logger) {
	frpLogOnce.Do(func() {
		frplog.Logger = golog.New(golog.WithOutput(frpLogWriter{write: write}), golog.WithLevel(golog.InfoLevel))
	})
}

// RunServerManaged uses one FRP control listener for the whole Server. Each
// mapping is a separate FRP proxy multiplexed over a Client's shared control
// session. A local FRP HTTP plugin keeps the mapping allowlist live without
// restarting the Server or editing the upstream FRP sources.
func RunServerManaged(ctx context.Context, c ServerConfig, switches *MappingSwitches, log logger) error {
	initFRPLogger(log)
	pluginListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("创建本地映射校验接口失败：%w", err)
	}
	policy := &serverMappingPolicy{fallback: append([]Mapping(nil), c.Mappings...), switches: switches, log: log}
	pluginServer := &http.Server{Handler: http.HandlerFunc(policy.handleHTTP)}
	pluginDone := make(chan error, 1)
	go func() { pluginDone <- pluginServer.Serve(pluginListener) }()
	defer func() { _ = pluginServer.Close() }()
	pluginAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(pluginListener.Addr().(*net.TCPAddr).Port))
	cfg := v1.ServerConfig{
		BindAddr: c.ControlAddress, BindPort: c.ControlPort,
		ProxyBindAddr: proxyBindAddress(c.Mappings),
		// FRP checks actual mapping authorization through the local plugin.
		// An empty AllowPorts list means no port-pool restriction in FRP.
		Auth:        v1.AuthServerConfig{Method: "token", Token: c.Token},
		Transport:   v1.ServerTransportConfig{TCPMux: ptr(true), TCPMuxKeepaliveInterval: 20, TCPKeepAlive: 30},
		HTTPPlugins: []v1.HTTPPluginOptions{{Name: "akita-mapping-policy", Addr: "http://" + pluginAddr, Path: "/check", Ops: []string{frpplugin.OpNewProxy}}},
	}
	cfg.Complete()
	svc, err := frpserver.NewService(&cfg)
	if err != nil {
		return fmt.Errorf("FRP Server 初始化失败：%w", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() { svc.Run(runCtx); close(done) }()
	controlAddress := net.JoinHostPort(c.ControlAddress, strconv.Itoa(c.ControlPort))
	log(fmt.Sprintf("控制端口监听于 %s", controlAddress))
	log(fmt.Sprintf("FRP Server 已启动：控制监听=%s，映射数=%d；所有映射共用一个 Server 握手连接", controlAddress, len(currentServerMappings(c.Mappings, switches))))
	lastSignature := serverSwitchSignature(currentServerMappings(c.Mappings, switches), switches)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = svc.Close()
			<-done
			return nil
		case err := <-pluginDone:
			if ctx.Err() != nil || err == http.ErrServerClosed {
				return nil
			}
			_ = svc.Close()
			<-done
			return fmt.Errorf("映射校验接口异常退出：%w", err)
		case <-done:
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("FRP Server unexpectedly stopped")
		case <-ticker.C:
			mappings := currentServerMappings(c.Mappings, switches)
			signature := serverSwitchSignature(mappings, switches)
			if signature == lastSignature {
				continue
			}
			lastSignature = signature
			log(fmt.Sprintf("Server 映射规则已热更新：当前映射数=%d；Server 握手连接和其他代理保持运行", len(mappings)))
		}
	}
}

type serverMappingPolicy struct {
	fallback []Mapping
	switches *MappingSwitches
	log      logger
}

func (p *serverMappingPolicy) handleHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Op      string          `json:"op"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid plugin request", http.StatusBadRequest)
		return
	}
	reply := frpplugin.Response{Unchange: true}
	switch req.Op {
	case frpplugin.OpNewProxy:
		var content frpplugin.NewProxyContent
		if err := json.Unmarshal(req.Content, &content); err != nil {
			reply.Reject, reply.RejectReason = true, "invalid proxy configuration"
		} else if !p.proxyAllowed(content.User.User, content.NewProxy) {
			reply.Reject, reply.RejectReason = true, "Server 映射列表中没有匹配的 Key、客户端 ID、协议或公网端口"
			if p.log != nil {
				p.log(fmt.Sprintf("拒绝未匹配的 FRP 映射注册：ClientID=%s，代理=%s，公网端口=%d", content.User.User, content.ProxyName, content.RemotePort))
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(reply)
}

func (p *serverMappingPolicy) proxyAllowed(user string, proxy frpmsg.NewProxy) bool {
	for _, m := range currentServerMappings(p.fallback, p.switches) {
		if !serverMappingEnabled(m, p.switches) || strings.TrimSpace(m.ClientID) != user || m.ListenPort != proxy.RemotePort {
			continue
		}
		for _, transport := range []string{"tcp", "udp"} {
			if mappingSupports(m.Protocol, transport) && proxy.ProxyType == transport && proxy.ProxyName == user+"."+proxyName(m.Key)+"_"+transport {
				return true
			}
		}
	}
	return false
}

func currentServerMappings(fallback []Mapping, switches *MappingSwitches) []Mapping {
	if switches == nil {
		return append([]Mapping(nil), fallback...)
	}
	return switches.ServerMappings(fallback)
}

func serverMappingEnabled(m Mapping, switches *MappingSwitches) bool {
	return !m.Disabled && (switches == nil || switches.Enabled(ServerMappingSwitchKey(m)))
}

func serverSwitchSignature(ms []Mapping, s *MappingSwitches) string {
	var b strings.Builder
	for _, m := range ms {
		fmt.Fprintf(&b, "%#v=%t;", m, s == nil || s.Enabled(ServerMappingSwitchKey(m)))
	}
	return b.String()
}

func RunClientManaged(ctx context.Context, c ClientConfig, switches *MappingSwitches, log logger) error {
	initFRPLogger(log)
	common := &v1.ClientCommonConfig{ServerAddr: c.ServerAddress, ServerPort: c.ServerPort, User: c.ClientID, Auth: v1.AuthClientConfig{Method: "token", Token: c.Token}, Transport: v1.ClientTransportConfig{Protocol: "tcp", TCPMux: ptr(true), TCPMuxKeepaliveInterval: 20, DialServerKeepAlive: 30}, LoginFailExit: ptr(true)}
	common.Complete()
	initialMappings := c.Mappings
	if switches != nil {
		initialMappings = switches.ClientMappings(c.Mappings)
	}
	proxies := buildClientProxies(initialMappings, switches, common.User)
	svc, err := frpclient.NewService(frpclient.ServiceOptions{Common: common, ProxyCfgs: proxies})
	if err != nil {
		return fmt.Errorf("FRP Client 初始化失败：%w", err)
	}
	log(fmt.Sprintf("启动官方 FRP Client：Server=%s，映射代理=%d；所有映射共用一个握手和多路复用连接", net.JoinHostPort(c.ServerAddress, strconv.Itoa(c.ServerPort)), len(proxies)))
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- svc.Run(runCtx) }()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	lastSignature := clientSwitchSignature(initialMappings, switches)
	for {
		select {
		case err = <-done:
			if ctx.Err() != nil {
				return nil
			}
			if err != nil {
				return fmt.Errorf("FRP Client 已退出：%w", err)
			}
			return nil
		case <-ctx.Done():
			svc.Close()
			<-done
			return nil
		case <-ticker.C:
			currentMappings := c.Mappings
			if switches != nil {
				currentMappings = switches.ClientMappings(c.Mappings)
			}
			signature := clientSwitchSignature(currentMappings, switches)
			if signature == lastSignature {
				continue
			}
			updated := buildClientProxies(currentMappings, switches, common.User)
			if err := svc.UpdateAllConfigurer(updated, nil); err != nil {
				return fmt.Errorf("更新 FRP 映射失败：%w", err)
			}
			lastSignature = signature
			log(fmt.Sprintf("已在线更新 FRP 映射代理：当前启用代理=%d；共享握手连接保持运行", len(updated)))
		}
	}
}

func buildClientProxies(mappings []LocalMapping, switches *MappingSwitches, user string) []v1.ProxyConfigurer {
	proxies := make([]v1.ProxyConfigurer, 0, len(mappings)*2)
	for _, m := range mappings {
		if switches != nil && !switches.Enabled(ClientMappingSwitchKey(m)) {
			continue
		}
		name := proxyName(m.Key)
		if mappingSupports(m.Protocol, "tcp") {
			p := &v1.TCPProxyConfig{ProxyBaseConfig: v1.ProxyBaseConfig{Name: name + "_tcp", Type: "tcp", ProxyBackend: v1.ProxyBackend{LocalIP: m.LocalHost, LocalPort: m.LocalPort}}, RemotePort: m.RemotePort}
			p.Complete(user)
			proxies = append(proxies, p)
		}
		if mappingSupports(m.Protocol, "udp") {
			p := &v1.UDPProxyConfig{ProxyBaseConfig: v1.ProxyBaseConfig{Name: name + "_udp", Type: "udp", ProxyBackend: v1.ProxyBackend{LocalIP: m.LocalHost, LocalPort: m.LocalPort}}, RemotePort: m.RemotePort}
			p.Complete(user)
			proxies = append(proxies, p)
		}
	}
	return proxies
}

func clientSwitchSignature(ms []LocalMapping, s *MappingSwitches) string {
	var b strings.Builder
	for _, m := range ms {
		fmt.Fprintf(&b, "%#v=%t;", m, s == nil || s.Enabled(ClientMappingSwitchKey(m)))
	}
	return b.String()
}

func proxyBindAddress(ms []Mapping) string {
	for _, m := range ms {
		if strings.TrimSpace(m.ListenAddress) != "" {
			return m.ListenAddress
		}
	}
	return "0.0.0.0"
}

func proxyName(key string) string {
	h := sha256.Sum256([]byte(strings.TrimSpace(key)))
	return "akita_" + hex.EncodeToString(h[:12])
}

func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err == nil {
		return hex.EncodeToString(b)
	}
	return fmt.Sprintf("%x", time.Now().UnixNano())
}

func shortKey(key string) string {
	key = strings.TrimSpace(key)
	if len(key) > 8 {
		return key[:8] + "…"
	}
	if key == "" {
		return "(空)"
	}
	return key
}

func mappingSupports(protocol, target string) bool {
	p := strings.ToLower(strings.TrimSpace(protocol))
	return p == target || p == "tcp+udp" || p == "tcp/udp"
}

func ptr[T any](v T) *T { return &v }
