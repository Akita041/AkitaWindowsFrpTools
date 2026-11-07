# AkitaFrp

AkitaFrp 是 Windows 桌面界面版端口映射工具。界面、托盘、自启动、配置文件和日志由本项目维护；TCP/UDP 隧道、连接保活、转发与重连由上游开源 FRP（fatedier/frp v0.61.2）实现。

## 编译
需要 Go 1.23 或更高版本。在项目根目录运行：

```powershell
go build -trimpath -ldflags "-s -w -H=windowsgui" -o Server\publish\AkitaFrp.Server.exe .\Server
go build -trimpath -ldflags "-s -w -H=windowsgui" -o Client\publish\AkitaFrp.Client.exe .\Client
```

## 使用
1. 在 Server 的连接设置里设置监听地址、端口和共享 Token，在映射列表里创建公网端口。
2. 在 Client 填写 Server 地址、端口和同一个 Token，并添加映射。
3. Client 映射需要填写本地服务地址、本地端口，以及与 Server 一致的公网端口和协议。
4. 两端分别启动。TCP 和 UDP 由 FRP 分别建立原生代理；TCP/UDP 会建立两条代理。

## 说明

映射 Key 用于生成 FRP 代理名称。FRP 使用共享 Token 验证 Client；Server 的允许端口列表限制可代理的公网端口。请妥善保管你的共享 Token。
FRP 的 TCPMux 使用 TCP；UDP 业务通过 FRP 的 UDP 代理实现。Client 使用系统网络栈和当前 Windows 路由（包含TUN/代理软件设置）。

## 上游许可

本程序集成 [fatedier/frp 上游仓库](https://github.com/fatedier/frp)，版本 v0.61.2，遵循 Apache License 2.0。许可文本见 `THIRD_PARTY_LICENSE_frp.txt`。
