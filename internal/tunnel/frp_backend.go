package tunnel

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	frpclient "github.com/fatedier/frp/client"
	frptypes "github.com/fatedier/frp/pkg/config/types"
	v1 "github.com/fatedier/frp/pkg/config/v1"
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

// AkitaFrp keeps the existing GUI/config model and delegates all data-plane
// work to the upstream FRP client and server implementations.
func RunServerManaged(ctx context.Context, c ServerConfig, switches *MappingSwitches, log logger) error {
	initFRPLogger(log)
	log("启动 FRP Server：控制监听=" + net.JoinHostPort(c.ControlAddress, strconv.Itoa(c.ControlPort)))
	for {
		cfg := v1.ServerConfig{BindAddr: c.ControlAddress, BindPort: c.ControlPort, ProxyBindAddr: proxyBindAddress(c.Mappings), Auth: v1.AuthServerConfig{Method: "token", Token: c.Token}, Transport: v1.ServerTransportConfig{TCPMux: ptr(true), TCPMuxKeepaliveInterval: 20, TCPKeepAlive: 30}}
		for _, m := range c.Mappings {
			if switches != nil && !switches.Enabled(ServerMappingSwitchKey(m)) {
				continue
			}
			cfg.AllowPorts = append(cfg.AllowPorts, frptypes.PortsRange{Single: m.ListenPort})
		}
		// FRP treats an empty AllowPorts list as "all ports". A dummy zero
		// entry keeps the set explicitly empty when every UI mapping is off.
		if len(cfg.AllowPorts) == 0 {
			cfg.AllowPorts = []frptypes.PortsRange{{Single: 0}}
		}
		cfg.Complete()
		svc, err := frpserver.NewService(&cfg)
		if err != nil {
			return fmt.Errorf("FRP Server 初始化失败：%w", err)
		}
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { svc.Run(runCtx); close(done) }()
		log(fmt.Sprintf("控制端口监听于 %s:%d（FRP），允许公网端口数=%d", cfg.BindAddr, cfg.BindPort, len(cfg.AllowPorts)))
		changed := waitMappingChange(ctx, cancel, func() string { return serverSwitchSignature(c.Mappings, switches) })
		cancel()
		_ = svc.Close()
		<-done
		if !changed {
			return nil
		}
		log("映射开关已变更，正在重载 FRP Server")
	}
}

func RunClientManaged(ctx context.Context, c ClientConfig, switches *MappingSwitches, log logger) error {
	initFRPLogger(log)
	common := &v1.ClientCommonConfig{ServerAddr: c.ServerAddress, ServerPort: c.ServerPort, User: c.ClientID, Auth: v1.AuthClientConfig{Method: "token", Token: c.Token}, Transport: v1.ClientTransportConfig{Protocol: "tcp", TCPMux: ptr(true), TCPMuxKeepaliveInterval: 20, DialServerKeepAlive: 30}, LoginFailExit: ptr(true)}
	common.Complete()
	proxies := buildClientProxies(c.Mappings, switches, common.User)
	svc, err := frpclient.NewService(frpclient.ServiceOptions{Common: common, ProxyCfgs: proxies})
	if err != nil {
		return fmt.Errorf("FRP Client 初始化失败：%w", err)
	}
	log(fmt.Sprintf("启动官方 FRP Client：Server=%s，映射代理=%d，传输=TCP/TCPMux", net.JoinHostPort(c.ServerAddress, strconv.Itoa(c.ServerPort)), len(proxies)))
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- svc.Run(runCtx) }()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	lastSignature := clientSwitchSignature(c.Mappings, switches)
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
			<-done
			return nil
		case <-ticker.C:
			signature := clientSwitchSignature(c.Mappings, switches)
			if signature == lastSignature {
				continue
			}
			updated := buildClientProxies(c.Mappings, switches, common.User)
			if err := svc.UpdateAllConfigurer(updated, nil); err != nil {
				return fmt.Errorf("更新 FRP 映射失败：%w", err)
			}
			lastSignature = signature
			log(fmt.Sprintf("已单独更新映射代理：当前启用代理=%d；其他映射保持连接", len(updated)))
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

func waitMappingChange(ctx context.Context, cancel context.CancelFunc, signature func() string) bool {
	initial := signature()
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			if signature() != initial {
				cancel()
				return true
			}
		}
	}
}
func serverSwitchSignature(ms []Mapping, s *MappingSwitches) string {
	var b strings.Builder
	for _, m := range ms {
		fmt.Fprintf(&b, "%s=%t;", m.Key, s == nil || s.Enabled(ServerMappingSwitchKey(m)))
	}
	return b.String()
}
func clientSwitchSignature(ms []LocalMapping, s *MappingSwitches) string {
	var b strings.Builder
	for _, m := range ms {
		fmt.Fprintf(&b, "%s=%t;", m.Key, s == nil || s.Enabled(ClientMappingSwitchKey(m)))
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
