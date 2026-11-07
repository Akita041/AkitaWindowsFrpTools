package tunnel

import (
	"crypto/rand"
	"encoding/hex"
)

type ServerConfig struct {
	ControlAddress string    `json:"ControlAddress"`
	ControlPort    int       `json:"ControlPort"`
	Token          string    `json:"Token"`
	Mappings       []Mapping `json:"Mappings"`
}
type Mapping struct {
	Name          string `json:"Name"`
	Key           string `json:"Key"`
	Protocol      string `json:"Protocol"`
	Disabled      bool   `json:"Disabled,omitempty"`
	ClientID      string `json:"ClientId"`
	ListenAddress string `json:"ListenAddress"`
	ListenPort    int    `json:"ListenPort"`
}
type ClientConfig struct {
	ServerAddress        string         `json:"ServerAddress"`
	ServerPort           int            `json:"ServerPort"`
	ReconnectAttempts    int            `json:"ReconnectAttempts"`
	Token                string         `json:"Token"`
	ClientID             string         `json:"ClientId"`
	CloseAction          string         `json:"CloseAction"`
	AutoStart            bool           `json:"AutoStart"`
	ResumeLastConnection bool           `json:"ResumeLastConnection"`
	LastConnected        bool           `json:"LastConnected"`
	Mappings             []LocalMapping `json:"Mappings"`
}
type LocalMapping struct {
	Name       string `json:"Name"`
	Key        string `json:"Key"`
	Protocol   string `json:"Protocol"`
	Disabled   bool   `json:"Disabled,omitempty"`
	LocalHost  string `json:"LocalHost"`
	LocalPort  int    `json:"LocalPort"`
	RemotePort int    `json:"RemotePort"`
}

func DefaultServer() ServerConfig {
	return ServerConfig{ControlAddress: "0.0.0.0", ControlPort: 5887, Token: randomToken(), Mappings: []Mapping{{Name: "web", Key: NewMappingKey(), Protocol: "tcp", ClientID: "default", ListenAddress: "0.0.0.0", ListenPort: 8080}}}
}
func DefaultClient() ClientConfig {
	return ClientConfig{ServerAddress: "", ServerPort: 5887, ReconnectAttempts: 5, Token: "", ClientID: "", CloseAction: "tray", AutoStart: false, Mappings: []LocalMapping{}}
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err == nil {
		return hex.EncodeToString(b)
	}
	return "REPLACE-ME-WITH-A-LONG-RANDOM-SECRET"
}

func NewMappingKey() string { return randomID() }

func ServerMappingSwitchKey(m Mapping) string {
	if m.Key != "" {
		return "key:" + m.Key
	}
	return "name:" + m.Name
}

func ClientMappingSwitchKey(m LocalMapping) string {
	if m.Key != "" {
		return "key:" + m.Key
	}
	return "name:" + m.Name
}
